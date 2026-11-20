package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// M8.4 — Responses sub-actions: GET /v1/responses/{id}/input_items.
//
// NOTE ON WIRING: the handler lives in responses_items.go, but its
// guard.go entry and server.go register line are the coordinator's
// step (this slice may not edit those files). These tests therefore
// wire the handler on a throwaway mux with the exact pattern the
// coordinator will register ("GET /v1/responses/{id}/input_items"),
// and seed the store directly with fixed ids the fixtures name.
// The apitest contract tests run the REAL handler through
// apitest.Run on that mux.

// inputItemsMux wires handleListResponseInputItems exactly as the
// coordinator's server.go registration will.
func inputItemsMux(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/responses/{id}/input_items", srv.handleListResponseInputItems)
	return mux
}

// seedInputItemsRecord stores one terminal record whose transcript is
// user "What is 2+2?" -> assistant tool call get_sum(call_1) -> tool
// result "4" -> assistant "The answer is 4." (the final assistant
// turn: output, not input, so input_items must drop it).
func seedInputItemsRecord(t *testing.T, srv *Server, id string) {
	t.Helper()
	transcript, err := json.Marshal([]provider.Message{
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{
			ID:   "call_1",
			Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "get_sum",
				Arguments: `{"a":2,"b":2}`,
			},
		}}},
		{Role: "tool", Content: "4", ToolCallID: "call_1"},
		{Role: "assistant", Content: "The answer is 4."},
	})
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	srv.responses.Put(responses.Record{
		ID:             id,
		Model:          "mock-model",
		Status:         responses.StatusCompleted,
		CreatedAt:      time.Now().Unix(),
		TranscriptJSON: transcript,
		ResponseJSON:   json.RawMessage(`{"id":"` + id + `","object":"response","status":"completed","model":"mock-model","output":[]}`),
	})
}

// TestM84InputItemsFixture200 is the apitest contract test for
// GET /v1/responses/{id}/input_items (200): items from the stored
// transcript, final assistant turn dropped, tool call + tool result
// as function_call / function_call_output items.
func TestM84InputItemsFixture200(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedInputItemsRecord(t, srv, "resp_m84inputitems01")
	f := apitest.LoadFixture(t, "m8.4", "responses-input-items-200")
	apitest.Run(t, inputItemsMux(srv), f)
}

// TestM84InputItemsFixture404 is the apitest contract test for the
// honest 404 on an unknown response id.
func TestM84InputItemsFixture404(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.4", "responses-input-items-404")
	apitest.Run(t, inputItemsMux(srv), f)
}

// TestM84InputItemsFixture409 is the apitest contract test for the
// in-progress rule: a stream whose placeholder record has no
// transcript yet answers 409, not an empty list.
func TestM84InputItemsFixture409(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.responses.Put(responses.Record{
		ID:        "resp_m84inprogress01",
		Model:     "mock-model",
		Status:    responses.StatusInProgress,
		CreatedAt: time.Now().Unix(),
	})
	f := apitest.LoadFixture(t, "m8.4", "responses-input-items-409")
	apitest.Run(t, inputItemsMux(srv), f)
}

// TestM84InputItemsToggleFails is the toggle proof for the 200
// fixture: the same recorded 200 body must FAIL when the fixture
// expects a different status — so the contract test fails if the
// handler is removed (mux 404) or regresses its status.
func TestM84InputItemsToggleFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.4", "responses-input-items-200")
	f.Assert.Status = 201 // wrong on purpose: the response is 200
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded body: %v", err)
	}
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 201")
	} else if !strings.Contains(err.Error(), "status") {
		t.Fatalf("wrong-status error should name status, got: %v", err)
	}
}

// getInputItems calls the handler on the test mux and decodes the
// list payload (or returns the raw recorder for error cases).
func getInputItems(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	inputItemsMux(srv).ServeHTTP(rec, req)
	return rec
}

func decodeInputItems(t *testing.T, rec *httptest.ResponseRecorder) responseInputItemsList {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var list responseInputItemsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode input items: %v (body %s)", err, rec.Body.String())
	}
	return list
}

// TestM84InputItemsPagination covers limit/after/order + the 400s,
// through the real handler (not the fixture harness).
func TestM84InputItemsPagination(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedInputItemsRecord(t, srv, "resp_m84inputitems01")
	base := "/v1/responses/resp_m84inputitems01/input_items"

	// Default: all three input items, chronological, no more pages.
	list := decodeInputItems(t, getInputItems(t, srv, base))
	if len(list.Data) != 3 || list.HasMore || list.Object != "list" {
		t.Fatalf("default list = %+v, want 3 items, has_more false", list)
	}
	if list.Data[0].Type != "message" || list.Data[0].Role != "user" || list.Data[0].Content[0].Text != "What is 2+2?" {
		t.Fatalf("data[0] = %+v, want the user message", list.Data[0])
	}
	if list.Data[1].Type != "function_call" || list.Data[1].Name != "get_sum" || list.Data[1].CallID != "call_1" {
		t.Fatalf("data[1] = %+v, want the get_sum function_call", list.Data[1])
	}
	if list.Data[2].Type != "function_call_output" || list.Data[2].Output != "4" {
		t.Fatalf("data[2] = %+v, want the tool result", list.Data[2])
	}
	// The final assistant turn ("The answer is 4.") must NOT appear.
	for _, it := range list.Data {
		for _, p := range it.Content {
			if p.Text == "The answer is 4." {
				t.Fatal("final assistant turn leaked into input items")
			}
		}
	}

	// limit=2: first page of two, has_more true; after=last_id continues.
	list = decodeInputItems(t, getInputItems(t, srv, base+"?limit=2"))
	if len(list.Data) != 2 || !list.HasMore || list.LastID == nil || *list.LastID != "item_1_call_0" {
		t.Fatalf("limit=2 page = %+v, want 2 items, has_more true, last item_1_call_0", list)
	}
	list = decodeInputItems(t, getInputItems(t, srv, base+"?limit=2&after=item_1_call_0"))
	if len(list.Data) != 1 || list.HasMore || list.Data[0].Type != "function_call_output" {
		t.Fatalf("after=item_1_call_0 page = %+v, want the 1 remaining item", list)
	}

	// order=desc reverses the chronological order.
	list = decodeInputItems(t, getInputItems(t, srv, base+"?order=desc"))
	if len(list.Data) != 3 || list.Data[0].Type != "function_call_output" || list.Data[2].Role != "user" {
		t.Fatalf("order=desc list = %+v, want reversed items", list)
	}

	// Bad limit / unknown cursor / bad order / unknown id are 400/404.
	for _, tc := range []struct {
		path string
		want int
	}{
		{base + "?limit=0", http.StatusBadRequest},
		{base + "?limit=abc", http.StatusBadRequest},
		{base + "?limit=200", http.StatusOK}, // clamped to 100, not an error
		{base + "?after=item_99", http.StatusBadRequest},
		{base + "?order=sideways", http.StatusBadRequest},
		{"/v1/responses/resp_nope/input_items", http.StatusNotFound},
	} {
		if rec := getInputItems(t, srv, tc.path); rec.Code != tc.want {
			t.Fatalf("GET %s: status = %d, want %d (body %s)", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}

	// Wrong method is a 405.
	req := httptest.NewRequest(http.MethodPost, base, nil)
	rec := httptest.NewRecorder()
	inputItemsMux(srv).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST input_items: status = %d, want 405", rec.Code)
	}
}

// TestM84TranscriptToInputItems is the pure-conversion unit test:
// image parts map to input_image, assistant text maps to output_text,
// a tool-call-only assistant turn yields no message item, and
// dropLastAssistant=false keeps a trailing assistant turn (the
// in-progress / not-yet-appended case never drops real input).
func TestM84TranscriptToInputItems(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "look at this", Parts: []provider.ContentPart{
			{Type: "text", Text: "look at this"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.invalid/cat.png"}},
		}},
		{Role: "assistant", Content: "prior answer"},
	}
	items := transcriptToInputItems(msgs, false)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Content[1].Type != "input_image" || items[0].Content[1].ImageURL != "https://example.invalid/cat.png" {
		t.Fatalf("image part = %+v, want input_image with the stored URL", items[0].Content[1])
	}
	if items[1].Content[0].Type != "output_text" || items[1].Content[0].Text != "prior answer" {
		t.Fatalf("assistant part = %+v, want output_text", items[1].Content[0])
	}
	// With dropLastAssistant the trailing assistant turn goes away.
	if kept := transcriptToInputItems(msgs, true); len(kept) != 1 {
		t.Fatalf("drop-last items = %d, want 1", len(kept))
	}
}
