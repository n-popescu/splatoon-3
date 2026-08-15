package server

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// TestProxyProtocolYieldsTheRealClientAddress: behind sni-router the TCP peer is
// the router, so /api/stats would show the router's IP for every player and the
// per-IP diagnostics an ICE problem needs would be worthless.
func TestProxyProtocolYieldsTheRealClientAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pl := &proxyListener{ln}

	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("PROXY TCP4 203.0.113.7 198.51.100.2 51234 443\r\nhello"))
		time.Sleep(50 * time.Millisecond)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("RemoteAddr is %T, want *net.TCPAddr", conn.RemoteAddr())
	}
	if got := addr.IP.String(); got != "203.0.113.7" {
		t.Errorf("client IP = %s, want the console's 203.0.113.7 (not the router's)", got)
	}
	if addr.Port != 51234 {
		t.Errorf("client port = %d, want 51234", addr.Port)
	}

	// The header must be consumed and nothing else: the bytes after it are the
	// start of the TLS handshake and must arrive intact.
	rest, err := bufio.NewReader(conn).ReadString('o')
	if err != nil {
		t.Fatalf("reading past the header: %v", err)
	}
	if rest != "hello" {
		t.Errorf("payload after the header = %q, want %q", rest, "hello")
	}
}

// TestWithoutProxyHeaderTheStreamIsUntouched is the property that makes the switch
// safe to enable before the router sends the header — and safe to leave enabled
// while testing against this server directly.
//
// If the first bytes were consumed on a header-less connection, the TLS
// ClientHello would be corrupted and every handshake would fail.
func TestWithoutProxyHeaderTheStreamIsUntouched(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pl := &proxyListener{ln}

	// 0x16 0x03 0x01 is the start of a real TLS ClientHello.
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2a, 0x01}
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write(hello)
		time.Sleep(50 * time.Millisecond)
	}()

	conn, err := pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	got := make([]byte, len(hello))
	if _, err := bufio.NewReader(conn).Read(got); err != nil {
		t.Fatal(err)
	}
	for i := range hello {
		if got[i] != hello[i] {
			t.Fatalf("the ClientHello was altered: got % x, want % x", got, hello)
		}
	}
	if conn.RemoteAddr().String() == "" {
		t.Error("RemoteAddr should fall back to the real peer")
	}
}

// TestMaxMessageBytesHonoursTheFleetKnob: NEXTENDO_MAX_MESSAGE_BYTES bounds one
// message on every Nextendo server, so one setting applies fleet-wide.
func TestMaxMessageBytesHonoursTheFleetKnob(t *testing.T) {
	if got := maxMessageBytes(); got != 8<<20 {
		t.Errorf("default = %d, want %d", got, 8<<20)
	}
	t.Setenv("NEXTENDO_MAX_MESSAGE_BYTES", "1048576")
	if got := maxMessageBytes(); got != 1<<20 {
		t.Errorf("configured = %d, want %d", got, 1<<20)
	}
	t.Setenv("NEXTENDO_MAX_MESSAGE_BYTES", "not-a-number")
	if got := maxMessageBytes(); got != 8<<20 {
		t.Errorf("garbage should fall back to the default, got %d", got)
	}
}

// TestProxyProtocolIsOffByDefault: the switch must be opt-in, matching the NEX
// servers, so an existing deployment behaves exactly as before an upgrade.
func TestProxyProtocolIsOffByDefault(t *testing.T) {
	t.Setenv("NEXTENDO_PROXY_PROTOCOL", "")
	if ProxyProtocolEnabled() {
		t.Error("PROXY protocol is enabled with the variable unset")
	}
	t.Setenv("NEXTENDO_PROXY_PROTOCOL", "1")
	if !ProxyProtocolEnabled() {
		t.Error("NEXTENDO_PROXY_PROTOCOL=1 did not enable it")
	}
}
