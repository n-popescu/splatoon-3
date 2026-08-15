package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NextendoNetwork/splatoon-3/npln/identity"
)

// TestKnownLeakedTokenIsRevoked is the regression guard for the drift recorded as
// F2 in audit/FINDINGS.md: the payload leaked by the 1.6.5-win release was revoked
// in three components of nine and still accepted by the other six.
func TestKnownLeakedTokenIsRevoked(t *testing.T) {
	const leaked = "1800000006.Kazuu.1787343209"
	if !revokedNexPayloads[leaked] {
		t.Fatalf("the payload leaked by the 1.6.5-win release is NOT revoked on this server")
	}
	if !nexPayloadRevoked(leaked) {
		t.Error("nexPayloadRevoked did not refuse it")
	}
}

// TestRevokedListFromConfig covers the loader that makes the next revocation a
// config change on every server instead of nine source edits.
func TestRevokedListFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.txt")
	content := "# leaked in some future incident\n1800000123.Someone.1800000000\n\n1800000124.Other.1800000000 # inline comment\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXTENDO_REVOKED_TOKENS_FILE", path)
	t.Setenv("NEXTENDO_REVOKED_TOKENS", "1800000125.Env.1800000000,1800000126.Env2.1800000000")

	loadRevokedPayloads()

	for _, want := range []string{
		"1800000123.Someone.1800000000",
		"1800000124.Other.1800000000",
		"1800000125.Env.1800000000",
		"1800000126.Env2.1800000000",
	} {
		if !revokedNexPayloads[want] {
			t.Errorf("payload %q was not loaded from configuration", want)
		}
	}
	if revokedNexPayloads["# leaked in some future incident"] {
		t.Error("a comment line was loaded as a payload")
	}
}

// TestRevokedTokenIsRefusedByTheResolver is the test that matters: it proves the
// denylist is wired INTO identity resolution, not merely present in the binary.
//
// The revoked token's signature verifies perfectly — that is the whole problem
// with a leaked credential — so the only thing that can refuse it is the
// denylist. And it must be refused outright rather than downgraded to the weaker
// NSA path, because a present-but-rejected binding means somebody edited a token.
func TestRevokedTokenIsRefusedByTheResolver(t *testing.T) {
	const secret = "test-secret-for-revocation"
	const pid = 1800000006
	const nsa = "8ca8d7842f865b2f"

	expiry := time.Now().Add(24 * time.Hour)
	legit := signNexTokenLikeAccountServer(secret, pid, "Legit", expiry)
	leaked := signNexTokenLikeAccountServer(secret, pid, "Kazuu", time.Unix(1787343209, 0))

	revokedNexPayloads[fmt.Sprintf("%d.%s.%d", pid, "Kazuu", int64(1787343209))] = true

	r, err := identity.NewResolver(identity.Options{
		Secret: []byte(secret),
		AppID:  "0100c2500fc20000",
		// Fail every NSA lookup, so a token that is refused cannot resolve by
		// any other route: what we observe is exactly the denylist's effect.
		LookupNSA: func(string) (uint64, error) { return 0, errors.New("no account") },
		Revoked:   nexPayloadRevoked,
	})
	if err != nil {
		t.Fatal(err)
	}

	id, err := r.Resolve(idTokenWithNnex(t, nsa, legit, expiry.Unix()), 0)
	if err != nil {
		t.Fatalf("a valid, unrevoked token was refused: %v", err)
	}
	if id.PID != pid {
		t.Errorf("pid = %d, want %d", id.PID, pid)
	}

	if _, err := r.Resolve(idTokenWithNnex(t, nsa, leaked, expiry.Unix()), 0); err == nil {
		t.Fatal("the revoked token was ACCEPTED — the denylist is not wired into verification")
	}
}

// signNexTokenLikeAccountServer reimplements nextendo-account's signNexToken, so
// the test proves the two agree rather than testing a helper against itself.
func signNexTokenLikeAccountServer(secret string, pid uint64, username string, expiry time.Time) string {
	payload := fmt.Sprintf("%d.%s.%d", pid, username, expiry.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("nex:" + payload))
	return "nx2." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// idTokenWithNnex builds the BAAS id_token a console presents, carrying the nx2
// binding in the "nnex" claim.
func idTokenWithNnex(t *testing.T, sub, nnex string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nextendo-baas-key-1"}`))
	body, err := json.Marshal(map[string]any{
		"sub":      sub,
		"bs:did":   "581ea786a91f1689",
		"exp":      exp,
		"iat":      time.Now().Unix(),
		"nintendo": map[string]any{"ai": "0100c2500fc20000"},
		"nnex":     nnex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}
