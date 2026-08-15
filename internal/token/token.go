// Package token issues and verifies the tokens the NPLN services hand out.
//
// Three kinds exist, all of them JWTs signed with the same ES256 key:
//
//	access token          returned by Auth.IssueToken & friends; the client
//	                      sends it back in the `authorization` metadata field
//	                      on every subsequent RPC.
//	matchmaking id token  minted per matched user session; a game session's
//	                      peers exchange it to prove who they are to each other
//	                      (this is what IssuePublicKey lets them verify).
//	delegation token      lets one console act on behalf of a second local
//	                      player (couch co-op) — it names delegator, mandatary
//	                      and what the mandatary is allowed to do.
//
// The shape of the access token follows the documented retail one:
//
//	header  {"alg":"ES256","jku":"jwkSets/nplnAccessToken","kid":"<uuid>"}
//	claims  {"exp","iat","iss","sub":"<user id>","npln":{
//	          "aid":"<account id>","app_id":"<title id>","tid":"<tenant id>",
//	          "ext_id":"<nsa id>","ext_id_type":1,
//	          "authorization":{"allow":["**"],"deny":[],"nso_restricted":false}}}
//
// ES256 (not HMAC) is used because the retail token is ES256 and because
// IssuePublicKey is expected to publish a *public* key: with a symmetric key
// there would be nothing safe to publish.
package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Errors returned when validating a token.
var (
	ErrMalformed = errors.New("token: malformed")
	ErrSignature = errors.New("token: bad signature")
	ErrExpired   = errors.New("token: expired")
)

// AccessClaims is the payload of an access token.
type AccessClaims struct {
	Exp  int64      `json:"exp"`
	Iat  int64      `json:"iat"`
	Iss  string     `json:"iss"`
	Sub  string     `json:"sub"` // the NPLN user id
	Npln NplnClaims `json:"npln"`
	// PID is a Nextendo addition: it saves every service a round-trip to the
	// account server to learn which Nextendo account is calling. It is inside a
	// signed token, so it is as trustworthy as the rest of the claims.
	PID uint64 `json:"nx_pid,omitempty"`
	// Anonymous marks the anonymous-user token (Auth.IssueAnonymousUserToken).
	// Most services must refuse it, exactly like retail NPLN does.
	Anonymous bool `json:"nx_anon,omitempty"`
	// UserIndex is the prearranged-user slot this token was issued for.
	UserIndex int32 `json:"nx_idx,omitempty"`
}

// NplnClaims is the "npln" object inside an access token.
type NplnClaims struct {
	AccountID     string        `json:"aid"`
	AppID         string        `json:"app_id"`
	TenantID      string        `json:"tid"`
	ExternalID    string        `json:"ext_id"`
	ExtIDType     int           `json:"ext_id_type"`
	Authorization Authorization `json:"authorization"`
}

// Authorization is the allow/deny list retail NPLN carries in its tokens.
type Authorization struct {
	Allow         []string `json:"allow"`
	Deny          []string `json:"deny"`
	NsoRestricted bool     `json:"nso_restricted"`
}

// MatchmakingClaims is the payload of a matchmaking id token.
type MatchmakingClaims struct {
	Exp         int64  `json:"exp"`
	Iat         int64  `json:"iat"`
	Iss         string `json:"iss"`
	Sub         string `json:"sub"` // the NPLN user id
	GameSession string `json:"gs"`
	UserSession string `json:"us"`
	NsaID       string `json:"ext_id"`
	PID         uint64 `json:"nx_pid,omitempty"`
}

// DelegationClaims is the payload of a user delegation token.
type DelegationClaims struct {
	Exp       int64    `json:"exp"`
	Iat       int64    `json:"iat"`
	Iss       string   `json:"iss"`
	Delegator string   `json:"delegator"`
	Mandatary string   `json:"mandatary"`
	Actions   []int32  `json:"actions"`
	PID       uint64   `json:"nx_pid,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

// Issuer signs and verifies every token this server hands out.
type Issuer struct {
	key      *ecdsa.PrivateKey
	kid      string
	iss      string
	tenantID string
	appID    string

	accessTTL      string
	accessTTLDur   time.Duration
	matchTokenTTL  time.Duration
	delegationTTL  time.Duration
}

// Options configures an Issuer.
type Options struct {
	// KeyFile is where the ES256 signing key lives. It is generated on first
	// start and reused afterwards, so tokens survive a restart (a fresh key
	// would invalidate every in-flight session at once).
	KeyFile  string
	Issuer   string
	TenantID string
	AppID    string
	// AccessTTL defaults to the 8 hours retail NPLN reports.
	AccessTTL time.Duration
}

// NewIssuer loads (or creates) the signing key.
func NewIssuer(o Options) (*Issuer, error) {
	if o.AccessTTL <= 0 {
		o.AccessTTL = 8 * time.Hour
	}
	if o.Issuer == "" {
		o.Issuer = "nextendo-npln"
	}
	key, kid, err := loadOrCreateKey(o.KeyFile)
	if err != nil {
		return nil, err
	}
	return &Issuer{
		key:           key,
		kid:           kid,
		iss:           o.Issuer,
		tenantID:      o.TenantID,
		appID:         o.AppID,
		accessTTLDur:  o.AccessTTL,
		matchTokenTTL: time.Hour,
		delegationTTL: time.Hour,
	}, nil
}

// AccessTTL is the lifetime of the access tokens this issuer mints.
func (i *Issuer) AccessTTL() time.Duration { return i.accessTTLDur }

// IssueAccess mints an access token for a user.
func (i *Issuer) IssueAccess(userID, accountID, nsaID string, pid uint64, userIndex int32, anonymous bool) (string, error) {
	now := time.Now()
	claims := AccessClaims{
		Exp: now.Add(i.accessTTLDur).Unix(),
		Iat: now.Unix(),
		Iss: i.iss,
		Sub: userID,
		Npln: NplnClaims{
			AccountID:  accountID,
			AppID:      i.appID,
			TenantID:   i.tenantID,
			ExternalID: nsaID,
			ExtIDType:  1, // NSA_ID
			Authorization: Authorization{
				// A Nextendo deployment has no per-service entitlements and no
				// paid membership, so every authenticated user may call every
				// service and nso_restricted is false. Kept as an explicit
				// claim (rather than omitted) because the client reads it.
				Allow:         []string{"**"},
				Deny:          []string{},
				NsoRestricted: false,
			},
		},
		PID:       pid,
		Anonymous: anonymous,
		UserIndex: userIndex,
	}
	return i.sign("jwkSets/nplnAccessToken", claims)
}

// VerifyAccess checks an access token and returns its claims.
func (i *Issuer) VerifyAccess(tok string) (*AccessClaims, error) {
	var claims AccessClaims
	if err := i.verify(tok, &claims); err != nil {
		return nil, err
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, ErrExpired
	}
	return &claims, nil
}

// IssueMatchmaking mints the per-user-session token peers exchange in a match.
func (i *Issuer) IssueMatchmaking(userID, gameSession, userSession, nsaID string, pid uint64) (string, error) {
	now := time.Now()
	return i.sign("jwkSets/nplnMatchmakingIdToken", MatchmakingClaims{
		Exp:         now.Add(i.matchTokenTTL).Unix(),
		Iat:         now.Unix(),
		Iss:         i.iss,
		Sub:         userID,
		GameSession: gameSession,
		UserSession: userSession,
		NsaID:       nsaID,
		PID:         pid,
	})
}

// IssueDelegation mints a user delegation token.
func (i *Issuer) IssueDelegation(delegator, mandatary string, actions []int32, pid uint64) (string, time.Duration, error) {
	now := time.Now()
	tok, err := i.sign("jwkSets/nplnUserDelegationToken", DelegationClaims{
		Exp:       now.Add(i.delegationTTL).Unix(),
		Iat:       now.Unix(),
		Iss:       i.iss,
		Delegator: delegator,
		Mandatary: mandatary,
		Actions:   actions,
		PID:       pid,
	})
	return tok, i.delegationTTL, err
}

// VerifyDelegation checks a delegation token and returns its claims.
func (i *Issuer) VerifyDelegation(tok string) (*DelegationClaims, error) {
	var claims DelegationClaims
	if err := i.verify(tok, &claims); err != nil {
		return nil, err
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, ErrExpired
	}
	return &claims, nil
}

// PublicKeyPEM returns the PEM-encoded public key, which is what
// GameSessionService.IssuePublicKey hands to a client so it can verify the
// matchmaking id tokens of its peers.
func (i *Issuer) PublicKeyPEM() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&i.key.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// sign serialises claims as a compact ES256 JWS.
func (i *Issuer) sign(jku string, claims any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "jku": jku, "kid": i.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, i.key, sum[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 signatures are the fixed-width R||S pair, NOT the ASN.1 form
	// crypto/ecdsa.SignASN1 produces — a verifier that follows the JWS spec
	// (which is what a game client does) rejects the ASN.1 encoding.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// verify checks an ES256 JWS produced by sign and unmarshals its claims.
func (i *Issuer) verify(tok string, out any) error {
	segs := strings.Split(strings.TrimSpace(tok), ".")
	if len(segs) != 3 {
		return ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(segs[2])
	if err != nil || len(sig) != 64 {
		return ErrMalformed
	}
	sum := sha256.Sum256([]byte(segs[0] + "." + segs[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&i.key.PublicKey, sum[:], r, s) {
		return ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(segs[1])
	if err != nil {
		return ErrMalformed
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%w: claims: %v", ErrMalformed, err)
	}
	return nil
}

// loadOrCreateKey reads the ES256 key at path, generating and persisting one on
// first run. The key id is derived from the key itself so it is stable.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, string, error) {
	if path == "" {
		return nil, "", errors.New("token: no key file configured")
	}
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, "", fmt.Errorf("token: no PEM block in %s", path)
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			k8, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err8 != nil {
				return nil, "", fmt.Errorf("token: cannot parse %s: %v / %v", path, err, err8)
			}
			ec, ok := k8.(*ecdsa.PrivateKey)
			if !ok {
				return nil, "", fmt.Errorf("token: %s is not an EC key", path)
			}
			key = ec
		}
		return key, keyID(key), nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	// 0600: this key signs every identity on the server.
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, "", err
	}
	return key, keyID(key), nil
}

// keyID formats a stable key id (a UUID-shaped digest of the public key, which
// is what the retail token's `kid` looks like).
func keyID(key *ecdsa.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	h := sha256.Sum256(der)
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
