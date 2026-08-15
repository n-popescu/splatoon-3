// Package matchmaking implements nn.npln.matchmaking.v1.GameSessionService and
// nn.npln.matchmaking.v1.Matchmaker.
//
// # The model
//
// A *game session* is a room. It has a host (the console running the match), a
// participant limit, an opaque property map the game fills in (mode, stage,
// rule, …), and a *user session* per player in it. Sessions live in memory and
// disappear when their host stops syncing — a room is by definition transient.
//
// Two ways in:
//
//	CreateGameSessionCreationTicket  the host creates the room. Splatoon 3 sends
//	                                 the room it wants (limit, properties,
//	                                 password), we register it and hand back the
//	                                 created session with its user sessions.
//	CreateMatchmakingTicket          a player asks to be placed. The matchmaker
//	                                 puts them in a compatible existing room, or
//	                                 groups waiting tickets into a new one.
//
// # Who publishes the peer address
//
// GameSession carries `host` and `port`. This server never invents them: the
// host publishes its own reachable address through SyncGameSession, and joiners
// read it back from the session. That mirrors how the Nextendo NEX servers deal
// with station URLs (the server relays what the host reported, it does not guess)
// and it is the only correct behaviour when the transport is peer-to-peer.
//
// NAT traversal itself is ICE: see ice.go and AllocateIceServerSet.
package matchmaking

import (
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	commonpb "github.com/NextendoNetwork/splatoon-3/gen/npln/common"
	matchmakingv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/matchmaking/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
)

// UserSession is one player inside a game session.
type UserSession struct {
	ID     string
	UserID string
	PID    uint64
	NsaID  string
	Team   string
	State  matchmakingv1.UserSession_State
	// Attributes is whatever the client attached to its user definition (player
	// name, gear, rank …). Passed through untouched.
	Attributes *commonpb.MapValue
	Latency    *matchmakingv1.LatencyData
	CreatedAt  time.Time
	LastSeen   time.Time
}

// Session is a room.
type Session struct {
	ID     string
	Config string // matchmaking config resource name, "" for a plain room
	// Host / Port are the peer address the HOST published. Empty until it does.
	Host string
	Port int32

	MaxParticipants int32
	Password        string
	IsPublic        bool
	CanParticipate  bool
	State           matchmakingv1.GameSession_State
	Properties      *commonpb.MapValue

	// Users is ordered: index 0 is the host's user session.
	Users []*UserSession

	ShortAlias string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// HostUser returns the host's user session (nil for an empty room).
func (s *Session) HostUser() *UserSession {
	if len(s.Users) == 0 {
		return nil
	}
	return s.Users[0]
}

// Vacancy returns how many more players fit.
func (s *Session) Vacancy() int32 {
	if s.MaxParticipants <= 0 {
		return 0
	}
	v := s.MaxParticipants - int32(len(s.Users))
	if v < 0 {
		return 0
	}
	return v
}

// Registry holds every live session. Safe for concurrent use.
type Registry struct {
	names names.Builder
	ttl   time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	byAlias  map[string]string // short code -> session id
	rnd      *rand.Rand
}

// NewRegistry builds the registry. ttl is how long a session survives without a
// SyncGameSession from its host.
func NewRegistry(nb names.Builder, ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &Registry{
		names:    nb,
		ttl:      ttl,
		sessions: map[string]*Session{},
		byAlias:  map[string]string{},
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Participant is a player being placed into a session.
type Participant struct {
	UserID     string
	PID        uint64
	NsaID      string
	Team       string
	Attributes *commonpb.MapValue
	Latency    *matchmakingv1.LatencyData
}

// Create registers a new session from the template the client sent plus its
// first participants. The template is honoured field by field; only the fields
// the SERVER owns (name, ids, timestamps, state, participant count) are set here.
func (r *Registry) Create(template *matchmakingv1.GameSession, config string, participants []Participant) *Session {
	now := time.Now()
	s := &Session{
		ID:              newID("gs"),
		Config:          config,
		State:           matchmakingv1.GameSession_ACTIVE,
		CanParticipate:  true,
		IsPublic:        true,
		MaxParticipants: 8,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if template != nil {
		if template.GetMaxParticipantCount() > 0 {
			s.MaxParticipants = template.GetMaxParticipantCount()
		}
		s.Password = template.GetPassword()
		s.IsPublic = template.GetIsPublic()
		s.CanParticipate = template.GetCanParticipate()
		s.Properties = template.GetProperties()
		s.Host = template.GetHost()
		s.Port = template.GetPort()
		// A client that did not set can_participate on the room it is creating
		// still means to let people in — a room nobody may join would simply
		// never fill, and every joiner would open its own. (The NEX side of this
		// stack hit exactly that with OpenParticipation.)
		if !template.GetCanParticipate() && template.GetMaxParticipantCount() > 1 {
			s.CanParticipate = true
		}
	}
	for i, p := range participants {
		s.Users = append(s.Users, newUserSession(p, i == 0, now))
	}
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.mu.Unlock()
	log.Printf("[mm] created session %s config=%s host=%q port=%d max=%d players=%d",
		s.ID, config, s.Host, s.Port, s.MaxParticipants, len(s.Users))
	return s
}

// Join adds participants to an existing session.
//
// Errors are deliberately typed so the service layer can answer with the exact
// NPLN detail code the client acts on: a full room makes it look for another
// one, a wrong password makes it say "wrong code".
var (
	ErrNoSuchSession = fmt.Errorf("matchmaking: no such session")
	ErrSessionFull   = fmt.Errorf("matchmaking: session is full")
	ErrWrongPassword = fmt.Errorf("matchmaking: wrong password")
	ErrClosed        = fmt.Errorf("matchmaking: session is not accepting participants")
)

// Join places participants into a session, returning the session and the user
// sessions that were created (or reused, when a player re-joins).
func (r *Registry) Join(sessionID, password string, participants []Participant) (*Session, []*UserSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	if s == nil {
		return nil, nil, ErrNoSuchSession
	}
	if s.Password != "" && s.Password != password {
		return nil, nil, ErrWrongPassword
	}
	if !s.CanParticipate || s.State != matchmakingv1.GameSession_ACTIVE {
		return nil, nil, ErrClosed
	}
	now := time.Now()
	var created []*UserSession
	for _, p := range participants {
		if existing := findUser(s, p.UserID); existing != nil {
			// Re-join (reconnect, or the client asking twice): refresh in place
			// instead of stacking a duplicate player into the room.
			existing.LastSeen = now
			existing.State = matchmakingv1.UserSession_ACTIVE
			if p.Attributes != nil {
				existing.Attributes = p.Attributes
			}
			created = append(created, existing)
			continue
		}
		if s.Vacancy() <= 0 {
			return nil, nil, ErrSessionFull
		}
		us := newUserSession(p, false, now)
		s.Users = append(s.Users, us)
		created = append(created, us)
	}
	s.UpdatedAt = now
	return s, created, nil
}

// Sync applies the host's update to a session and refreshes its liveness.
//
// Only the host may change the room's shape: the peer address, the participant
// limit, whether it is joinable, and the property map are the host's to publish.
// A joiner calling Sync only refreshes its own presence in the room (which is
// what keep_alive_only means).
func (r *Registry) Sync(sessionID, callerUserID string, update *matchmakingv1.GameSession, keepAliveOnly bool) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	if s == nil {
		return nil, ErrNoSuchSession
	}
	now := time.Now()
	if us := findUser(s, callerUserID); us != nil {
		us.LastSeen = now
	}
	s.UpdatedAt = now
	if keepAliveOnly || update == nil {
		return s, nil
	}
	host := s.HostUser()
	if host == nil || host.UserID != callerUserID {
		// Not an error: a joiner syncing is normal, we simply do not let it
		// rewrite somebody else's room.
		return s, nil
	}
	if update.GetHost() != "" {
		if s.Host != update.GetHost() || s.Port != update.GetPort() {
			log.Printf("[mm] session %s host published its address: %s:%d", s.ID, update.GetHost(), update.GetPort())
		}
		s.Host = update.GetHost()
		s.Port = update.GetPort()
	}
	if update.GetMaxParticipantCount() > 0 {
		s.MaxParticipants = update.GetMaxParticipantCount()
	}
	if update.GetProperties() != nil {
		s.Properties = update.GetProperties()
	}
	s.CanParticipate = update.GetCanParticipate()
	s.IsPublic = update.GetIsPublic()
	if update.GetPassword() != "" {
		s.Password = update.GetPassword()
	}
	if st := update.GetState(); st != matchmakingv1.GameSession_STATE_UNSPECIFIED {
		s.State = st
	}
	return s, nil
}

// Get returns a session by id.
func (r *Registry) Get(sessionID string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sessionID]
	return s, ok
}

// QueryFilter describes a QueryGameSessions request.
type QueryFilter struct {
	Config        string
	MinVacancy    int32
	Properties    *commonpb.MapValue
	Users         []string
	Limit         int
	ExcludeUser   string
	RequireOpen   bool
	RequirePublic bool
}

// Query returns the sessions matching a filter, best (fullest joinable) first —
// filling a nearly-complete room beats spreading players across empty ones.
func (r *Registry) Query(f QueryFilter) []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Session
	for _, s := range r.sessions {
		if !r.matches(s, f) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Users) != len(out[j].Users) {
			return len(out[i].Users) > len(out[j].Users)
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// matches applies a query filter to a session. Caller holds the lock.
func (r *Registry) matches(s *Session, f QueryFilter) bool {
	if s.State != matchmakingv1.GameSession_ACTIVE {
		return false
	}
	if f.RequireOpen && !s.CanParticipate {
		return false
	}
	if f.RequirePublic && !s.IsPublic {
		return false
	}
	if f.Config != "" && s.Config != "" && s.Config != f.Config {
		return false
	}
	if f.MinVacancy > 0 && s.Vacancy() < f.MinVacancy {
		return false
	}
	if f.ExcludeUser != "" && findUser(s, f.ExcludeUser) != nil {
		return false
	}
	if len(f.Users) > 0 {
		// "Which room are these users in?" — used to follow a friend into their
		// lobby. Any match is enough.
		hit := false
		for _, u := range f.Users {
			if findUser(s, u) != nil {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	// Property filter: every key the client asked for must be present with an
	// equal value. Extra properties on the session are fine — that is how a
	// "regular battle, this stage set" query matches a room that also carries
	// the host's own bookkeeping.
	if !propertiesSubset(f.Properties, s.Properties) {
		return false
	}
	return true
}

// Leave removes a user from a session, deleting the room when it empties or when
// its host leaves.
func (r *Registry) Leave(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		us := findUser(s, userID)
		if us == nil {
			continue
		}
		isHost := s.HostUser() == us
		filtered := s.Users[:0]
		for _, u := range s.Users {
			if u.UserID != userID {
				filtered = append(filtered, u)
			}
		}
		s.Users = filtered
		s.UpdatedAt = time.Now()
		if isHost || len(s.Users) == 0 {
			// The room belongs to its host; nothing here migrates a P2P host,
			// so it dies with them. Leaving it registered would hand a dead
			// room to the next player looking for a match.
			r.remove(id)
			log.Printf("[mm] session %s removed (host left / empty)", id)
		}
	}
}

// FindByUser returns the session a user is in.
func (r *Registry) FindByUser(userID string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if findUser(s, userID) != nil {
			return s, true
		}
	}
	return nil, false
}

// SetAlias attaches a short room code to a session, generating one when the
// client did not ask for a specific code.
func (r *Registry) SetAlias(sessionID, code string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	if s == nil {
		return "", ErrNoSuchSession
	}
	if code == "" {
		for i := 0; i < 32; i++ {
			candidate := r.randomCode()
			if _, taken := r.byAlias[candidate]; !taken {
				code = candidate
				break
			}
		}
		if code == "" {
			return "", fmt.Errorf("matchmaking: could not allocate a room code")
		}
	} else if other, taken := r.byAlias[code]; taken && other != sessionID {
		return "", fmt.Errorf("matchmaking: room code %s is already in use", code)
	}
	if s.ShortAlias != "" && s.ShortAlias != code {
		delete(r.byAlias, s.ShortAlias)
	}
	s.ShortAlias = code
	r.byAlias[code] = sessionID
	return code, nil
}

// ResolveAlias turns a room code back into a session.
func (r *Registry) ResolveAlias(code string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byAlias[strings.ToUpper(code)]
	if !ok {
		id, ok = r.byAlias[code]
	}
	if !ok {
		return nil, false
	}
	s, ok := r.sessions[id]
	return s, ok
}

// Sessions returns a snapshot of every live session (for the dashboard).
func (r *Registry) Sessions() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Reap deletes sessions nobody has synced within the TTL.
//
// Without this, a crashed host leaves a room that still looks joinable: players
// are matched into it, wait for a peer that no longer exists, and get a
// communication error. The NEX servers in this stack learned the same lesson.
func (r *Registry) Reap() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, s := range r.sessions {
		if time.Since(s.UpdatedAt) <= r.ttl {
			continue
		}
		log.Printf("[mm] session %s reaped: no sync for %s (%d player(s))", id, time.Since(s.UpdatedAt).Truncate(time.Second), len(s.Users))
		r.remove(id)
		n++
	}
	return n
}

// StartReaper runs Reap on a loop.
func (r *Registry) StartReaper(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(r.ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.Reap()
			}
		}
	}()
}

// remove deletes a session and its alias. Caller holds the lock.
func (r *Registry) remove(id string) {
	if s := r.sessions[id]; s != nil && s.ShortAlias != "" {
		delete(r.byAlias, s.ShortAlias)
	}
	delete(r.sessions, id)
}

// randomCode generates a room code. The alphabet skips the characters players
// misread aloud (0/O, 1/I), because these codes get typed from a friend's screen.
func (r *Registry) randomCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[r.rnd.Intn(len(alphabet))]
	}
	return string(b)
}

// Proto renders a session as the protobuf resource.
//
// view controls how much is included: BASIC hides the user sessions (used by
// room browsing, where the joiner has no business reading everybody's
// attributes), FULL includes them.
func (r *Registry) Proto(s *Session, view matchmakingv1.GameSessionView) *matchmakingv1.GameSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.protoLocked(s, view)
}

func (r *Registry) protoLocked(s *Session, view matchmakingv1.GameSessionView) *matchmakingv1.GameSession {
	out := &matchmakingv1.GameSession{
		Name:                    r.names.GameSession(s.ID),
		MaxParticipantCount:     s.MaxParticipants,
		CurrentParticipantCount: int32(len(s.Users)),
		CanParticipate:          s.CanParticipate,
		IsPublic:                s.IsPublic,
		State:                   s.State,
		Host:                    s.Host,
		Port:                    s.Port,
		CreateTime:              timestamppb.New(s.CreatedAt),
		Properties:              s.Properties,
	}
	// The password is a secret shared between the host and the players it gave
	// the code to; it is never echoed into a browse result.
	if view == matchmakingv1.GameSessionView_FULL || view == matchmakingv1.GameSessionView_USER_SESSION_BASIC {
		for _, us := range s.Users {
			out.UserSessions = append(out.UserSessions, r.userProtoLocked(s, us, view))
		}
	}
	return out
}

// UserProto renders a user session.
func (r *Registry) UserProto(s *Session, us *UserSession) *matchmakingv1.UserSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.userProtoLocked(s, us, matchmakingv1.GameSessionView_FULL)
}

func (r *Registry) userProtoLocked(s *Session, us *UserSession, view matchmakingv1.GameSessionView) *matchmakingv1.UserSession {
	out := &matchmakingv1.UserSession{
		Name:       r.names.UserSession(s.ID, us.ID),
		User:       r.names.User(us.UserID),
		CreateTime: timestamppb.New(us.CreatedAt),
		State:      us.State,
		Team:       us.Team,
	}
	if view != matchmakingv1.GameSessionView_USER_SESSION_BASIC {
		out.Attributes = us.Attributes
		out.LatencyData = us.Latency
	}
	return out
}

// newUserSession builds a user session for a participant.
func newUserSession(p Participant, host bool, now time.Time) *UserSession {
	_ = host // the host is simply index 0; kept for readability at the call site
	return &UserSession{
		ID:         newID("us"),
		UserID:     p.UserID,
		PID:        p.PID,
		NsaID:      p.NsaID,
		Team:       p.Team,
		State:      matchmakingv1.UserSession_ACTIVE,
		Attributes: p.Attributes,
		Latency:    p.Latency,
		CreatedAt:  now,
		LastSeen:   now,
	}
}

// findUser returns a user session by NPLN user id. Caller holds the lock.
func findUser(s *Session, userID string) *UserSession {
	for _, u := range s.Users {
		if u.UserID == userID {
			return u
		}
	}
	return nil
}

// propertiesSubset reports whether every key/value in want is present in have.
func propertiesSubset(want, have *commonpb.MapValue) bool {
	if want == nil || len(want.GetFields()) == 0 {
		return true
	}
	if have == nil {
		return false
	}
	for k, wv := range want.GetFields() {
		hv, ok := have.GetFields()[k]
		if !ok || !valueEqual(wv, hv) {
			return false
		}
	}
	return true
}

// valueEqual compares two NPLN Values. Only the scalar kinds a matchmaking
// filter can meaningfully use are compared; anything else falls back to the
// protobuf text form, which is exact for the nested cases too.
func valueEqual(a, b *commonpb.Value) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch av := a.GetValueType().(type) {
	case *commonpb.Value_IntegerValue:
		bv, ok := b.GetValueType().(*commonpb.Value_IntegerValue)
		return ok && av.IntegerValue == bv.IntegerValue
	case *commonpb.Value_StringValue:
		bv, ok := b.GetValueType().(*commonpb.Value_StringValue)
		return ok && av.StringValue == bv.StringValue
	case *commonpb.Value_BooleanValue:
		bv, ok := b.GetValueType().(*commonpb.Value_BooleanValue)
		return ok && av.BooleanValue == bv.BooleanValue
	case *commonpb.Value_DoubleValue:
		bv, ok := b.GetValueType().(*commonpb.Value_DoubleValue)
		return ok && av.DoubleValue == bv.DoubleValue
	case *commonpb.Value_FloatValue:
		bv, ok := b.GetValueType().(*commonpb.Value_FloatValue)
		return ok && av.FloatValue == bv.FloatValue
	default:
		return a.String() == b.String()
	}
}

// newID mints a resource id with a readable prefix ("gs-…", "us-…", "mt-…").
//
// Resource ids are not secrets — every resource is access-checked against the
// caller's token — so a cheap PRNG is appropriate; it only needs to not collide.
func newID(prefix string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	idMu.Lock()
	defer idMu.Unlock()
	b := make([]byte, 16)
	for i := range b {
		b[i] = alphabet[idRand.Intn(len(alphabet))]
	}
	return prefix + "-" + string(b)
}

var (
	idMu   sync.Mutex
	idRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)
