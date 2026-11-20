// TCP_NODELAY on streaming connections (CHECKLIST items 135-136).
//
// Honest starting point, verified before writing this file:
// Go's stdlib ALREADY sets TCP_NODELAY on every TCP connection
// it creates — dialed or accepted — in net.newTCPConn
// (go/src/net/tcpsock.go: setNoDelay(fd, true)). So "enabling
// TCP_NODELAY" here does not change a socket option the kernel
// would otherwise leave off. What this file does instead, and
// what the tests prove, is make the property EXPLICIT and
// ENFORCED rather than an incidental stdlib default:
//
//   - the per-provider dialer sets NoDelay on every dialed
//     connection after the dial returns (noDelayDialContext),
//     so the property survives a future stdlib change and is
//     visible in this codebase, not just in Go's;
//   - the server's listener (server.NoDelayListener) sets
//     NoDelay on every accepted connection, same reasoning;
//   - both are proven by a real GetsockoptInt probe in the
//     tests (provider/nodelay_test.go,
//     server/nodelay_listener_test.go), not by reading code.
//
// Why it matters for streaming: Nagle's algorithm holds a
// small segment until an ACK arrives or a full segment fills.
// A token event is a few hundred bytes — exactly the size
// Nagle would hold. With NoDelay every flushed event leaves
// at once; without it, a slow-ACK client would see token
// delivery delayed by up to a delayed-ACK timeout (~40 ms on
// Linux) per event. On this loopback rig the difference is
// unmeasurable (ACKs are instant), so no latency win is
// claimed here — the claim is only that the option is set on
// both paths, proven by the probe.
package provider

import (
	"context"
	"net"
)

// noDelayDialContext wraps a DialContext func so every
// connection it returns has TCP_NODELAY set. A SetNoDelay
// failure never fails the dial — the connection is still
// usable, and the stdlib default already sets the option —
// but a failure is worth knowing about, so it is counted
// alongside the TFO dial errors in TFOStatus.
func noDelayDialContext(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			if err := tc.SetNoDelay(true); err != nil {
				tfoErrors.Add(1)
			}
		}
		return c, nil
	}
}
