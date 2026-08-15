// Package server assembles the gRPC server: TLS, metadata validation,
// authentication, logging, and the unknown-method handler that makes bringing
// the title up possible.
package server

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/NextendoNetwork/splatoon-3/npln/token"
)

// Metadata field names NPLN uses. `npln-tenant-id` is always required;
// `authorization` and `uid` only on methods that need an identity.
const (
	MDTenant        = "npln-tenant-id"
	MDAuthorization = "authorization"
	MDUID           = "uid"
)

// Caller is the authenticated identity behind an RPC, derived from the access
// token. It is put in the context by the auth interceptor so no service ever
// has to re-parse a token — and, just as importantly, so no service can invent
// an identity of its own.
type Caller struct {
	// UserID is the NPLN user id ("u-…") the token was issued for.
	UserID string
	// AccountID is the NPLN account id ("a-…").
	AccountID string
	// NsaID is the console's BAAS/NSA id.
	NsaID string
	// PID is the Nextendo account principal id.
	PID uint64
	// Anonymous reports whether this is the anonymous user, which most services
	// must refuse.
	Anonymous bool
	// UserIndex is the prearranged-user slot (0 = the console's main player).
	UserIndex int32
	// Token is the raw access token, kept for logging correlation only.
	Token string
}

type callerKey struct{}

// WithCaller returns a context carrying the authenticated caller.
func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the authenticated caller, if the method required one.
func CallerFrom(ctx context.Context) (*Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(*Caller)
	return c, ok && c != nil
}

// callerFromClaims converts verified token claims into a Caller.
func callerFromClaims(raw string, claims *token.AccessClaims) *Caller {
	return &Caller{
		UserID:    claims.Sub,
		AccountID: claims.Npln.AccountID,
		NsaID:     claims.Npln.ExternalID,
		PID:       claims.PID,
		Anonymous: claims.Anonymous,
		UserIndex: claims.UserIndex,
		Token:     raw,
	}
}

// bearer extracts the token from an "authorization: bearer <token>" value. The
// scheme is case-insensitive because the retail client sends it lowercase while
// most tooling sends "Bearer".
func bearer(values []string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
	}
	return ""
}

// metaFirst returns the first value of a metadata key, or "".
func metaFirst(md metadata.MD, key string) string {
	if vs := md.Get(key); len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}
