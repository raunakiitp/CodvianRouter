// TCP Fast Open (TFO) on the per-provider dialer.
//
// TFO lets the first application data ride in the opening SYN on a
// repeat connection to a host the kernel already has a TFO cookie
// for, saving up to one RTT on a cold/failover dial. For this router
// that early data is the TLS ClientHello — TLS itself handles it, and
// the router NEVER carries a raw POST body in early data (a replayed
// SYN could deliver a non-idempotent request twice; RFC 7413
// Applicability). Warm HTTP/2 connection pools are the primary
// defence against dial latency; TFO only pays on cold dials, which
// is exactly when a failover opens a fresh connection.
//
// Implementation: the per-provider http.Transport's Dialer sets the
// client-side TFO socket option in its Control hook
// (TCP_FASTOPEN_CONNECT on Linux, TCP_FASTOPEN on macOS/Windows —
// see the per-OS files). The hook NEVER returns an error: a platform
// or middlebox that rejects the option is recorded (DialErrors) and
// the dial proceeds as a normal handshake — graceful fallback, never
// a failed request because TFO was refused.
//
// Honesty: TFOState reports what was actually probed on this
// platform — a setsockopt probe on a scratch socket plus, on Linux,
// the net.ipv4.tcp_fastopen sysctl bitmap (bit 0x1 = client TFO).
// "Active" means the probe succeeded AND the kernel has client TFO
// enabled (or the kernel state is unknown on non-Linux platforms,
// where the probe alone decides). A first-ever connection to a host
// still does a normal handshake to obtain a cookie; only repeat
// dials can use it, so no latency claim is made here — the
// measurement lives in the test (cold dials with/without TFO).
package provider

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/egress"
)

// TFOState is the measured TFO state for this platform. All fields
// are real probe results, never assumed: Supported is whether the
// client-side setsockopt succeeded on a scratch socket, Active is
// whether the dialer will actually attempt TFO (probe + kernel
// client flag), KernelValue is the raw sysctl value on Linux ("" +
// KernelKnown=false elsewhere), DialAttempts/DialErrors count
// Control-hook runs and their setsockopt failures across all
// provider clients in this process.
type TFOState struct {
	Supported    bool   `json:"supported"`
	Active       bool   `json:"active"`
	Platform     string `json:"platform"`
	KernelValue  string `json:"kernel_value,omitempty"`
	KernelKnown  bool   `json:"kernel_known"`
	Reason       string `json:"reason"`
	DialAttempts uint64 `json:"dial_attempts"`
	DialErrors   uint64 `json:"dial_errors"`
}

var (
	tfoProbeOnce sync.Once
	tfoProbe     TFOState
	tfoAttempts  atomic.Uint64
	tfoErrors    atomic.Uint64
)

// TFOStatus returns the probed TFO state for this platform. The probe
// runs once per process: a setsockopt on a scratch TCP socket, plus
// the Linux sysctl read. DialAttempts/DialErrors are live counters
// and are refreshed on every call.
func TFOStatus() TFOState {
	tfoProbeOnce.Do(func() { tfoProbe = probeTFO() })
	st := tfoProbe
	st.DialAttempts = tfoAttempts.Load()
	st.DialErrors = tfoErrors.Load()
	return st
}

// tfoControl is the net.Dialer Control hook: it enables client-side
// TFO on the socket about to be dialed. It records the attempt and
// any setsockopt failure, and ALWAYS returns nil — a refused option
// must fall back to a normal handshake, never fail the dial.
//
// The option itself is set by tfoSetSocketOption in the per-OS files
// (tfo_linux.go, tfo_darwin.go, tfo_windows.go, tfo_other.go): the
// socket-option call is NOT portable — on Windows SetsockoptInt
// takes a syscall.Handle (the RawConn fd is a SOCKET handle), while
// on Unix it takes an int fd — so the call lives behind the
// platform split instead of in this shared file.
func tfoControl(network, address string, c syscall.RawConn) error {
	tfoAttempts.Add(1)
	_ = c.Control(func(fd uintptr) {
		if err := tfoSetSocketOption(fd); err != nil {
			tfoErrors.Add(1)
		}
	})
	return nil
}

// tfoDialer returns a net.Dialer that attempts TFO when enabled.
// When disabled it is a plain dialer with the same timeouts, so the
// with/without measurement compares dialers, not timeouts.
func tfoDialer(enabled bool) *net.Dialer {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if enabled {
		d.Control = tfoControl
	}
	return d
}

// tfoTransport returns an http.Transport identical to
// http.DefaultTransport except its DialContext uses tfoDialer, so
// provider clients keep the default proxy/TLS/idle settings and
// only the dial changes. The dial is wrapped with
// noDelayDialContext: TCP_NODELAY is set explicitly on every
// dialed connection (the stdlib already sets it by default —
// see nodelay.go — this makes it an enforced property of this
// dialer, proven by the GetsockoptInt test). A nil Transport
// (the previous behaviour) used DefaultTransport implicitly;
// this makes the dialer explicit without changing any other
// setting.
func tfoTransport(enabled bool, bypassEgressGuard bool) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = noDelayDialContext(tfoDialer(enabled).DialContext)
	// 7A: enforce the egress SSRF policy at the CONNECTION point.
	// Validation-time checks (see ValidateBaseURL) can only judge a
	// literal IP; a hostname can still resolve to loopback, RFC1918
	// space or the cloud metadata endpoint, and a redirect can point
	// somewhere else entirely. The egress guard resolves the name
	// exactly once, rejects the whole answer if ANY address is
	// disallowed, and dials only a validated IP literal - so a
	// rebinding server cannot swap in 127.0.0.1 after validation,
	// and every redirect hop goes through the same check because the
	// transport's dial path is the only way out.
	if !bypassEgressGuard {
		tr.DialContext = egress.DialContext(tr.DialContext)
	}
	return tr
}

// WithTCP FastOpen enables or disables TCP Fast Open on the client's
// dialer. The default (no option) is enabled: the Control hook
// falls back gracefully on platforms that reject it, so enabling by
// default cannot break a dial. Disable it only to measure the
// with/without difference.
func WithTCPFastOpen(enabled bool) ClientOption {
	return func(c *openAIClient) {
		c.tfoEnabled = enabled
		c.tfoSet = true
	}
}
