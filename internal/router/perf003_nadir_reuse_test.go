// AUD-PERF-003 regression: the default Nadir decision transport must
// send its POST over a shared warm transport, not a bare
// http.Client{Timeout: …} that falls back to http.DefaultTransport
// (MaxIdleConnsPerHost=2). Two sequential bursts of concurrent decision
// calls against one backend must not re-dial the whole burst; the
// backend's accepted-connection count is the deterministic evidence.
package router

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDefaultNadirTransportReusesWarmConnections(t *testing.T) {
	const burst = 8

	var mu sync.Mutex
	newConns := 0
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"selected_model":"m","bucket":"b","confidence":0.9}`))
	}))
	backend.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	backend.Start()
	defer backend.Close()

	endpoint := backend.URL + "/v1/bucket"
	body := []byte(`{"prompt":"which model?","menu":["m"],"source":"codvian-router"}`)

	fireBurst := func() {
		var wg sync.WaitGroup
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, _, err := defaultNadirTransport(context.Background(), endpoint, body, "k", 5*time.Second)
				if err != nil {
					t.Errorf("defaultNadirTransport: %v", err)
					return
				}
				if status != http.StatusOK {
					t.Errorf("decision status = %d, want 200", status)
				}
			}()
		}
		wg.Wait()
	}

	fireBurst()
	time.Sleep(200 * time.Millisecond) // let the burst's connections go idle
	fireBurst()

	mu.Lock()
	got := newConns
	mu.Unlock()

	// Warm transport (sized 20 ≥ burst): the second burst reuses the
	// first burst's idle connections, so ~burst total dials. Bare
	// http.DefaultTransport keeps only 2 idle and re-dials burst-2:
	// ~2*burst-2 total. The threshold sits between the two.
	want := burst + 1
	if got > want {
		t.Errorf("backend accepted %d new TCP connections across two bursts of %d Nadir decision calls, want <= %d: the decision transport is not reusing warm connections (AUD-PERF-003)", got, burst, want)
	}
}
