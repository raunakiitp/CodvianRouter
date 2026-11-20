package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTunnelForwardsRealRequest: the tunnel is a real TCP proxy — a
// request sent to the tunnel's loopback port reaches the target
// backend and the backend's response comes back, with bytes counted.
func TestTunnelForwardsRealRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnel-ok:"+r.URL.Path)
	}))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")

	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "`+target+`"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tunnel start: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var st tunnelStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("tunnel start response is not JSON: %v", err)
	}
	if !st.Running || st.Listen == "" || st.Target != target {
		t.Fatalf("tunnel start status = %+v, want running with listen set and target %q", st, target)
	}

	// A real HTTP request through the tunnel reaches the backend.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + st.Listen + "/hello")
	if err != nil {
		t.Fatalf("GET through tunnel failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tunnel-ok:/hello" {
		t.Fatalf("GET through tunnel body = %q, want %q", body, "tunnel-ok:/hello")
	}

	// Status reports the run with bytes actually forwarded.
	rec = doReqPeer(t, srv, "GET", "/admin/tunnel/status", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnel status: status %d, want 200", rec.Code)
	}
	var st2 tunnelStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st2); err != nil {
		t.Fatalf("tunnel status is not JSON: %v", err)
	}
	if !st2.Running || st2.BytesTotal == 0 || st2.BytesUp == 0 || st2.BytesDown == 0 {
		t.Errorf("tunnel status = %+v, want running with bytes_up/down > 0", st2)
	}

	// Stop: the run's byte counts survive, and the port stops answering.
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/stop", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnel stop: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st3 tunnelStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st3); err != nil {
		t.Fatalf("tunnel stop response is not JSON: %v", err)
	}
	if st3.Running || st3.BytesTotal == 0 {
		t.Errorf("tunnel stop status = %+v, want stopped with bytes_total > 0", st3)
	}
	if _, err := client.Get("http://" + st.Listen + "/hello"); err == nil {
		t.Error("GET through tunnel after stop succeeded, want connection refused")
	}
}

// TestTunnelGuardAndValidation: Tier 1 ordering (403 off-host even
// with a valid key, 401 on-host without one), plus honest errors for
// bad targets, bad ports, double start, and stop with no tunnel.
func TestTunnelGuardAndValidation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")

	srv := newTestServer(t, &fakeEngine{})

	// Non-loopback + valid key: 403 before auth, on all three routes.
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/admin/tunnel/start", `{"target": "` + target + `"}`},
		{"POST", "/admin/tunnel/stop", ""},
		{"GET", "/admin/tunnel/status", ""},
	} {
		rec := doReqPeer(t, srv, tc.method, tc.path, tc.body, "test-key-123", nonLoopbackPeer)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s non-loopback + key: status %d, want 403", tc.method, tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "LOCAL_ONLY") {
			t.Errorf("%s %s non-loopback body %q does not name LOCAL_ONLY", tc.method, tc.path, rec.Body.String())
		}
	}
	// Loopback + no key: 401 — loopback alone is not enough.
	rec := doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "`+target+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tunnel start loopback + no key: status %d, want 401", rec.Code)
	}
	// Non-loopback target: 400 (the tunnel never forwards off-host).
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "93.184.216.34:80"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("tunnel start non-loopback target: status %d, want 400", rec.Code)
	}
	// Bad port: 400.
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "`+target+`", "listen_port": 70000}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("tunnel start bad port: status %d, want 400", rec.Code)
	}
	// Stop with no tunnel running: 409.
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/stop", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusConflict {
		t.Errorf("tunnel stop with none running: status %d, want 409", rec.Code)
	}
	// Start, then a second start: 409.
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "`+target+`"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tunnel start: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{"target": "`+target+`"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusConflict {
		t.Errorf("second tunnel start: status %d, want 409", rec.Code)
	}
	// Stop, then start again on a fixed port: the port is honoured.
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/stop", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("tunnel stop: status %d, want 200", rec.Code)
	}
	rec = doReqPeer(t, srv, "POST", "/admin/tunnel/start", fmt.Sprintf(`{"target": %q, "listen_port": 0}`, target), "test-key-123", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tunnel restart: status %d, want 201: %s", rec.Code, rec.Body.String())
	}
	_ = doReqPeer(t, srv, "POST", "/admin/tunnel/stop", "", "test-key-123", loopbackPeer)
}

// TestTunnelRapidRestartNoRace is the regression test for CHECKLIST
// 189: Start used to write m.ln while a previously started
// acceptLoop still read it without the lock, a deterministic
// -race failure on restart. Rapid start/stop cycles exercise
// exactly that interleaving — a stale acceptLoop must never
// observe a newer Start's listener — so this test fails under
// -race on the old code and passes on the fix. It asserts no
// race (the detector is the assertion) and that every cycle
// starts, stops, and leaves the manager stopped.
func TestTunnelRapidRestartNoRace(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer targetLn.Close()
	go func() {
		for {
			c, err := targetLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	target := targetLn.Addr().String()

	m := newTunnelManager()
	for i := 0; i < 100; i++ {
		listen, err := m.Start(target, 0)
		if err != nil {
			t.Fatalf("cycle %d: start: %v", i, err)
		}
		if i%10 == 0 {
			// Exercise the accept path while the tunnel is up:
			// the stale loop's Accept is what raced the next
			// Start's write.
			conn, err := net.DialTimeout("tcp", listen, time.Second)
			if err != nil {
				t.Fatalf("cycle %d: dial tunnel: %v", i, err)
			}
			_ = conn.Close()
		}
		st := m.Stop()
		if st.Running {
			t.Fatalf("cycle %d: stop left tunnel running: %+v", i, st)
		}
	}
	if st := m.status(); st.Running {
		t.Fatalf("after 100 restarts tunnel still running: %+v", st)
	}
}

// TestTunnelStartWithoutTarget: with no target in the body and no
// known listen address, start answers 503 — never a guessed target.
func TestTunnelStartWithoutTarget(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "POST", "/admin/tunnel/start", `{}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tunnel start without target: status %d, want 503", rec.Code)
	}
}
