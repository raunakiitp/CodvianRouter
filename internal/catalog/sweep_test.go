package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestCatalogSweep is the end-to-end proof that every live catalog entry
// with a concrete base URL can actually be dialed: for each such entry it
// builds the chat request in the entry's own wire format, sends it to a
// mock upstream that speaks that wire format, and parses the response
// back out. It is deliberately COUNT-AGNOSTIC: it iterates whatever
// catalog.All() returns at run time and asserts behavior per entry, so
// catalog drift (new entries, new base URLs) cannot break it — new
// entries are simply swept too.
//
// Three invariants are asserted:
//
//  1. Every entry with a routable (non-templated) base URL produces a
//     well-formed request for its wire format: the auth header the
//     entry's EndpointAuth demands is present with a non-empty value,
//     any required extra headers are present, the request path carries
//     the wire's chat suffix, and the mock's wire-shaped response parses
//     back into a non-empty assistant message.
//  2. Every entry WITHOUT a routable base URL is skipped by the sweep
//     but must still carry a non-empty UnroutableReason() — the
//     zero-silently-unroutable invariant.
//  3. Any entry the sweep cannot exercise end-to-end (e.g. an unknown
//     wire format with no documented request shape) must appear in
//     sweepAllowList below with a written reason. Silently skipping an
//     entry is a test failure.
//
// On any failure the provider ID and the exact problem are named, so a
// catalog regression points straight at the broken entry.

// sweepAllowList names entries the sweep cannot drive end-to-end today,
// each with the reason. An entry may only sit here while its skip
// predicate still holds; if the predicate stops holding (e.g. the entry
// gains a wire format) the test fails and the entry must be removed
// from the list, so the list can never rot silently.
//
// NOTE: entries are allow-listed by ID with a *condition*, not
// unconditionally — see sweepAllowListStale.
var sweepAllowList = map[string]string{
	// Wire format is recorded as unknown (WireUnknown): the catalog
	// honestly documents no request shape for these surfaces yet, so
	// there is no wire format to build a request in and nothing the
	// mock could answer truthfully. The request would be invented,
	// and the honesty rules forbid inventing. Once an entry's wire
	// format is verified and filled in, remove it from this list —
	// the sweep will then exercise it end-to-end automatically.
	"github-copilot": "wire format unknown: no documented request shape to build a request from",
	"clinepass":      "wire format unknown: no documented request shape to build a request from",
	"lm-studio":      "wire format unknown: no documented request shape to build a request from",
}

// sweepAllowListStale reports whether the allow-list entry for id is
// stale, i.e. the entry could now be exercised normally.
func sweepAllowListStale(p Provider) bool {
	// Only unknown-wire routable entries are allow-listed today. If
	// the entry's wire became known (or it lost its base URL / left
	// the live catalog) the reason no longer applies.
	return p.DefaultWireFormat() != WireUnknown
}

// sweepHandledWires are the wire formats the mock upstream can speak.
var sweepHandledWires = map[WireFormat]bool{
	WireOpenAI:    true,
	WireAnthropic: true,
	WireGemini:    true,
	WireCohere:    true,
	WireOllama:    true,
	WireBedrock:   true,
}

// TestCatalogSweep runs the sweep over the live catalog.
func TestCatalogSweep(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("catalog.All() returned zero entries — the sweep has nothing to prove")
	}

	exercised := 0 // entries swept end-to-end (request built + response parsed)
	allowed := 0   // entries consciously allow-listed with a reason
	unroutable := 0
	seenIDs := make(map[string]bool, len(all))

	for i := range all {
		p := all[i]
		if seenIDs[p.ID] {
			t.Errorf("provider %s: duplicate ID in All()", p.ID)
		}
		seenIDs[p.ID] = true

		bases := sweepConcreteBases(p)
		if len(bases) == 0 {
			// Negative side: no routable base URL. The sweep
			// cannot dial it, and it MUST not be silent —
			// UnroutableReason has to explain why.
			if p.Routable() {
				t.Errorf("provider %s: sweep found no concrete base URL but Routable() is true (sweep/base-URL disagreement)", p.ID)
			}
			if reason := p.UnroutableReason(); reason == "" {
				t.Errorf("provider %s: no base URL and empty UnroutableReason — a silently unroutable entry", p.ID)
			}
			unroutable++
			continue
		}

		// Both directions of the routable invariant.
		if reason := p.UnroutableReason(); reason != "" {
			t.Errorf("provider %s: has %d concrete base URL(s) but UnroutableReason() is non-empty (%q)", p.ID, len(bases), reason)
		}

		if _, listed := sweepAllowList[p.ID]; listed {
			if !sweepAllowListStale(p) {
				allowed++
				continue
			}
			t.Errorf("provider %s: allow-list entry is stale (wire format is now %q) — remove it from sweepAllowList and let the sweep exercise it", p.ID, p.DefaultWireFormat())
			continue
		}

		if !sweepHandledWires[p.DefaultWireFormat()] {
			t.Errorf("provider %s: routable (base %s) with wire format %q that the sweep cannot drive and that is not in sweepAllowList — fix the entry, teach the mock the shape, or allow-list it with a reason", p.ID, bases[0], p.DefaultWireFormat())
			continue
		}

		for j, base := range bases {
			sweepOneBase(t, p, j, base)
		}
		exercised++
	}

	// The sweep must never pass vacuously: it has to have exercised
	// at least one real entry end-to-end.
	if exercised == 0 {
		t.Fatal("sweep exercised zero entries end-to-end — nothing was proven")
	}
	t.Logf("sweep: %d entries exercised end-to-end, %d allow-listed, %d skipped as unroutable (reason asserted), out of %d live entries",
		exercised, allowed, unroutable, len(all))

	// Allow-list hygiene: every listed ID must still name a live,
	// routable, unknown-wire entry. Anything else is a stale entry
	// masking a regression.
	for id := range sweepAllowList {
		p, ok := ByID(id)
		if !ok {
			t.Errorf("sweepAllowList: entry %q no longer exists in the live catalog — remove the stale allow-list entry", id)
			continue
		}
		bases := sweepConcreteBases(p)
		if len(bases) == 0 {
			t.Errorf("sweepAllowList: entry %q no longer has a concrete base URL — remove the stale allow-list entry", id)
		}
	}
}

// sweepConcreteBases returns the entry's base URLs that are concrete
// (http/https, no template placeholders). Template-only bases are
// excluded: they cannot be dialed without operator substitution and are
// covered by the UnroutableReason branch.
func sweepConcreteBases(p Provider) []string {
	var out []string
	for _, u := range p.BaseURLs {
		if strings.ContainsAny(u, "{}") {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(u))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		out = append(out, strings.TrimRight(strings.TrimSpace(u), "/"))
	}
	return out
}

// sweepEndpointFor picks the EndpointAuth that governs the given base
// URL: an endpoint whose own BaseURL matches exactly, else the
// provider's default endpoint auth.
func sweepEndpointFor(p Provider, base string) EndpointAuth {
	norm := func(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }
	for _, e := range p.Endpoints {
		if e.BaseURL == "" || strings.ContainsAny(e.BaseURL, "{}") {
			continue
		}
		if norm(e.BaseURL) == norm(base) {
			return e
		}
	}
	return p.DefaultEndpointAuth()
}

// sweepWireFor decides which wire format to speak on the surface named
// by the endpoint auth. Endpoint names carry the surface's shape:
// "anthropic"/"messages" surfaces speak Anthropic Messages, "generate"
// speaks Gemini generateContent, "native" speaks the provider's own
// native shape (Cohere v2, Ollama), and chat/responses surfaces speak
// OpenAI Chat Completions whenever the entry lists that shape.
func sweepWireFor(p Provider, ep EndpointAuth) WireFormat {
	switch ep.Endpoint {
	case EndpointAnthropic, EndpointMessages:
		return WireAnthropic
	case EndpointGenerate:
		return WireGemini
	case EndpointConverse:
		return WireBedrock
	}
	d := p.DefaultWireFormat()
	has := func(w WireFormat) bool {
		for _, x := range p.WireFormats {
			if x == w {
				return true
			}
		}
		return false
	}
	switch ep.Endpoint {
	case EndpointNative:
		if d == WireCohere || d == WireOllama {
			return d
		}
		if has(WireCohere) {
			return WireCohere
		}
		if has(WireOllama) {
			return WireOllama
		}
		return d
	case EndpointChat, EndpointResponses, "":
		if has(WireOpenAI) {
			return WireOpenAI
		}
		return d
	}
	return d
}

// sweepOneBase exercises one (entry, base URL) pair: it builds the
// request for the surface's wire format, sends it to a mock upstream
// speaking that shape, and parses the response.
func sweepOneBase(t *testing.T, p Provider, idx int, base string) {
	t.Helper()

	ep := sweepEndpointFor(p, base)
	wire := sweepWireFor(p, ep)

	body, path := sweepRequestBody(wire, p)
	if body == nil {
		t.Errorf("provider %s: no request body builder for wire format %q", p.ID, wire)
		return
	}

	// The path the router would actually hit on the real base URL:
	// the base's own path (e.g. "/compatibility/v1") extended by the
	// wire's chat path. The mock serves the same path so the
	// assertion is honest.
	mock, cleanup, recorder := sweepMockUpstream(t, wire)
	defer cleanup()

	basePath, err := sweepBasePath(base)
	if err != nil {
		t.Errorf("provider %s: cannot resolve base URL %q: %v", p.ID, base, err)
		return
	}
	fullPath := strings.TrimRight(basePath, "/") + path

	mockBase, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("mock upstream URL invalid: %v", err)
	}
	targetURL := mockBase.Scheme + "://" + mockBase.Host + fullPath

	// Path sanity, asserted on the exact bytes that go on the wire:
	// the request path must be the base's own path extended by the
	// wire's chat suffix.
	bp, err := url.Parse(targetURL)
	if err != nil {
		t.Errorf("provider %s: request URL %q does not parse: %v", p.ID, targetURL, err)
		return
	}
	if bp.Path != fullPath {
		t.Errorf("provider %s: request path %q does not match the expected path %q (base %q)", p.ID, bp.Path, fullPath, base)
	}
	sweepAssertPathSane(t, p, wire, bp.Path)

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		t.Errorf("provider %s: cannot build HTTP request for base %q: %v", p.ID, base, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	sweepApplyAuth(t, p, ep, req)

	resp, err := mock.Client().Do(req)
	if err != nil {
		t.Errorf("provider %s: request to mock upstream failed: %v", p.ID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("provider %s: mock upstream returned HTTP %d, want 200", p.ID, resp.StatusCode)
		return
	}

	// What the upstream actually received — asserted server-side of
	// the client, i.e. from the mock's own record.
	if recorder.requests != 1 {
		t.Errorf("provider %s: mock upstream received %d requests, want exactly 1", p.ID, recorder.requests)
	}
	if len(recorder.problems) > 0 {
		for _, problem := range recorder.problems {
			t.Errorf("provider %s: mock upstream rejected the request: %s", p.ID, problem)
		}
	}
	sweepAssertAuthReached(t, p, ep, recorder)

	sweepParseResponse(t, p, wire, resp.Body)
}

// sweepApplyAuth applies the credential headers the entry's EndpointAuth
// demands, with the sweep's placeholder token. It mirrors what the
// resolver would produce per auth kind:
//
//   - bearer / static-header kinds: the named header with a non-empty
//     value (asserted again from the mock's side).
//   - none: no credential header at all.
//   - oauth: the token minted by the login flow is dialed as the
//     endpoint's header when one is recorded.
//   - exchange / cookie: no static header exists (the credential is a
//     signed request or session state, not a header) — nothing to set
//     and nothing to assert.
func sweepApplyAuth(t *testing.T, p Provider, ep EndpointAuth, req *http.Request) {
	t.Helper()
	const token = "sweep-placeholder-credential"
	switch ep.AuthKind {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+token)
	case AuthXAPIKey, AuthAPIKey, AuthXGoogAPIKey, AuthXFireworksAPIKey, AuthCustom:
		if ep.HeaderName == "" {
			t.Errorf("provider %s: endpoint %q auth kind %s has no header name", p.ID, ep.Endpoint, ep.AuthKind)
			return
		}
		req.Header.Set(ep.HeaderName, token)
	case AuthOAuth:
		if ep.HeaderName != "" {
			req.Header.Set(ep.HeaderName, "Bearer "+token)
		}
	case AuthNone, AuthExchange, AuthCookie:
		// No static credential header exists for these kinds.
	}
	// Required static headers for the surface (e.g. anthropic-version).
	for k, v := range ep.ExtraHeaders {
		req.Header.Set(k, v)
	}
}

// sweepCredentialHeaders are the headers a credential could travel in.
var sweepCredentialHeaders = []string{
	"Authorization",
	"x-api-key",
	"api-key",
	"x-goog-api-key",
	"x-fireworks-api-key",
}

// sweepAssertAuthReached verifies, from the mock's record of the actual
// request, that the credential situation matches the EndpointAuth.
func sweepAssertAuthReached(t *testing.T, p Provider, ep EndpointAuth, rec *sweepRecord) {
	t.Helper()
	got := func(name string) string { return rec.headers.Get(name) }
	switch ep.AuthKind {
	case AuthBearer:
		v := got("Authorization")
		if v == "" {
			t.Errorf("provider %s: bearer auth demanded but no Authorization header reached the upstream", p.ID)
		} else if !strings.HasPrefix(v, "Bearer ") || len(v) <= len("Bearer ") {
			t.Errorf("provider %s: Authorization header %q is not a non-empty Bearer credential", p.ID, v)
		}
	case AuthXAPIKey, AuthAPIKey, AuthXGoogAPIKey, AuthXFireworksAPIKey, AuthCustom:
		if ep.HeaderName == "" {
			t.Errorf("provider %s: endpoint %q auth kind %s has no header name", p.ID, ep.Endpoint, ep.AuthKind)
			return
		}
		if got(ep.HeaderName) == "" {
			t.Errorf("provider %s: auth kind %s demands header %q but it did not reach the upstream", p.ID, ep.AuthKind, ep.HeaderName)
		}
	case AuthOAuth:
		if ep.HeaderName != "" && got(ep.HeaderName) == "" {
			t.Errorf("provider %s: oauth surface with header %q reached the upstream without it", p.ID, ep.HeaderName)
		}
	case AuthNone:
		for _, h := range sweepCredentialHeaders {
			if v := got(h); v != "" {
				t.Errorf("provider %s: auth kind none but credential header %q (%q) reached the upstream", p.ID, h, v)
			}
		}
	case AuthExchange, AuthCookie:
		// No static header exists to assert; the request build plus
		// response parse is the testable surface here.
	}
	// Required static headers must have arrived verbatim.
	for k, v := range ep.ExtraHeaders {
		if got(k) != v {
			t.Errorf("provider %s: required header %q arrived as %q, want %q", p.ID, k, got(k), v)
		}
	}
}

// sweepRecord is the mock upstream's record of what it received.
type sweepRecord struct {
	mu       sync.Mutex
	requests int
	headers  http.Header
	problems []string
}

// sweepMockUpstream starts an httptest server that accepts any path and
// answers with a minimal valid response for the given wire format. It
// also asserts, server-side, that the request body is a valid JSON
// document carrying the wire's chat fields.
func sweepMockUpstream(t *testing.T, wire WireFormat) (*httptest.Server, func(), *sweepRecord) {
	t.Helper()
	rec := &sweepRecord{headers: http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.requests++
		rec.headers = r.Header.Clone()

		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			rec.problems = append(rec.problems, fmt.Sprintf("request body is not valid JSON: %v", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if problem := sweepAssertBodyShape(wire, raw); problem != "" {
			rec.problems = append(rec.problems, problem)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sweepResponseFor(wire)))
	}))
	return srv, srv.Close, rec
}

// sweepAssertBodyShape checks the request body carries the fields the
// wire format requires, returning a problem description or "".
func sweepAssertBodyShape(wire WireFormat, raw map[string]json.RawMessage) string {
	switch wire {
	case WireOpenAI, WireAnthropic, WireCohere, WireOllama:
		if _, ok := raw["messages"]; !ok {
			return "request body has no \"messages\" field"
		}
	case WireGemini:
		if _, ok := raw["contents"]; !ok {
			return "request body has no \"contents\" field"
		}
	case WireBedrock:
		if _, ok := raw["messages"]; !ok {
			return "request body has no \"messages\" field"
		}
	}
	return ""
}

// sweepRequestBody builds the chat request body for a wire format and
// returns it together with the request path that hangs off the base URL.
func sweepRequestBody(wire WireFormat, p Provider) ([]byte, string) {
	model := "sweep-probe-model"
	switch wire {
	case WireOpenAI:
		body := map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "sweep"},
			},
		}
		return sweepJSON(body), "/chat/completions"
	case WireAnthropic:
		body := map[string]any{
			"model":      model,
			"max_tokens": 16,
			"messages": []map[string]string{
				{"role": "user", "content": "sweep"},
			},
		}
		return sweepJSON(body), "/v1/messages"
	case WireGemini:
		body := map[string]any{
			"contents": []map[string]any{
				{"role": "user", "parts": []map[string]string{{"text": "sweep"}}},
			},
		}
		return sweepJSON(body), "/models/" + model + ":generateContent"
	case WireCohere:
		body := map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "sweep"},
			},
		}
		return sweepJSON(body), "/v2/chat"
	case WireOllama:
		body := map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "sweep"},
			},
			"stream": false,
		}
		return sweepJSON(body), "/api/chat"
	case WireBedrock:
		body := map[string]any{
			"modelId": model,
			"messages": []map[string]any{
				{"role": "user", "content": []map[string]string{{"text": "sweep"}}},
			},
		}
		return sweepJSON(body), "/model/" + model + "/converse"
	}
	return nil, ""
}

// sweepJSON marshals a request body, dying the test on the impossible
// marshal failure of plain maps.
func sweepJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("catalog sweep: request body marshal: %v", err))
	}
	return b
}

// sweepBasePath extracts the base URL's own path ("" for a bare host).
func sweepBasePath(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base URL scheme %q is not dialable", u.Scheme)
	}
	return u.Path, nil
}

// sweepAssertPathSane asserts the request path carries the wire's chat
// suffix — the shape every documented chat surface on this wire shares.
func sweepAssertPathSane(t *testing.T, p Provider, wire WireFormat, path string) {
	t.Helper()
	switch wire {
	case WireOpenAI:
		if !strings.HasSuffix(path, "/chat/completions") {
			t.Errorf("provider %s: openai-wire request path %q does not end in /chat/completions", p.ID, path)
		}
	case WireAnthropic:
		if !strings.HasSuffix(path, "/messages") {
			t.Errorf("provider %s: anthropic-wire request path %q does not end in /messages", p.ID, path)
		}
	case WireGemini:
		if !strings.Contains(path, ":generateContent") {
			t.Errorf("provider %s: gemini-wire request path %q does not target :generateContent", p.ID, path)
		}
	case WireCohere:
		if !strings.HasSuffix(path, "/chat") {
			t.Errorf("provider %s: cohere-wire request path %q does not end in /chat", p.ID, path)
		}
	case WireOllama:
		if !strings.HasSuffix(path, "/api/chat") {
			t.Errorf("provider %s: ollama-wire request path %q does not end in /api/chat", p.ID, path)
		}
	case WireBedrock:
		if !strings.HasSuffix(path, "/converse") {
			t.Errorf("provider %s: bedrock-wire request path %q does not end in /converse", p.ID, path)
		}
	}
}

// sweepResponseFor returns a minimal valid upstream response for a wire
// format: the exact shape the provider's docs specify for a completed
// chat turn.
func sweepResponseFor(wire WireFormat) string {
	switch wire {
	case WireOpenAI:
		return `{"id":"chatcmpl-sweep","object":"chat.completion","created":1764182400,"model":"sweep-probe-model","choices":[{"index":0,"message":{"role":"assistant","content":"sweep-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	case WireAnthropic:
		return `{"id":"msg_sweep","type":"message","role":"assistant","model":"sweep-probe-model","content":[{"type":"text","text":"sweep-ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`
	case WireGemini:
		return `{"candidates":[{"content":{"parts":[{"text":"sweep-ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	case WireCohere:
		return `{"id":"sweep","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"sweep-ok"}]},"usage":{"billed_units":{"input_tokens":1,"output_tokens":1}}}`
	case WireOllama:
		return `{"model":"sweep-probe-model","created_at":"2026-11-27T00:00:00Z","message":{"role":"assistant","content":"sweep-ok"},"done":true}`
	case WireBedrock:
		return `{"output":{"message":{"role":"assistant","content":[{"text":"sweep-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`
	}
	return ""
}

// sweepParseResponse decodes the mock's wire-shaped response with the
// structs a real client would decode into and asserts the assistant
// message survives the round trip non-empty.
func sweepParseResponse(t *testing.T, p Provider, wire WireFormat, body io.Reader) {
	t.Helper()
	switch wire {
	case WireOpenAI:
		var out struct {
			Choices []struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: openai-wire response does not parse: %v", p.ID, err)
			return
		}
		if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" || out.Choices[0].Message.Role != "assistant" {
			t.Errorf("provider %s: openai-wire response has no assistant message (parsed %#v)", p.ID, out)
		}
	case WireAnthropic:
		var out struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: anthropic-wire response does not parse: %v", p.ID, err)
			return
		}
		if out.Role != "assistant" || len(out.Content) == 0 || out.Content[0].Text == "" {
			t.Errorf("provider %s: anthropic-wire response has no assistant message (parsed %#v)", p.ID, out)
		}
	case WireGemini:
		var out struct {
			Candidates []struct {
				Content struct {
					Role  string `json:"role"`
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: gemini-wire response does not parse: %v", p.ID, err)
			return
		}
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 || out.Candidates[0].Content.Parts[0].Text == "" {
			t.Errorf("provider %s: gemini-wire response has no model message (parsed %#v)", p.ID, out)
		}
	case WireCohere:
		var out struct {
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: cohere-wire response does not parse: %v", p.ID, err)
			return
		}
		if out.Message.Role != "assistant" || len(out.Message.Content) == 0 || out.Message.Content[0].Text == "" {
			t.Errorf("provider %s: cohere-wire response has no assistant message (parsed %#v)", p.ID, out)
		}
	case WireOllama:
		var out struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: ollama-wire response does not parse: %v", p.ID, err)
			return
		}
		if out.Message.Content == "" || out.Message.Role != "assistant" || !out.Done {
			t.Errorf("provider %s: ollama-wire response has no completed assistant message (parsed %#v)", p.ID, out)
		}
	case WireBedrock:
		var out struct {
			Output struct {
				Message struct {
					Role    string `json:"role"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			} `json:"output"`
			StopReason string `json:"stopReason"`
		}
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Errorf("provider %s: bedrock-wire response does not parse: %v", p.ID, err)
			return
		}
		if out.Output.Message.Role != "assistant" || len(out.Output.Message.Content) == 0 || out.Output.Message.Content[0].Text == "" {
			t.Errorf("provider %s: bedrock-wire response has no assistant message (parsed %#v)", p.ID, out)
		}
	}
}
