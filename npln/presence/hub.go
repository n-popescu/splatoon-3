// Package presence is the live presence hub.
//
// Two things feed it, and both matter for the friend experience:
//
//	the game        PresenceService.KeepAlive carries the player's own presence:
//	                ONLINE/OFFLINE plus a map of game attributes (which lobby it
//	                is in, whether it can be joined, …). We store it, fan it out
//	                to whoever subscribed, and expire it if the keepalive stops.
//	the account     nextendo-account knows the whole Nextendo network: a friend
//	                playing Splatoon 2, or sitting in the Switch home menu, is
//	                online too, and Splatoon 3 should show them as such.
//
// It also pushes the other way: the set of players currently in Splatoon 3 is
// reported to nextendo-account on a loop, which is what makes a Switch friend
// list say "playing Splatoon 3" instead of showing everybody offline. That
// direction was the missing half of presence in the stack (only a NEX game
// server and the emulator ever reported anything), and it is the reason friends
// appeared permanently offline — see docs/FRIENDS.md.
package presence

import (
	"context"
	"log"
	"sync"
	"time"

	commonpb "github.com/NextendoNetwork/splatoon-3/gen/npln/common"
	friendsv1 "github.com/NextendoNetwork/splatoon-3/gen/npln/friends/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NextendoNetwork/splatoon-3/npln/account"
	"github.com/NextendoNetwork/splatoon-3/npln/names"
)

// Entry is one player's presence as this server knows it.
type Entry struct {
	UserID string
	PID    uint64
	State  friendsv1.State
	// Attributes is the game's own presence payload. Splatoon 3 puts what a
	// friend needs in order to join (lobby id, mode, whether it is open) in
	// here, so it is passed through verbatim: inventing values would be worse
	// than passing none.
	Attributes map[string]*commonpb.Value
	LastOnline time.Time
	Updated    time.Time
}

// Hub stores presences and fans updates out to subscribers.
type Hub struct {
	names    names.Builder
	accounts *account.Client
	appID    string
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]*Entry // by NPLN user id
	subs    map[int64]*subscription
	nextSub int64
}

// subscription is one open SubscribePresences (or SubscribeFriendUsers) stream.
type subscription struct {
	// watch is the set of user ids the subscriber cares about; empty means
	// "everything", which is only used by internal tooling.
	watch map[string]bool
	ch    chan *Entry
}

// Options configures the hub.
type Options struct {
	Names    names.Builder
	Accounts *account.Client
	AppID    string
	// TTL drops a presence whose keepalive stopped. The console sends a
	// keepalive on the interval WE hand it in the Heartbeat, so this is simply a
	// few of those intervals.
	TTL time.Duration
}

// New builds a hub.
func New(o Options) *Hub {
	if o.TTL <= 0 {
		o.TTL = 90 * time.Second
	}
	return &Hub{
		names:    o.Names,
		accounts: o.Accounts,
		appID:    o.AppID,
		ttl:      o.TTL,
		entries:  map[string]*Entry{},
		subs:     map[int64]*subscription{},
	}
}

// Set records a presence update coming from the game and notifies subscribers.
func (h *Hub) Set(userID string, pid uint64, state friendsv1.State, attrs map[string]*commonpb.Value) {
	if userID == "" {
		return
	}
	now := time.Now()
	h.mu.Lock()
	e := h.entries[userID]
	if e == nil {
		e = &Entry{UserID: userID, PID: pid}
		h.entries[userID] = e
	}
	e.PID = pid
	if state != friendsv1.State_STATE_UNSPECIFIED {
		e.State = state
	}
	// A keepalive with no attributes is a heartbeat, not "clear my attributes":
	// dropping them would make a friend's joinable lobby disappear every few
	// seconds.
	if attrs != nil {
		e.Attributes = attrs
	}
	if e.State == friendsv1.State_ONLINE {
		e.LastOnline = now
	}
	e.Updated = now
	snapshot := *e
	h.mu.Unlock()

	h.publish(&snapshot)
}

// Touch refreshes the freshness of a presence without changing it. Called on any
// authenticated RPC, so a player who is playing but whose keepalive stream broke
// does not blink offline for their friends.
func (h *Hub) Touch(userID string, pid uint64) {
	if userID == "" {
		return
	}
	h.mu.Lock()
	e := h.entries[userID]
	if e == nil {
		e = &Entry{UserID: userID, PID: pid, State: friendsv1.State_ONLINE, LastOnline: time.Now()}
		h.entries[userID] = e
	}
	e.PID = pid
	e.Updated = time.Now()
	h.mu.Unlock()
}

// Offline marks a player offline (their stream closed / they left online play)
// and notifies subscribers immediately, instead of waiting for the TTL.
func (h *Hub) Offline(userID string) {
	h.mu.Lock()
	e := h.entries[userID]
	if e == nil {
		h.mu.Unlock()
		return
	}
	e.State = friendsv1.State_OFFLINE
	e.Updated = time.Now()
	snapshot := *e
	h.mu.Unlock()
	h.publish(&snapshot)
}

// Get returns a fresh presence for a user.
func (h *Hub) Get(userID string) (*Entry, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[userID]
	if !ok || time.Since(e.Updated) > h.ttl {
		return nil, false
	}
	copy := *e
	return &copy, true
}

// ActivePIDs returns the Nextendo PIDs currently playing Splatoon 3, i.e. the
// set reported to the account server as "playing".
func (h *Hub) ActivePIDs() []uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uint64, 0, len(h.entries))
	for _, e := range h.entries {
		if e.PID == 0 || time.Since(e.Updated) > h.ttl {
			continue
		}
		if e.State == friendsv1.State_OFFLINE {
			continue
		}
		out = append(out, e.PID)
	}
	return out
}

// Count returns how many players are present (for /api/stats).
func (h *Hub) Count() int { return len(h.ActivePIDs()) }

// Subscribe opens a subscription for the given watched user ids. The returned
// cancel function must be called when the stream ends, or the channel leaks.
func (h *Hub) Subscribe(watch []string) (<-chan *Entry, func()) {
	set := map[string]bool{}
	for _, w := range watch {
		set[w] = true
	}
	// Buffered: a slow console must not block another player's presence update.
	// If it overflows we drop the oldest — the next full snapshot repairs it.
	sub := &subscription{watch: set, ch: make(chan *Entry, 64)}
	h.mu.Lock()
	h.nextSub++
	id := h.nextSub
	h.subs[id] = sub
	h.mu.Unlock()
	return sub.ch, func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
		close(sub.ch)
	}
}

// publish sends an entry to every interested subscriber.
func (h *Hub) publish(e *Entry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sub := range h.subs {
		if len(sub.watch) > 0 && !sub.watch[e.UserID] {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			// Full: drop. Presence is a "latest value wins" signal, and the
			// stream re-sends a full snapshot on its poll tick anyway.
		}
	}
}

// Proto renders an entry as an NPLN Presence resource.
func (h *Hub) Proto(e *Entry) *friendsv1.Presence {
	p := &friendsv1.Presence{
		Name:       h.names.Presence(e.UserID),
		State:      e.State,
		Attributes: e.Attributes,
	}
	if !e.LastOnline.IsZero() {
		p.LastOnlineTime = timestamppb.New(e.LastOnline)
	}
	return p
}

// PresenceFor builds the Presence resource of a user for a subscriber,
// falling back to the account server's network-wide view.
//
// The fallback is what makes a friend who is NOT in Splatoon 3 still show as
// online: nextendo-account tracks presence across every game and the console
// itself, so "online in Splatoon 2" becomes ONLINE here (with no attributes,
// because there is no Splatoon 3 lobby to join).
func (h *Hub) PresenceFor(userID string, fallback *account.Friend) *friendsv1.Presence {
	if e, ok := h.Get(userID); ok {
		return h.Proto(e)
	}
	p := &friendsv1.Presence{Name: h.names.Presence(userID), State: friendsv1.State_OFFLINE}
	if fallback != nil {
		switch fallback.Presence2().Status {
		case account.PresencePlaying, account.PresenceOnline:
			p.State = friendsv1.State_ONLINE
			p.LastOnlineTime = timestamppb.New(time.Now())
		}
	}
	return p
}

// StartReporter reports the players currently in Splatoon 3 to
// nextendo-account, every interval, until ctx is done.
//
// This is the half of presence that was missing from the stack: without it a
// player IS online but nothing tells the identity hub, so every one of their
// friends — on a Switch, in another game, or on the website — sees them offline.
func (h *Hub) StartReporter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pids := h.ActivePIDs()
				if len(pids) == 0 {
					continue
				}
				if err := h.accounts.ReportPresence(ctx, h.appID, account.PresencePlaying, pids); err != nil {
					log.Printf("[presence] report of %d player(s) failed: %v", len(pids), err)
				}
			}
		}
	}()
}

// StartReaper drops stale presences (a console that stopped keeping alive) and
// tells subscribers, so a crashed player does not stay "online" for their
// friends forever. Same idea as the NEX servers' connection reaper.
func (h *Hub) StartReaper(ctx context.Context) {
	go func() {
		t := time.NewTicker(h.ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				var stale []*Entry
				h.mu.Lock()
				for id, e := range h.entries {
					if time.Since(e.Updated) <= h.ttl {
						continue
					}
					if e.State != friendsv1.State_OFFLINE {
						e.State = friendsv1.State_OFFLINE
						snapshot := *e
						stale = append(stale, &snapshot)
					}
					// Keep the row for one more TTL so LastOnline stays
					// available, then forget it entirely.
					if time.Since(e.Updated) > 2*h.ttl {
						delete(h.entries, id)
					}
				}
				h.mu.Unlock()
				for _, e := range stale {
					log.Printf("[presence] %s (pid=%d) went stale -> OFFLINE", e.UserID, e.PID)
					h.publish(e)
				}
			}
		}
	}()
}
