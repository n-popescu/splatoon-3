// Package identity turns what a console presents into a Nextendo identity, and
// derives every NPLN name from it.
//
// # The chain
//
// A Switch (or the Nextendo emulator) reaches NPLN with a BAAS `id_token`: a
// JWT issued by the account layer that identifies the *user on that console*
// and proves ownership of the title. NPLN's Auth service takes it as the
// `external_id_token.nsa_id_token`.
//
//	id_token claims we care about
//	  sub          the BAAS/NSA user id (16 hex) = nextendo-account's baas_id
//	  bs:did       the device account id         = nextendo-account's bs_did
//	  nintendo.ai  the application id            (must be Splatoon 3)
//	  nnex         Nextendo EXTRA claim: the signed "nx2." token that proves
//	               WHICH Nextendo account (PID) this console is logged into
//
// The `nnex` claim is what makes identity here cryptographic instead of
// advisory: it is an HMAC over "pid.username.expiry" keyed with the secret
// shared with nextendo-account, so a client cannot claim to be another player.
// This is exactly the binding splatoon-2 enforces at LoginEx; we reuse it.
//
// # Why we never fall back to "some account"
//
// The Switch-side friend bug this repository documents (docs/FRIENDS.md) is a
// direct consequence of a server resolving "who is calling" loosely and then
// falling back to a default identity when resolution failed — every console
// then acted as, and displayed the friend code of, that one account. So the
// rules here are:
//
//   - resolution is per-request, from the token the request carries;
//   - a token we cannot resolve is an ERROR (Unauthenticated), never a default;
//   - nothing is ever cached "globally", only per resolved identity.
package identity

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Errors returned by Resolve. Callers map them to NPLN/gRPC statuses.
var (
	// ErrNoToken means the request carried no external id token at all.
	ErrNoToken = errors.New("identity: no external id token")
	// ErrMalformedToken means the token is not a readable JWT.
	ErrMalformedToken = errors.New("identity: malformed id token")
	// ErrExpiredToken means the id token's exp is in the past.
	ErrExpiredToken = errors.New("identity: id token expired")
	// ErrBadSignature means the RS256 signature did not verify.
	ErrBadSignature = errors.New("identity: id token signature invalid")
	// ErrWrongApplication means the token was minted for another title.
	ErrWrongApplication = errors.New("identity: id token is for another application")
	// ErrUnproven means no signed Nextendo binding was present but one is required.
	ErrUnproven = errors.New("identity: no signed Nextendo token in id token")
	// ErrUnknownAccount means the console's identity matches no Nextendo account.
	ErrUnknownAccount = errors.New("identity: no Nextendo account for this console")
)

// nplnUserB32 is the alphabet NPLN user/account ids are printed in. It matches
// nextendo-account/npln_friends.go so both sides derive the same id.
var nplnUserB32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Claims is the subset of a BAAS id_token this server reads.
type Claims struct {
	Subject  string // "sub": the BAAS/NSA user id (16 hex)
	DeviceID string // "bs:did": the device account id
	AppID    string // "nintendo"."ai": the title id
	Nnex     string // "nnex": the signed nx2 Nextendo token (Nextendo extension)
	IssuedAt int64
	Expires  int64
}

// Identity is one authenticated player: the Nextendo account behind the console
// plus every NPLN name derived from it.
type Identity struct {
	// PID is the Nextendo account principal id (the id every other Nextendo
	// service — the website, the NEX games, the friend graph — knows the player by).
	PID uint64
	// NsaID is the console-visible BAAS/NSA id (16 hex). NPLN reports it to
	// friends as FriendUser.nsa_id, and the Switch friend list matches on it.
	NsaID string
	// DeviceID is the "bs:did" the console presented, kept for logging only.
	DeviceID string
	// UserIndex is the prearranged-user slot (0 for the console's main user;
	// a second local player on the same console gets 1, and so on).
	UserIndex int32
	// UserID is the NPLN user id ("u-…"). For UserIndex 0 it is byte-identical
	// to nextendo-account's nplnUserID(pid), so the friend graph lines up.
	UserID string
	// AccountID is the NPLN account id ("a-…"). Accounts are per-console-user
	// and shared across tenants; users are per-tenant.
	AccountID string
	// Proven reports whether the identity was established from the signed nx2
	// binding (true) or from the NSA id alone (false).
	Proven bool
}

// Resolver turns id tokens into identities. It is safe for concurrent use.
type Resolver struct {
	secret []byte
	appID  string

	requireProof bool
	verifySig    bool
	pubKey       *rsa.PublicKey

	// lookupNSA resolves an NSA/BAAS id (16 hex) to a Nextendo account PID.
	// Provided by the caller so this package stays free of HTTP concerns; it is
	// backed by nextendo-account's /internal/resolve?baas=<hex>.
	lookupNSA func(nsaHex string) (uint64, error)
}

// Options configures a Resolver.
type Options struct {
	// Secret is the shared Nextendo secret (NEXTENDO_SECRET).
	Secret []byte
	// AppID is the expected title id; "" disables the check.
	AppID string
	// RequireProof demands the signed nx2 binding (emulator >= 1.7.1). A retail
	// CFW Switch cannot provide it, so leave it off for mixed deployments.
	RequireProof bool
	// VerifySignature verifies the id token's RS256 signature.
	VerifySignature bool
	// SigningKeyFile is the RSA key the BAAS id_token is signed with. Either a
	// private key (the same file the signer uses — we only read its public
	// half) or a public key.
	SigningKeyFile string
	// LookupNSA resolves an NSA id to a PID; required.
	LookupNSA func(nsaHex string) (uint64, error)
}

// NewResolver builds a Resolver, loading the signature-verification key when
// signature verification is enabled.
func NewResolver(o Options) (*Resolver, error) {
	r := &Resolver{
		secret:       o.Secret,
		appID:        strings.ToLower(o.AppID),
		requireProof: o.RequireProof,
		verifySig:    o.VerifySignature,
		lookupNSA:    o.LookupNSA,
	}
	if r.lookupNSA == nil {
		return nil, errors.New("identity: LookupNSA is required")
	}
	if r.verifySig {
		if o.SigningKeyFile == "" {
			return nil, errors.New("identity: signature verification requested without a key file")
		}
		key, err := loadRSAPublicKey(o.SigningKeyFile)
		if err != nil {
			return nil, err
		}
		r.pubKey = key
	}
	return r, nil
}

// Resolve authenticates an external id token and returns the identity behind it.
//
// userIndex is the prearranged-user slot the client asked for (0 unless the
// console has several local players in the same match).
func (r *Resolver) Resolve(idToken string, userIndex int32) (*Identity, error) {
	if strings.TrimSpace(idToken) == "" {
		return nil, ErrNoToken
	}
	claims, signed, err := parseJWT(idToken)
	if err != nil {
		return nil, err
	}
	if claims.Expires > 0 && time.Now().Unix() > claims.Expires {
		return nil, ErrExpiredToken
	}
	if r.appID != "" && claims.AppID != "" && strings.ToLower(claims.AppID) != r.appID {
		return nil, fmt.Errorf("%w: %s", ErrWrongApplication, claims.AppID)
	}
	if r.verifySig {
		if err := verifyRS256(r.pubKey, signed.signingInput, signed.signature); err != nil {
			return nil, err
		}
	}

	// 1. The strong path: the signed nx2 binding names the account outright.
	if claims.Nnex != "" {
		if pid, ok := r.pidFromNexToken(claims.Nnex); ok {
			return r.identity(pid, claims, userIndex, true), nil
		}
		// A PRESENT but INVALID binding is an attack signal, not a fallback:
		// somebody edited a token. Refuse rather than quietly downgrading to
		// the weaker NSA path.
		return nil, ErrBadSignature
	}
	if r.requireProof {
		return nil, ErrUnproven
	}

	// 2. The retail-console path: the NSA id in "sub" is looked up in the
	//    Nextendo friend/account directory. Fail-closed — an NSA id that
	//    belongs to no account never plays online (and, crucially, never
	//    borrows somebody else's identity).
	if claims.Subject == "" {
		return nil, ErrMalformedToken
	}
	pid, err := r.lookupNSA(claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("%w (nsa=%s): %v", ErrUnknownAccount, claims.Subject, err)
	}
	return r.identity(pid, claims, userIndex, false), nil
}

// identity assembles the derived NPLN names for a resolved PID.
func (r *Resolver) identity(pid uint64, claims *Claims, userIndex int32, proven bool) *Identity {
	nsa := claims.Subject
	if nsa == "" {
		nsa = r.NsaID(pid)
	}
	return &Identity{
		PID:       pid,
		NsaID:     nsa,
		DeviceID:  claims.DeviceID,
		UserIndex: userIndex,
		UserID:    r.UserID(pid, userIndex),
		AccountID: r.AccountID(pid),
		Proven:    proven,
	}
}

// UserID derives the NPLN user id for an account.
//
// Slot 0 MUST equal nextendo-account's nplnUserID(pid) — that function is what
// the account server publishes to this service as the identity of a friend, so
// a mismatch would make every friend list disagree with the logged-in user.
// Extra slots (a second local player on the same console) are domain-separated
// so they cannot collide with another account's slot 0.
func (r *Resolver) UserID(pid uint64, userIndex int32) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], pid)
	h := hmac.New(sha256.New, r.secret)
	if userIndex == 0 {
		h.Write([]byte("npln-user:"))
	} else {
		fmt.Fprintf(h, "npln-user-%d:", userIndex)
	}
	h.Write(b[:])
	return "u-" + nplnUserB32.EncodeToString(h.Sum(nil)[:12])
}

// AccountID derives the NPLN account id ("a-…") of an account.
func (r *Resolver) AccountID(pid uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], pid)
	h := hmac.New(sha256.New, r.secret)
	h.Write([]byte("npln-account:"))
	h.Write(b[:])
	return "a-" + nplnUserB32.EncodeToString(h.Sum(nil)[:12])
}

// NsaID derives the BAAS/NSA id of an account the same way nextendo-account
// does (deriveID(secret, "baas", pid)). Used when a token did not carry one.
func (r *Resolver) NsaID(pid uint64) string {
	h := hmac.New(sha256.New, r.secret)
	fmt.Fprintf(h, "baas:%d", pid)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ShortID is the small integer id NPLN exposes next to a user
// (auth.v1.User.short_id, QueryUserShortIds). We use the account PID: it is
// already a compact, stable, unique per-account number in the Nextendo world,
// which keeps every log line correlatable across services.
func ShortID(pid uint64) int64 { return int64(pid) }

// pidFromNexToken validates a "nx2.<b64(pid.username.expiry)>.<b64(hmac)>"
// token minted by nextendo-account and returns the account PID.
//
// Kept byte-compatible with nextendo-account's signNexToken and with each NEX
// game server's nextendoPIDFromToken: the MAC covers "nex:" + payload.
func (r *Resolver) pidFromNexToken(tok string) (uint64, bool) {
	if len(r.secret) == 0 || !strings.HasPrefix(tok, "nx2.") {
		return 0, false
	}
	parts := strings.Split(tok[len("nx2."):], ".")
	if len(parts) != 2 {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, false
	}
	mac := hmac.New(sha256.New, r.secret)
	mac.Write([]byte("nex:" + string(raw)))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return 0, false
	}
	fields := strings.SplitN(string(raw), ".", 3) // pid.username.expiry
	if len(fields) != 3 {
		return 0, false
	}
	pid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	if exp, err := strconv.ParseInt(fields[2], 10, 64); err != nil || time.Now().Unix() > exp {
		return 0, false
	}
	return pid, true
}

// signedParts holds the raw pieces needed to verify a JWT signature.
type signedParts struct {
	signingInput []byte // header + "." + payload, as transmitted
	signature    []byte
}

// parseJWT decodes a compact JWS without verifying it, returning the claims we
// use plus the raw pieces a signature check needs.
func parseJWT(token string) (*Claims, *signedParts, error) {
	segs := strings.Split(strings.TrimSpace(token), ".")
	if len(segs) < 2 {
		return nil, nil, ErrMalformedToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segs[1], "="))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: payload: %v", ErrMalformedToken, err)
	}
	var raw struct {
		Sub      string `json:"sub"`
		BsDid    string `json:"bs:did"`
		Nnex     string `json:"nnex"`
		Iat      int64  `json:"iat"`
		Exp      int64  `json:"exp"`
		Nintendo struct {
			AI string `json:"ai"` // application id
		} `json:"nintendo"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, nil, fmt.Errorf("%w: claims: %v", ErrMalformedToken, err)
	}
	claims := &Claims{
		Subject:  raw.Sub,
		DeviceID: raw.BsDid,
		AppID:    raw.Nintendo.AI,
		Nnex:     raw.Nnex,
		IssuedAt: raw.Iat,
		Expires:  raw.Exp,
	}
	sp := &signedParts{signingInput: []byte(segs[0] + "." + segs[1])}
	if len(segs) >= 3 {
		if sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segs[2], "=")); err == nil {
			sp.signature = sig
		}
	}
	return claims, sp, nil
}

// verifyRS256 checks a JWT's RSA-SHA256 signature.
func verifyRS256(pub *rsa.PublicKey, signingInput, signature []byte) error {
	if pub == nil {
		return ErrBadSignature
	}
	if len(signature) == 0 {
		return ErrBadSignature
	}
	sum := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], signature); err != nil {
		return fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	return nil
}

// loadRSAPublicKey reads an RSA public key from a PEM file that may hold either
// a private key (PKCS#1 or PKCS#8 — we take its public half) or a public key.
// This mirrors baas-jwks, which is pointed at the signer's private key file.
func loadRSAPublicKey(path string) (*rsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("identity: no PEM block in %s", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &k.PublicKey, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return &rk.PublicKey, nil
		}
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
	}
	if k, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("identity: %s holds no usable RSA key", path)
}
