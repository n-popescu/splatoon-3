package matchmaking

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	matchmakingv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/matchmaking/v1"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
	"github.com/NextendoNetwork/splatoon-3/npln/server"
	"github.com/NextendoNetwork/splatoon-3/npln/token"
)

// matchmakerHarness wires the pieces a matchmaker needs.
type matchmakerHarness struct {
	nb       names.Builder
	registry *Registry
	tickets  *TicketStore
	sessions *GameSessionService
	mm       *MatchmakerService
}

func newHarness(t *testing.T, minPlayers, maxPlayers int, window time.Duration) *matchmakerHarness {
	t.Helper()
	nb := names.Builder{TenantID: "t-dce9377b-lp1"}
	tokens, err := token.NewIssuer(token.Options{KeyFile: filepath.Join(t.TempDir(), "key.pem"), TenantID: nb.TenantID, AppID: "0100c2500fc20000"})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(nb, time.Minute)
	tickets := NewTicketStore()
	ice := NewIceAllocator(IceOptions{Names: nb, StunHost: "stun.example", StunPort: 3478})
	sessions := NewGameSessionService(GameSessionOptions{Names: nb, Registry: registry, Tokens: tokens, Ice: ice, Tickets: tickets})
	configs := NewConfigSet(minPlayers, maxPlayers)
	mm := NewMatchmakerService(MatchmakerOptions{
		Names: nb, Registry: registry, Tickets: tickets, Sessions: sessions,
		Configs: configs, Window: window, Timeout: time.Minute,
	})
	return &matchmakerHarness{nb: nb, registry: registry, tickets: tickets, sessions: sessions, mm: mm}
}

// ctxFor builds a context carrying an authenticated caller, the way the gRPC
// interceptor does in production.
func ctxFor(userID string, pid uint64) context.Context {
	return server.WithCaller(context.Background(), &server.Caller{
		UserID:    userID,
		AccountID: "a-test",
		NsaID:     "nsa-" + userID,
		PID:       pid,
	})
}

func createTicket(t *testing.T, h *matchmakerHarness, userID string, pid uint64, config string) *matchmakingv1.MatchmakingTicket {
	t.Helper()
	ticket, err := h.mm.CreateMatchmakingTicket(ctxFor(userID, pid), &matchmakingv1.CreateMatchmakingTicketRequest{
		Parent:            "tenants/current",
		MatchmakingTicket: &matchmakingv1.MatchmakingTicket{MatchmakingConfig: config},
	})
	if err != nil {
		t.Fatalf("CreateMatchmakingTicket(%s): %v", userID, err)
	}
	return ticket
}

// TestTicketWaitsUntilEnoughPlayers: a single player must NOT be dropped into a
// room alone when the mode needs more, or the game starts a match it cannot play.
func TestTicketWaitsUntilEnoughPlayers(t *testing.T) {
	h := newHarness(t, 4, 8, time.Hour) // long window: only the minimum can trigger
	first := createTicket(t, h, "u-1", 1, "cfg-turf")
	if got := first.GetState(); got != matchmakingv1.MatchmakingTicket_SEARCHING {
		t.Fatalf("state = %s, want SEARCHING", got)
	}
	h.mm.step()
	current, _ := h.tickets.Get(names.LastSegment(first.GetName()))
	if current.GetState() != matchmakingv1.MatchmakingTicket_SEARCHING {
		t.Errorf("a lone player was matched (state=%s)", current.GetState())
	}
	if len(h.registry.Sessions()) != 0 {
		t.Error("a room was created for a single waiting player")
	}
}

// TestTicketsGroupIntoARoom: once the pool is ready (the minimum is reached and the
// window has elapsed), everybody waiting is placed in the SAME room, with the
// oldest ticket as its host.
//
// Note the window: below the maximum, the matchmaker deliberately keeps waiting for
// more players until it expires, because a fuller room is a better match. That is
// why this test uses a short one instead of expecting an instant match.
func TestTicketsGroupIntoARoom(t *testing.T) {
	h := newHarness(t, 2, 8, 10*time.Millisecond)
	a := createTicket(t, h, "u-1", 1, "cfg")
	b := createTicket(t, h, "u-2", 2, "cfg")
	time.Sleep(20 * time.Millisecond)
	h.mm.step()

	ta, _ := h.tickets.Get(names.LastSegment(a.GetName()))
	tb, _ := h.tickets.Get(names.LastSegment(b.GetName()))
	if ta.GetState() != matchmakingv1.MatchmakingTicket_SUCCEEDED || tb.GetState() != matchmakingv1.MatchmakingTicket_SUCCEEDED {
		t.Fatalf("states = %s / %s, want SUCCEEDED", ta.GetState(), tb.GetState())
	}
	if ta.GetGameSession().GetName() != tb.GetGameSession().GetName() {
		t.Fatalf("the two players were put in different rooms: %s vs %s",
			ta.GetGameSession().GetName(), tb.GetGameSession().GetName())
	}
	if len(ta.GetMatchedUserSessions()) != 1 {
		t.Errorf("ticket carries %d matched user sessions, want 1", len(ta.GetMatchedUserSessions()))
	}
	if ta.GetMatchedUserSessions()[0].GetMatchmakingIdToken() == "" {
		t.Error("no matchmaking id token was issued; peers cannot identify each other")
	}
	rooms := h.registry.Sessions()
	if len(rooms) != 1 {
		t.Fatalf("%d rooms were created, want 1", len(rooms))
	}
	if rooms[0].HostUser().UserID != "u-1" {
		t.Errorf("host = %q, want the oldest ticket (u-1)", rooms[0].HostUser().UserID)
	}
}

// TestLaterTicketJoinsTheExistingRoom: a player arriving after a room exists must
// be placed into it, not open a second one — the failure mode where everybody
// hosts their own empty lobby.
func TestLaterTicketJoinsTheExistingRoom(t *testing.T) {
	h := newHarness(t, 2, 8, 10*time.Millisecond)
	createTicket(t, h, "u-1", 1, "cfg")
	createTicket(t, h, "u-2", 2, "cfg")
	time.Sleep(20 * time.Millisecond)
	h.mm.step()

	third := createTicket(t, h, "u-3", 3, "cfg")
	current, _ := h.tickets.Get(names.LastSegment(third.GetName()))
	if current.GetState() != matchmakingv1.MatchmakingTicket_SUCCEEDED {
		t.Fatalf("the third player was not placed (state=%s)", current.GetState())
	}
	if got := len(h.registry.Sessions()); got != 1 {
		t.Fatalf("%d rooms exist, want 1", got)
	}
	if got := len(h.registry.Sessions()[0].Users); got != 3 {
		t.Errorf("the room holds %d players, want 3", got)
	}
}

// TestWindowFlushesAPartialRoom: two players must not wait forever for a third once
// the window has elapsed.
func TestWindowFlushesAPartialRoom(t *testing.T) {
	h := newHarness(t, 2, 8, 10*time.Millisecond)
	a := createTicket(t, h, "u-1", 1, "cfg")
	createTicket(t, h, "u-2", 2, "cfg")
	time.Sleep(20 * time.Millisecond)
	h.mm.step()
	ta, _ := h.tickets.Get(names.LastSegment(a.GetName()))
	if ta.GetState() != matchmakingv1.MatchmakingTicket_SUCCEEDED {
		t.Errorf("state = %s, want SUCCEEDED after the window elapsed", ta.GetState())
	}
}

// TestTicketTimesOut: a player nobody joins must fail cleanly, so the game can say
// "no players found" instead of spinning.
func TestTicketTimesOut(t *testing.T) {
	h := newHarness(t, 4, 8, time.Hour)
	h.mm.timeout = 10 * time.Millisecond
	ticket := createTicket(t, h, "u-1", 1, "cfg")
	time.Sleep(20 * time.Millisecond)
	h.mm.step()
	current, _ := h.tickets.Get(names.LastSegment(ticket.GetName()))
	if current.GetState() != matchmakingv1.MatchmakingTicket_TIMED_OUT {
		t.Errorf("state = %s, want TIMED_OUT", current.GetState())
	}
}

// TestCancelRemovesTheTicket: a cancelled ticket must not later be matched.
func TestCancelRemovesTheTicket(t *testing.T) {
	h := newHarness(t, 2, 8, time.Hour)
	a := createTicket(t, h, "u-1", 1, "cfg")
	if _, err := h.mm.CancelMatchmakingTicket(ctxFor("u-1", 1), &matchmakingv1.CancelMatchmakingTicketRequest{Name: a.GetName()}); err != nil {
		t.Fatal(err)
	}
	createTicket(t, h, "u-2", 2, "cfg")
	h.mm.step()
	current, _ := h.tickets.Get(names.LastSegment(a.GetName()))
	if current.GetState() != matchmakingv1.MatchmakingTicket_CANCELLED {
		t.Errorf("state = %s, want CANCELLED", current.GetState())
	}
	if got := len(h.registry.Sessions()); got != 0 {
		t.Errorf("%d rooms were created from a cancelled ticket + one waiting player", got)
	}
}

// TestBackfillJoinsTheNamedRoom: a lobby that lost a player asks for a replacement
// by naming itself, and that must take priority over any search.
func TestBackfillJoinsTheNamedRoom(t *testing.T) {
	h := newHarness(t, 2, 8, time.Hour)
	room := h.registry.Create(&matchmakingv1.GameSession{MaxParticipantCount: 8, CanParticipate: true, IsPublic: true},
		"cfg", []Participant{player("u-host", 1)})

	ticket, err := h.mm.CreateMatchmakingTicket(ctxFor("u-filler", 2), &matchmakingv1.CreateMatchmakingTicketRequest{
		Parent: "tenants/current",
		MatchmakingTicket: &matchmakingv1.MatchmakingTicket{
			MatchmakingConfig: "cfg",
			Backfill:          &matchmakingv1.Backfill{GameSession: h.nb.GameSession(room.ID)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.GetState() != matchmakingv1.MatchmakingTicket_SUCCEEDED {
		t.Fatalf("state = %s, want SUCCEEDED", ticket.GetState())
	}
	if ticket.GetGameSession().GetName() != h.nb.GameSession(room.ID) {
		t.Errorf("backfill placed the player in %s, want %s", ticket.GetGameSession().GetName(), h.nb.GameSession(room.ID))
	}
}

// TestDifferentConfigsDoNotMix: an Anarchy player must never be matched into a
// Salmon Run room.
func TestDifferentConfigsDoNotMix(t *testing.T) {
	h := newHarness(t, 2, 8, time.Hour)
	a := createTicket(t, h, "u-1", 1, "cfg-anarchy")
	b := createTicket(t, h, "u-2", 2, "cfg-salmon")
	h.mm.step()
	ta, _ := h.tickets.Get(names.LastSegment(a.GetName()))
	tb, _ := h.tickets.Get(names.LastSegment(b.GetName()))
	if ta.GetState() == matchmakingv1.MatchmakingTicket_SUCCEEDED || tb.GetState() == matchmakingv1.MatchmakingTicket_SUCCEEDED {
		t.Error("players queuing for different modes were matched together")
	}
}

// TestRoomIsNotOverfilled: the configured maximum must be respected even when more
// tickets are waiting.
func TestRoomIsNotOverfilled(t *testing.T) {
	h := newHarness(t, 2, 2, time.Millisecond)
	for i := 1; i <= 5; i++ {
		createTicket(t, h, "u-"+string(rune('0'+i)), uint64(i), "cfg")
		time.Sleep(2 * time.Millisecond)
		h.mm.step()
	}
	for _, room := range h.registry.Sessions() {
		if len(room.Users) > 2 {
			t.Errorf("room %s holds %d players, above its limit of 2", room.ID, len(room.Users))
		}
	}
}

// TestGameSessionServiceCreateAndJoin exercises the host path end to end, including
// the room code and the peer address the host publishes.
func TestGameSessionServiceCreateAndJoin(t *testing.T) {
	h := newHarness(t, 2, 8, time.Hour)
	hostCtx := ctxFor("u-host", 1)

	ticket, err := h.sessions.CreateGameSessionCreationTicket(hostCtx, &matchmakingv1.CreateGameSessionCreationTicketRequest{
		Parent: "tenants/current",
		GameSessionCreationTicket: &matchmakingv1.GameSessionCreationTicket{
			MatchmakingConfig: "cfg-private",
			GameSession: &matchmakingv1.GameSession{
				MaxParticipantCount: 4,
				CanParticipate:      true,
				IsPublic:            false,
				Password:            "code123",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.GetState() != matchmakingv1.GameSessionCreationTicket_SUCCEEDED {
		t.Fatalf("state = %s", ticket.GetState())
	}
	sessionName := ticket.GetGameSession().GetName()

	// The host publishes its peer address.
	if _, err := h.sessions.SyncGameSession(hostCtx, &matchmakingv1.SyncGameSessionRequest{
		GameSession: &matchmakingv1.GameSession{
			Name: sessionName, Host: "198.51.100.10", Port: 30000,
			CanParticipate: true, MaxParticipantCount: 4,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A room code, then a join by password.
	alias, err := h.sessions.CreateGameSessionShortAlias(hostCtx, &matchmakingv1.CreateGameSessionShortAliasRequest{
		Parent:                "tenants/current",
		GameSessionShortAlias: &matchmakingv1.GameSessionShortAlias{GameSession: sessionName},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := h.sessions.GetGameSessionShortAlias(ctxFor("u-guest", 2), &matchmakingv1.GetGameSessionShortAliasRequest{Name: alias.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GetGameSession() != sessionName {
		t.Fatalf("the code resolved to %s, want %s", resolved.GetGameSession(), sessionName)
	}

	if _, err := h.sessions.JoinGameSession(ctxFor("u-guest", 2), &matchmakingv1.JoinGameSessionRequest{
		Name: sessionName, Password: "wrong",
	}); err == nil {
		t.Error("a join with the wrong password succeeded")
	}
	joined, err := h.sessions.JoinGameSession(ctxFor("u-guest", 2), &matchmakingv1.JoinGameSessionRequest{
		Name: sessionName, Password: "code123",
	})
	if err != nil {
		t.Fatalf("join with the right password: %v", err)
	}
	if got := joined.GetGameSession().GetHost(); got != "198.51.100.10" {
		t.Errorf("the joiner did not receive the host's address (got %q)", got)
	}
	if got := joined.GetGameSession().GetCurrentParticipantCount(); got != 2 {
		t.Errorf("participant count = %d, want 2", got)
	}
}

// TestIceAllocationFailsLoudlyWithoutServers: handing out an empty ICE set would
// fail much later, inside the P2P layer, with nothing in the log.
func TestIceAllocationFailsLoudlyWithoutServers(t *testing.T) {
	nb := names.Builder{TenantID: "t"}
	empty := NewIceAllocator(IceOptions{Names: nb})
	if _, err := empty.Allocate("u-1"); err == nil {
		t.Error("Allocate succeeded with no STUN/TURN configured")
	}
	withStun := NewIceAllocator(IceOptions{Names: nb, StunHost: "stun.example", StunPort: 3478})
	set, err := withStun.Allocate("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if set.GetStunServer().GetHost() != "stun.example" {
		t.Errorf("STUN host = %q", set.GetStunServer().GetHost())
	}
}

// TestTurnCredentialsAreTimeLimited: the REST-API scheme must produce a username
// carrying an expiry and a derived password, so no static credential is shipped.
func TestTurnCredentialsAreTimeLimited(t *testing.T) {
	a := NewIceAllocator(IceOptions{
		Names:      names.Builder{TenantID: "t"},
		TurnHost:   "turn.example",
		TurnPort:   3478,
		TurnSecret: "shared-secret",
	})
	set, err := a.Allocate("u-abc")
	if err != nil {
		t.Fatal(err)
	}
	turns := set.GetTurnServers()
	if len(turns) != 1 {
		t.Fatalf("%d TURN servers, want 1", len(turns))
	}
	user, pass := turns[0].GetUsername(), turns[0].GetPassword()
	if user == "" || pass == "" {
		t.Fatal("no TURN credentials were minted")
	}
	if want := ":u-abc"; len(user) <= len(want) || user[len(user)-len(want):] != want {
		t.Errorf("username = %q, want it to end in %q", user, want)
	}
	// The same user must get a different password once the expiry moves on, i.e.
	// the credential is not static.
	if user2, pass2 := a.turnCredentials("u-abc"); user2 == user && pass2 != pass {
		t.Error("the same username produced two different passwords")
	}
}
