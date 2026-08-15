package server

// PROXY protocol v1 — the same convention, and the same environment variable, as
// the NEX game servers.
//
// # Why this file has to exist
//
// sni-router shares :443 between every title, routing by SNI. It forwards the
// encrypted stream unopened, so the TCP peer each backend sees is the ROUTER, not
// the console. nextendo-nex solved this with proxyproto.go (Server.ListenSecureProxy,
// enabled per game with NEXTENDO_PROXY_PROTOCOL=1), and the router was taught to
// send the header with SNI_SEND_PROXY_PROTOCOL=1 (audit finding F7).
//
// This server has to honour the SAME switch, for two reasons — the second is the
// one that bites:
//
//  1. without it, every player's IP in /api/stats is the router's, and the
//     per-IP diagnostics that make an ICE problem debuggable are useless;
//  2. with SNI_SEND_PROXY_PROTOCOL=1 turned on for the fleet and this server not
//     parsing it, the PROXY line is fed straight into the TLS handshake as if it
//     were a ClientHello. TLS fails, and the game shows a network error with
//     nothing to explain it. An operator enabling a fleet-wide setting must not
//     be able to break one title by doing so.
//
// The parsing matches nextendo-nex byte for byte, including the deliberate
// pass-through: a connection that arrives WITHOUT the header is served normally,
// so the switch is safe to turn on before the router starts sending it, and safe
// to leave on while testing against the server directly.

import (
	"bufio"
	"net"
	"os"
	"strconv"
	"strings"
)

// ProxyProtocolEnabled reports whether NEXTENDO_PROXY_PROTOCOL=1, the same
// variable the NEX servers read.
func ProxyProtocolEnabled() bool {
	return strings.TrimSpace(os.Getenv("NEXTENDO_PROXY_PROTOCOL")) == "1"
}

// proxyConn overrides RemoteAddr with the real client address from the header.
type proxyConn struct {
	net.Conn
	reader     *bufio.Reader
	remoteAddr net.Addr
}

func (c *proxyConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

func (c *proxyConn) RemoteAddr() net.Addr {
	if c.remoteAddr != nil {
		return c.remoteAddr
	}
	return c.Conn.RemoteAddr()
}

// proxyListener parses the PROXY-protocol v1 header — one CRLF-terminated line,
// "PROXY TCP4 <srcIP> <dstIP> <srcPort> <dstPort>" — at the start of each accepted
// connection, before TLS. Connections without it pass through unchanged.
type proxyListener struct{ net.Listener }

func (l *proxyListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc := &proxyConn{Conn: c, reader: bufio.NewReader(c)}
	// Peek, never consume: a connection with no header must be handed to TLS with
	// its first bytes intact.
	if head, _ := pc.reader.Peek(6); string(head) == "PROXY " {
		line, lerr := pc.reader.ReadString('\n')
		if lerr == nil {
			f := strings.Fields(strings.TrimSpace(line))
			// f = [PROXY, TCP4|TCP6|UNKNOWN, srcIP, dstIP, srcPort, dstPort]
			if len(f) >= 6 && (f[1] == "TCP4" || f[1] == "TCP6") {
				port, _ := strconv.Atoi(f[4])
				if ip := net.ParseIP(f[2]); ip != nil {
					pc.remoteAddr = &net.TCPAddr{IP: ip, Port: port}
				}
			}
		}
	}
	return pc, nil
}
