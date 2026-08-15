package main

// Online GATES — the same rules, in the same file, as every other Nextendo game
// server (compare splatoon-2/gates.go and mario-kart-8-deluxe/gates.go):
//
//   - a Nextendo account is MANDATORY (requireAccount): no account identity, no
//     online play;
//   - online is for Nextendo accounts ONLY: an unlinked console NSA, or an
//     unreachable account server, is refused — fail-CLOSED, so a non-Nextendo
//     profile never gets in, and never borrows somebody else's identity;
//   - e-mail verified, account not disabled, and one place at a time — all owned
//     by nextendo-account and answered by /internal/online-check.
//
// FAIL-OPEN on an online-check transport error (a transient hiccup must never
// lock everyone out of online play); FAIL-CLOSED on an unverifiable identity.
// That asymmetry is deliberate and matches the NEX servers exactly.
//
// # Why this file exists here
//
// The NEX servers call resolveNSAtoPID from LoginEx. This one calls it from the
// NPLN Auth service, through the LookupNSA hook npln/identity takes — the
// protocol differs, the account contract does not. Keeping the file name,
// function names and semantics identical means the account integration reads the
// same in all nine servers, and a change to the contract can be applied the same
// way everywhere.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var gateClient = &http.Client{Timeout: 3 * time.Second}

// nsaStatus distinguishes the outcomes of resolving an NSA id to a Nextendo account.
type nsaStatus int

const (
	nsaOK          nsaStatus = iota // linked to a Nextendo account (pid is valid)
	nsaUnknown                      // 404: no account owns this NSA -> non-Nextendo profile
	nsaUnreachable                  // account server unreachable -> identity unverifiable
)

var (
	nsaCacheMu sync.Mutex
	nsaCache   = map[uint64]uint64{}
	// nsaNegCache memoises FAILED resolutions (404 / unreachable). Without it every
	// login attempt carrying an unknown NSA id triggers a fresh outbound call to the
	// shared account service, so a flood of bogus ids on a public listener turns into
	// amplification against the service every game depends on. A short TTL keeps it
	// responsive right after an account is linked.
	nsaNegCache = map[uint64]nsaNegEntry{}
	// nsaInflight caps CONCURRENT /api/nsa calls: past the cap we answer
	// "unreachable" (fail-closed) instead of opening one more connection.
	nsaInflight = make(chan struct{}, nsaMaxInflight)
)

type nsaNegEntry struct {
	status nsaStatus
	at     time.Time
}

const (
	nsaNegTTL      = 60 * time.Second
	nsaMaxInflight = 16
	nsaNegCacheMax = 4096
)

// resolveNSAtoPID maps an NSA id (the baasUserID a real Switch presents) to the
// PID of its Nextendo account: (pid, nsaOK) when linked, (0, nsaUnknown) when no
// account owns it, (0, nsaUnreachable) when the account server cannot be reached.
// Positive results are cached.
func resolveNSAtoPID(nsa uint64) (uint64, nsaStatus) {
	nsaCacheMu.Lock()
	if pid, ok := nsaCache[nsa]; ok {
		nsaCacheMu.Unlock()
		return pid, nsaOK
	}
	if neg, ok := nsaNegCache[nsa]; ok && time.Since(neg.at) < nsaNegTTL {
		nsaCacheMu.Unlock()
		return 0, neg.status // recently resolved as unknown/unreachable: no new call
	}
	nsaCacheMu.Unlock()

	// Cap concurrent outbound calls to the shared account service.
	select {
	case nsaInflight <- struct{}{}:
		defer func() { <-nsaInflight }()
	default:
		return 0, nsaUnreachable // saturated: fail-closed, without opening a connection
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/nsa?id=%d", accountBaseURL(), nsa), nil)
	if err != nil {
		rememberNSAFailure(nsa, nsaUnreachable)
		return 0, nsaUnreachable
	}
	if k := internalKey(); k != "" {
		req.Header.Set("X-Internal-Key", k)
	}
	resp, err := gateClient.Do(req)
	if err != nil {
		rememberNSAFailure(nsa, nsaUnreachable)
		return 0, nsaUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		rememberNSAFailure(nsa, nsaUnknown)
		return 0, nsaUnknown
	}
	if resp.StatusCode != http.StatusOK {
		rememberNSAFailure(nsa, nsaUnreachable)
		return 0, nsaUnreachable
	}
	var out struct {
		PID uint64 `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.PID == 0 {
		rememberNSAFailure(nsa, nsaUnreachable)
		return 0, nsaUnreachable
	}
	nsaCacheMu.Lock()
	nsaCache[nsa] = out.PID
	delete(nsaNegCache, nsa) // the account was just linked: a stale negative must not survive
	nsaCacheMu.Unlock()
	return out.PID, nsaOK
}

// rememberNSAFailure memoises a failed resolution for nsaNegTTL. The table is
// bounded: a flood of bogus ids must not grow it without end (it is cleared
// wholesale at the cap — entries are only worth 60 s anyway).
func rememberNSAFailure(nsa uint64, st nsaStatus) {
	nsaCacheMu.Lock()
	if len(nsaNegCache) >= nsaNegCacheMax {
		nsaNegCache = map[uint64]nsaNegEntry{}
	}
	nsaNegCache[nsa] = nsaNegEntry{status: st, at: time.Now()}
	nsaCacheMu.Unlock()
}

// lookupNSAHex is the adapter npln/identity calls. A BAAS id_token spells the NSA
// id as 16 hex characters; /api/nsa — the endpoint all nine servers share — takes
// it as a decimal u64. Converting here, rather than adding a second account
// endpoint, is what keeps this server on the same contract as its siblings.
func lookupNSAHex(nsaHex string) (uint64, error) {
	nsa, err := strconv.ParseUint(nsaHex, 16, 64)
	if err != nil {
		// Not 16 hex digits: some consoles present the decimal form.
		if nsa, err = strconv.ParseUint(nsaHex, 10, 64); err != nil {
			return 0, fmt.Errorf("nsa %q is not a 64-bit id", nsaHex)
		}
	}
	pid, st := resolveNSAtoPID(nsa)
	switch st {
	case nsaOK:
		return pid, nil
	case nsaUnknown:
		// The line an operator needs when a player says "it says I have no
		// account": the console reached us with an NSA nobody has linked.
		log.Printf("[gates] nsa=%s (%d) -> NO NEXTENDO ACCOUNT (refused; never attached to a default account)", nsaHex, nsa)
		return 0, fmt.Errorf("nsa %s belongs to no Nextendo account", nsaHex)
	default:
		log.Printf("[gates] nsa=%s (%d) -> account server unreachable (refused, fail-closed)", nsaHex, nsa)
		return 0, fmt.Errorf("nsa %s: account server unreachable", nsaHex)
	}
}
