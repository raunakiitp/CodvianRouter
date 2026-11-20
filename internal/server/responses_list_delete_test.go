package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// decodeResponsesList decodes a GET /v1/responses page: the OpenAI
// list shape with full stored response objects in data.
func decodeResponsesList(t *testing.T, recBody []byte) responsesList {
	t.Helper()
	var list responsesList
	if err := json.Unmarshal(recBody, &list); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, recBody)
	}
	return list
}

func listIDs(t *testing.T, list responsesList) []string {
	t.Helper()
	ids := make([]string, 0, len(list.Data))
	for _, raw := range list.Data {
		var obj struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("decode list item: %v (%s)", err, raw)
		}
		ids = append(ids, obj.ID)
	}
	return ids
}

// createResponse is a one-line Responses create returning the minted id.
func createResponse(t *testing.T, srv *Server, input string) string {
	t.Helper()
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"gpt-4o","input":`+jsonString(input)+`}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create %q: status = %d, want 200 (body %s)", input, rec.Code, rec.Body.String())
	}
	return decodeResponsesBody(t, rec).ID
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestResponsesListPagination is the server gate for CHECKLIST 204:
// GET /v1/responses lists newest first with limit + after-cursor
// paging, full stored objects in data, and honest has_more /
// first_id / last_id. Bad limits, bad cursors and non-desc orders
// are 400s; the list needs a gateway key like the other /v1 routes.
func TestResponsesListPagination(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "hello back", Model: "gpt-4o",
		Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))

	// Empty store: an empty page, null cursors, has_more false.
	rec := doReq(t, srv, http.MethodGet, "/v1/responses", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty list: status = %d, want 200", rec.Code)
	}
	list := decodeResponsesList(t, rec.Body.Bytes())
	if list.Object != "list" || len(list.Data) != 0 || list.HasMore || list.FirstID != nil || list.LastID != nil {
		t.Fatalf("empty list = %+v, want list/empty/no-more/null cursors", list)
	}

	id1 := createResponse(t, srv, "one")
	id2 := createResponse(t, srv, "two")
	id3 := createResponse(t, srv, "three")

	// Default limit (20) returns all three, newest first.
	list = decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses", "", "test-key-123").Body.Bytes())
	ids := listIDs(t, list)
	if len(ids) != 3 || ids[0] != id3 || ids[1] != id2 || ids[2] != id1 {
		t.Fatalf("default list ids = %v, want [%s %s %s] (newest first)", ids, id3, id2, id1)
	}
	if list.HasMore || list.FirstID == nil || *list.FirstID != id3 || list.LastID == nil || *list.LastID != id1 {
		t.Fatalf("default list pagination = has_more %v first %v last %v, want false/%s/%s", list.HasMore, list.FirstID, list.LastID, id3, id1)
	}
	// The data items are the full stored objects, not summaries.
	var first struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(list.Data[0], &first); err != nil {
		t.Fatalf("decode first item: %v", err)
	}
	if first.Object != "response" || first.Model != "gpt-4o" || first.Status != "completed" || first.Output[0].Content[0].Text != "hello back" {
		t.Fatalf("first item = %+v, want the full stored response object", first)
	}

	// limit=2: first page of two, has_more true; after=last_id
	// continues with the remaining one, has_more false.
	list = decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses?limit=2", "", "test-key-123").Body.Bytes())
	ids = listIDs(t, list)
	if len(ids) != 2 || ids[0] != id3 || ids[1] != id2 || !list.HasMore {
		t.Fatalf("limit=2 page = %v has_more %v, want [%s %s] true", ids, list.HasMore, id3, id2)
	}
	if list.LastID == nil || *list.LastID != id2 {
		t.Fatalf("limit=2 last_id = %v, want %s", list.LastID, id2)
	}
	list = decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses?limit=2&after="+id2, "", "test-key-123").Body.Bytes())
	ids = listIDs(t, list)
	if len(ids) != 1 || ids[0] != id1 || list.HasMore {
		t.Fatalf("after=%s page = %v has_more %v, want [%s] false", id2, ids, list.HasMore, id1)
	}
	// after the last id: an empty page, has_more false.
	list = decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses?limit=2&after="+id1, "", "test-key-123").Body.Bytes())
	if len(list.Data) != 0 || list.HasMore {
		t.Fatalf("after-last page = %+v, want empty / no-more", list)
	}
	// limit above the max clamps instead of erroring.
	list = decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses?limit=500", "", "test-key-123").Body.Bytes())
	if len(list.Data) != 3 {
		t.Fatalf("limit=500 page = %d items, want 3 (clamped to 100)", len(list.Data))
	}
	// Bad limits, bad cursors and non-desc orders are 400s.
	for _, path := range []string{
		"/v1/responses?limit=0",
		"/v1/responses?limit=-1",
		"/v1/responses?limit=abc",
		"/v1/responses?after=resp_never",
		"/v1/responses?order=asc",
	} {
		if rec := doReq(t, srv, http.MethodGet, path, "", "test-key-123"); rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400 (body %q)", path, rec.Code, rec.Body.String())
		}
	}
	// The list needs a gateway key, like the other /v1 routes.
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key list: status = %d, want 401", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses", "", "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key list: status = %d, want 401", rec.Code)
	}
}

// TestResponsesDelete is the server gate for CHECKLIST 204: DELETE
// removes the stored transcript — retrieve and continuation both
// 404 afterwards — deleting an unknown id is an honest 404 (never
// a silent success), and deleting twice 404s the second time.
func TestResponsesDelete(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "hello back", Model: "gpt-4o",
		Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))
	id1 := createResponse(t, srv, "one")
	id2 := createResponse(t, srv, "two")

	// Auth: no key → 401, and nothing is deleted.
	if rec := doReq(t, srv, http.MethodDelete, "/v1/responses/"+id1, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key delete: status = %d, want 401", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/"+id1, "", "test-key-123"); rec.Code != http.StatusOK {
		t.Fatalf("retrieve after refused delete: status = %d, want 200", rec.Code)
	}
	// Unknown id → 404, never a silent success.
	if rec := doReq(t, srv, http.MethodDelete, "/v1/responses/resp_never", "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	// Delete id1: the OpenAI delete shape.
	rec := doReq(t, srv, http.MethodDelete, "/v1/responses/"+id1, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var deleted struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if deleted.ID != id1 || deleted.Object != "response" || !deleted.Deleted {
		t.Fatalf("delete response = %+v, want id %s / response / true", deleted, id1)
	}
	// Retrieve after delete → 404; continuation after delete → 404.
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/"+id1, "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("retrieve after delete: status = %d, want 404", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"gpt-4o","input":"again","previous_response_id":"`+id1+`"}`, "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("continuation after delete: status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	// Deleting twice: the second delete 404s.
	if rec := doReq(t, srv, http.MethodDelete, "/v1/responses/"+id1, "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404", rec.Code)
	}
	// The survivor is untouched: still listed and retrievable.
	list := decodeResponsesList(t, doReq(t, srv, http.MethodGet, "/v1/responses", "", "test-key-123").Body.Bytes())
	ids := listIDs(t, list)
	if len(ids) != 1 || ids[0] != id2 {
		t.Fatalf("list after delete = %v, want [%s]", ids, id2)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/"+id2, "", "test-key-123"); rec.Code != http.StatusOK {
		t.Fatalf("retrieve survivor: status = %d, want 200", rec.Code)
	}
	// Method check: POST to the item path is not a delete — the
	// mux answers 405 (the path exists for GET and DELETE only).
	if rec := doReq(t, srv, http.MethodPost, "/v1/responses/"+id2, `{}`, "test-key-123"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST item path: status = %d, want 405 (body %q)", rec.Code, rec.Body.String())
	}
}
