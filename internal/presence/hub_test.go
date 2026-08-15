package presence

import (
	"testing"
	"time"

	commonpb "github.com/n-popescu/splatoon-3/gen/npln/common"
	friendsv1 "github.com/n-popescu/splatoon-3/gen/npln/friends/v1"

	"github.com/n-popescu/splatoon-3/internal/account"
	"github.com/n-popescu/splatoon-3/internal/names"
)

func testHub(ttl time.Duration) *Hub {
	return New(Options{
		Names:    names.Builder{TenantID: "t-dce9377b-lp1"},
		Accounts: account.New("http://127.0.0.1:1", "", time.Millisecond),
		AppID:    "0100c2500fc20000",
		TTL:      ttl,
	})
}

func attrs(key, value string) map[string]*commonpb.Value {
	return map[string]*commonpb.Value{
		key: {ValueType: &commonpb.Value_StringValue{StringValue: value}},
	}
}

// TestSetAndGet stores a presence and reads it back.
func TestSetAndGet(t *testing.T) {
	h := testHub(time.Minute)
	h.Set("u-abc", 1800000042, friendsv1.State_ONLINE, attrs("lobby", "gs-1"))
	e, ok := h.Get("u-abc")
	if !ok {
		t.Fatal("presence not found")
	}
	if e.State != friendsv1.State_ONLINE || e.PID != 1800000042 {
		t.Errorf("entry = %+v", e)
	}
	if got := e.Attributes["lobby"].GetStringValue(); got != "gs-1" {
		t.Errorf("attributes were not stored: %q", got)
	}
}

// TestHeartbeatDoesNotClearAttributes is a real trap: a keepalive with no
// attributes means "still here", not "I left my lobby". Clearing them would make
// a friend's joinable room blink out of existence every few seconds.
func TestHeartbeatDoesNotClearAttributes(t *testing.T) {
	h := testHub(time.Minute)
	h.Set("u-abc", 1, friendsv1.State_ONLINE, attrs("lobby", "gs-1"))
	h.Set("u-abc", 1, friendsv1.State_ONLINE, nil)
	e, _ := h.Get("u-abc")
	if e.Attributes["lobby"].GetStringValue() != "gs-1" {
		t.Error("a heartbeat cleared the presence attributes")
	}
}

// TestStalePresenceIsNotServed: a console that stopped keeping alive must not stay
// "online" for its friends.
func TestStalePresenceIsNotServed(t *testing.T) {
	h := testHub(20 * time.Millisecond)
	h.Set("u-abc", 1, friendsv1.State_ONLINE, nil)
	time.Sleep(40 * time.Millisecond)
	if _, ok := h.Get("u-abc"); ok {
		t.Error("a stale presence is still served")
	}
	if pids := h.ActivePIDs(); len(pids) != 0 {
		t.Errorf("ActivePIDs = %v, want none", pids)
	}
}

// TestTouchKeepsPlayerOnline: any authenticated RPC counts as proof of life, so a
// player whose keepalive stream broke does not blink offline.
func TestTouchKeepsPlayerOnline(t *testing.T) {
	h := testHub(50 * time.Millisecond)
	h.Set("u-abc", 7, friendsv1.State_ONLINE, nil)
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		h.Touch("u-abc", 7)
	}
	if _, ok := h.Get("u-abc"); !ok {
		t.Error("a player being touched went stale")
	}
}

// TestActivePIDsFeedsTheAccountServer: the reported set is what makes friends see
// "playing Splatoon 3", so it must contain exactly the live players.
func TestActivePIDsFeedsTheAccountServer(t *testing.T) {
	h := testHub(time.Minute)
	h.Set("u-a", 1, friendsv1.State_ONLINE, nil)
	h.Set("u-b", 2, friendsv1.State_ONLINE, nil)
	h.Set("u-c", 3, friendsv1.State_OFFLINE, nil)
	got := map[uint64]bool{}
	for _, pid := range h.ActivePIDs() {
		got[pid] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("online players missing from ActivePIDs: %v", got)
	}
	if got[3] {
		t.Error("an offline player is reported as playing")
	}
}

// TestSubscribeReceivesUpdates covers the fan-out a SubscribePresences stream
// relies on.
func TestSubscribeReceivesUpdates(t *testing.T) {
	h := testHub(time.Minute)
	updates, cancel := h.Subscribe([]string{"u-friend"})
	defer cancel()

	h.Set("u-friend", 9, friendsv1.State_ONLINE, attrs("mode", "turf"))
	select {
	case e := <-updates:
		if e.UserID != "u-friend" || e.State != friendsv1.State_ONLINE {
			t.Errorf("update = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no update was delivered to the subscriber")
	}

	// A presence for somebody we did not subscribe to must not be delivered.
	h.Set("u-stranger", 10, friendsv1.State_ONLINE, nil)
	select {
	case e := <-updates:
		t.Errorf("received an unrelated presence: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestOfflineIsPublishedImmediately: when a player leaves, their friends should
// know at once rather than after the TTL.
func TestOfflineIsPublishedImmediately(t *testing.T) {
	h := testHub(time.Minute)
	h.Set("u-abc", 1, friendsv1.State_ONLINE, nil)
	updates, cancel := h.Subscribe([]string{"u-abc"})
	defer cancel()
	h.Offline("u-abc")
	select {
	case e := <-updates:
		if e.State != friendsv1.State_OFFLINE {
			t.Errorf("state = %s, want OFFLINE", e.State)
		}
	case <-time.After(time.Second):
		t.Fatal("going offline was not published")
	}
}

// TestPresenceForFallsBackToTheAccountServer is the other half of the "friends
// never appear online" fix: a friend who is online in ANOTHER game (or just on the
// console) must show as ONLINE here, not offline.
func TestPresenceForFallsBackToTheAccountServer(t *testing.T) {
	h := testHub(time.Minute)

	playingElsewhere := &account.Friend{
		PID:    1800000002,
		UserID: "u-friend",
		Presence: map[string]any{
			"status": float64(account.PresencePlaying),
			"app_id": "0100f8f0000a2000", // Splatoon 2
		},
	}
	p := h.PresenceFor("u-friend", playingElsewhere)
	if p.GetState() != friendsv1.State_ONLINE {
		t.Errorf("state = %s, want ONLINE for a friend playing another game", p.GetState())
	}

	offlineFriend := &account.Friend{PID: 3, UserID: "u-off", Presence: map[string]any{"status": float64(account.PresenceOffline)}}
	if got := h.PresenceFor("u-off", offlineFriend).GetState(); got != friendsv1.State_OFFLINE {
		t.Errorf("state = %s, want OFFLINE", got)
	}

	// A local Splatoon 3 presence always wins over the account server's view,
	// because only it carries the attributes needed to join.
	h.Set("u-friend", 1800000002, friendsv1.State_ONLINE, attrs("lobby", "gs-7"))
	if got := h.PresenceFor("u-friend", playingElsewhere); got.GetAttributes()["lobby"].GetStringValue() != "gs-7" {
		t.Error("the local presence did not win over the account server's")
	}
}

// TestProtoNamesThePresenceResource: the name must be the documented resource path,
// which is what the client matches on.
func TestProtoNamesThePresenceResource(t *testing.T) {
	h := testHub(time.Minute)
	h.Set("u-abc", 1, friendsv1.State_ONLINE, nil)
	e, _ := h.Get("u-abc")
	if got := h.Proto(e).GetName(); got != "tenants/t-dce9377b-lp1/users/u-abc/presence" {
		t.Errorf("name = %q", got)
	}
}
