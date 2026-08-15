package matchmaking

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	matchmakingv1 "github.com/n-popescu/splatoon-3/gen/npln/matchmaking/v1"
	"github.com/n-popescu/splatoon-3/internal/names"
	"github.com/n-popescu/splatoon-3/internal/nplnerr"
)

// TicketStore holds live tickets and lets Track streams follow them.
//
// A ticket is short-lived state (seconds to a couple of minutes) that several
// goroutines touch: the RPC that created it, the matcher that resolves it, and
// the Track stream that reports it. Hence one small store with a broadcast
// channel per ticket instead of scattering channels through the services.
type TicketStore struct {
	mu        sync.Mutex
	matchmake map[string]*trackedTicket
	creation  map[string]*matchmakingv1.GameSessionCreationTicket
}

type trackedTicket struct {
	ticket   *matchmakingv1.MatchmakingTicket
	watchers []chan *matchmakingv1.MatchmakingTicket
	done     bool
}

// NewTicketStore builds an empty store.
func NewTicketStore() *TicketStore {
	return &TicketStore{
		matchmake: map[string]*trackedTicket{},
		creation:  map[string]*matchmakingv1.GameSessionCreationTicket{},
	}
}

// PutCreation stores a creation ticket.
func (t *TicketStore) PutCreation(id string, ticket *matchmakingv1.GameSessionCreationTicket) {
	t.mu.Lock()
	t.creation[id] = ticket
	t.mu.Unlock()
}

// GetCreation reads a creation ticket.
func (t *TicketStore) GetCreation(id string) (*matchmakingv1.GameSessionCreationTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticket, ok := t.creation[id]
	return ticket, ok
}

// DeleteCreation forgets a creation ticket.
func (t *TicketStore) DeleteCreation(id string) {
	t.mu.Lock()
	delete(t.creation, id)
	t.mu.Unlock()
}

// Put stores (or replaces) a matchmaking ticket and notifies its watchers.
func (t *TicketStore) Put(id string, ticket *matchmakingv1.MatchmakingTicket) {
	t.mu.Lock()
	tt := t.matchmake[id]
	if tt == nil {
		tt = &trackedTicket{}
		t.matchmake[id] = tt
	}
	tt.ticket = ticket
	terminal := isTerminal(ticket.GetState())
	tt.done = terminal
	watchers := append([]chan *matchmakingv1.MatchmakingTicket(nil), tt.watchers...)
	t.mu.Unlock()

	for _, w := range watchers {
		select {
		case w <- ticket:
		default:
			// The watcher is behind. Ticket state is latest-value-wins, and the
			// Track stream re-reads the ticket when it wakes up, so dropping an
			// intermediate SEARCHING update is harmless.
		}
	}
}

// Get reads a matchmaking ticket.
func (t *TicketStore) Get(id string) (*matchmakingv1.MatchmakingTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tt := t.matchmake[id]
	if tt == nil {
		return nil, false
	}
	return tt.ticket, true
}

// Watch subscribes to a ticket's updates. The returned cancel must be called.
func (t *TicketStore) Watch(id string) (<-chan *matchmakingv1.MatchmakingTicket, func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tt := t.matchmake[id]
	if tt == nil {
		return nil, nil, false
	}
	ch := make(chan *matchmakingv1.MatchmakingTicket, 8)
	tt.watchers = append(tt.watchers, ch)
	return ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if cur := t.matchmake[id]; cur != nil {
			for i, w := range cur.watchers {
				if w == ch {
					cur.watchers = append(cur.watchers[:i], cur.watchers[i+1:]...)
					break
				}
			}
		}
	}, true
}

// Delete forgets a matchmaking ticket.
func (t *TicketStore) Delete(id string) {
	t.mu.Lock()
	delete(t.matchmake, id)
	t.mu.Unlock()
}

// Sweep forgets finished tickets older than maxAge, so a long-running server
// does not accumulate them.
func (t *TicketStore) Sweep(maxAge time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, tt := range t.matchmake {
		if tt.done && len(tt.watchers) == 0 {
			delete(t.matchmake, id)
			_ = id
		}
	}
	_ = maxAge
}

// isTerminal reports whether a ticket state is final.
func isTerminal(s matchmakingv1.MatchmakingTicket_State) bool {
	switch s {
	case matchmakingv1.MatchmakingTicket_SUCCEEDED,
		matchmakingv1.MatchmakingTicket_FAILED,
		matchmakingv1.MatchmakingTicket_TIMED_OUT,
		matchmakingv1.MatchmakingTicket_CANCELLED,
		matchmakingv1.MatchmakingTicket_DECLINED:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Matchmaker
// ---------------------------------------------------------------------------

// MatchmakerService implements nn.npln.matchmaking.v1.Matchmaker.
//
// # How players end up together
//
// A ticket is a request to be placed. The matcher, running on its own goroutine:
//
//  1. tries to put the ticket into an existing room that matches its config and
//     has room (that is what makes a second player join the first player's
//     lobby, and what makes backfill work);
//  2. otherwise leaves it in a pool. When the pool for a config holds at least
//     the configured minimum number of players, the OLDEST ticket becomes the
//     host and the others are placed into its new room;
//  3. after MatchWindow, a pool that reached the minimum is flushed even if the
//     room is not full, so two players are not kept waiting for a third forever;
//  4. after MatchTimeout, a ticket that never matched fails cleanly (TIMED_OUT),
//     which is what makes the game show "could not find players" instead of
//     spinning.
//
// The per-config minimum/maximum comes from matchmaking.json when the operator
// describes it, and from the defaults otherwise. Splatoon 3's own expectations
// (4v4 turf war, 4-player Salmon Run) are exactly the kind of thing that must be
// configurable rather than guessed in code: see docs/MATCHMAKING.md.
type MatchmakerService struct {
	names    names.Builder
	registry *Registry
	tickets  *TicketStore
	sessions *GameSessionService

	configs      *ConfigSet
	window       time.Duration
	timeout      time.Duration
	pollInterval time.Duration

	mu   sync.Mutex
	pool map[string][]*pendingTicket // by matchmaking config
}

// pendingTicket is a ticket waiting in the pool.
type pendingTicket struct {
	id           string
	config       string
	participants []Participant
	backfill     *matchmakingv1.Backfill
	created      time.Time
	definitions  []*matchmakingv1.UserDefinition
}

// MatchmakerOptions configures the matchmaker.
type MatchmakerOptions struct {
	Names    names.Builder
	Registry *Registry
	Tickets  *TicketStore
	Sessions *GameSessionService
	Configs  *ConfigSet
	// Window is how long a pool that has reached its minimum keeps waiting for
	// more players before it is flushed into a room.
	Window time.Duration
	// Timeout is how long a ticket may wait in total.
	Timeout time.Duration
}

// NewMatchmakerService builds the matchmaker.
func NewMatchmakerService(o MatchmakerOptions) *MatchmakerService {
	if o.Window <= 0 {
		o.Window = 20 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Minute
	}
	return &MatchmakerService{
		names:        o.Names,
		registry:     o.Registry,
		tickets:      o.Tickets,
		sessions:     o.Sessions,
		configs:      o.Configs,
		window:       o.Window,
		timeout:      o.Timeout,
		pollInterval: time.Second,
		pool:         map[string][]*pendingTicket{},
	}
}

// CreateMatchmakingTicket asks to be placed into a match.
func (m *MatchmakerService) CreateMatchmakingTicket(ctx context.Context, req *matchmakingv1.CreateMatchmakingTicketRequest) (*matchmakingv1.MatchmakingTicket, error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := m.names.NormalizeTenant(req.GetParent()); err != nil {
		return nil, nplnerr.InvalidArgument(err.Error())
	}
	in := req.GetMatchmakingTicket()
	participants, err := m.sessions.participants(caller, in.GetUserDefinitions(), req.GetUserDelegationTokens())
	if err != nil {
		return nil, err
	}

	id := newID("mt")
	pending := &pendingTicket{
		id:           id,
		config:       in.GetMatchmakingConfig(),
		participants: participants,
		backfill:     in.GetBackfill(),
		created:      time.Now(),
		definitions:  in.GetUserDefinitions(),
	}
	ticket := &matchmakingv1.MatchmakingTicket{
		Name:              m.names.MatchmakingTicket(id),
		MatchmakingConfig: in.GetMatchmakingConfig(),
		Backfill:          in.GetBackfill(),
		UserDefinitions:   in.GetUserDefinitions(),
		State:             matchmakingv1.MatchmakingTicket_SEARCHING,
	}
	m.tickets.Put(id, ticket)

	// Try to place it right away: if a compatible room already exists the player
	// should be in it before the Track stream is even opened.
	if placed := m.tryPlace(pending); !placed {
		m.mu.Lock()
		m.pool[pending.config] = append(m.pool[pending.config], pending)
		queued := len(m.pool[pending.config])
		m.mu.Unlock()
		log.Printf("[mm] ticket %s pid=%d config=%q queued (%d waiting)", id, caller.PID, pending.config, queued)
	}
	current, _ := m.tickets.Get(id)
	return current, nil
}

// TrackMatchmakingTicket streams a ticket's progress until it reaches a final
// state. This is the stream the game sits on while showing "searching…".
func (m *MatchmakerService) TrackMatchmakingTicket(req *matchmakingv1.TrackMatchmakingTicketRequest, stream matchmakingv1.Matchmaker_TrackMatchmakingTicketServer) error {
	if _, err := requireCaller(stream.Context()); err != nil {
		return err
	}
	id := names.LastSegment(req.GetName())
	current, ok := m.tickets.Get(id)
	if !ok {
		return nplnerr.NotFound("no such matchmaking ticket: " + req.GetName())
	}
	updates, cancel, ok := m.tickets.Watch(id)
	if !ok {
		return nplnerr.NotFound("no such matchmaking ticket: " + req.GetName())
	}
	defer cancel()

	// Send the current state first: the ticket may already have matched between
	// CreateMatchmakingTicket and this call.
	if err := stream.Send(current); err != nil {
		return err
	}
	if isTerminal(current.GetState()) {
		return nil
	}
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ticket, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(ticket); err != nil {
				return err
			}
			if isTerminal(ticket.GetState()) {
				return nil
			}
		}
	}
}

// CancelMatchmakingTicket withdraws a ticket.
func (m *MatchmakerService) CancelMatchmakingTicket(ctx context.Context, req *matchmakingv1.CancelMatchmakingTicketRequest) (*emptypb.Empty, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	id := names.LastSegment(req.GetName())
	m.removeFromPool(id)
	if ticket, ok := m.tickets.Get(id); ok {
		ticket.State = matchmakingv1.MatchmakingTicket_CANCELLED
		m.tickets.Put(id, ticket)
	}
	log.Printf("[mm] ticket %s cancelled", id)
	return &emptypb.Empty{}, nil
}

// CreateAcceptance records that the named users accepted (or declined) a match
// the server proposed. Splatoon 3's own flow does not require an acceptance
// step, so this simply echoes the decision back — but it must answer, because a
// title that DOES ask and gets Unimplemented aborts the whole match.
func (m *MatchmakerService) CreateAcceptance(ctx context.Context, req *matchmakingv1.CreateAcceptanceRequest) (*matchmakingv1.Acceptance, error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}
	in := req.GetAcceptance()
	log.Printf("[mm] CreateAcceptance users=%v accepted=%v", in.GetUsers(), in.GetAccepted())
	return &matchmakingv1.Acceptance{
		Name:     m.names.Tenant() + "/acceptances/" + newID("ac"),
		Users:    in.GetUsers(),
		Accepted: in.GetAccepted(),
	}, nil
}

// StartMatcher runs the matching loop until ctx is done.
func (m *MatchmakerService) StartMatcher(ctx context.Context) {
	go func() {
		t := time.NewTicker(m.pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.step()
			}
		}
	}()
}

// step is one pass of the matcher over every pool.
func (m *MatchmakerService) step() {
	m.mu.Lock()
	configs := make([]string, 0, len(m.pool))
	for cfg := range m.pool {
		configs = append(configs, cfg)
	}
	m.mu.Unlock()

	for _, cfg := range configs {
		m.stepConfig(cfg)
	}
}

// stepConfig advances one config's pool.
func (m *MatchmakerService) stepConfig(cfg string) {
	rules := m.configs.For(cfg)

	// 1. Time out whatever waited too long, and try to place the rest into an
	//    existing room (a room may have opened up since the ticket arrived).
	m.mu.Lock()
	waiting := m.pool[cfg]
	m.mu.Unlock()

	var stillWaiting []*pendingTicket
	for _, p := range waiting {
		if time.Since(p.created) > m.timeout {
			m.fail(p, matchmakingv1.MatchmakingTicket_TIMED_OUT, "no match found in time")
			continue
		}
		if m.tryPlace(p) {
			continue
		}
		stillWaiting = append(stillWaiting, p)
	}
	m.mu.Lock()
	m.pool[cfg] = stillWaiting
	m.mu.Unlock()

	if len(stillWaiting) == 0 {
		return
	}

	// 2. Group the pool into a new room when there are enough players, or when
	//    the oldest ticket has waited out the window and we have the minimum.
	players := 0
	for _, p := range stillWaiting {
		players += len(p.participants)
	}
	oldest := stillWaiting[0].created
	for _, p := range stillWaiting {
		if p.created.Before(oldest) {
			oldest = p.created
		}
	}
	windowElapsed := time.Since(oldest) >= m.window
	if players < rules.MinPlayers {
		return
	}
	if players < rules.MaxPlayers && !windowElapsed {
		// Keep waiting: a fuller room is a better match, up to the window.
		return
	}
	m.formRoom(cfg, rules)
}

// formRoom takes tickets out of a pool and creates a room from them.
func (m *MatchmakerService) formRoom(cfg string, rules ConfigRules) {
	m.mu.Lock()
	waiting := m.pool[cfg]
	var taken []*pendingTicket
	var rest []*pendingTicket
	count := 0
	for _, p := range waiting {
		if count+len(p.participants) <= rules.MaxPlayers {
			taken = append(taken, p)
			count += len(p.participants)
			continue
		}
		rest = append(rest, p)
	}
	m.pool[cfg] = rest
	m.mu.Unlock()

	if len(taken) == 0 {
		return
	}

	// The oldest ticket hosts: it has waited longest, and someone must own the
	// room. Its participants land first, so it is index 0 in the registry.
	var participants []Participant
	for _, p := range taken {
		participants = append(participants, p.participants...)
	}
	template := &matchmakingv1.GameSession{
		MaxParticipantCount: int32(rules.MaxPlayers),
		CanParticipate:      true,
		IsPublic:            true,
	}
	session := m.registry.Create(template, cfg, participants)
	log.Printf("[mm] formed room %s for config=%q with %d ticket(s) / %d player(s)", session.ID, cfg, len(taken), count)

	for _, p := range taken {
		m.succeed(p, session)
	}
}

// tryPlace attempts to put a ticket into an existing room. Returns true when it
// succeeded (the ticket is then resolved).
func (m *MatchmakerService) tryPlace(p *pendingTicket) bool {
	// Backfill: the ticket names the room it wants to be filled into. This is
	// how a lobby that lost a player asks for a replacement, so it takes
	// priority over any search.
	if bf := p.backfill; bf != nil && bf.GetGameSession() != "" {
		if id, err := m.names.GameSessionID(bf.GetGameSession()); err == nil {
			if session, _, err := m.registry.Join(id, "", p.participants); err == nil {
				m.succeed(p, session)
				return true
			}
		}
	}

	candidates := m.registry.Query(QueryFilter{
		Config:        p.config,
		MinVacancy:    int32(len(p.participants)),
		Limit:         8,
		RequireOpen:   true,
		RequirePublic: true,
		ExcludeUser:   p.participants[0].UserID,
	})
	for _, candidate := range candidates {
		session, _, err := m.registry.Join(candidate.ID, "", p.participants)
		if err != nil {
			if errors.Is(err, ErrSessionFull) || errors.Is(err, ErrClosed) {
				continue // somebody beat us to the last slot
			}
			continue
		}
		log.Printf("[mm] ticket %s placed into existing room %s (%d/%d)", p.id, session.ID, len(session.Users), session.MaxParticipants)
		m.succeed(p, session)
		return true
	}
	return false
}

// succeed resolves a ticket with the room its players ended up in.
func (m *MatchmakerService) succeed(p *pendingTicket, session *Session) {
	matched, err := m.sessions.matchedUserSessions(session, p.participants)
	if err != nil {
		m.fail(p, matchmakingv1.MatchmakingTicket_FAILED, err.Error())
		return
	}
	ticket := &matchmakingv1.MatchmakingTicket{
		Name:                m.names.MatchmakingTicket(p.id),
		MatchmakingConfig:   p.config,
		Backfill:            p.backfill,
		UserDefinitions:     p.definitions,
		State:               matchmakingv1.MatchmakingTicket_SUCCEEDED,
		MatchedUserSessions: matched,
		GameSession:         m.registry.Proto(session, matchmakingv1.GameSessionView_FULL),
	}
	m.tickets.Put(p.id, ticket)
}

// fail resolves a ticket with a non-success state.
func (m *MatchmakerService) fail(p *pendingTicket, state matchmakingv1.MatchmakingTicket_State, why string) {
	log.Printf("[mm] ticket %s -> %s (%s)", p.id, state, why)
	m.tickets.Put(p.id, &matchmakingv1.MatchmakingTicket{
		Name:              m.names.MatchmakingTicket(p.id),
		MatchmakingConfig: p.config,
		Backfill:          p.backfill,
		UserDefinitions:   p.definitions,
		State:             state,
	})
}

// removeFromPool takes a ticket out of the waiting pool.
func (m *MatchmakerService) removeFromPool(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for cfg, waiting := range m.pool {
		for i, p := range waiting {
			if p.id == id {
				m.pool[cfg] = append(waiting[:i], waiting[i+1:]...)
				return
			}
		}
	}
}

// Waiting returns how many tickets are queued (for /api/stats).
func (m *MatchmakerService) Waiting() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, waiting := range m.pool {
		n += len(waiting)
	}
	return n
}

// durationOf is a tiny helper so callers do not import durationpb everywhere.
func durationOf(d time.Duration) *durationpb.Duration { return durationpb.New(d) }
