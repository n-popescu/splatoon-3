package server

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/NextendoNetwork/splatoon-3/npln/nplnerr"
)

// Options configures the gRPC server.
type Options struct {
	ListenAddr string
	CertFile   string
	KeyFile    string
	DisableTLS bool

	Interceptors *Interceptors

	// LogUnknown logs the method and payload of any RPC that has no handler.
	// This is how you find out what a title actually calls: the console tells
	// you, one Unimplemented at a time.
	LogUnknown bool
}

// New builds the gRPC server with the NPLN interceptors installed.
func New(o Options) (*grpc.Server, error) {
	if o.LogUnknown {
		// Needed by unknownHandler to hexdump the payload of a method we do not
		// implement. Only installed when that diagnostic is enabled.
		installRawCodec()
	}
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(o.Interceptors.Unary()),
		grpc.ChainStreamInterceptor(o.Interceptors.Stream()),
		// NPLN leans on long-lived server streams (presence, friends,
		// matchmaking progress, messages) that are idle for minutes at a time.
		// Without these settings the default gRPC keepalive policy would close
		// them as idle or complain about the client's pings, and the game would
		// silently stop receiving friend/presence updates.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// The console's gRPC library is grpc-c++ 1.31; message sizes stay small
		// except for cloud saves and UGC payloads, hence a generous but bounded
		// receive limit.
		grpc.MaxRecvMsgSize(8 << 20),
	}
	if o.LogUnknown {
		opts = append(opts, grpc.UnknownServiceHandler(unknownHandler))
	}
	if !o.DisableTLS {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("server: load TLS keypair: %w", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// The console negotiates h2 over ALPN; being explicit avoids a
			// silent fallback that shows up as an unexplained connection reset.
			NextProtos: []string{"h2"},
		})))
	}
	return grpc.NewServer(opts...), nil
}

// Listen starts serving. It blocks until the server stops.
func Listen(srv *grpc.Server, addr string, tlsEnabled bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}
	log.Printf("[npln] gRPC listening on %s (tls=%v)", addr, tlsEnabled)
	return srv.Serve(ln)
}

// unknownHandler answers — and above all LOGS — any method this server does not
// implement.
//
// Splatoon 3 calls services whose semantics are not publicly documented. Rather
// than guessing, we answer Unimplemented and print the full method path plus a
// hexdump of the request, so a single play session produces the exact list of
// what is still missing, with real payloads to implement it against.
func unknownHandler(_ any, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		method = "<unknown>"
	}
	var raw RawMessage
	if err := stream.RecvMsg(&raw); err != nil {
		log.Printf("[npln] UNHANDLED %s from %s (no readable payload: %v)", method, peerAddr(stream.Context()), err)
		return nplnerr.Unimplemented("method not implemented by this server: " + method)
	}
	log.Printf("[npln] UNHANDLED %s from %s payload=%d bytes\n%s",
		method, peerAddr(stream.Context()), len(raw.Data), hex.Dump(clip(raw.Data, 512)))
	return nplnerr.Unimplemented("method not implemented by this server: " + method)
}

// clip bounds a hexdump so one fat payload cannot flood the log.
func clip(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}
