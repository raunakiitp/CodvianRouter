// Package server tests NEW-01: cross-tenant Responses access control.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

func TestResponsesOwnership_StoreLevel(t *testing.T) {
	// Test the store-level accessor rejecting a foreign owner directly
	store := responses.NewStore(2*time.Hour, 200)

	keyA := "key-a"
	keyB := "key-b"

	// Store a response under keyA
	rec := responses.Record{
		ID:        "resp_1",
		Model:     "gpt-4",
		Status:    responses.StatusCompleted,
		CreatedAt: time.Now().Unix(),
		Owner:     keyA,
	}
	store.Put(rec)

	// KeyA can read
	got, ok := store.Get("resp_1", keyA)
	if !ok || got.ID != "resp_1" {
		t.Fatalf("keyA should read own response: ok=%v, id=%s", ok, got.ID)
	}

	// KeyB gets 404 (not found) - store returns false
	_, ok = store.Get("resp_1", keyB)
	if ok {
		t.Fatalf("keyB should NOT read keyA's response")
	}

	// KeyB gets 404 on delete (record still exists)
	deleted := store.Delete("resp_1", keyB)
	if deleted {
		t.Fatalf("keyB should NOT delete keyA's response")
	}
	// Verify record still exists for keyA
	_, ok = store.Get("resp_1", keyA)
	if !ok {
		t.Fatalf("keyA's record should still exist after keyB's failed delete")
	}

	// KeyA can delete
	deleted = store.Delete("resp_1", keyA)
	if !deleted {
		t.Fatalf("keyA should delete own response")
	}
	_, ok = store.Get("resp_1", keyA)
	if ok {
		t.Fatalf("keyA's record should be gone after delete")
	}
}

func TestResponsesOwnership_HTTP(t *testing.T) {
	// Two independent gateway keys
	keyA := "key-a-123"
	keyB := "key-b-456"

	srv := newTestServer(t, &fakeEngineResponses{}, WithStaticKeys(keyA, keyB))
	defer srv.Close()

	// Key A creates a response
	createBody := `{"model":"gpt-4","input":"Hello"}`
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", createBody, keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyA create failed: %d %s", rec.Code, rec.Body.String())
	}

	var createResp struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &createResp)
	respID := createResp.ID
	if respID == "" {
		t.Fatalf("no response id returned")
	}

	// Key B tries GET -> 404
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+respID, "", keyB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("keyB GET should be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Key B tries DELETE -> 404 (record still exists)
	rec = doReq(t, srv, http.MethodDelete, "/v1/responses/"+respID, "", keyB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("keyB DELETE should be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify record still exists for key A
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+respID, "", keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyA GET should still work after keyB's failed DELETE: %d %s", rec.Code, rec.Body.String())
	}

	// Key B tries LIST -> empty list
	rec = doReq(t, srv, http.MethodGet, "/v1/responses", "", keyB)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyB LIST should be 200 with empty list, got %d: %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Data []json.RawMessage `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Data) != 0 {
		t.Fatalf("keyB LIST should be empty, got %d items", len(listResp.Data))
	}

	// Key B tries input_items -> 404
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+respID+"/input_items", "", keyB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("keyB input_items should be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Key A can still read and delete
	rec = doReq(t, srv, http.MethodDelete, "/v1/responses/"+respID, "", keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyA DELETE should succeed: %d %s", rec.Code, rec.Body.String())
	}

	// Key A GET after delete -> 404
	rec = doReq(t, srv, http.MethodGet, "/v1/responses/"+respID, "", keyA)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("keyA GET after delete should be 404, got %d", rec.Code)
	}
}

func TestResponsesOwnership_Continuation(t *testing.T) {
	// Key A creates a response, then tries to continue it with key B -> 404
	keyA := "key-a-123"
	keyB := "key-b-456"

	srv := newTestServer(t, &fakeEngineResponses{}, WithStaticKeys(keyA, keyB))
	defer srv.Close()

	// Key A creates first response
	createBody := `{"model":"gpt-4","input":"Hello"}`
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", createBody, keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyA create failed: %d %s", rec.Code, rec.Body.String())
	}
	var createResp struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &createResp)
	respID := createResp.ID

	// Key B tries to continue key A's response -> 404
	continueBody := `{"model":"gpt-4","input":"World","previous_response_id":"` + respID + `"}`
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", continueBody, keyB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("keyB continuation should be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Key A can continue
	rec = doReq(t, srv, http.MethodPost, "/v1/responses", continueBody, keyA)
	if rec.Code != http.StatusOK {
		t.Fatalf("keyA continuation should succeed: %d %s", rec.Code, rec.Body.String())
	}
}

// fakeEngineResponses is a minimal engine that returns canned responses.
type fakeEngineResponses struct{}

func (e *fakeEngineResponses) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{
		Model:        req.Model,
		Content:      "Hello from fake engine",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func (e *fakeEngineResponses) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
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

func (e *fakeEngineResponses) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return nil, nil
}

func (e *fakeEngineResponses) ListModels(ctx context.Context) ([]provider.Model, error) {
	return nil, nil
}

func (e *fakeEngineResponses) HealthCheck(ctx context.Context) error {
	return nil
}

func (e *fakeEngineResponses) SupportsModel(model string) bool {
	return true
}
