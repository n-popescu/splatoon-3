package token

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	i, err := NewIssuer(Options{
		KeyFile:  filepath.Join(t.TempDir(), "key.pem"),
		Issuer:   "test",
		TenantID: "t-dce9377b-lp1",
		AppID:    "0100c2500fc20000",
	})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

// TestAccessTokenRoundTrip checks that a minted token verifies and carries the
// identity every service downstream relies on.
func TestAccessTokenRoundTrip(t *testing.T) {
	i := newTestIssuer(t)
	tok, err := i.IssueAccess("u-abc", "a-def", "cafebabe12345678", 1800000042, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := i.VerifyAccess(tok)
	if err != nil {
		t.Fatalf("VerifyAccess: %v", err)
	}
	if claims.Sub != "u-abc" {
		t.Errorf("sub = %q, want u-abc", claims.Sub)
	}
	if claims.PID != 1800000042 {
		t.Errorf("pid = %d, want 1800000042", claims.PID)
	}
	if claims.Npln.TenantID != "t-dce9377b-lp1" {
		t.Errorf("tenant = %q", claims.Npln.TenantID)
	}
	if claims.Npln.ExtIDType != 1 {
		t.Errorf("ext_id_type = %d, want 1 (NSA_ID)", claims.Npln.ExtIDType)
	}
	if claims.Anonymous {
		t.Error("a normal token is marked anonymous")
	}
}

// TestAccessTokenShape checks the compact JWS shape a client expects: an ES256
// header with a jku and a kid, and a 64-byte R||S signature (NOT the ASN.1 form
// crypto/ecdsa produces by default, which a JWS verifier rejects).
func TestAccessTokenShape(t *testing.T) {
	i := newTestIssuer(t)
	tok, err := i.IssueAccess("u-abc", "a-def", "nsa", 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("header is not base64url: %v", err)
	}
	var h map[string]string
	if err := json.Unmarshal(header, &h); err != nil {
		t.Fatalf("header is not JSON: %v", err)
	}
	if h["alg"] != "ES256" {
		t.Errorf("alg = %q, want ES256", h["alg"])
	}
	if h["jku"] == "" || h["kid"] == "" {
		t.Errorf("header is missing jku/kid: %v", h)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	if len(sig) != 64 {
		t.Errorf("signature is %d bytes, want the 64-byte R||S pair", len(sig))
	}
}

// TestTamperedTokenRejected: flipping a claim must invalidate the signature.
func TestTamperedTokenRejected(t *testing.T) {
	i := newTestIssuer(t)
	tok, err := i.IssueAccess("u-abc", "a-def", "nsa", 1800000042, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims AccessClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims.PID = 1800000001 // become somebody else
	claims.Sub = "u-victim"
	forged, _ := json.Marshal(claims)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]

	if _, err := i.VerifyAccess(tampered); err == nil {
		t.Fatal("a tampered token verified")
	}
}

// TestExpiredTokenRejectedDistinctly: the client must be told to REFRESH rather
// than treated as unauthenticated, so the error has to be distinguishable.
func TestExpiredTokenRejectedDistinctly(t *testing.T) {
	i := newTestIssuer(t)
	i.accessTTLDur = -time.Minute // already expired
	tok, err := i.IssueAccess("u-abc", "a-def", "nsa", 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = i.VerifyAccess(tok)
	if err != ErrExpired {
		t.Fatalf("error = %v, want ErrExpired", err)
	}
}

// TestKeyIsReused: restarting must not invalidate every live token, so the key is
// persisted and reloaded.
func TestKeyIsReused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	first, err := NewIssuer(Options{KeyFile: path, TenantID: "t", AppID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := first.IssueAccess("u-abc", "a-def", "nsa", 7, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewIssuer(Options{KeyFile: path, TenantID: "t", AppID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.VerifyAccess(tok); err != nil {
		t.Fatalf("a token minted before the restart no longer verifies: %v", err)
	}
}

// TestDelegationToken covers the couch-co-op consent token.
func TestDelegationToken(t *testing.T) {
	i := newTestIssuer(t)
	tok, ttl, err := i.IssueDelegation("u-second-player", "u-console", []int32{1}, 1800000042)
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Error("delegation ttl must be positive")
	}
	claims, err := i.VerifyDelegation(tok)
	if err != nil {
		t.Fatalf("VerifyDelegation: %v", err)
	}
	if claims.Delegator != "u-second-player" || claims.Mandatary != "u-console" {
		t.Errorf("claims = %+v", claims)
	}
}

// TestPublicKeyPEM: IssuePublicKey must publish a usable public key, and never the
// private one.
func TestPublicKeyPEM(t *testing.T) {
	i := newTestIssuer(t)
	pem, err := i.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pem, "BEGIN PUBLIC KEY") {
		t.Errorf("PEM does not hold a public key:\n%s", pem)
	}
	if strings.Contains(pem, "PRIVATE") {
		t.Fatal("the private key leaked into the published PEM")
	}
}

// TestMatchmakingTokenCarriesSession: peers verify each other with these, so the
// session and user must be in the claims.
func TestMatchmakingTokenCarriesSession(t *testing.T) {
	i := newTestIssuer(t)
	tok, err := i.IssueMatchmaking("u-abc", "tenants/t/gameSessions/gs-1", "tenants/t/gameSessions/gs-1/userSessions/us-1", "nsa", 42)
	if err != nil {
		t.Fatal(err)
	}
	var claims MatchmakingClaims
	if err := i.verify(tok, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.GameSession != "tenants/t/gameSessions/gs-1" || claims.Sub != "u-abc" || claims.PID != 42 {
		t.Errorf("claims = %+v", claims)
	}
}
