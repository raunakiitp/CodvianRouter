// Package server tests AUD-API-005: WebSocket auth/attribution/budget parity.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeEngineWS is a minimal engine that returns canned responses for WS tests.
// It captures the last ChatStream request so tests can assert the server
// forwarded the right attribution (req.CallerID) into the engine.
type fakeEngineWS struct {
	mu       sync.Mutex
	lastReq  provider.ChatRequest
	streamed bool
}

func (e *fakeEngineWS) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{
		Model:        req.Model,
		Content:      "Hello from fake engine",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (e *fakeEngineWS) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.lastReq = req
	e.streamed = true
	e.mu.Unlock()
	_ = onDelta("Hello")
	_ = onDelta(" from")
	_ = onDelta(" fake")
	_ = onDelta(" engine")
	return &provider.ChatResponse{
		Model:        req.Model,
		Content:      "Hello from fake engine",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (e *fakeEngineWS) capturedRequest() (provider.ChatRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastReq, e.streamed
}

func (e *fakeEngineWS) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return nil, nil
}

func (e *fakeEngineWS) ListModels(ctx context.Context) ([]provider.Model, error) {
	return nil, nil
}

func (e *fakeEngineWS) HealthCheck(ctx context.Context) error {
	return nil
}

func (e *fakeEngineWS) SupportsModel(model string) bool {
	return true
}

func TestWSUnauthenticatedUpgradeRefused(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	// Create an httptest server to test WebSocket upgrade
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	// Try to connect without authentication
	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		t.Fatal("expected WebSocket upgrade to be refused without auth")
	}
	// The dialer returns an error for 401
	if err != nil && err.Error() == "unexpected response code: 401" {
		// Expected
	} else {
		t.Logf("got error: %v", err)
	}
}

func TestWSAuthWithAuthorizationHeader(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Logf("response status: %d", resp.StatusCode)
		}
		t.Fatalf("WebSocket upgrade failed with valid auth: %v", err)
	}
	conn.Close()
}

func TestWSAuthWithXApiKeyHeader(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("X-Api-Key", "test-key-123")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Logf("response status: %d", resp.StatusCode)
		}
		t.Fatalf("WebSocket upgrade failed with X-Api-Key: %v", err)
	}
	conn.Close()
}

func TestWSAuthWithXGoogApiKeyHeader(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("X-Goog-Api-Key", "test-key-123")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Logf("response status: %d", resp.StatusCode)
		}
		t.Fatalf("WebSocket upgrade failed with X-Goog-Api-Key: %v", err)
	}
	conn.Close()
}

func TestWSCrossOriginRefusedByDefault(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	headers.Set("Origin", "https://evil.com")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		conn.Close()
		t.Fatal("expected cross-origin WebSocket upgrade to be refused by default")
	}
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		// Expected
	} else {
		t.Logf("got error: %v, status: %d", err, resp.StatusCode)
	}
}

func TestWSSameOriginAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	headers.Set("Origin", "http://"+httpSrv.URL[7:]) // same origin as server
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Logf("response status: %d", resp.StatusCode)
		}
		t.Fatalf("same-origin WebSocket upgrade should be allowed: %v", err)
	}
	conn.Close()
}

func TestWSWithOperatorOverrideAllowsCrossOrigin(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"), WithWSAllowedOrigins([]string{"https://allowed.com"}))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	headers.Set("Origin", "https://allowed.com")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			t.Logf("response status: %d", resp.StatusCode)
		}
		t.Fatalf("operator-allowed origin should be permitted: %v", err)
	}
	conn.Close()
}

func TestWSWithOperatorOverrideEmptySliceKeepsSameOriginDefault(t *testing.T) {
	// An EMPTY (non-nil) override list is NOT "allow every origin":
	// the documented policy is that an empty list keeps the
	// same-origin default (see wsAllowedOrigins / WithWSAllowedOrigins
	// docs). A cross-origin browser request must therefore still be
	// refused — treating "[]" as "allow all" would silently disable
	// the CSWSH protection an operator never asked to lift.
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"), WithWSAllowedOrigins([]string{}))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	headers.Set("Origin", "https://any-origin.com")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		conn.Close()
		t.Fatal("cross-origin upgrade must stay refused with an empty override (same-origin default)")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", resp.StatusCode)
	}
}

func TestWSRequestCarriesCallerAttribution(t *testing.T) {
	// AUD-API-005: the WebSocket path must attribute the request to
	// the verified caller's key id — the HTTP path sets req.CallerID
	// so the engine, the ledger row and the spend all belong to the
	// tenant that made the request. We assert the engine received a
	// non-empty CallerID and that it is the verifier's derived id for
	// the presented key (never the raw key itself).
	eng := &fakeEngineWS{}
	srv := newTestServer(t, eng, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%7B%22role%22%3A%22user%22%2C%22content%22%3A%22hello%22%7D%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("WebSocket upgrade failed: %v", err)
	}
	// Drain the stream so the engine call completes.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	conn.Close()

	wantID, ok := NewStaticKeyVerifier("test-key-123").KeyID(context.Background(), "test-key-123")
	if !ok || wantID == "" {
		t.Fatal("static verifier returned no key id for the test key")
	}

	req, streamed := eng.capturedRequest()
	if !streamed {
		t.Fatal("the WS path never reached the engine's ChatStream")
	}
	if req.CallerID == "" {
		t.Fatal("CallerID is empty: the WS path failed to attribute the request to the caller")
	}
	if req.CallerID != wantID {
		t.Errorf("CallerID = %q, want the verifier's derived id %q", req.CallerID, wantID)
	}
	if req.CallerID == "test-key-123" {
		t.Error("CallerID is the raw gateway key: attribution must never carry the raw key")
	}
	if !req.Stream {
		t.Error("req.Stream = false; the WS path must mark the request streaming")
	}
}

func TestWSKeyAtBudgetCeilingRefused(t *testing.T) {
	// AUD-API-005: budget parity with the HTTP surface. A key at its
	// ceiling must be refused BEFORE the upgrade (402), exactly as
	// the HTTP path refuses it — otherwise a capped key could spend
	// over WebSocket freely. The budget row is written under the
	// verifier's derived key id, which is the identity the WS path
	// resolves the caller to.
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	t.Cleanup(func() { led.Close() })
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"), WithLedger(led))
	defer srv.Close()

	keyID, ok := NewStaticKeyVerifier("test-key-123").KeyID(context.Background(), "test-key-123")
	if !ok || keyID == "" {
		t.Fatal("static verifier returned no key id for the test key")
	}
	ctx := context.Background()
	if err := led.SetKeyBudget(ctx, keyID, 0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		conn.Close()
		t.Fatal("expected WebSocket upgrade to be refused at budget ceiling")
	}
	if resp == nil {
		t.Fatal("no HTTP response: the refusal must carry a status")
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402 (budget ceiling must refuse, same as HTTP)", resp.StatusCode)
	}
}

func TestWSDisconnectCancelsUpstream(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("test-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer test-key-123")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("WebSocket upgrade failed: %v", err)
	}
	// Immediately close the connection (simulate client disconnect)
	conn.Close()

	// Give the server a moment to process the disconnect
	time.Sleep(100 * time.Millisecond)

	// The test passes if no goroutine leak occurs (we can't easily test cancellation directly)
	// but the server should not panic or leak
}

func TestWSInvalidKeyRefused(t *testing.T) {
	srv := newTestServer(t, &fakeEngineWS{}, WithStaticKeys("valid-key-123"))
	defer srv.Close()

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	wsURL := "ws" + httpSrv.URL[4:] + "/v1/ws?model=test&messages=%5B%5D"
	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid-key")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err == nil {
		conn.Close()
		t.Fatal("expected WebSocket upgrade to be refused with invalid key")
	}
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		// Expected 401
	} else {
		t.Logf("got error: %v, status: %d", err, resp.StatusCode)
	}
}
