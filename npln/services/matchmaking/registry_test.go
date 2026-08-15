package matchmaking

import (
	"testing"
	"time"

	commonpb "github.com/NextendoNetwork/splatoon-3/gen/npln/common"
	matchmakingv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/matchmaking/v1"

	"github.com/NextendoNetwork/splatoon-3/npln/names"
)

func testRegistry(ttl time.Duration) *Registry {
	return NewRegistry(names.Builder{TenantID: "t-dce9377b-lp1"}, ttl)
}

func player(user string, pid uint64) Participant {
	return Participant{UserID: user, PID: pid, NsaID: "nsa-" + user}
}

// TestCreateAndJoin covers the normal room lifecycle.
func TestCreateAndJoin(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true, IsPublic: true},
		"tenants/current/matchmakingConfigs/regular", []Participant{player("u-host", 1)})

	if got := len(s.Users); got != 1 {
		t.Fatalf("new room has %d players, want 1", got)
	}
	if s.HostUser().UserID != "u-host" {
		t.Errorf("host = %q, want u-host", s.HostUser().UserID)
	}

	if _, joined, err := r.Join(s.ID, "", []Participant{player("u-joiner", 2)}); err != nil {
		t.Fatalf("Join: %v", err)
	} else if len(joined) != 1 {
		t.Fatalf("Join created %d user sessions, want 1", len(joined))
	}
	if got := len(s.Users); got != 2 {
		t.Errorf("room holds %d players after the join, want 2", got)
	}
	// The host must not change when somebody joins.
	if s.HostUser().UserID != "u-host" {
		t.Errorf("host changed to %q after a join", s.HostUser().UserID)
	}
}

// TestJoinIsIdempotent: a client that re-sends its join (a reconnect, or a retry)
// must not be counted twice — a 9/8 room is exactly the bug the NEX side hit.
func TestJoinIsIdempotent(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	for i := 0; i < 3; i++ {
		if _, _, err := r.Join(s.ID, "", []Participant{player("u-joiner", 2)}); err != nil {
			t.Fatalf("Join %d: %v", i, err)
		}
	}
	if got := len(s.Users); got != 2 {
		t.Errorf("room holds %d players after three identical joins, want 2", got)
	}
}

// TestJoinFullRoom must report the specific "full" error, because that is what
// makes the client look for another room instead of failing the match.
func TestJoinFullRoom(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 2, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	if _, _, err := r.Join(s.ID, "", []Participant{player("u-2", 2)}); err != nil {
		t.Fatalf("second player: %v", err)
	}
	_, _, err := r.Join(s.ID, "", []Participant{player("u-3", 3)})
	if err != ErrSessionFull {
		t.Fatalf("third player error = %v, want ErrSessionFull", err)
	}
	if got := len(s.Users); got != 2 {
		t.Errorf("room grew past its limit: %d players", got)
	}
}

// TestJoinWrongPassword: the password must actually be checked.
func TestJoinWrongPassword(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true, Password: "secret"}, "", []Participant{player("u-host", 1)})
	if _, _, err := r.Join(s.ID, "wrong", []Participant{player("u-2", 2)}); err != ErrWrongPassword {
		t.Fatalf("error = %v, want ErrWrongPassword", err)
	}
	if _, _, err := r.Join(s.ID, "secret", []Participant{player("u-2", 2)}); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
}

// TestJoinClosedRoom: a room that stopped accepting players (the match started)
// must refuse joiners.
func TestJoinClosedRoom(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	if _, err := r.Sync(s.ID, "u-host", &matchmakingv1.GameSession{
		Name:                "tenants/current/gameSessions/" + s.ID,
		CanParticipate:      false,
		MaxParticipantCount: 4,
	}, false); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, _, err := r.Join(s.ID, "", []Participant{player("u-2", 2)}); err != ErrClosed {
		t.Fatalf("error = %v, want ErrClosed", err)
	}
}

// TestOnlyHostPublishesPeerAddress: a joiner must not be able to rewrite the
// room's peer address — that would redirect everybody's P2P traffic.
func TestOnlyHostPublishesPeerAddress(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	if _, _, err := r.Join(s.ID, "", []Participant{player("u-2", 2)}); err != nil {
		t.Fatal(err)
	}
	name := "tenants/current/gameSessions/" + s.ID

	if _, err := r.Sync(s.ID, "u-host", &matchmakingv1.GameSession{Name: name, Host: "198.51.100.10", Port: 30000, CanParticipate: true}, false); err != nil {
		t.Fatal(err)
	}
	if s.Host != "198.51.100.10" || s.Port != 30000 {
		t.Fatalf("host address = %s:%d, want 198.51.100.10:30000", s.Host, s.Port)
	}
	if _, err := r.Sync(s.ID, "u-2", &matchmakingv1.GameSession{Name: name, Host: "203.0.113.5", Port: 40000, CanParticipate: true}, false); err != nil {
		t.Fatal(err)
	}
	if s.Host != "198.51.100.10" || s.Port != 30000 {
		t.Errorf("a joiner rewrote the peer address to %s:%d", s.Host, s.Port)
	}
}

// TestHostLeavingRemovesRoom: nothing migrates a peer-to-peer host, so the room
// must die with it rather than being handed to the next player looking for a match.
func TestHostLeavingRemovesRoom(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	if _, _, err := r.Join(s.ID, "", []Participant{player("u-2", 2)}); err != nil {
		t.Fatal(err)
	}
	r.Leave("u-host")
	if _, ok := r.Get(s.ID); ok {
		t.Error("the room survived its host leaving")
	}
}

// TestJoinerLeavingKeepsRoom: a joiner leaving must free its slot, not the room.
func TestJoinerLeavingKeepsRoom(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	if _, _, err := r.Join(s.ID, "", []Participant{player("u-2", 2)}); err != nil {
		t.Fatal(err)
	}
	r.Leave("u-2")
	got, ok := r.Get(s.ID)
	if !ok {
		t.Fatal("the room disappeared when a joiner left")
	}
	if len(got.Users) != 1 {
		t.Errorf("room holds %d players, want 1", len(got.Users))
	}
}

// TestReapDropsAbandonedRooms: a room whose host stopped syncing must be reaped,
// or players get matched into a lobby whose host no longer exists.
func TestReapDropsAbandonedRooms(t *testing.T) {
	r := testRegistry(10 * time.Millisecond)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	time.Sleep(30 * time.Millisecond)
	if n := r.Reap(); n != 1 {
		t.Fatalf("Reap removed %d rooms, want 1", n)
	}
	if _, ok := r.Get(s.ID); ok {
		t.Error("the abandoned room is still registered")
	}
}

// TestSyncKeepsRoomAlive: syncing must reset the reaper's clock.
func TestSyncKeepsRoomAlive(t *testing.T) {
	r := testRegistry(50 * time.Millisecond)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		if _, err := r.Sync(s.ID, "u-host", nil, true); err != nil {
			t.Fatal(err)
		}
		if n := r.Reap(); n != 0 {
			t.Fatalf("Reap removed a room that is being synced (iteration %d)", i)
		}
	}
}

// TestQueryFiltersOnProperties covers the room browser's property matching: a
// query must match a room that carries EXTRA properties, but not one whose value
// differs.
func TestQueryFiltersOnProperties(t *testing.T) {
	r := testRegistry(time.Minute)
	props := func(pairs map[string]int64) *commonpb.MapValue {
		fields := map[string]*commonpb.Value{}
		for k, v := range pairs {
			fields[k] = &commonpb.Value{ValueType: &commonpb.Value_IntegerValue{IntegerValue: v}}
		}
		return &commonpb.MapValue{Fields: fields}
	}
	wanted := r.Create(&matchmakingv1.GameSession{
		MaxParticipantCount: 8, CanParticipate: true, IsPublic: true,
		Properties: props(map[string]int64{"mode": 1, "stage": 7, "extra": 99}),
	}, "cfg", []Participant{player("u-a", 1)})
	r.Create(&matchmakingv1.GameSession{
		MaxParticipantCount: 8, CanParticipate: true, IsPublic: true,
		Properties: props(map[string]int64{"mode": 2}),
	}, "cfg", []Participant{player("u-b", 2)})

	found := r.Query(QueryFilter{Properties: props(map[string]int64{"mode": 1}), RequireOpen: true, RequirePublic: true})
	if len(found) != 1 || found[0].ID != wanted.ID {
		t.Fatalf("query matched %d room(s), want exactly the mode=1 room", len(found))
	}
}

// TestQueryPrefersFullerRooms: filling a nearly-complete room beats spreading
// players across empty ones.
func TestQueryPrefersFullerRooms(t *testing.T) {
	r := testRegistry(time.Minute)
	r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 8, CanParticipate: true, IsPublic: true}, "cfg", []Participant{player("u-a", 1)})
	fuller := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 8, CanParticipate: true, IsPublic: true}, "cfg", []Participant{player("u-b", 2)})
	if _, _, err := r.Join(fuller.ID, "", []Participant{player("u-c", 3)}); err != nil {
		t.Fatal(err)
	}
	found := r.Query(QueryFilter{Config: "cfg", RequireOpen: true, RequirePublic: true})
	if len(found) < 2 {
		t.Fatalf("query returned %d room(s), want 2", len(found))
	}
	if found[0].ID != fuller.ID {
		t.Error("the fuller room is not first")
	}
}

// TestQueryHidesPrivateRoomsButFindsFriends: a plain browse must not expose a
// private room, but a "where is my friend" query must find it.
func TestQueryHidesPrivateRoomsButFindsFriends(t *testing.T) {
	r := testRegistry(time.Minute)
	private := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 8, CanParticipate: true, IsPublic: false}, "cfg", []Participant{player("u-friend", 9)})
	if got := r.Query(QueryFilter{Config: "cfg", RequireOpen: true, RequirePublic: true}); len(got) != 0 {
		t.Errorf("a public browse returned %d private room(s)", len(got))
	}
	got := r.Query(QueryFilter{Config: "cfg", Users: []string{"u-friend"}, RequireOpen: true})
	if len(got) != 1 || got[0].ID != private.ID {
		t.Error("a friend lookup did not find the private room")
	}
}

// TestRoomCodes: a code must resolve back to its room, and be freed with it.
func TestRoomCodes(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true}, "", []Participant{player("u-host", 1)})
	code, err := r.SetAlias(s.ID, "")
	if err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if got, ok := r.ResolveAlias(code); !ok || got.ID != s.ID {
		t.Fatalf("ResolveAlias(%q) did not return the room", code)
	}
	r.Leave("u-host")
	if _, ok := r.ResolveAlias(code); ok {
		t.Error("the code still resolves after the room was removed")
	}
}

// TestProtoHidesPasswordAndUsers: a BASIC view (what a browse returns) must not
// carry the room password or its players' attributes.
func TestProtoHidesPasswordAndUsers(t *testing.T) {
	r := testRegistry(time.Minute)
	s := r.Create(&matchmakingv1.GameSession{MaxParticipantCount: 4, CanParticipate: true, Password: "secret"}, "", []Participant{player("u-host", 1)})
	basic := r.Proto(s, matchmakingv1.GameSessionView_BASIC)
	if basic.GetPassword() != "" {
		t.Error("the BASIC view leaks the room password")
	}
	if len(basic.GetUserSessions()) != 0 {
		t.Error("the BASIC view lists the players")
	}
	full := r.Proto(s, matchmakingv1.GameSessionView_FULL)
	if len(full.GetUserSessions()) != 1 {
		t.Error("the FULL view does not list the players")
	}
	if full.GetPassword() != "" {
		t.Error("even the FULL view must not echo the password back")
	}
}
