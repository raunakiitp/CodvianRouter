package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

type evalTestEngine struct{}

func (evalTestEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "role content org/team/gpt-5-mini reasoning prompt_tokens total_tokens fallback strategy request_id"}, nil
}

func (evalTestEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	msg := "role content org/team/gpt-5-mini reasoning prompt_tokens total_tokens fallback strategy request_id"
	if onDelta != nil {
		_ = onDelta(msg)
	}
	return &provider.ChatResponse{Content: msg}, nil
}

func evalTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{DataDir: t.TempDir()}
	return newTestServer(t, &evalTestEngine{}, WithConfig(cfg))
}

func doEvalReq(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	// Populate the {id} path value for the subresource handlers (they
	// are invoked directly, bypassing the ServeMux that would bind it).
	if strings.HasPrefix(path, "/api/eval/schedules/") {
		rest := strings.TrimPrefix(path, "/api/eval/schedules/")
		id := strings.TrimSuffix(rest, "/trigger")
		id = strings.TrimSuffix(id, "/history")
		req.SetPathValue("id", strings.Trim(id, "/"))
	}
	switch {
	case strings.HasPrefix(path, "/api/eval/schedules/") && strings.HasSuffix(path, "/trigger"):
		srv.handleEvalTrigger(rec, req)
	case strings.HasPrefix(path, "/api/eval/schedules/") && strings.HasSuffix(path, "/history"):
		srv.handleEvalHistory(rec, req)
	case strings.HasPrefix(path, "/api/eval/schedules/"):
		srv.handleEvalScheduleByID(rec, req)
	default:
		srv.handleEvalSchedules(rec, req)
	}
	return rec
}

func TestEvalSchedulesCRUDAndHistory(t *testing.T) {
	srv := evalTestServer(t)

	rec := doEvalReq(t, srv, http.MethodGet, "/api/eval/schedules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	var list evalSchedulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Schedules) != 0 {
		t.Fatalf("expected empty, got %d", len(list.Schedules))
	}

	rec = doEvalReq(t, srv, http.MethodPost, "/api/eval/schedules",
		`{"id":"nightly","suite":"permanent","every":"30m","regression_max_pass_rate_drop":0.15,"min_runs_for_baseline":2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body=%s", rec.Code, rec.Body.String())
	}
	var view evalScheduleView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ID != "nightly" || !view.Enabled {
		t.Fatalf("create view wrong: %+v", view)
	}

	rec = doEvalReq(t, srv, http.MethodPost, "/api/eval/schedules", `{"every":"5m"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad create: status %d", rec.Code)
	}

	if rec = doEvalReq(t, srv, http.MethodGet, "/api/eval/schedules/nightly", ""); rec.Code != http.StatusOK {
		t.Fatalf("get: status %d", rec.Code)
	}

	if rec = doEvalReq(t, srv, http.MethodPut, "/api/eval/schedules/nightly", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("update: status %d body=%s", rec.Code, rec.Body.String())
	}
	var up evalScheduleView
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	if up.Enabled {
		t.Fatal("expected schedule paused")
	}

	rec = doEvalReq(t, srv, http.MethodPost, "/api/eval/schedules/nightly/trigger", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger: status %d body=%s", rec.Code, rec.Body.String())
	}
	var run evalRunView
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || run.PassRate != 1.0 || run.Total == 0 {
		t.Fatalf("trigger run wrong: %+v", run)
	}

	rec = doEvalReq(t, srv, http.MethodGet, "/api/eval/schedules/nightly/history", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history: status %d", rec.Code)
	}
	var hist evalHistoryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &hist)
	if len(hist.Runs) == 0 || hist.Runs[0].Status != "completed" {
		t.Fatalf("history wrong: %+v", hist.Runs)
	}

	if rec = doEvalReq(t, srv, http.MethodDelete, "/api/eval/schedules/nightly", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d", rec.Code)
	}
	if rec = doEvalReq(t, srv, http.MethodGet, "/api/eval/schedules/nightly", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: status %d", rec.Code)
	}
}

func TestEvalSchedulePersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	srv1 := newTestServer(t, &evalTestEngine{}, WithConfig(cfg))
	if rec := doEvalReq(t, srv1, http.MethodPost, "/api/eval/schedules", `{"id":"persist","every":"1h"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	if rec := doEvalReq(t, srv1, http.MethodPost, "/api/eval/schedules/persist/trigger", ""); rec.Code != http.StatusOK {
		t.Fatalf("trigger: %d", rec.Code)
	}
	if err := srv1.Close(); err != nil {
		t.Fatal(err)
	}
	srv2 := newTestServer(t, &evalTestEngine{}, WithConfig(cfg))
	if rec := doEvalReq(t, srv2, http.MethodGet, "/api/eval/schedules/persist", ""); rec.Code != http.StatusOK {
		t.Fatalf("reopen get: %d body=%s", rec.Code, rec.Body.String())
	}
	rec := doEvalReq(t, srv2, http.MethodGet, "/api/eval/schedules/persist/history", "")
	var hist evalHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Runs) == 0 || hist.Runs[0].Status != "completed" {
		t.Fatalf("history not restored: %+v", hist.Runs)
	}
}
