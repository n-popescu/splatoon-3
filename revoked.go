package main

// Token revocation — the same denylist every Nextendo game server carries, in the
// same file, with the same name.
//
// # Why it exists
//
// A "nx2." login token is an HMAC over "pid.username.expiry" with a 30-day life,
// minted by nextendo-account. When one leaks there is no way to invalidate it
// short of rotating the shared secret — which would log out every player and
// change every account's derived BAAS/NA ids — so each server refuses known
// payloads even though their signature verifies.
//
// This server reaches the same code path: Splatoon 3 does not log in over NEX,
// but the "nnex" claim inside the console's id_token IS an nx2 token, verified by
// npln/identity with the same secret and the same MAC. Without this file a
// credential revoked everywhere else would still be accepted here, which is the
// drift documented as F2 in audit/FINDINGS.md — six components out of nine
// accepted a token the other three had revoked.
//
// # Configuration
//
//	NEXTENDO_REVOKED_TOKENS       payloads separated by commas, semicolons or newlines
//	NEXTENDO_REVOKED_TOKENS_FILE  a file with one payload per line ("#" starts a comment)
//
// Revoking the next leak is then one config change deployed to every server,
// rather than nine source edits that drift apart. Both sources are merged at
// start-up, before the listener accepts anything, so the login path only ever
// reads a map nobody writes to afterwards.
//
// A payload is the DECODED middle segment of the token: "1800000006.Kazuu.1787343209".

import (
	"log"
	"os"
	"strings"
)

// revokedNexPayloads is the denylist. Same built-in entry as the rest of the
// fleet: the payload shipped to every downloader by the 1.6.5-win release.
var revokedNexPayloads = map[string]bool{
	"1800000006.Kazuu.1787343209": true,
}

// loadRevokedPayloads merges the configured denylist into revokedNexPayloads. It
// must run before the server starts accepting connections.
func loadRevokedPayloads() {
	added := 0
	for _, p := range parseRevokedList(os.Getenv("NEXTENDO_REVOKED_TOKENS")) {
		if !revokedNexPayloads[p] {
			revokedNexPayloads[p] = true
			added++
		}
	}
	if path := os.Getenv("NEXTENDO_REVOKED_TOKENS_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			// Loud, because a denylist that silently failed to load is worse than
			// no denylist: the operator believes a leaked token is dead when it
			// is still being accepted.
			log.Printf("[auth] WARNING: revocation list %s unreadable (%v) — the tokens it names are still ACCEPTED", path, err)
		} else {
			for _, p := range parseRevokedList(string(b)) {
				if !revokedNexPayloads[p] {
					revokedNexPayloads[p] = true
					added++
				}
			}
		}
	}
	log.Printf("[auth] %d revoked nx2 token(s) (%d from configuration)", len(revokedNexPayloads), added)
}

// nexPayloadRevoked reports whether an nx2 payload has been revoked. This is the
// function handed to npln/identity, so the check happens inside token
// verification rather than beside it.
func nexPayloadRevoked(payload string) bool {
	if revokedNexPayloads[payload] {
		log.Printf("[auth] REFUSED a revoked nx2 token (%s)", payload)
		return true
	}
	return false
}

// parseRevokedList splits a denylist on newlines, commas and semicolons, dropping
// blanks and "#" comments so a file can document why each entry is there.
func parseRevokedList(raw string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	}) {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
