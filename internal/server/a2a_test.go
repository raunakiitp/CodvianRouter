// M9.8 A2A HTTP layer tests (internal/server/a2a.go): real HTTP via
// httptest — no mocks standing in for the wire. The "test provider"
// is the repo's fakeEngine / small capturing engines below, i.e.
// the real router dispatch seam (s.engine.Chat) with a stubbed
// engine, never a canned A2A response: every happy-path assertion
// reads the engine's own reply back out of the task result.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

const (
	a2aKeyOne = "a2a-key-one"
	a2aKeyTwo = "a2a-key-two"
)

// newA2ATestServer builds a Server with the A2A flag as given and
// two static gateway keys. WithConfig comes first so s.cfg (data
// dir + flag) is set; WithStaticKeys after it so the verifier is
// the static one (callerKeyID can name both keys, which is what
// owner derivation needs).
func newA2ATestServer(t *testing.T, eng Engine, dataDir string, enabled bool, opts ...Option) *Server {
	t.Helper()
	cfg := config.DefaultConfig(dataDir)
	cfg.A2AEnabled = enabled
	all := append([]Option{
		WithConfig(cfg),
		WithStaticKeys(a2aKeyOne, a2aKeyTwo),
		WithLogger(log.New(io.Discard, "", 0)),
	}, opts...)
	srv := New(eng, all...)
	// Close the server-owned stores at test end: on Windows an
	// open tasks.db blocks t.TempDir cleanup, failing the test
	// at teardown even when every assertion passed (same
	// pattern as newTestServer).
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// a2aOwnerForKey derives the A2A owner id for a static key through
// the server's own callerKeyID, so the test never re-implements
// the derivation.
func a2aOwnerForKey(t *testing.T, srv *Server, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	id := srv.callerKeyID(req)
	if id == "" {
		t.Fatalf("callerKeyID(%q) is empty", key)
	}
	return a2a.OwnerIDForKey(id)
}

// a2aPost issues one JSON-RPC call over the test recorder and
// decodes the envelope.
func a2aPost(t *testing.T, srv *Server, body, key string) (int, map[string]any) {
	t.Helper()
	rec := doReq(t, srv, http.MethodPost, "/a2a", body, key)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST /a2a response is not JSON (status %d): %v: %s", rec.Code, err, rec.Body.String())
	}
	return rec.Code, out
}

// a2aErrCode extracts the JSON-RPC error code, or -1 when the
// envelope carries no error.
func a2aErrCode(t *testing.T, out map[string]any) int {
	t.Helper()
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		return -1
	}
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("error code is not a number: %#v", errObj["code"])
	}
	return int(code)
}

// a2aTask extracts result.task from a successful envelope.
func a2aTask(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no result: %v", out)
	}
	task, ok := res["task"].(map[string]any)
	if !ok {
		t.Fatalf("result has no task: %v", res)
	}
	return task
}

func a2aSendBody(method string, params string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
}

const a2aHelloParams = `{"messages":[{"role":"user","content":"hello"}],"model":"model-a"}`

// a2aCaptureEngine records the ChatRequest the A2A processor
// built, so tests can prove the real dispatch path was used with
// the caller's key and the requested model.
type a2aCaptureEngine struct {
	mu      sync.Mutex
	lastReq provider.ChatRequest
	calls   int
	resp    provider.ChatResponse
	err     error
}

func (e *a2aCaptureEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.lastReq = req
	e.calls++
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	r := e.resp
	return &r, nil
}

func (e *a2aCaptureEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	resp, err := e.Chat(context.Background(), req)
	if err != nil {
		return nil, err
	}
	if onDelta != nil {
		if err := onDelta(resp.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (e *a2aCaptureEngine) lastRequest() provider.ChatRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastReq
}

// a2aBlockingEngine blocks inside Chat until its context is done
// (cancel/timeout) or release is closed. returned reports whether
// the call ended via ctx.Done — the proof that a cancel actually
// aborted the router call instead of merely discarding its result.
type a2aBlockingEngine struct {
	started  chan struct{}
	returned chan bool
	release  chan struct{}
	once     sync.Once
	resp     provider.ChatResponse
}

func newA2ABlockingEngine() *a2aBlockingEngine {
	return &a2aBlockingEngine{
		started:  make(chan struct{}),
		returned: make(chan bool, 1),
		release:  make(chan struct{}),
		resp:     provider.ChatResponse{Content: "released", Model: "model-a"},
	}
}

func (e *a2aBlockingEngine) Chat(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		e.returned <- true
		return nil, ctx.Err()
	case <-e.release:
		e.returned <- false
		r := e.resp
		return &r, nil
	}
}

func (e *a2aBlockingEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	resp, err := e.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if onDelta != nil {
		if err := onDelta(resp.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// TestA2ADisabledAnswers404AndCreatesNoDB: the flag off means 404
// for an authenticated call, and no tasks.db (nor its directory)
// is created — the surface does not exist until enabled.
func TestA2ADisabledAnswers404AndCreatesNoDB(t *testing.T) {
	dir := t.TempDir()
	srv := newA2ATestServer(t, &fakeEngine{}, dir, false)
	status, out := a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if status != http.StatusNotFound {
		t.Fatalf("disabled /a2a: status %d, want 404: %v", status, out)
	}
	if got := a2aErrCode(t, out); got != -32000 {
		t.Errorf("disabled /a2a: error code %d, want -32000", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "a2a", "tasks.db")); !os.IsNotExist(err) {
		t.Errorf("disabled /a2a created tasks.db (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a2a")); !os.IsNotExist(err) {
		t.Errorf("disabled /a2a created the a2a directory (stat err %v)", err)
	}
	// A disabled endpoint still requires auth first: no key is 401,
	// not a 404 that would confirm the route exists to strangers.
	rec := doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams), "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("disabled /a2a without key: status %d, want 401", rec.Code)
	}
}

// TestA2ARequiresAuth: no key and an invalid key are both 401 on
// the enabled endpoint — never keyless.
func TestA2ARequiresAuth(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	rec := doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams), "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams), "not-a-key")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid key: status %d, want 401", rec.Code)
	}
}

// TestA2AMessageSendHappyPath: message/send runs the message
// through the real dispatch (engine.Chat) with the caller's own
// key id as CallerID and the requested model, and the task comes
// back Completed carrying the router's raw reply as its result.
func TestA2AMessageSendHappyPath(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "hello from router", Model: "model-a"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("message/send: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("message/send: unexpected error %d: %v", code, out)
	}
	task := a2aTask(t, out)
	if task["state"] != "completed" {
		t.Fatalf("task state = %v, want completed: %v", task["state"], task)
	}
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatal("task has no id")
	}
	// The result is the router's own reply, not a canned A2A string.
	raw, err := json.Marshal(task["result"])
	if err != nil {
		t.Fatalf("marshal task result: %v", err)
	}
	var resp provider.ChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("task result is not a ChatResponse: %v (%s)", err, raw)
	}
	if resp.Content != "hello from router" {
		t.Errorf("task result content = %q, want the engine reply", resp.Content)
	}
	// The processor called the engine with the caller's key and
	// the requested model, as a chat completion.
	got := eng.lastRequest()
	wantCaller := srv.callerKeyID(mustA2AReq(t, a2aKeyOne))
	if got.CallerID != wantCaller || wantCaller == "" {
		t.Errorf("engine CallerID = %q, want the caller's key id %q", got.CallerID, wantCaller)
	}
	if got.Model != "model-a" {
		t.Errorf("engine Model = %q, want model-a", got.Model)
	}
	if got.Endpoint != provider.EndpointChatCompletions {
		t.Errorf("engine Endpoint = %q, want chat completions", got.Endpoint)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" || got.Messages[0].Role != "user" {
		t.Errorf("engine Messages = %+v, want one user message 'hello'", got.Messages)
	}
	// The task is persisted: tasks/get returns the same task.
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, id)), a2aKeyOne)
	gotTask := a2aTask(t, out)
	if gotTask["id"] != id || gotTask["state"] != "completed" {
		t.Errorf("tasks/get = %v, want id %s completed", gotTask, id)
	}
}

func mustA2AReq(t *testing.T, key string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// TestA2AMessageSendRouterErrorFailsTask: a router error fails
// the task with the real error — no fabricated success, and the
// response still returns the task.
func TestA2AMessageSendRouterErrorFailsTask(t *testing.T) {
	eng := &a2aCaptureEngine{err: errors.New("upstream boom")}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("message/send on router error: status %d, want 200 with a failed task: %v", status, out)
	}
	task := a2aTask(t, out)
	if task["state"] != "failed" {
		t.Fatalf("task state = %v, want failed: %v", task["state"], task)
	}
	errStr, _ := task["error"].(string)
	if !strings.Contains(errStr, "upstream boom") {
		t.Errorf("task error = %q, want the real router error", errStr)
	}
}

// TestA2AMessageSendTimeoutFailsTask: a processor that never
// returns is bounded by the server timeout — the task fails with
// a timeout error and the request returns instead of hanging.
func TestA2AMessageSendTimeoutFailsTask(t *testing.T) {
	eng := &fakeEngine{blocked: make(chan struct{})} // never released
	srv := newA2ATestServer(t, eng, t.TempDir(), true, WithTimeout(100*time.Millisecond))
	start := time.Now()
	status, out := a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("message/send took %v, want the timeout to bound it", elapsed)
	}
	if status != http.StatusOK {
		t.Fatalf("message/send on timeout: status %d, want 200 with a failed task: %v", status, out)
	}
	task := a2aTask(t, out)
	if task["state"] != "failed" {
		t.Fatalf("task state = %v, want failed: %v", task["state"], task)
	}
	errStr, _ := task["error"].(string)
	if !strings.Contains(errStr, "deadline exceeded") && !strings.Contains(errStr, "context deadline") {
		t.Errorf("task error = %q, want a timeout error", errStr)
	}
}

// a2aSSELines reads SSE lines from a live response into a channel;
// the channel closes when the stream ends.
func a2aSSELines(t *testing.T, resp *http.Response) <-chan string {
	t.Helper()
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

// TestA2AMessageStreamSSE: message/stream emits task-state SSE
// events and a final task event, then closes the stream on the
// terminal state.
func TestA2AMessageStreamSSE(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "streamed hello", Model: "model-a"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("message/stream: status %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	lines := a2aSSELines(t, resp)
	var sawState, sawFinal bool
	var finalState string
	timeout := time.After(10 * time.Second)
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			if line == "event: task-state" {
				sawState = true
			}
			if line == "event: task" {
				sawFinal = true
			}
			if strings.HasPrefix(line, "data: ") && sawFinal {
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err == nil {
					if task, ok := data["task"].(map[string]any); ok {
						finalState, _ = task["state"].(string)
						if result, ok := task["result"].(map[string]any); ok {
							if result["content"] != "streamed hello" {
								t.Errorf("final task result content = %v, want the engine reply", result["content"])
							}
						} else {
							t.Errorf("final task has no result: %v", task)
						}
					}
				}
			}
		case <-timeout:
			t.Fatal("message/stream did not close within 10s")
		}
	}
	if !sawState {
		t.Error("stream emitted no task-state event")
	}
	if !sawFinal {
		t.Error("stream emitted no final task event")
	}
	if finalState != "completed" {
		t.Errorf("final task state = %q, want completed", finalState)
	}
}

// TestA2ATasksGetAndCancel: tasks/get returns the task; cancelling
// a Submitted task succeeds; cancelling a terminal task is -32602.
func TestA2ATasksGetAndCancel(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	owner := a2aOwnerForKey(t, srv, a2aKeyOne)
	task, err := svc.CreateTask(owner, json.RawMessage(a2aHelloParams))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// get
	_, out := a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, task.ID)), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("tasks/get: unexpected error %d: %v", code, out)
	}
	if got := a2aTask(t, out); got["id"] != task.ID || got["state"] != "submitted" {
		t.Errorf("tasks/get = %v, want id %s submitted", got, task.ID)
	}
	// cancel (happy path, from Submitted)
	_, out = a2aPost(t, srv, a2aSendBody("tasks/cancel", fmt.Sprintf(`{"id":%q}`, task.ID)), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("tasks/cancel: unexpected error %d: %v", code, out)
	}
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Errorf("tasks/cancel state = %v, want cancelled", got["state"])
	}
	// cancel again: terminal now, illegal transition is -32602.
	_, out = a2aPost(t, srv, a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, task.ID)), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("cancel of terminal task: error code %d, want -32602: %v", code, out)
	}
	// get still works after cancel.
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, task.ID)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Errorf("tasks/get after cancel: state %v, want cancelled", got["state"])
	}
}

// TestA2ACrossOwnerIsNotFound: another owner's get/cancel behave
// exactly as if the task did not exist (-32602, never a hint it
// exists), while the owner's own access still works.
func TestA2ACrossOwnerIsNotFound(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "for key one", Model: "model-a"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	_, out := a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	task := a2aTask(t, out)
	id, _ := task["id"].(string)
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, id)), a2aKeyTwo)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("cross-owner get: error code %d, want -32602: %v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "forbidden") {
		t.Errorf("cross-owner get hints the task exists: %v", out)
	}
	_, out = a2aPost(t, srv, a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, id)), a2aKeyTwo)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("cross-owner cancel: error code %d, want -32602: %v", code, out)
	}
	// Owner's own get still works: the foreign cancel changed nothing.
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, id)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "completed" {
		t.Errorf("owner get after foreign cancel: state %v, want completed", got["state"])
	}
}

// TestA2ACancelAbortsProcessor: cancelling a Working stream task
// aborts the in-flight router call — the engine's Chat returns
// via its context (not by running to completion), the task ends
// Cancelled, and the SSE stream closes.
func TestA2ACancelAbortsProcessor(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	post := func(body, key string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("POST /a2a response is not JSON (status %d): %v: %s", resp.StatusCode, err, raw)
		}
		return resp.StatusCode, out
	}
	// Start the stream; the processor blocks inside the engine.
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("message/stream: status %d: %s", resp.StatusCode, body)
	}
	lines := a2aSSELines(t, resp)
	// Wait for the engine to be inside Chat, then read the task id
	// off the first state event.
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat was never entered")
	}
	var taskID string
	timeout := time.After(10 * time.Second)
readID:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the task id arrived")
			}
			if strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err == nil {
					if id, ok := data["task_id"].(string); ok && id != "" {
						taskID = id
						break readID
					}
				}
			}
		case <-timeout:
			t.Fatal("no task-state event within 10s")
		}
	}
	// Cancel it: the handler proves ownership via the store
	// transition first, then aborts the processor.
	status, out := post(a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("tasks/cancel: status %d: %v", status, out)
	}
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Fatalf("tasks/cancel state = %v, want cancelled", got["state"])
	}
	// Proof the router call actually stopped: the blocked Chat
	// returned, and it returned because its context was cancelled.
	select {
	case viaCtx := <-eng.returned:
		if !viaCtx {
			t.Error("engine Chat returned normally after cancel — the cancel did not abort the router call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after cancel — the router call was not aborted")
	}
	// The stream closes on the terminal state.
	drainTimeout := time.After(10 * time.Second)
drain:
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				break drain
			}
		case <-drainTimeout:
			t.Fatal("SSE stream did not close after cancel")
		}
	}
	// The task stays Cancelled (the aborted result is not stored).
	_, out = post(a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Errorf("tasks/get after cancel: state %v, want cancelled", got["state"])
	}
}

// TestA2AForeignCancelOfWorkingTaskChangesNothing: a foreign
// caller who knows a victim's task id must not abort the victim's
// in-flight router call. The cancel-func map is keyed by task id
// only, so abort-before-ownership-check used to let a -32602
// "task not found" answer carry a real side effect: the victim's
// Working task flipped to Failed. After the fix the foreign
// cancel changes nothing — the victim's task stays Working, the
// victim's processor is not aborted (the blocking engine stays
// blocked and the stream stays live), and the foreign caller
// still gets -32602.
func TestA2AForeignCancelOfWorkingTaskChangesNothing(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	post := func(body, key string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("POST /a2a response is not JSON (status %d): %v: %s", resp.StatusCode, err, raw)
		}
		return resp.StatusCode, out
	}
	// Victim (key one) starts a stream; the processor blocks
	// inside the engine.
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("message/stream: status %d: %s", resp.StatusCode, body)
	}
	lines := a2aSSELines(t, resp)
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat was never entered")
	}
	var taskID string
	timeout := time.After(10 * time.Second)
readID:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the task id arrived")
			}
			if strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err == nil {
					if id, ok := data["task_id"].(string); ok && id != "" {
						taskID = id
						break readID
					}
				}
			}
		case <-timeout:
			t.Fatal("no task-state event within 10s")
		}
	}
	// Foreign cancel (key two): still -32602, "task not found".
	_, out := post(a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyTwo)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("foreign cancel of a Working task: error code %d, want -32602: %v", code, out)
	}
	if errObj, ok := out["error"].(map[string]any); ok {
		if msg, _ := errObj["message"].(string); msg != "task not found" {
			t.Errorf("foreign cancel error message = %q, want \"task not found\"", msg)
		}
	}
	// The victim's task is untouched: still Working.
	_, out = post(a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "working" {
		t.Errorf("victim task after foreign cancel: state %v, want working", got["state"])
	}
	// The victim's processor was NOT aborted: the engine is still
	// blocked inside Chat.
	select {
	case viaCtx := <-eng.returned:
		t.Errorf("engine Chat returned after foreign cancel (viaCtx=%v) — the foreign cancel aborted the victim's router call", viaCtx)
	case <-time.After(300 * time.Millisecond):
	}
	// The victim's stream is still live: its channel is not closed.
	select {
	case _, ok := <-lines:
		if !ok {
			t.Error("victim SSE stream closed after foreign cancel")
		}
	default:
	}
	// The victim's own cancel still works afterwards: it aborts
	// the processor and the task ends Cancelled, not Failed.
	status, out := post(a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("owner cancel after foreign cancel: status %d: %v", status, out)
	}
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Fatalf("owner cancel state = %v, want cancelled", got["state"])
	}
	select {
	case viaCtx := <-eng.returned:
		if !viaCtx {
			t.Error("engine Chat returned normally after owner cancel — the cancel did not abort the router call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after owner cancel")
	}
	drainTimeout := time.After(10 * time.Second)
drain:
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				break drain
			}
		case <-drainTimeout:
			t.Fatal("SSE stream did not close after owner cancel")
		}
	}
	_, out = post(a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Errorf("tasks/get after owner cancel: state %v, want cancelled", got["state"])
	}
}

// TestA2AV1Aliases: SendMessage behaves like message/send with the
// 1.0 Task shape (TASK_STATE_* + reply text in
// status.message.parts), and SendStreamingMessage streams like
// message/stream. No other aliases exist: tasks/get has none.
func TestA2AV1Aliases(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "v1 reply", Model: "model-a"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	_, out := a2aPost(t, srv, a2aSendBody("SendMessage", `{"message":{"role":"user","content":"hi","contextId":"ctx-1"}}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("SendMessage: unexpected error %d: %v", code, out)
	}
	task := a2aTask(t, out)
	status, _ := task["status"].(map[string]any)
	if status["state"] != "TASK_STATE_COMPLETED" {
		t.Errorf("SendMessage status.state = %v, want TASK_STATE_COMPLETED", status["state"])
	}
	msg, _ := status["message"].(map[string]any)
	parts, _ := msg["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("SendMessage status.message.parts = %v, want one text part", parts)
	}
	part, _ := parts[0].(map[string]any)
	if part["text"] != "v1 reply" {
		t.Errorf("SendMessage reply text = %v, want the engine reply", part["text"])
	}
	if task["contextId"] != "ctx-1" {
		t.Errorf("SendMessage contextId = %v, want ctx-1", task["contextId"])
	}
	// SendStreamingMessage streams and closes like message/stream.
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("SendStreamingMessage", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SendStreamingMessage: status %d: %s", resp.StatusCode, body)
	}
	lines := a2aSSELines(t, resp)
	sawFinal := false
	sawV1Final := false
	pendingFinalData := false
	timeout := time.After(10 * time.Second)
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			if line == "event: task" {
				sawFinal = true
				pendingFinalData = true
			}
			if pendingFinalData && strings.HasPrefix(line, "data: ") {
				pendingFinalData = false
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
					t.Errorf("SendStreamingMessage final task event is not JSON: %v", err)
					break
				}
				task, _ := data["task"].(map[string]any)
				status, _ := task["status"].(map[string]any)
				if status["state"] != "TASK_STATE_COMPLETED" {
					t.Errorf("SendStreamingMessage final task status.state = %v, want TASK_STATE_COMPLETED (v1 shape)", status["state"])
				} else {
					sawV1Final = true
				}
			}
		case <-timeout:
			t.Fatal("SendStreamingMessage stream did not close within 10s")
		}
	}
	if !sawFinal {
		t.Error("SendStreamingMessage emitted no final task event")
	}
	if !sawV1Final {
		t.Error("SendStreamingMessage final task event did not carry the v1 Task shape")
	}
	// There is no v1 alias for tasks/get: GetTask is unknown.
	_, out = a2aPost(t, srv, a2aSendBody("GetTask", `{"taskId":"x"}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32601 {
		t.Errorf("GetTask: error code %d, want -32601 (no such alias)", code)
	}
}

// TestA2AJSONRPCErrors: unknown method -32601, bad params -32602,
// malformed JSON -32700.
func TestA2AJSONRPCErrors(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	_, out := a2aPost(t, srv, a2aSendBody("tasks/list", `{}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32601 {
		t.Errorf("unknown method: error code %d, want -32601: %v", code, out)
	}
	_, out = a2aPost(t, srv, a2aSendBody("message/send", `{}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("message/send with no message: error code %d, want -32602: %v", code, out)
	}
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", `{}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("tasks/get with no id: error code %d, want -32602: %v", code, out)
	}
	_, out = a2aPost(t, srv, a2aSendBody("tasks/get", `{"taskId":"does-not-exist"}`), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -32602 {
		t.Errorf("tasks/get unknown id: error code %d, want -32602: %v", code, out)
	}
	rec := doReq(t, srv, http.MethodPost, "/a2a", `{not json`, a2aKeyOne)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse-error response is not JSON: %v: %s", err, rec.Body.String())
	}
	if code := a2aErrCode(t, raw); code != -32700 {
		t.Errorf("malformed JSON: error code %d, want -32700: %v", code, raw)
	}
}

// TestA2AMessagePartsShape: a message given as parts[] text is
// joined into the chat input, and a parts message with a model in
// metadata routes with that model.
func TestA2AMessagePartsShape(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "parts ok", Model: "model-b"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	params := `{"message":{"role":"user","parts":[{"text":"first "},{"text":"second"}]},"metadata":{"model":"model-b"}}`
	_, out := a2aPost(t, srv, a2aSendBody("message/send", params), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("parts message/send: unexpected error %d: %v", code, out)
	}
	got := eng.lastRequest()
	if len(got.Messages) != 1 || got.Messages[0].Content != "first \nsecond" {
		t.Errorf("engine Messages = %+v, want one message 'first \\nsecond'", got.Messages)
	}
	if got.Model != "model-b" {
		t.Errorf("engine Model = %q, want model-b from metadata", got.Model)
	}
}

// ---------------------------------------------------------------------------
// M9.8 follow-up: pre-call admission, send cancel-abort, disconnect
// race, 503 shape, trailing-bytes decoder, cancelled-task errors.
// ---------------------------------------------------------------------------

// a2aAdmissionSetup builds one issued gateway key, a real ledger,
// and a price book where "priced-model" costs $10/1M in + $30/1M
// out — the same rig as keyBudgetSetup, plus A2A enabled, so the
// admission tests drive the real gates with a real key id.
type a2aAdmissionSetup struct {
	cfg  *config.Config
	led  *ledger.Ledger
	book *config.PriceBook
	eng  *a2aCaptureEngine
	raw  string
	id   string
}

func newA2AAdmissionSetup(t *testing.T) *a2aAdmissionSetup {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	cfg.A2AEnabled = true
	key, raw, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	book := config.NewPriceBook()
	book.Entries["priced-model"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "hi", Model: "priced-model"}}
	return &a2aAdmissionSetup{
		cfg: cfg, led: led, book: book, eng: eng, raw: raw, id: key.ID,
	}
}

func (s *a2aAdmissionSetup) server(t *testing.T, extra ...Option) *Server {
	t.Helper()
	opts := []Option{WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}),
		WithLogger(log.New(io.Discard, "", 0))}
	srv := New(s.eng, append(opts, extra...)...)
	// Close the server-owned stores at test end: on Windows an
	// open tasks.db blocks t.TempDir cleanup, failing the test
	// at teardown even when every assertion passed (same
	// pattern as newTestServer / newA2ATestServer).
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func (s *a2aAdmissionSetup) seedPricedSpend(t *testing.T, prompt, completion int) {
	t.Helper()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
		PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
}

// a2aErrMessage extracts the JSON-RPC error message, or "" when
// the envelope carries no error.
func a2aErrMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := errObj["message"].(string)
	return msg
}

func a2aPricedSendBody() string {
	return a2aSendBody("message/send", `{"messages":[{"role":"user","content":"hello"}],"model":"priced-model"}`)
}

// TestA2AAdmissionOverBudgetKeyRefusedPreCall: a key over its
// rolling 5h budget is refused by message/send BEFORE any
// provider call — JSON-RPC error, zero engine calls, and the
// refusal names the budget.
func TestA2AAdmissionOverBudgetKeyRefusedPreCall(t *testing.T) {
	s := newA2AAdmissionSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	status, out := a2aPost(t, srv, a2aPricedSendBody(), s.raw)
	if status != http.StatusOK {
		t.Fatalf("over-budget send: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code == -1 {
		t.Fatalf("over-budget send: no JSON-RPC error, want a refusal: %v", out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(strings.ToLower(msg), "budget") {
		t.Errorf("over-budget send: error message %q, want it to name the budget", msg)
	}
	if s.eng.calls != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the over-budget key must be refused before any provider is called", s.eng.calls)
	}
	// No task was created for the refused call: the result
	// carries no task, so there is nothing to cancel or get.
	if _, ok := out["result"]; ok {
		t.Errorf("over-budget send: envelope carries a result %v, want none (no task created)", out["result"])
	}
}

// TestA2AAdmissionStreamOverBudgetKeyRefusedPreCall: the same
// gate on message/stream — a JSON-RPC error, never an SSE
// stream, and zero engine calls.
func TestA2AAdmissionStreamOverBudgetKeyRefusedPreCall(t *testing.T) {
	s := newA2AAdmissionSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	rec := doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/stream", `{"messages":[{"role":"user","content":"hello"}],"model":"priced-model"}`), s.raw)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("over-budget stream: response is not JSON (status %d): %v: %s", rec.Code, err, rec.Body.String())
	}
	if code := a2aErrCode(t, out); code == -1 {
		t.Fatalf("over-budget stream: no JSON-RPC error, want a refusal: %v", out)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("over-budget stream: Content-Type = %q, want a JSON error, not an SSE stream", ct)
	}
	if s.eng.calls != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the over-budget key must be refused before any provider is called", s.eng.calls)
	}
}

// TestA2AAdmissionGuardrailBlockedPromptRefusedPreCall: a prompt
// carrying PII is refused by message/send with the policy-refusal
// code -32001 BEFORE any provider call, and the refusal never
// echoes the prompt back.
func TestA2AAdmissionGuardrailBlockedPromptRefusedPreCall(t *testing.T) {
	eng := &a2aCaptureEngine{resp: provider.ChatResponse{Content: "hi", Model: "model-a"}}
	srv := newA2ATestServer(t, eng, t.TempDir(), true,
		WithGuardrails(guardrails.NewStoreWith(guardrails.Config{BlockPII: true})))
	params := `{"messages":[{"role":"user","content":"reach me at leaked@example.com"}],"model":"model-a"}`
	status, out := a2aPost(t, srv, a2aSendBody("message/send", params), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("guardrail-blocked send: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -32001 {
		t.Errorf("guardrail-blocked send: error code %d, want -32001 (policy refusal): %v", code, out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, "guardrail blocked") {
		t.Errorf("guardrail-blocked send: error message %q, want the guardrail refusal", msg)
	}
	if strings.Contains(fmt.Sprint(out), "leaked@example.com") {
		t.Errorf("guardrail-blocked send: refusal echoes the blocked prompt: %v", out)
	}
	if eng.calls != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the guardrail-blocked prompt must be refused before any provider is called", eng.calls)
	}
}

// TestA2AAdmissionRPMRefusedPreCall: a key with
// RequestsPerMinute=1 serves its first A2A send and refuses the
// second within the same minute — JSON-RPC error, zero further
// engine calls.
func TestA2AAdmissionRPMRefusedPreCall(t *testing.T) {
	s := newA2AAdmissionSetup(t)
	s.cfg.GatewayKeys[0].RequestsPerMinute = 1
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	status, out := a2aPost(t, srv, a2aPricedSendBody(), s.raw)
	if status != http.StatusOK || a2aErrCode(t, out) != -1 {
		t.Fatalf("first send under RPM=1: status %d, want a completed task: %v", status, out)
	}
	if task := a2aTask(t, out); task["state"] != "completed" {
		t.Fatalf("first send under RPM=1: task state %v, want completed", task["state"])
	}
	if s.eng.calls != 1 {
		t.Fatalf("engine Chat calls after first send = %d, want 1", s.eng.calls)
	}
	_, out = a2aPost(t, srv, a2aPricedSendBody(), s.raw)
	if code := a2aErrCode(t, out); code == -1 {
		t.Fatalf("second send under RPM=1: no JSON-RPC error, want a rate-limit refusal: %v", out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, "requests per minute") {
		t.Errorf("second send under RPM=1: error message %q, want the requests-per-minute refusal", msg)
	}
	if s.eng.calls != 1 {
		t.Fatalf("engine Chat calls after refused send = %d, want 1 — the RPM-limited key must be refused before any provider is called", s.eng.calls)
	}
}

// TestA2AMessageSendCancelAbortsProcessor: cancelling a Working
// message/send aborts the in-flight router call — the engine's
// Chat returns via its context, the task ends Cancelled, and the
// send call itself returns the Cancelled task view, never a
// -32603 naming the lost Failed transition.
func TestA2AMessageSendCancelAbortsProcessor(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	type sendResult struct {
		status int
		out    map[string]any
		raw    string
	}
	resCh := make(chan sendResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/send", a2aHelloParams)))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("message/send response is not JSON (status %d): %v: %s", resp.StatusCode, err, raw)
			return
		}
		resCh <- sendResult{resp.StatusCode, out, string(raw)}
	}()
	// Wait for the engine to be inside Chat, then read the task
	// id off the store (the send response cannot arrive until
	// RunTask returns).
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat was never entered")
	}
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	owner := a2aOwnerForKey(t, srv, a2aKeyOne)
	var taskID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := svc.ListTasks(owner)
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(tasks) > 0 {
			taskID = tasks[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if taskID == "" {
		t.Fatal("no task appeared in the store for the in-flight send")
	}
	// Cancel it: the handler proves ownership via the store
	// transition first, then aborts the processor.
	_, out := a2aPost(t, srv, a2aSendBody("tasks/cancel", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("tasks/cancel of in-flight send: unexpected error %d: %v", code, out)
	}
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Fatalf("tasks/cancel state = %v, want cancelled", got["state"])
	}
	// Proof the router call actually stopped: the blocked Chat
	// returned, and it returned because its context was cancelled.
	select {
	case viaCtx := <-eng.returned:
		if !viaCtx {
			t.Error("engine Chat returned normally after cancel — the cancel did not abort the router call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after cancel — the router call was not aborted")
	}
	// The send call itself returns the Cancelled task — not a
	// JSON-RPC error for the lost Working→Failed transition.
	select {
	case res := <-resCh:
		if res.status != http.StatusOK {
			t.Fatalf("message/send after cancel: status %d, want 200 with the cancelled task: %s", res.status, res.raw)
		}
		if code := a2aErrCode(t, res.out); code != -1 {
			t.Fatalf("message/send after cancel: JSON-RPC error %d, want the Cancelled task view: %v", code, res.out)
		}
		if got := a2aTask(t, res.out); got["state"] != "cancelled" {
			t.Fatalf("message/send after cancel: task state %v, want cancelled", got["state"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("message/send did not return after cancel")
	}
}

// TestA2ADisconnectEndsCancelled: when the SSE client disconnects
// mid-stream, the task ends Cancelled — never nondeterministically
// Failed. The disconnect path lands CancelTask before aborting
// the processor, so the aborted RunTask cannot commit a Failed
// transition first.
func TestA2ADisconnectEndsCancelled(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("message/stream: status %d: %s", resp.StatusCode, body)
	}
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		resp.Body.Close()
		t.Fatal("engine Chat was never entered")
	}
	svc, err := srv.a2aService()
	if err != nil {
		resp.Body.Close()
		t.Fatalf("a2aService: %v", err)
	}
	owner := a2aOwnerForKey(t, srv, a2aKeyOne)
	var taskID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := svc.ListTasks(owner)
		if err != nil {
			resp.Body.Close()
			t.Fatalf("ListTasks: %v", err)
		}
		if len(tasks) > 0 {
			taskID = tasks[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if taskID == "" {
		resp.Body.Close()
		t.Fatal("no task appeared in the store for the stream")
	}
	// Disconnect the client: closing the body drops the
	// connection, which cancels the server-side request context.
	_ = resp.Body.Close()
	// The task must land Cancelled (poll: the server needs a
	// moment to notice the disconnect). It must never be Failed.
	deadline = time.Now().Add(10 * time.Second)
	for {
		cur, err := svc.GetTask(owner, taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if cur.State == a2a.StateCancelled {
			break
		}
		if cur.State == a2a.StateFailed {
			t.Fatalf("task after disconnect: state failed, want cancelled (error %q)", cur.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("task after disconnect: state %q after 10s, want cancelled", cur.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The router call was aborted via its context too.
	select {
	case viaCtx := <-eng.returned:
		if !viaCtx {
			t.Error("engine Chat returned normally after disconnect — the disconnect did not abort the router call")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after disconnect — the router call was not aborted")
	}
	// The wire view agrees: tasks/get reports Cancelled.
	_, out := a2aPost(t, srv, a2aSendBody("tasks/get", fmt.Sprintf(`{"taskId":%q}`, taskID)), a2aKeyOne)
	if got := a2aTask(t, out); got["state"] != "cancelled" {
		t.Errorf("tasks/get after disconnect: state %v, want cancelled", got["state"])
	}
}

// TestA2AStoreUnavailableIsGeneric: when the task store cannot be
// opened, POST /a2a answers 503 with a generic "a2a: store
// unavailable" — the store-open error (which carries the data-dir
// path) is never echoed to the client.
func TestA2AStoreUnavailableIsGeneric(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newA2ATestServer(t, &fakeEngine{}, blocker, true)
	rec := doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store-unavailable /a2a: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a2a: store unavailable") {
		t.Errorf("store-unavailable /a2a: body %s, want the generic message", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), blocker) {
		t.Errorf("store-unavailable /a2a: body echoes the data-dir path: %s", rec.Body.String())
	}
}

// TestA2ATrailingBytesRejected: bytes after the first JSON value
// are a parse error (-32700), not silently ignored — while
// trailing whitespace alone is still accepted.
func TestA2ATrailingBytesRejected(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	rec := doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams)+` {"jsonrpc":"2.0"}`, a2aKeyOne)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("trailing-bytes response is not JSON: %v: %s", err, rec.Body.String())
	}
	if code := a2aErrCode(t, out); code != -32700 {
		t.Errorf("trailing JSON value: error code %d, want -32700: %v", code, out)
	}
	rec = doReq(t, srv, http.MethodPost, "/a2a", a2aSendBody("message/send", a2aHelloParams)+` garbage`, a2aKeyOne)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("trailing-garbage response is not JSON: %v: %s", err, rec.Body.String())
	}
	if code := a2aErrCode(t, out); code != -32700 {
		t.Errorf("trailing garbage: error code %d, want -32700: %v", code, out)
	}
	// Trailing whitespace is not trailing data: the send succeeds.
	_, out = a2aPost(t, srv, a2aSendBody("message/send", a2aHelloParams)+"  \n", a2aKeyOne)
	if code := a2aErrCode(t, out); code != -1 {
		t.Errorf("trailing whitespace: unexpected error %d: %v", code, out)
	}
}
