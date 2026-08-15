package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
	"github.com/NextendoNetwork/splatoon-3/npln/token"
)

// noAuthMethods are the RPCs a client may call before it has an access token.
// Everything else requires one.
//
// This is the entire trust boundary of the server: an RPC not listed here can
// only run with a verified identity in its context, so no service can be tricked
// into acting for a user it did not authenticate.
var noAuthMethods = map[string]bool{
	"/nn.npln.auth.v1.Auth/CreateUser":                true,
	"/nn.npln.auth.v1.Auth/IssueToken":                true,
	"/nn.npln.auth.v1.Auth/RefreshToken":              true,
	"/nn.npln.auth.v1.Auth/IssuePrearrangedUserToken": true,
	"/nn.npln.auth.v1.Auth/IssueAnonymousUserToken":   true,
	"/nn.npln.auth.v1.Auth/RefreshAnonymousUserToken": true,
	// ValidateToken is listed as "no access token required" by the protocol
	// documentation; we still read the token when one is present so the answer
	// is meaningful.
	"/nn.npln.auth.v1.Auth/ValidateToken": true,
}

// Observer receives per-RPC events. The monitoring dashboard implements it so
// /api/stats knows who is connected and what they are doing, exactly like the
// NEX game servers feed theirs from OnRMC.
type Observer interface {
	NoteRPC(pid uint64, userID, method, peerAddr string)
}

// Interceptors builds the unary and stream interceptors.
type Interceptors struct {
	TenantID string
	Tokens   *token.Issuer
	Observer Observer
	// LogBodies dumps request/response protojson. Only for bring-up.
	LogBodies bool
	// Maintenance, when it returns a non-empty reason, makes every RPC answer
	// UNAVAILABLE_UNDER_MAINTENANCE so the game shows its maintenance screen
	// instead of a communication error.
	Maintenance func() string

	calls atomic.Int64
}

// Calls returns the number of RPCs served since start (for /api/stats).
func (in *Interceptors) Calls() int64 { return in.calls.Load() }

// Unary returns the unary interceptor.
func (in *Interceptors) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		ctx, err := in.enter(ctx, info.FullMethod)
		if err != nil {
			log.Printf("[rpc] %s REFUSED: %v", info.FullMethod, err)
			return nil, err
		}
		if in.LogBodies {
			log.Printf("[rpc] --> %s %s", info.FullMethod, jsonOf(req))
		}
		resp, err := handler(ctx, req)
		took := time.Since(start).Milliseconds()
		if err != nil {
			log.Printf("[rpc] <-- %s %s ERR %v (%dms)", info.FullMethod, in.who(ctx), err, took)
			return nil, err
		}
		if in.LogBodies {
			log.Printf("[rpc] <-- %s %s %s (%dms)", info.FullMethod, in.who(ctx), jsonOf(resp), took)
		} else {
			log.Printf("[rpc] %s %s ok (%dms)", info.FullMethod, in.who(ctx), took)
		}
		return resp, nil
	}
}

// Stream returns the streaming interceptor. Streams are the backbone of NPLN
// (presence, friends, matchmaking progress, messages), so they get the same
// metadata validation and the same caller in their context.
func (in *Interceptors) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		ctx, err := in.enter(ss.Context(), info.FullMethod)
		if err != nil {
			log.Printf("[rpc] %s (stream) REFUSED: %v", info.FullMethod, err)
			return err
		}
		log.Printf("[rpc] %s %s stream opened", info.FullMethod, in.who(ctx))
		err = handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx, logBodies: in.LogBodies, method: info.FullMethod})
		log.Printf("[rpc] %s %s stream closed after %s (err=%v)", info.FullMethod, in.who(ctx), time.Since(start).Truncate(time.Second), err)
		return err
	}
}

// enter validates the NPLN metadata, authenticates the caller when the method
// requires it, and records the call for the dashboard.
func (in *Interceptors) enter(ctx context.Context, method string) (context.Context, error) {
	in.calls.Add(1)

	if in.Maintenance != nil {
		if reason := in.Maintenance(); reason != "" {
			return ctx, nplnerr.UnderMaintenance(reason)
		}
	}

	md, _ := metadata.FromIncomingContext(ctx)

	// The tenant id is mandatory on EVERY request, authenticated or not. Retail
	// NPLN answers Unimplemented when the metadata is invalid; we do the same so
	// a misconfigured client behaves the same way against both.
	tenant := metaFirst(md, MDTenant)
	if tenant == "" || (tenant != in.TenantID && tenant != "current") {
		return ctx, nplnerr.Unimplemented(fmt.Sprintf("invalid %s (%q); this server serves %q", MDTenant, tenant, in.TenantID))
	}

	if noAuthMethods[method] {
		// Best-effort: attach the caller if a valid token happens to be present
		// (ValidateToken needs it), but never fail the call over it.
		if raw := bearer(md.Get(MDAuthorization)); raw != "" {
			if claims, err := in.Tokens.VerifyAccess(raw); err == nil {
				ctx = WithCaller(ctx, callerFromClaims(raw, claims))
			}
		}
		return ctx, nil
	}

	raw := bearer(md.Get(MDAuthorization))
	if raw == "" {
		return ctx, nplnerr.TokenInvalid("missing authorization metadata")
	}
	claims, err := in.Tokens.VerifyAccess(raw)
	if err != nil {
		if err == token.ErrExpired {
			return ctx, nplnerr.TokenExpired("access token expired")
		}
		return ctx, nplnerr.TokenInvalid("access token rejected: " + err.Error())
	}
	caller := callerFromClaims(raw, claims)

	// The `uid` metadata field must agree with the token. A mismatch is either a
	// confused client or an attempt to act as somebody else; either way the
	// answer is the same, and it is NOT "trust the uid".
	if uid := metaFirst(md, MDUID); uid != "" && uid != caller.UserID {
		return ctx, nplnerr.UserMismatch(fmt.Sprintf("uid metadata (%s) does not match the access token (%s)", uid, caller.UserID))
	}

	if in.Observer != nil {
		in.Observer.NoteRPC(caller.PID, caller.UserID, shortMethod(method), peerAddr(ctx))
	}
	return WithCaller(ctx, caller), nil
}

// who renders the caller for a log line.
func (in *Interceptors) who(ctx context.Context) string {
	if c, ok := CallerFrom(ctx); ok {
		return fmt.Sprintf("[pid=%d user=%s]", c.PID, c.UserID)
	}
	return "[anonymous]"
}

// wrappedStream swaps the context of a server stream so the handler sees the
// authenticated caller, and optionally logs every message.
type wrappedStream struct {
	grpc.ServerStream
	ctx       context.Context
	logBodies bool
	method    string
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

func (w *wrappedStream) RecvMsg(m any) error {
	err := w.ServerStream.RecvMsg(m)
	if err == nil && w.logBodies {
		log.Printf("[rpc] --> %s (stream) %s", w.method, jsonOf(m))
	}
	return err
}

func (w *wrappedStream) SendMsg(m any) error {
	if w.logBodies {
		log.Printf("[rpc] <-- %s (stream) %s", w.method, jsonOf(m))
	}
	return w.ServerStream.SendMsg(m)
}

// jsonOf renders a protobuf message as compact JSON for the bring-up log.
func jsonOf(m any) string {
	msg, ok := m.(proto.Message)
	if !ok {
		return fmt.Sprintf("%v", m)
	}
	b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(msg)
	if err != nil {
		return "<unprintable>"
	}
	const max = 4000
	if len(b) > max {
		return string(b[:max]) + "…(truncated)"
	}
	return string(b)
}

// shortMethod turns "/nn.npln.friends.v1.Friends/ListFriendUsers" into
// "Friends::ListFriendUsers" for the dashboard's "last action" column.
func shortMethod(full string) string {
	full = strings.TrimPrefix(full, "/")
	svc, meth, ok := strings.Cut(full, "/")
	if !ok {
		return full
	}
	if i := strings.LastIndex(svc, "."); i >= 0 {
		svc = svc[i+1:]
	}
	return svc + "::" + meth
}

// peerAddr returns the client address, for logs and the dashboard.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
