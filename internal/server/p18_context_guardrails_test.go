// P1.8 — context/output guardrails: the embeddings/rerank refusal must
// be a consistent 400 (a property of the request + the operator's
// configuration), never a 502 — and it happens before any provider is
// called, so nothing is billed.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
)

func TestP18EmbeddingContextTooLargeIsBadRequest(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := httptest.NewRecorder()
	srv.handleEmbedEngineError(rec, &router.EmbeddingContextTooLarge{
		Model:         "emb-model",
		PromptTokens:  900,
		Endpoint:      "embeddings",
		LargestWindow: 512,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no provider was called") {
		t.Fatalf("body = %s, want the honest no-provider-call wording", body)
	}
	if !strings.Contains(body, "512") {
		t.Fatalf("body = %s, want the largest known window named", body)
	}
}

func TestP18EmbeddingContextTooLargeWithoutKnownWindowStillBadRequest(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := httptest.NewRecorder()
	srv.handleEmbedEngineError(rec, &router.EmbeddingContextTooLarge{
		Model:        "emb-model",
		PromptTokens: 900,
		Endpoint:     "embeddings",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no configured model reports a context window") {
		t.Fatalf("body = %s, want the unknown-window wording", rec.Body.String())
	}
}
