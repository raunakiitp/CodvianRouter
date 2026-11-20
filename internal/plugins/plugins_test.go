package plugins

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func testReq() *Request {
	return &Request{Chat: &provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, Headers: map[string]string{}}
}

type stubPlugin struct {
	name  string
	hooks []HookKind
	pre   func(ctx context.Context, req *Request) (bool, string, error)
	post  func(ctx context.Context, req *Request, resp *provider.ChatResponse) error
}

func (s *stubPlugin) Name() string        { return s.name }
func (s *stubPlugin) Description() string { return "stub " + s.name }
func (s *stubPlugin) Hooks() []HookKind   { return s.hooks }
func (s *stubPlugin) PreRequest(ctx context.Context, req *Request) (bool, string, error) {
	if s.pre == nil {
		return false, "", nil
	}
	return s.pre(ctx, req)
}
func (s *stubPlugin) PostResponse(ctx context.Context, req *Request, resp *provider.ChatResponse) error {
	if s.post == nil {
		return nil
	}
	return s.post(ctx, req, resp)
}

func TestRegisterDuplicateNameRejected(t *testing.T) {
	r := New()
	if err := r.Register(&stubPlugin{name: "a", hooks: []HookKind{HookPreRequest}}, false); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(&stubPlugin{name: "a", hooks: []HookKind{HookPostResponse}}, false); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestRegisterNoHooksRejected(t *testing.T) {
	r := New()
	if err := r.Register(&stubPlugin{name: "a", hooks: nil}, false); err == nil {
		t.Fatal("plugin with no hooks accepted")
	}
}

func TestRegisterHookWithoutImplementationRejected(t *testing.T) {
	r := New()
	// stubPlugin implements both interfaces, so use a minimal plugin
	// that lists a hook it does not serve.
	p := &preOnlyPlugin{name: "x"}
	if err := r.Register(p, false); err == nil {
		t.Fatal("plugin listing post_response without implementing it accepted")
	}
}

type preOnlyPlugin struct{ name string }

func (p *preOnlyPlugin) Name() string        { return p.name }
func (p *preOnlyPlugin) Description() string { return "pre only" }
func (p *preOnlyPlugin) Hooks() []HookKind   { return []HookKind{HookPreRequest, HookPostResponse} }
func (p *preOnlyPlugin) PreRequest(_ context.Context, _ *Request) (bool, string, error) {
	return false, "", nil
}

func TestPreRequestOrdering(t *testing.T) {
	r := New()
	var order []string
	for _, name := range []string{"first", "second", "third"} {
		n := name
		if err := r.Register(&stubPlugin{name: n, hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, req *Request) (bool, string, error) {
			order = append(order, n)
			return false, "", nil
		}}, false); err != nil {
			t.Fatal(err)
		}
	}
	out := r.PreRequest(context.Background(), testReq())
	if out.Denied || out.Err != nil || out.Panic != nil {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if strings.Join(order, ",") != "first,second,third" {
		t.Fatalf("order = %v, want first,second,third", order)
	}
}

func TestPreRequestDenyStopsChainFailClosed(t *testing.T) {
	r := New()
	seen := 0
	_ = r.Register(&stubPlugin{name: "denier", hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, _ *Request) (bool, string, error) {
		return true, "nope", nil
	}}, false)
	_ = r.Register(&stubPlugin{name: "later", hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, _ *Request) (bool, string, error) {
		seen++
		return false, "", nil
	}}, false)
	out := r.PreRequest(context.Background(), testReq())
	if !out.Denied || out.Reason != "nope" || out.Plugin != "denier" {
		t.Fatalf("outcome = %+v", out)
	}
	if seen != 0 {
		t.Fatal("later plugin ran after a denial")
	}
}

func TestPreRequestErrorFailsClosed(t *testing.T) {
	r := New()
	_ = r.Register(&stubPlugin{name: "flaky", hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, _ *Request) (bool, string, error) {
		return false, "", errors.New("classifier down")
	}}, false)
	out := r.PreRequest(context.Background(), testReq())
	if out.Denied || out.Err == nil || out.Reason != "plugin_denied" {
		t.Fatalf("outcome = %+v, want error fail-closed", out)
	}
	stats := r.Snapshot()
	if stats[0].Stats[HookPreRequest].Errors != 1 {
		t.Fatalf("errors = %d, want 1", stats[0].Stats[HookPreRequest].Errors)
	}
}

func TestPreRequestPanicIsolatedFailClosed(t *testing.T) {
	r := New()
	_ = r.Register(&stubPlugin{name: "bomber", hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, _ *Request) (bool, string, error) {
		panic("boom")
	}}, false)
	_ = r.Register(&stubPlugin{name: "after", hooks: []HookKind{HookPreRequest}, pre: func(_ context.Context, _ *Request) (bool, string, error) {
		return false, "", nil
	}}, false)
	out := r.PreRequest(context.Background(), testReq())
	if out.Panic == nil || out.Plugin != "bomber" {
		t.Fatalf("outcome = %+v, want panic isolated", out)
	}
	if out.Denied || out.Err != nil {
		t.Fatalf("panic outcome must not also be deny/error: %+v", out)
	}
	// Registry still works after the panic: a second call panics
	// again (counted, not silently disabled) and the gateway lives.
	out2 := r.PreRequest(context.Background(), testReq())
	if out2.Panic == nil {
		t.Fatal("second call after panic did not panic again (plugin silently disabled?)")
	}
	stats := r.Snapshot()
	if stats[0].Stats[HookPreRequest].Panics != 2 {
		t.Fatalf("panics = %d, want 2", stats[0].Stats[HookPreRequest].Panics)
	}
	if stats[0].Stats[HookPreRequest].Calls != 2 {
		t.Fatalf("calls = %d, want 2", stats[0].Stats[HookPreRequest].Calls)
	}
	if stats[1].Stats[HookPreRequest].Calls != 0 {
		t.Fatal("plugin after the panicking one ran")
	}
}

func TestPostResponseErrorFailsOpen(t *testing.T) {
	r := New()
	ran := 0
	_ = r.Register(&stubPlugin{name: "badlog", hooks: []HookKind{HookPostResponse}, post: func(_ context.Context, _ *Request, _ *provider.ChatResponse) error {
		return errors.New("disk full")
	}}, false)
	_ = r.Register(&stubPlugin{name: "goodlog", hooks: []HookKind{HookPostResponse}, post: func(_ context.Context, _ *Request, _ *provider.ChatResponse) error {
		ran++
		return nil
	}}, false)
	resp := &provider.ChatResponse{Model: "m", Content: "hello"}
	failures := r.PostResponse(context.Background(), testReq(), resp)
	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1 (fail open: reported, not fatal)", len(failures))
	}
	if ran != 1 {
		t.Fatal("later post-response plugin did not run after an error")
	}
}

func TestPostResponsePanicFailsOpen(t *testing.T) {
	r := New()
	ran := 0
	_ = r.Register(&stubPlugin{name: "bomber", hooks: []HookKind{HookPostResponse}, post: func(_ context.Context, _ *Request, _ *provider.ChatResponse) error {
		panic("boom")
	}}, false)
	_ = r.Register(&stubPlugin{name: "after", hooks: []HookKind{HookPostResponse}, post: func(_ context.Context, _ *Request, _ *provider.ChatResponse) error {
		ran++
		return nil
	}}, false)
	failures := r.PostResponse(context.Background(), testReq(), &provider.ChatResponse{Model: "m"})
	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(failures))
	}
	if ran != 1 {
		t.Fatal("later plugin did not run after a panic")
	}
	stats := r.Snapshot()
	if stats[0].Stats[HookPostResponse].Panics != 1 {
		t.Fatalf("panics = %d, want 1", stats[0].Stats[HookPostResponse].Panics)
	}
}

// --- Provider adapter ---

type stubAdapter struct {
	name       string
	providerID string
	adapt      func(ctx context.Context, req *http.Request) (*http.Request, error)
}

func (s *stubAdapter) Name() string        { return s.name }
func (s *stubAdapter) Description() string { return "stub adapter" }
func (s *stubAdapter) Hooks() []HookKind   { return []HookKind{HookProviderAdapter} }
func (s *stubAdapter) ProviderID() string  { return s.providerID }
func (s *stubAdapter) AdaptHTTPRequest(ctx context.Context, req *http.Request) (*http.Request, error) {
	return s.adapt(ctx, req)
}

func TestAdapterPassthroughWhenNoneRegistered(t *testing.T) {
	r := New()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/v1/chat/completions", nil)
	out, err := r.AdaptHTTP(context.Background(), "unknown-provider", req)
	if err != nil || out != req {
		t.Fatalf("out=%v err=%v, want unchanged passthrough", out, err)
	}
}

func TestAdapterRewritesAndFailsClosed(t *testing.T) {
	r := New()
	_ = r.Register(&stubAdapter{name: "rewrite", providerID: "acme", adapt: func(_ context.Context, req *http.Request) (*http.Request, error) {
		clone := req.Clone(context.Background())
		clone.Header.Set("X-Acme-Wire", "v2")
		return clone, nil
	}}, false)
	req, _ := http.NewRequest(http.MethodPost, "https://acme.invalid/v1/chat", nil)
	out, err := r.AdaptHTTP(context.Background(), "acme", req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Header.Get("X-Acme-Wire") != "v2" {
		t.Fatal("adapter rewrite did not apply")
	}
	if req.Header.Get("X-Acme-Wire") != "" {
		t.Fatal("adapter mutated the original request instead of a clone")
	}

	// Error fails closed: the caller gets an error, never the
	// unadapted request.
	r2 := New()
	_ = r2.Register(&stubAdapter{name: "broken", providerID: "acme", adapt: func(_ context.Context, _ *http.Request) (*http.Request, error) {
		return nil, errors.New("cannot adapt")
	}}, false)
	if _, err := r2.AdaptHTTP(context.Background(), "acme", req); err == nil {
		t.Fatal("adapter error returned success")
	}
}

func TestAdapterPanicFailsClosed(t *testing.T) {
	r := New()
	_ = r.Register(&stubAdapter{name: "bomber", providerID: "acme", adapt: func(_ context.Context, _ *http.Request) (*http.Request, error) {
		panic("boom")
	}}, false)
	req, _ := http.NewRequest(http.MethodPost, "https://acme.invalid/v1/chat", nil)
	if _, err := r.AdaptHTTP(context.Background(), "acme", req); err == nil {
		t.Fatal("adapter panic returned success")
	}
	stats := r.Snapshot()
	if stats[0].Stats[HookProviderAdapter].Panics != 1 {
		t.Fatalf("panics = %d, want 1", stats[0].Stats[HookProviderAdapter].Panics)
	}
}

func TestAdapterDuplicateProviderRejected(t *testing.T) {
	r := New()
	_ = r.Register(&stubAdapter{name: "one", providerID: "acme", adapt: func(_ context.Context, req *http.Request) (*http.Request, error) { return req, nil }}, false)
	if err := r.Register(&stubAdapter{name: "two", providerID: "acme", adapt: func(_ context.Context, req *http.Request) (*http.Request, error) { return req, nil }}, false); err == nil {
		t.Fatal("second adapter for the same provider accepted")
	}
}

func TestSnapshotCountsAreReal(t *testing.T) {
	r := New()
	_ = r.Register(&stubPlugin{name: "a", hooks: []HookKind{HookPreRequest, HookPostResponse}}, false)
	if got := r.Snapshot(); len(got) != 1 || got[0].Stats[HookPreRequest].Calls != 0 {
		t.Fatalf("empty snapshot = %+v", got)
	}
	r.PreRequest(context.Background(), testReq())
	r.PostResponse(context.Background(), testReq(), &provider.ChatResponse{Model: "m"})
	snap := r.Snapshot()
	if snap[0].Stats[HookPreRequest].Calls != 1 || snap[0].Stats[HookPostResponse].Calls != 1 {
		t.Fatalf("snapshot = %+v, want 1 call each", snap[0].Stats)
	}
	if snap[0].Name != "a" || snap[0].Builtin {
		t.Fatalf("snapshot identity = %+v", snap[0])
	}
}

func TestLoadBuiltinsUnknownAndOrder(t *testing.T) {
	r := New()
	if err := LoadBuiltins(r, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want unknown name named", err)
	}
	r2 := New()
	if err := LoadBuiltins(r2, []string{ResponseLogName, HeaderAddName}); err != nil {
		t.Fatal(err)
	}
	snap := r2.Snapshot()
	if len(snap) != 2 || snap[0].Name != ResponseLogName || snap[1].Name != HeaderAddName {
		t.Fatalf("order = %v, want config order kept", snap)
	}
	if !snap[0].Builtin || !snap[1].Builtin {
		t.Fatal("built-ins not flagged builtin")
	}
	if err := LoadBuiltins(r2, []string{HeaderAddName}); err == nil {
		t.Fatal("duplicate built-in load accepted")
	}
}

func TestBuiltinNames(t *testing.T) {
	if !IsBuiltinName(HeaderAddName) || !IsBuiltinName(ResponseLogName) || IsBuiltinName("nope") {
		t.Fatal("IsBuiltinName wrong")
	}
}
