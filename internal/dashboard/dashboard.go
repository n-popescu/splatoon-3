// Package dashboard exposes /api/stats, the per-game monitoring endpoint the
// unified Nextendo dashboard polls, plus the HTTP endpoints the UGC attachments
// need.
//
// The JSON keys deliberately mirror the ones the NEX game servers publish
// (splatoon-2/dashboard.go), so the existing aggregator renders Splatoon 3
// without a single change on its side. The concepts translate directly:
//
//	NEX                        NPLN / here
//	PRUDP connection           an authenticated player with a live token
//	RMC call                   a gRPC call
//	gathering                  a game session
//	idleSeconds                seconds since that player's last RPC
//
// idleSeconds matters beyond the display: nextendo-account's "one place at a
// time" gate polls this endpoint and treats a player whose session went idle for
// too long as a ghost. Without it, a crashed console would keep its own account
// locked out of online play — the exact bug the NEX side had to fix.
package dashboard

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n-popescu/splatoon-3/internal/presence"
	"github.com/n-popescu/splatoon-3/internal/services/matchmaking"
	"github.com/n-popescu/splatoon-3/internal/services/ugc"
)

// playerInfo is what we remember about a connected player.
type playerInfo struct {
	PID        uint64
	UserID     string
	FirstSeen  time.Time
	LastSeen   time.Time
	Addr       string
	Calls      int64
	LastAction string
}

// Dashboard collects per-player activity and serves the JSON.
type Dashboard struct {
	game      string
	token     string
	started   time.Time
	ghostIdle time.Duration

	registry *matchmaking.Registry
	presence *presence.Hub
	ugc      *ugc.Store

	mu      sync.Mutex
	players map[uint64]*playerInfo
	events  []event
	methods map[string]int64
	peak    int

	calls atomic.Int64
}

type event struct {
	At     time.Time
	PID    uint64
	Action string
}

// Options configures the dashboard.
type Options struct {
	// Game is the tag the aggregator groups this server under.
	Game string
	// Token, when set, is required as ?key= on /api/stats. The endpoint exposes
	// player names, PIDs and IP addresses, so it is not public.
	Token     string
	GhostIdle time.Duration
	Registry  *matchmaking.Registry
	Presence  *presence.Hub
	UGC       *ugc.Store
}

// New builds a dashboard.
func New(o Options) *Dashboard {
	if o.Game == "" {
		o.Game = "splatoon3"
	}
	if o.GhostIdle <= 0 {
		o.GhostIdle = 15 * time.Minute
	}
	return &Dashboard{
		game:      o.Game,
		token:     o.Token,
		started:   time.Now(),
		ghostIdle: o.GhostIdle,
		registry:  o.Registry,
		presence:  o.Presence,
		ugc:       o.UGC,
		players:   map[uint64]*playerInfo{},
		methods:   map[string]int64{},
	}
}

// NoteRPC records a call. It implements server.Observer, so it is fed from the
// gRPC interceptor exactly like the NEX servers feed theirs from OnRMC.
func (d *Dashboard) NoteRPC(pid uint64, userID, method, peerAddr string) {
	if pid == 0 {
		return
	}
	d.calls.Add(1)
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.players[pid]
	if p == nil {
		p = &playerInfo{PID: pid, UserID: userID, FirstSeen: now}
		d.players[pid] = p
	} else if !p.LastSeen.IsZero() && now.Sub(p.LastSeen) > d.ghostIdle {
		// The player was dormant: this is a NEW online session, so "online for"
		// counts from here. Otherwise a console left running for days reports an
		// absurd uptime.
		p.FirstSeen = now
	}
	p.UserID = userID
	p.LastSeen = now
	p.Calls++
	p.LastAction = method
	if peerAddr != "" {
		p.Addr = peerAddr
	}
	d.methods[method]++
	d.events = append(d.events, event{At: now, PID: pid, Action: method})
	if len(d.events) > 100 {
		d.events = d.events[len(d.events)-100:]
	}
	if live := d.liveCountLocked(); live > d.peak {
		d.peak = live
	}
}

// liveCountLocked counts players seen recently. Caller holds the lock.
func (d *Dashboard) liveCountLocked() int {
	n := 0
	for _, p := range d.players {
		if time.Since(p.LastSeen) <= d.ghostIdle {
			n++
		}
	}
	return n
}

// ---- JSON shapes (keys mirror the NEX game servers') ----------------------

type apiPlayer struct {
	PID        uint64 `json:"pid"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	State      string `json:"state"`
	Gathering  string `json:"gathering"`
	OnlineSecs int    `json:"onlineSeconds"`
	Calls      int64  `json:"calls"`
	LastAction string `json:"lastAction"`
	IdleSecs   int    `json:"idleSeconds"`
	IsHost     bool   `json:"isHost"`
}

type apiSessionPlayer struct {
	PID  uint64 `json:"pid"`
	Name string `json:"name"`
	Host bool   `json:"host"`
	Team string `json:"team,omitempty"`
}

type apiSession struct {
	ID      string             `json:"id"`
	HostPID uint64             `json:"hostPid"`
	Count   int                `json:"count"`
	Max     int32              `json:"max"`
	State   string             `json:"state"`
	Public  bool               `json:"public"`
	Open    bool               `json:"open"`
	Code    string             `json:"code,omitempty"`
	Peer    string             `json:"peer,omitempty"`
	Config  string             `json:"config,omitempty"`
	Players []apiSessionPlayer `json:"players"`
}

type apiEvent struct {
	Ago    int    `json:"agoSeconds"`
	PID    uint64 `json:"pid"`
	Action string `json:"action"`
}

type apiMethod struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type apiStats struct {
	Game          string       `json:"game"`
	ServerTime    string       `json:"serverTime"`
	UptimeSeconds int          `json:"uptimeSeconds"`
	Connected     int          `json:"connected"`
	InLobby       int          `json:"inLobby"`
	ActiveLobbies int          `json:"activeLobbies"`
	TotalRPC      int64        `json:"totalRmc"`
	PeakConnected int          `json:"peakConnected"`
	Presences     int          `json:"presences"`
	Documents     int          `json:"documents"`
	Players       []apiPlayer  `json:"players"`
	Sessions      []apiSession `json:"gatherings"`
	Events        []apiEvent   `json:"events"`
	Methods       []apiMethod  `json:"methods"`
}

// Handler returns the HTTP handler serving /api/stats and the UGC endpoints.
func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", d.stats)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "game": d.game})
	})
	if d.ugc != nil {
		mux.HandleFunc("/ugc/upload/", d.upload)
		mux.HandleFunc("/ugc/blob/", d.blob)
	}
	return mux
}

// stats serves the monitoring JSON.
func (d *Dashboard) stats(w http.ResponseWriter, r *http.Request) {
	// The endpoint lists players, their PIDs and their IP addresses; without a
	// token it must not be reachable. Constant-time compare so the check itself
	// leaks nothing.
	if d.token != "" {
		key := r.URL.Query().Get("key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(d.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	writeJSON(w, http.StatusOK, d.build())
}

// build assembles the stats snapshot.
func (d *Dashboard) build() apiStats {
	sessions := d.registry.Sessions()

	inSession := map[uint64]*matchmaking.Session{}
	isHost := map[uint64]bool{}
	apiSessions := make([]apiSession, 0, len(sessions))
	inLobby := 0
	for _, s := range sessions {
		entry := apiSession{
			ID:      s.ID,
			Count:   len(s.Users),
			Max:     s.MaxParticipants,
			Public:  s.IsPublic,
			Open:    s.CanParticipate,
			Code:    s.ShortAlias,
			Config:  s.Config,
			State:   sessionState(s),
		}
		if s.Host != "" {
			entry.Peer = fmt.Sprintf("%s:%d", s.Host, s.Port)
		}
		for i, us := range s.Users {
			inSession[us.PID] = s
			if i == 0 {
				entry.HostPID = us.PID
				isHost[us.PID] = true
			}
			entry.Players = append(entry.Players, apiSessionPlayer{
				PID:  us.PID,
				Name: displayName(us.PID, us.UserID),
				Host: i == 0,
				Team: us.Team,
			})
			inLobby++
		}
		apiSessions = append(apiSessions, entry)
	}

	d.mu.Lock()
	players := make([]apiPlayer, 0, len(d.players))
	for pid, p := range d.players {
		idle := time.Since(p.LastSeen)
		if idle > d.ghostIdle {
			// Ghost: the token is still valid but nothing has been heard from
			// this console in a long time. Excluded, so the counters — and the
			// account server's "one place" gate — reflect reality.
			continue
		}
		entry := apiPlayer{
			PID:        pid,
			Name:       displayName(pid, p.UserID),
			IP:         p.Addr,
			OnlineSecs: int(time.Since(p.FirstSeen).Seconds()),
			Calls:      p.Calls,
			LastAction: p.LastAction,
			IdleSecs:   int(idle.Seconds()),
			State:      "en ligne",
			IsHost:     isHost[pid],
		}
		if s, ok := inSession[pid]; ok {
			entry.Gathering = s.ID
			entry.State = "en partie"
		}
		players = append(players, entry)
	}
	events := make([]apiEvent, 0, len(d.events))
	for _, e := range d.events {
		events = append(events, apiEvent{Ago: int(time.Since(e.At).Seconds()), PID: e.PID, Action: e.Action})
	}
	methods := make([]apiMethod, 0, len(d.methods))
	for name, count := range d.methods {
		methods = append(methods, apiMethod{Name: name, Count: count})
	}
	peak := d.peak
	d.mu.Unlock()

	sort.Slice(players, func(i, j int) bool { return players[i].PID < players[j].PID })
	sort.Slice(methods, func(i, j int) bool { return methods[i].Count > methods[j].Count })

	docs := 0
	if d.ugc != nil {
		docs = len(d.ugc.Browse("", 1_000_000))
	}
	return apiStats{
		Game:          d.game,
		ServerTime:    time.Now().UTC().Format(time.RFC3339),
		UptimeSeconds: int(time.Since(d.started).Seconds()),
		Connected:     len(players),
		InLobby:       inLobby,
		ActiveLobbies: len(apiSessions),
		TotalRPC:      d.calls.Load(),
		PeakConnected: peak,
		Presences:     d.presence.Count(),
		Documents:     docs,
		Players:       players,
		Sessions:      apiSessions,
		Events:        events,
		Methods:       methods,
	}
}

// upload receives a UGC attachment on a one-shot URI.
func (d *Dashboard) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "PUT expected", http.StatusMethodNotAllowed)
		return
	}
	token := filepath.Base(r.URL.Path)
	blobID, ok := d.ugc.ClaimUpload(token)
	if !ok {
		// One-shot: an expired or replayed token is refused, so an upload URI
		// cannot be shared around to write arbitrary blobs.
		http.Error(w, "unknown or expired upload URI", http.StatusForbidden)
		return
	}
	// Bounded read: a replay is large but not unbounded, and this endpoint is
	// reachable by anything that holds a token.
	const maxAttachment = 32 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAttachment))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	path := filepath.Join(d.ugc.BlobDir(), blobID)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Printf("[ugc] cannot store an upload: %v", err)
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	log.Printf("[ugc] stored attachment %s (%d bytes)", blobID, len(body))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": blobID})
}

// blob serves a stored attachment.
func (d *Dashboard) blob(w http.ResponseWriter, r *http.Request) {
	id := filepath.Base(r.URL.Path)
	// filepath.Base already strips any traversal, and the id is a hex digest, so
	// reject anything that is not one rather than touching the filesystem.
	if !isHex(id) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(d.ugc.BlobDir(), id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

// Serve starts the HTTP listener.
func (d *Dashboard) Serve(addr string) error {
	log.Printf("[dashboard] HTTP listening on %s (/api/stats, /ugc/*)", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// sessionState renders a room's state for the monitoring display.
func sessionState(s *matchmaking.Session) string {
	switch {
	case len(s.Users) == 0:
		return "vide"
	case s.CanParticipate && int32(len(s.Users)) < s.MaxParticipants:
		return "en recherche"
	default:
		return "complet"
	}
}

// displayName renders a player for the monitoring display. Real nicknames live in
// nextendo-account; this server deliberately does not fetch them per request just
// to draw a table, so it shows the stable derived label the NEX dashboards use.
func displayName(pid uint64, userID string) string {
	if userID != "" {
		return fmt.Sprintf("Joueur-%d (%s)", pid%100000, userID)
	}
	return fmt.Sprintf("Joueur-%d", pid%100000)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
