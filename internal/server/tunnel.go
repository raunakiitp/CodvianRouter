// Local TCP tunnel (POST /admin/tunnel/start, POST /admin/tunnel/stop,
// GET /admin/tunnel/status).
//
// A real TCP proxy, stdlib only, no external service and no spawned
// binary: it listens on a loopback port and forwards every byte to
// the gateway's own listen address (or an explicit loopback target
// in the start request). Both ends are loopback-only — the listener
// binds 127.0.0.1 and the target must be a loopback address (or
// "localhost") — so the tunnel can never be used to reach a remote
// host or to expose the gateway off-host. One tunnel runs at a time;
// starting a second answers 409.
//
// The routes stay Tier 1 LOCAL_ONLY (see guard.go): the loopback
// check runs before auth, and a valid gateway key is still required
// after it. The tunnel never logs or returns keys, and it proxies
// bytes only — it does not inspect, store, or modify traffic.
package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// tunnelManager owns the single live tunnel (if any) and its counters.
type tunnelManager struct {
	mu        sync.Mutex
	ln        net.Listener
	running   bool
	listen    string // e.g. "127.0.0.1:54321"
	target    string // e.g. "127.0.0.1:20128"
	startedAt time.Time
	stoppedAt time.Time
	bytesUp   atomic.Uint64 // client -> target
	bytesDown atomic.Uint64 // target -> client
	active    atomic.Int64  // proxied connections currently open
	conns     map[net.Conn]struct{}
	done      chan struct{}
}

func newTunnelManager() *tunnelManager {
	return &tunnelManager{conns: map[net.Conn]struct{}{}}
}

// countReader counts the bytes read through it into n.
type countReader struct {
	r io.Reader
	n *atomic.Uint64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(uint64(n))
	}
	return n, err
}

// tunnelStatus is the JSON status of the tunnel (current or last run).
type tunnelStatus struct {
	Running           bool      `json:"running"`
	Listen            string    `json:"listen,omitempty"`
	Target            string    `json:"target,omitempty"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	StoppedAt         time.Time `json:"stopped_at,omitempty"`
	BytesUp           uint64    `json:"bytes_up"`
	BytesDown         uint64    `json:"bytes_down"`
	BytesTotal        uint64    `json:"bytes_total"`
	ActiveConnections int64     `json:"active_connections"`
}

// status snapshots the manager state. Counters belong to the current
// (or most recent) run: Start resets them, Stop keeps them so the
// stop response still reports what the run forwarded.
func (m *tunnelManager) status() tunnelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	up := m.bytesUp.Load()
	down := m.bytesDown.Load()
	return tunnelStatus{
		Running:           m.running,
		Listen:            m.listen,
		Target:            m.target,
		StartedAt:         m.startedAt,
		StoppedAt:         m.stoppedAt,
		BytesUp:           up,
		BytesDown:         down,
		BytesTotal:        up + down,
		ActiveConnections: m.active.Load(),
	}
}

// Start begins forwarding listenPort (0 = an ephemeral loopback
// port) to target. target must be a loopback address; an empty
// target is an error (the handler fills in the gateway listen
// address first). Returns the actual listen address.
func (m *tunnelManager) Start(target string, listenPort int) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port == "" {
		return "", errInvalidTarget
	}
	if !isLoopbackHost(host) {
		return "", errInvalidTarget
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", errInvalidTarget
	}
	if listenPort < 0 || listenPort > 65535 {
		return "", errInvalidPort
	}
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return "", errTunnelRunning
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(listenPort))
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.ln = ln
	m.listen = ln.Addr().String()
	m.target = target
	m.startedAt = time.Now().UTC()
	m.stoppedAt = time.Time{}
	m.bytesUp.Store(0)
	m.bytesDown.Store(0)
	m.running = true
	m.done = make(chan struct{})
	m.mu.Unlock()
	// The listener is passed as a parameter, never re-read from
	// m.ln: acceptLoop used to read m.ln without the lock, so a
	// restart (Stop then Start) could write m.ln while the
	// previous loop still read it — a deterministic -race
	// failure (CHECKLIST 189). Each loop now owns the exact
	// listener its Start created; a stale loop can only Accept
	// on its own (already closed) listener and exits.
	go m.acceptLoop(ln)
	return m.listen, nil
}

var (
	errInvalidTarget = errString("target must be a loopback address (host:port)")
	errInvalidPort   = errString("listen_port must be 0 (ephemeral) or 1-65535")
	errTunnelRunning = errString("a tunnel is already running")
	errNoTunnel      = errString("no tunnel is running")
)

type errString string

func (e errString) Error() string { return string(e) }

// isLoopbackHost reports whether host is a loopback IP or "localhost".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Stop closes the listener and every proxied connection, then
// returns the final status of the run that just ended.
func (m *tunnelManager) Stop() tunnelStatus {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return tunnelStatus{}
	}
	ln := m.ln
	done := m.done
	conns := make([]net.Conn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.running = false
	m.stoppedAt = time.Now().UTC()
	m.mu.Unlock()
	close(done)
	_ = ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	return m.status()
}

// acceptLoop accepts loopback connections on ln (the listener its
// Start created, passed in — it never reads m.ln) and proxies each
// one to the target in both directions until either side closes.
func (m *tunnelManager) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by Stop
		}
		m.mu.Lock()
		if !m.running {
			m.mu.Unlock()
			_ = conn.Close()
			continue
		}
		m.conns[conn] = struct{}{}
		target := m.target
		m.mu.Unlock()
		m.active.Add(1)
		go m.proxy(conn, target)
	}
}

// proxy forwards one accepted connection to the target and back.
func (m *tunnelManager) proxy(client net.Conn, targetAddr string) {
	defer func() {
		m.mu.Lock()
		delete(m.conns, client)
		m.mu.Unlock()
		m.active.Add(-1)
		_ = client.Close()
	}()
	target, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.conns[target] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.conns, target)
		m.mu.Unlock()
		_ = target.Close()
	}()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, &countReader{r: client, n: &m.bytesUp})
		_ = target.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, &countReader{r: target, n: &m.bytesDown})
		_ = client.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// tunnelStartBody is the POST /admin/tunnel/start request body.
// target defaults to the gateway's listen address; listen_port 0
// (or omitted) picks an ephemeral loopback port.
type tunnelStartBody struct {
	Target     string `json:"target"`
	ListenPort int    `json:"listen_port"`
}

// handleTunnelStart serves POST /admin/tunnel/start (Tier 1): it
// starts the loopback TCP tunnel. The target defaults to the
// gateway's listen address (s.listenAddr); with neither a target in
// the body nor a known listen address it answers 503 — an honest
// "cannot forward anywhere" instead of a guessed destination.
func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var body tunnelStartBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	target := body.Target
	if target == "" {
		target = s.listenAddr
	}
	if target == "" {
		writeError(w, http.StatusServiceUnavailable, "tunnel unavailable: no target — pass {\"target\": \"127.0.0.1:port\"} or run the server with a known listen address")
		return
	}
	listen, err := s.tunnel.Start(target, body.ListenPort)
	if err != nil {
		switch err {
		case errTunnelRunning:
			writeError(w, http.StatusConflict, err.Error())
		case errInvalidTarget, errInvalidPort:
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.logger.Printf("tunnel: start failed: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not start tunnel")
		}
		return
	}
	s.logger.Printf("tunnel: started %s -> %s", listen, target)
	st := s.tunnel.status()
	writeJSON(w, http.StatusCreated, st)
}

// handleTunnelStop serves POST /admin/tunnel/stop (Tier 1): it stops
// the running tunnel (409 when none is running) and reports the
// run's forwarded byte counts.
func (s *Server) handleTunnelStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st := s.tunnel.status()
	if !st.Running {
		writeError(w, http.StatusConflict, errNoTunnel.Error())
		return
	}
	st = s.tunnel.Stop()
	s.logger.Printf("tunnel: stopped %s -> %s (%d bytes forwarded)", st.Listen, st.Target, st.BytesTotal)
	writeJSON(w, http.StatusOK, st)
}

// handleTunnelStatus serves GET /admin/tunnel/status (Tier 1): the
// live tunnel state — running or stopped, ports, and bytes
// forwarded. No key material is ever in the payload.
func (s *Server) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.tunnel.status())
}
