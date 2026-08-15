// Package account is the client for nextendo-account, the identity hub of the
// Nextendo Network stack.
//
// Splatoon 3 shares its accounts, friend graph and online presence with every
// other Nextendo game; none of that is stored here. This package is the only
// place that talks to the account server, so the rules of engagement live in
// one file:
//
//	fail-CLOSED on identity   an NSA id we cannot resolve must never become
//	                          "some account" — see docs/FRIENDS.md for what
//	                          that mistake looks like from a player's seat.
//	fail-OPEN on the gate     a transient error while asking "may this account
//	                          go online?" must not lock the whole player base
//	                          out, which is the rule splatoon-2 settled on.
//	never cache identity      only positive NSA→PID lookups are cached (they
//	                          cannot change), never "who is calling".
//
// Endpoints used (all of them already exist in nextendo-account):
//
//	GET  /internal/npln-friends?pid=  the account's NPLN identity + friends + presence
//	GET  /internal/resolve?baas=      NSA/BAAS id            -> account
//	GET  /internal/identity?pid=      nickname, friend code, friends, requests
//	POST /internal/online-check       the online gates (verified, one place, bans)
//	POST /internal/presence-batch     "these PIDs are playing Splatoon 3 now"
package account

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to nextendo-account.
type Client struct {
	baseURL     string
	internalKey string
	http        *http.Client
}

// New builds a client. timeout applies to each individual request.
func New(baseURL, internalKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		internalKey: internalKey,
		http:        &http.Client{Timeout: timeout},
	}
}

// Friend is one entry of the unified Nextendo friend graph, in the NPLN shape
// the account server publishes for this service.
type Friend struct {
	PID        uint64         `json:"pid"`
	UserID     string         `json:"user_id"`
	AccountHex string         `json:"account_hex"` // the friend's NSA/BAAS id
	Name       string         `json:"name"`
	Favorite   bool           `json:"favorite"`
	Presence   map[string]any `json:"presence"`
}

// NplnIdentity is the answer of /internal/npln-friends: the caller's own NPLN
// identity, whether it is allowed online, and its friends.
type NplnIdentity struct {
	PID        uint64   `json:"pid"`
	UserID     string   `json:"user_id"`
	AccountHex string   `json:"account_hex"`
	Verified   bool     `json:"verified"`
	Friends    []Friend `json:"friends"`
	// Blocked is served by the patched account server (see the fork described
	// in docs/FRIENDS.md). An older account server simply omits it, and
	// ListBlockingUsers then answers an empty list.
	Blocked []Friend `json:"blocked"`
}

// PresenceState mirrors nn::friends / NPLN presence states as the account
// server stores them: 0 offline, 1 online, 2 playing.
const (
	PresenceOffline = 0
	PresenceOnline  = 1
	PresencePlaying = 2
)

// FriendPresence is the presence of a friend, extracted from the map the
// account server sends.
type FriendPresence struct {
	Status    int
	AppID     string
	AppField  string
	AppDetail string
}

// Presence reads the presence block of a friend entry.
func (f Friend) Presence2() FriendPresence {
	p := FriendPresence{}
	if f.Presence == nil {
		return p
	}
	if v, ok := f.Presence["status"]; ok {
		switch n := v.(type) {
		case float64:
			p.Status = int(n)
		case int:
			p.Status = n
		}
	}
	str := func(key string) string {
		if v, ok := f.Presence[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	p.AppID, p.AppField, p.AppDetail = str("app_id"), str("app_field"), str("app_detail")
	return p
}

// NplnFriends fetches the caller's NPLN identity + friend graph.
func (c *Client) NplnFriends(ctx context.Context, pid uint64) (*NplnIdentity, error) {
	var out NplnIdentity
	if err := c.getJSON(ctx, fmt.Sprintf("/internal/npln-friends?pid=%d", pid), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Identity is the answer of /internal/identity — the human-facing side of an
// account (nickname, friend code) plus its friend/request lists. Splatoon 3
// only needs the nickname, but the friend code is logged so an operator can
// correlate a player with what the console shows them.
type Identity struct {
	PID        uint64 `json:"pid"`
	Nickname   string `json:"nickname"`
	FriendCode string `json:"friendCode"`
	BaasUserID string `json:"baasUserID"`
	BsDid      string `json:"bsDid"`
}

// Identity fetches the display identity of an account.
func (c *Client) Identity(ctx context.Context, pid uint64) (*Identity, error) {
	var out Identity
	if err := c.getJSON(ctx, fmt.Sprintf("/internal/identity?pid=%d", pid), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OnlineCheck asks the account server whether this account may go online now:
// e-mail verified, not disabled/banned, and not already playing somewhere else.
//
// FAIL-OPEN on a transport error, matching splatoon-2/gates.go: a hiccup on the
// account server must never lock every player out of online play. A definite
// "no" from the server IS honoured.
func (c *Client) OnlineCheck(ctx context.Context, pid uint64, kind string) (bool, string) {
	body, err := json.Marshal(map[string]any{"pid": pid, "kind": kind})
	if err != nil {
		return true, ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/online-check", bytes.NewReader(body))
	if err != nil {
		return true, ""
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("[account] online-check pid=%d: %v -> fail-open", pid, err)
		return true, ""
	}
	defer resp.Body.Close()
	var out struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return true, ""
	}
	return out.Allow, out.Reason
}

// ReportPresence tells the account server which PIDs are playing Splatoon 3
// right now, so the Switch friend list (and the other games) show them online.
// The account server expires an entry that is not re-reported, so this must be
// called on a loop while players are connected.
func (c *Client) ReportPresence(ctx context.Context, appID string, status int, pids []uint64) error {
	if len(pids) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"appId": appID, "status": status, "pids": pids})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/presence-batch", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("account: presence-batch: HTTP %d", resp.StatusCode)
	}
	return nil
}

// getJSON performs an internal GET and decodes the JSON body.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("account: %s: not found", path)
	default:
		return fmt.Errorf("account: %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// auth adds the shared internal key. /internal/* is a control plane that must
// never be reachable from the internet; the key is the second line of defence
// behind the account server's source-address check.
func (c *Client) auth(req *http.Request) {
	if c.internalKey != "" {
		req.Header.Set("X-Internal-Key", c.internalKey)
	}
}

// FormatPID is a tiny helper for log lines and resource ids.
func FormatPID(pid uint64) string { return strconv.FormatUint(pid, 10) }
