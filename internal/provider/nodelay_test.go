//go:build linux || darwin

package provider

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// tcpNoDelaySet reads the live TCP_NODELAY socket option off a
// connection via GetsockoptInt. It proves the option is really
// set on the socket, not merely requested in code.
func tcpNoDelaySet(t *testing.T, c net.Conn) bool {
	t.Helper()
	tc, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("connection is %T, want *net.TCPConn", c)
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var nodelay int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		nodelay, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
	}); err != nil {
		t.Fatalf("RawConn.Control: %v", err)
	}
	if gerr != nil {
		t.Fatalf("GetsockoptInt(TCP_NODELAY): %v", gerr)
	}
	return nodelay == 1
}

// TestProviderDialerSetsTCPNoDelay dials through the real
// provider transport (TFO on, the production path) and proves
// TCP_NODELAY is set on the dialed socket. This is the
// outbound half of item 135: the option is enforced by the
// dialer, not left to the stdlib default alone.
func TestProviderDialerSetsTCPNoDelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	// Accept one connection so the dial completes, then hold
	// it until the test reads the socket option.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	// bypassEgressGuard=true: this probe deliberately dials a loopback
	// listener to read the socket option, so the SSRF egress guard
	// (which correctly refuses loopback at connection time) is not
	// what is under test here.
	tr := tfoTransport(true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer c.Close()
	srv := <-accepted
	defer srv.Close()

	if !tcpNoDelaySet(t, c) {
		t.Errorf("TCP_NODELAY not set on the provider-dialed connection")
	}
	// The accepted side is also a Go TCP connection (stdlib
	// default); assert it too so the probe covers both ends
	// of the same socket pair.
	if !tcpNoDelaySet(t, srv) {
		t.Errorf("TCP_NODELAY not set on the accepted side of the dial")
	}
}

// TestProviderDialerSetsTCPNoDelayWithoutTFO is the same probe
// with TFO disabled: NoDelay is a property of the dialer
// wrapper, not of the TFO hook, so it must hold either way.
func TestProviderDialerSetsTCPNoDelayWithoutTFO(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			// Hold the connection briefly, then close.
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()

	// bypassEgressGuard=true: same reason as above — the probe's target
	// is a loopback listener.
	tr := tfoTransport(false, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer c.Close()

	if !tcpNoDelaySet(t, c) {
		t.Errorf("TCP_NODELAY not set on the provider-dialed connection (TFO off)")
	}
}
