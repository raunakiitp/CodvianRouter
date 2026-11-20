// NoDelayListener (CHECKLIST items 135-136).
//
// Go's stdlib already sets TCP_NODELAY on every accepted TCP
// connection (net.newTCPConn — see provider/nodelay.go for the
// full note), but this wrapper makes the property explicit on
// the server side too: every connection the gateway accepts for
// streaming gets SetNoDelay(true) in Accept, so the option is
// enforced by this codebase rather than inherited silently.
// A SetNoDelay failure never fails the accept — the connection
// is still usable. Proven by the GetsockoptInt probe in
// nodelay_listener_test.go.
package server

import "net"

// noDelayListener wraps a net.Listener and sets TCP_NODELAY
// on every accepted TCP connection.
type noDelayListener struct {
	net.Listener
}

// NoDelayListener wraps l so every accepted TCP connection has
// TCP_NODELAY set. Non-TCP connections (e.g. unix sockets in
// tests) pass through unchanged.
func NoDelayListener(l net.Listener) net.Listener {
	return &noDelayListener{Listener: l}
}

// Accept accepts a connection and sets TCP_NODELAY on it when
// it is a *net.TCPConn.
func (l *noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return c, nil
}
