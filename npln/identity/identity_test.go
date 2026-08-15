package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// testSecret stands in for NEXTENDO_SECRET.
var testSecret = []byte("nextendo-test-secret-0123456789")

// makeIDToken builds an unsigned BAAS-shaped id token with the given claims.
func makeIDToken(t *testing.T, sub, bsdid, appID, nnex string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nextendo-baas-key-1"}`))
	claims := map[string]any{
		"sub":    sub,
		"bs:did": bsdid,
		"exp":    exp,
		"iat":    time.Now().Unix(),
		"nintendo": map[string]any{
			"ai": appID,
		},
	}
	if nnex != "" {
		claims["nnex"] = nnex
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

// signNexToken is nextendo-account's signNexToken, reimplemented here so the test
// proves the two agree rather than testing our own helper against itself.
func signNexToken(secret []byte, pid uint64, username string, expiry time.Time) string {
	payload := fmt.Sprintf("%d.%s.%d", pid, username, expiry.Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("nex:" + payload))
	return "nx2." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newTestResolver(t *testing.T, requireProof bool, known map[string]uint64) *Resolver {
	t.Helper()
	r, err := NewResolver(Options{
		Secret:       testSecret,
		AppID:        "0100c2500fc20000",
		RequireProof: requireProof,
		LookupNSA: func(nsa string) (uint64, error) {
			if pid, ok := known[nsa]; ok {
				return pid, nil
			}
			return 0, fmt.Errorf("unknown")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestResolveWithSignedBinding is the strong path: the nx2 token in the id token
// names the account, so no directory lookup is needed.
func TestResolveWithSignedBinding(t *testing.T) {
	r := newTestResolver(t, false, nil)
	nnex := signNexToken(testSecret, 1800000042, "player", time.Now().Add(time.Hour))
	tok := makeIDToken(t, "abcdef0123456789", "1234567890abcdef", "0100c2500fc20000", nnex, time.Now().Add(time.Hour).Unix())

	id, err := r.Resolve(tok, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id.PID != 1800000042 {
		t.Errorf("PID = %d, want 1800000042", id.PID)
	}
	if !id.Proven {
		t.Error("Proven = false, want true for a signed binding")
	}
	if id.NsaID != "abcdef0123456789" {
		t.Errorf("NsaID = %q, want the token's sub", id.NsaID)
	}
}

// TestResolveRejectsForgedBinding: a PRESENT but invalid nx2 must be refused
// outright, never silently downgraded to the weaker NSA lookup.
func TestResolveRejectsForgedBinding(t *testing.T) {
	// The attacker knows the NSA id of a real account and forges an nx2 for
	// another PID with the wrong key.
	r := newTestResolver(t, false, map[string]uint64{"abcdef0123456789": 1800000042})
	forged := signNexToken([]byte("wrong-secret"), 1800000001, "victim", time.Now().Add(time.Hour))
	tok := makeIDToken(t, "abcdef0123456789", "1234567890abcdef", "0100c2500fc20000", forged, time.Now().Add(time.Hour).Unix())

	if _, err := r.Resolve(tok, 0); err == nil {
		t.Fatal("Resolve accepted a forged nx2 binding")
	}
}

// TestResolveExpiredBinding: an expired nx2 is not a valid proof.
func TestResolveExpiredBinding(t *testing.T) {
	r := newTestResolver(t, true, nil)
	expired := signNexToken(testSecret, 1800000042, "player", time.Now().Add(-time.Minute))
	tok := makeIDToken(t, "abcdef0123456789", "did", "0100c2500fc20000", expired, time.Now().Add(time.Hour).Unix())
	if _, err := r.Resolve(tok, 0); err == nil {
		t.Fatal("Resolve accepted an expired nx2 binding")
	}
}

// TestResolveByNSA is the retail-console path: no nx2, so the NSA id is looked up.
func TestResolveByNSA(t *testing.T) {
	r := newTestResolver(t, false, map[string]uint64{"cafebabe12345678": 1800000007})
	tok := makeIDToken(t, "cafebabe12345678", "did", "0100c2500fc20000", "", time.Now().Add(time.Hour).Unix())
	id, err := r.Resolve(tok, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id.PID != 1800000007 {
		t.Errorf("PID = %d, want 1800000007", id.PID)
	}
	if id.Proven {
		t.Error("Proven = true, want false for the NSA path")
	}
}

// TestResolveUnknownNSAFailsClosed is the rule the friend bug came from: an
// identity we cannot resolve must be an ERROR, never a fallback account.
func TestResolveUnknownNSAFailsClosed(t *testing.T) {
	r := newTestResolver(t, false, map[string]uint64{"cafebabe12345678": 1800000007})
	tok := makeIDToken(t, "0000000000000000", "did", "0100c2500fc20000", "", time.Now().Add(time.Hour).Unix())
	id, err := r.Resolve(tok, 0)
	if err == nil {
		t.Fatalf("Resolve returned identity %+v for an unknown console; it must fail closed", id)
	}
}

// TestResolveWrongApplication: a token minted for another title is refused.
func TestResolveWrongApplication(t *testing.T) {
	r := newTestResolver(t, false, map[string]uint64{"cafebabe12345678": 1800000007})
	tok := makeIDToken(t, "cafebabe12345678", "did", "0100f8f0000a2000", "", time.Now().Add(time.Hour).Unix())
	if _, err := r.Resolve(tok, 0); err == nil {
		t.Fatal("Resolve accepted a Splatoon 2 id token")
	}
}

// TestResolveExpiredIDToken: an expired id token is refused.
func TestResolveExpiredIDToken(t *testing.T) {
	r := newTestResolver(t, false, map[string]uint64{"cafebabe12345678": 1800000007})
	tok := makeIDToken(t, "cafebabe12345678", "did", "0100c2500fc20000", "", time.Now().Add(-time.Minute).Unix())
	if _, err := r.Resolve(tok, 0); err == nil {
		t.Fatal("Resolve accepted an expired id token")
	}
}

// TestRequireProof: with the binding required, a retail-style token (no nnex) is
// refused instead of falling back.
func TestRequireProof(t *testing.T) {
	r := newTestResolver(t, true, map[string]uint64{"cafebabe12345678": 1800000007})
	tok := makeIDToken(t, "cafebabe12345678", "did", "0100c2500fc20000", "", time.Now().Add(time.Hour).Unix())
	if _, err := r.Resolve(tok, 0); err == nil {
		t.Fatal("Resolve accepted an unproven identity while proof was required")
	}
}

// TestUserIDMatchesAccountServer is the most important test in this package.
//
// nextendo-account derives the NPLN user id of an account with
// "u-" + base32(HMAC(secret, "npln-user:" + pid_le)[:12]) (npln_friends.go). This
// server must derive the SAME id for slot 0, or the friend list it builds from the
// account server's data will never match the logged-in user.
func TestUserIDMatchesAccountServer(t *testing.T) {
	r := newTestResolver(t, false, nil)
	const pid = uint64(1800000042)

	// Reference implementation, copied from nextendo-account/npln_friends.go.
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], pid)
	h := hmac.New(sha256.New, testSecret)
	h.Write([]byte("npln-user:"))
	h.Write(b[:])
	enc := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	want := "u-" + enc.EncodeToString(h.Sum(nil)[:12])

	if got := r.UserID(pid, 0); got != want {
		t.Errorf("UserID(slot 0) = %q, want %q (the account server's derivation)", got, want)
	}
	// Extra local players must NOT collide with slot 0 of any account.
	if got := r.UserID(pid, 1); got == want {
		t.Error("slot 1 derives the same user id as slot 0")
	}
}

// TestNsaIDMatchesAccountServer: the derived BAAS id must match
// nextendo-account's deriveID(secret, "baas", pid).
func TestNsaIDMatchesAccountServer(t *testing.T) {
	r := newTestResolver(t, false, nil)
	const pid = uint64(1800000042)
	h := hmac.New(sha256.New, testSecret)
	fmt.Fprintf(h, "baas:%d", pid)
	want := fmt.Sprintf("%x", h.Sum(nil)[:8])
	if got := r.NsaID(pid); got != want {
		t.Errorf("NsaID = %q, want %q", got, want)
	}
}

// TestAccountIDStable: the NPLN account id is deterministic and distinct from the
// user id.
func TestAccountIDStable(t *testing.T) {
	r := newTestResolver(t, false, nil)
	a1 := r.AccountID(1800000042)
	a2 := r.AccountID(1800000042)
	if a1 != a2 {
		t.Errorf("AccountID is not stable: %q vs %q", a1, a2)
	}
	if a1 == r.UserID(1800000042, 0) {
		t.Error("AccountID equals UserID; they must be distinct namespaces")
	}
	if len(a1) < 3 || a1[:2] != "a-" {
		t.Errorf("AccountID = %q, want an \"a-\" prefix", a1)
	}
}
