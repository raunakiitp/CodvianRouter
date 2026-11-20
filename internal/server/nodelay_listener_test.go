//go:build linux || darwin

package server

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// tcpNoDelaySet reads the live TCP_NODELAY socket option off a
// connection via GetsockoptInt — the same probe as
// provider/nodelay_test.go, duplicated here because the two
// packages cannot share test helpers.
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

// TestNoDelayListenerSetsTCPNoDelay accepts through the real
// NoDelayListener (the production serve path) and proves
// TCP_NODELAY is set on the accepted socket. This is the
// inbound half of item 135: the option is enforced by the
// listener, not left to the stdlib default alone.
func TestNoDelayListenerSetsTCPNoDelay(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer inner.Close()
	ln := NoDelayListener(inner)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	d := &net.Dialer{Timeout: 5 * time.Second}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	select {
	case srv := <-accepted:
		defer srv.Close()
		if !tcpNoDelaySet(t, srv) {
			t.Errorf("TCP_NODELAY not set on the connection accepted through NoDelayListener")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Accept")
	}
}
