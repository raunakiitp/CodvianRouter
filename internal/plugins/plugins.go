// Package plugins is the safe, in-process plugin system for Codvian
// Router (CHECKLIST 201-202; competitor-sweep P2 gap — LiteLLM /
// Portkey / Kong plugin hooks for guardrails, transforms, and
// provider adapters).
//
// Design — what it is and what it deliberately is not:
//
//   - Plugins are Go-interface implementations registered IN-PROCESS
//     at startup from the config file (`plugins.enabled` names
//     built-in plugins, in order). There is NO dynamic loading:
//     no `.so`, no Wasm, no JS file, no plugin binary path in the
//     config. Loading arbitrary code from config would be arbitrary
//     code execution by anyone who can write the config file, and
//     the gateway's threat model treats the config file as
//     operator input, not as a code-signing boundary. A plugin
//     ships only as compiled-in Go code reviewed with the tree, so
//     `POST /admin/plugin/load` stays 501 by design. Hot-loadable
//     Wasm plugins (Higress-style, research-log entry 137) are a
//     future step, not this one.
//   - Hook points (three, matching the Respan "one trait, four
//     hooks" shape, research-log entry 144, and the Bifrost
//     separately-versioned plugin split, entry 94):
//     HookPreRequest — runs after the built-in guardrails and
//     before any provider is dialled; may allow, deny, or
//     transform the request (add headers, rewrite the model).
//     HookPostResponse — runs on a delivered provider response
//     (non-streaming before delivery, streaming after the stream
//     completes); logging / metrics / transform only.
//     HookProviderAdapter — rewrites one outgoing upstream HTTP
//     request for a named provider (custom wire for a new
//     provider); applied by the provider client.
//   - Failure semantics, per hook (documented, tested):
//     pre-request error  -> FAIL CLOSED: the request is refused
//     (403, policy-refusal -32001, reason "plugin_denied") and
//     never reaches a provider. A guardrail that cannot decide
//     must not let the request through.
//     pre-request panic  -> FAIL CLOSED as a gateway panic: 502
//     with JSON-RPC code -32603 (the CHECKLIST 165-166 panic
//     rule — the request fails honestly, the gateway survives).
//     post-response error or panic -> FAIL OPEN: the response is
//     still delivered; the failure is logged (redacted) and
//     counted. A logging plugin must never take down a response.
//     adapter error or panic -> FAIL CLOSED for that attempt:
//     no request is sent on the unadapted wire; the error
//     surfaces as an upstream failure for that attempt, so the
//     engine's normal failover may still try the next provider.
//   - Panic isolation: every plugin call runs behind its own
//     recover boundary (one plugin's panic never touches another
//     plugin, the engine, or the gateway process). A plugin that
//     panics is counted, not disabled: the next request calls it
//     again, so a transient panic does not silently change the
//     pipeline, and the counts make a flapping plugin visible.
//   - Ordering: pre-request and post-response hooks run in
//     registration order (config order for built-ins). Adapter
//     hooks are looked up by provider ID; at most one adapter
//     per provider ID, and registering a second is an error.
//   - No sample/fake data: the registry reports only real counts
//     from real calls. An empty registry reports zero plugins,
//     and the dashboard says so.
//
// This package is a leaf except for its use of provider chat
// types: it imports internal/provider only (no internal imports
// that import plugins back — the provider package never imports
// this one; the adapter is applied through a plain func type,
// provider.HTTPAdapterFunc, set by main).
package plugins

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// HookKind names a hook point in the request/response pipeline.
type HookKind string

// The three hook points (see the package doc).
const (
	HookPreRequest      HookKind = "pre_request"
	HookPostResponse    HookKind = "post_response"
	HookProviderAdapter HookKind = "provider_adapter"
)

// Request is what a pre-request plugin sees and may mutate. Chat is
// the live request: mutating its fields (model, messages) changes
// what is sent upstream, so a transform plugin rewrites in place.
// Headers are outbound HTTP headers added to the upstream request
// (never auth headers — see apply rules in provider.newRequest);
// they travel to the provider client through the request context,
// not through ChatRequest, so no client-visible field is invented.
type Request struct {
	Chat    *provider.ChatRequest
	Headers map[string]string
}

// ResponseEvent is one post-response observation kept by the
// response-log built-in (and any plugin that reports recent
// events): counts and shapes only — never prompt or response
// content.
type ResponseEvent struct {
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	FinishReason     string    `json:"finish_reason"`
	ContentChars     int       `json:"content_chars"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
}

// Plugin is the base interface every plugin implements. Name must
// be unique in a registry; Description is operator-facing text for
// the dashboard. Hooks lists the hook points the plugin serves —
// a plugin that lists a hook must implement that hook's interface,
// or Register rejects it.
type Plugin interface {
	Name() string
	Description() string
	Hooks() []HookKind
}

// PreRequestPlugin serves HookPreRequest. It returns (deny, reason,
// err): deny=true refuses the request with the plugin's reason;
// err != nil fails closed (refused, reason "plugin_denied"); nil,
// nil lets the request continue, with any in-place mutation of
// req applied.
type PreRequestPlugin interface {
	Plugin
	PreRequest(ctx context.Context, req *Request) (deny bool, reason string, err error)
}

// RefusalDetails is optionally implemented by a pre-request plugin
// whose denial carries a client-safe refusal of its own: an HTTP
// status and a message naming only numbers and model/provider
// names — never prompt or response content. The registry surfaces
// it on PreRequestOutcome; the server writes it verbatim instead
// of the generic 403 plugin_denied refusal. A plugin that does not
// implement this gets the generic refusal. The cost-cap plugin
// implements it (402 naming the estimated cost and the cap).
type RefusalDetails interface {
	RefusalStatus() int
	RefusalMessage(reason string) string
}

// PostResponsePlugin serves HookPostResponse. Its error is fail-open
// (logged + counted, response still delivered), so implementations
// should only return an error for a real failure of their own
// bookkeeping, never to veto a response.
type PostResponsePlugin interface {
	Plugin
	PostResponse(ctx context.Context, req *Request, resp *provider.ChatResponse) error
}

// ProviderAdapterPlugin serves HookProviderAdapter for exactly one
// provider ID (ProviderID). AdaptHTTPRequest rewrites the outgoing
// upstream HTTP request (URL, headers, body) for that provider's
// custom wire. It must key off req.URL.Path: the same client builds
// chat, stream, models and probe requests through one constructor.
// An error fails the attempt closed (no request on the unadapted
// wire); a panic is recovered the same way.
type ProviderAdapterPlugin interface {
	Plugin
	ProviderID() string
	AdaptHTTPRequest(ctx context.Context, req *http.Request) (*http.Request, error)
}

// RecentResponder is optionally implemented by plugins that keep a
// recent-events list for the dashboard (the response-log built-in).
type RecentResponder interface {
	RecentResponses() []ResponseEvent
}

// CostCapper is optionally implemented by a plugin that enforces a
// per-request cost cap (the cost-cap built-in): CostCapUSD is the
// cap in USD, 0 = unlimited. The server feed reads it through
// Registry.CostCap so the dashboard can say which cap the
// unpriced-model list applies to, without parsing the cap out of
// the plugin's description string.
type CostCapper interface {
	CostCapUSD() float64
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// hookStats counts one plugin's calls on one hook point, honestly:
// calls that ran, errors returned, denials issued (pre-request
// only), panics recovered.
type hookStats struct {
	Calls   uint64 `json:"calls"`
	Errors  uint64 `json:"errors"`
	Denials uint64 `json:"denials"`
	Panics  uint64 `json:"panics"`
}

// PluginStats is one plugin's dashboard row: identity, hooks, and
// per-hook counts, plus recent response events when the plugin
// reports them.
type PluginStats struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Builtin     bool                   `json:"builtin"`
	Hooks       []HookKind             `json:"hooks"`
	Stats       map[HookKind]hookStats `json:"stats"`
	Recent      []ResponseEvent        `json:"recent,omitempty"`
	AdapterFor  string                 `json:"adapter_for,omitempty"`
}

type registered struct {
	plugin  Plugin
	builtin bool
	stats   map[HookKind]*hookStats
}

// Registry holds the plugins for one gateway process, in
// registration order. It is safe for concurrent use: hook calls
// run concurrently across requests, and stats update under a mutex.
type Registry struct {
	mu       sync.Mutex
	plugins  []*registered
	byName   map[string]*registered
	adapters map[string]*registered
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{byName: map[string]*registered{}, adapters: map[string]*registered{}}
}

// Register adds a plugin. It fails on a duplicate name, on a plugin
// with no hooks, on a listed hook the plugin does not implement,
// and on a second adapter for the same provider ID. Registration
// happens at startup only; there is no unregister (a running
// gateway's pipeline does not change shape mid-flight).
func (r *Registry) Register(p Plugin, builtin bool) error {
	if p == nil {
		return fmt.Errorf("plugins: cannot register a nil plugin")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("plugins: plugin name must not be empty")
	}
	hooks := p.Hooks()
	if len(hooks) == 0 {
		return fmt.Errorf("plugins: plugin %q lists no hooks", name)
	}
	for _, h := range hooks {
		switch h {
		case HookPreRequest:
			if _, ok := p.(PreRequestPlugin); !ok {
				return fmt.Errorf("plugins: plugin %q lists hook %q but does not implement PreRequest", name, h)
			}
		case HookPostResponse:
			if _, ok := p.(PostResponsePlugin); !ok {
				return fmt.Errorf("plugins: plugin %q lists hook %q but does not implement PostResponse", name, h)
			}
		case HookProviderAdapter:
			ap, ok := p.(ProviderAdapterPlugin)
			if !ok {
				return fmt.Errorf("plugins: plugin %q lists hook %q but does not implement AdaptHTTPRequest", name, h)
			}
			if ap.ProviderID() == "" {
				return fmt.Errorf("plugins: adapter plugin %q names no provider", name)
			}
		default:
			return fmt.Errorf("plugins: plugin %q lists unknown hook %q", name, h)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; ok {
		return fmt.Errorf("plugins: duplicate plugin name %q", name)
	}
	reg := &registered{plugin: p, builtin: builtin, stats: map[HookKind]*hookStats{}}
	for _, h := range hooks {
		reg.stats[h] = &hookStats{}
	}
	if ap, ok := p.(ProviderAdapterPlugin); ok {
		if _, taken := r.adapters[ap.ProviderID()]; taken {
			return fmt.Errorf("plugins: provider %q already has an adapter plugin", ap.ProviderID())
		}
		r.adapters[ap.ProviderID()] = reg
	}
	r.plugins = append(r.plugins, reg)
	r.byName[name] = reg
	return nil
}

// Len reports how many plugins are registered.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.plugins)
}

// CostCap reports the cost-cap plugin's per-request cap in USD and
// whether the plugin is registered: (0, true) means registered
// with no cap (unlimited — it counts calls and never denies);
// (0, false) means the plugin is not registered at all. Only the
// first registered cost-cap plugin counts — Register rejects a
// duplicate name, so there is at most one.
func (r *Registry) CostCap() (float64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, reg := range r.plugins {
		if cc, ok := reg.plugin.(CostCapper); ok {
			return cc.CostCapUSD(), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Pre-request
// ---------------------------------------------------------------------------

// PreRequestOutcome is the result of running the pre-request hooks
// in registration order. Exactly one of Denied / Err / Panic is set
// on a refusal; all three empty means allow. Status / Detail are
// set only on a denial by a plugin that implements RefusalDetails:
// the HTTP status and client-safe message the server writes
// instead of the generic 403 plugin_denied refusal.
type PreRequestOutcome struct {
	Denied bool   // a plugin denied the request (fail closed, 403)
	Reason string // the denying plugin's reason (Denied) or "plugin_denied" (Err)
	Plugin string // the plugin that denied, errored, or panicked
	Err    error  // a plugin returned an error (fail closed, 403)
	Panic  any    // a plugin panicked (fail closed, 502 per the 165-166 rule)
	Status int    // RefusalDetails status for this denial (0 = generic 403)
	Detail string // RefusalDetails message for this denial
}

// PreRequest runs every registered pre-request plugin in order on
// req. The first denial, error, or panic stops the chain — a later
// plugin never sees a request an earlier one refused — and the
// outcome names the plugin responsible. A plugin that panics is
// isolated per call (see callPreRequest); the gateway survives.
func (r *Registry) PreRequest(ctx context.Context, req *Request) PreRequestOutcome {
	r.mu.Lock()
	chain := make([]*registered, 0, len(r.plugins))
	for _, reg := range r.plugins {
		if _, ok := reg.plugin.(PreRequestPlugin); ok {
			chain = append(chain, reg)
		}
	}
	r.mu.Unlock()
	for _, reg := range chain {
		deny, reason, err, panicked := r.callPreRequest(ctx, reg, req)
		if panicked != nil {
			return PreRequestOutcome{Plugin: reg.plugin.Name(), Panic: panicked}
		}
		if err != nil {
			return PreRequestOutcome{Plugin: reg.plugin.Name(), Err: err, Reason: "plugin_denied"}
		}
		if deny {
			if reason == "" {
				reason = "plugin_denied"
			}
			out := PreRequestOutcome{Denied: true, Reason: reason, Plugin: reg.plugin.Name()}
			if rd, ok := reg.plugin.(RefusalDetails); ok {
				out.Status = rd.RefusalStatus()
				out.Detail = rd.RefusalMessage(reason)
			}
			return out
		}
	}
	return PreRequestOutcome{}
}

// callPreRequest invokes one pre-request plugin behind a recover
// boundary and counts the call. A panic is recovered here, counted,
// and reported as panicked — it never propagates to the caller.
func (r *Registry) callPreRequest(ctx context.Context, reg *registered, req *Request) (deny bool, reason string, err error, panicked any) {
	p := reg.plugin.(PreRequestPlugin)
	defer func() {
		if v := recover(); v != nil {
			deny, reason, err = false, "", nil
			panicked = v
			r.bump(reg, HookPreRequest, func(s *hookStats) { s.Panics++ })
		}
	}()
	r.bump(reg, HookPreRequest, func(s *hookStats) { s.Calls++ })
	deny, reason, err = p.PreRequest(ctx, req)
	if err != nil {
		r.bump(reg, HookPreRequest, func(s *hookStats) { s.Errors++ })
	}
	if deny {
		r.bump(reg, HookPreRequest, func(s *hookStats) { s.Denials++ })
	}
	return deny, reason, err, nil
}

// ---------------------------------------------------------------------------
// Post-response
// ---------------------------------------------------------------------------

// PostResponse runs every registered post-response plugin in order
// on the delivered response. Errors and panics are fail-open: each
// is logged by the caller (the registry returns them) and counted,
// and the response is still delivered. The returned slice holds one
// entry per failed plugin call (empty on full success).
func (r *Registry) PostResponse(ctx context.Context, req *Request, resp *provider.ChatResponse) []error {
	r.mu.Lock()
	chain := make([]*registered, 0, len(r.plugins))
	for _, reg := range r.plugins {
		if _, ok := reg.plugin.(PostResponsePlugin); ok {
			chain = append(chain, reg)
		}
	}
	r.mu.Unlock()
	var failures []error
	for _, reg := range chain {
		if err := r.callPostResponse(ctx, reg, req, resp); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

// callPostResponse invokes one post-response plugin behind a recover
// boundary. A panic is recovered, counted, and returned as an error
// so the caller can log it; it never propagates.
func (r *Registry) callPostResponse(ctx context.Context, reg *registered, req *Request, resp *provider.ChatResponse) (err error) {
	p := reg.plugin.(PostResponsePlugin)
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("plugins: plugin %q panicked in post-response hook: %v", reg.plugin.Name(), v)
			r.bump(reg, HookPostResponse, func(s *hookStats) { s.Panics++ })
		}
	}()
	r.bump(reg, HookPostResponse, func(s *hookStats) { s.Calls++ })
	if err = p.PostResponse(ctx, req, resp); err != nil {
		r.bump(reg, HookPostResponse, func(s *hookStats) { s.Errors++ })
		err = fmt.Errorf("plugins: plugin %q post-response hook failed: %w", reg.plugin.Name(), err)
	}
	return err
}

// ---------------------------------------------------------------------------
// Provider adapter
// ---------------------------------------------------------------------------

// AdaptHTTP rewrites one outgoing upstream HTTP request through the
// adapter registered for providerID, or returns req unchanged when
// no adapter is registered for that provider. An adapter error or
// panic fails the attempt closed: the error is returned and the
// caller must not send the unadapted request.
func (r *Registry) AdaptHTTP(ctx context.Context, providerID string, req *http.Request) (*http.Request, error) {
	r.mu.Lock()
	reg, ok := r.adapters[providerID]
	r.mu.Unlock()
	if !ok {
		return req, nil
	}
	out, err, panicked := r.callAdapter(ctx, reg, req)
	if panicked != nil {
		return nil, fmt.Errorf("plugins: adapter plugin %q panicked: %v", reg.plugin.Name(), panicked)
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: adapter plugin %q failed: %w", reg.plugin.Name(), err)
	}
	if out == nil {
		return nil, fmt.Errorf("plugins: adapter plugin %q returned a nil request", reg.plugin.Name())
	}
	return out, nil
}

// callAdapter invokes one adapter plugin behind a recover boundary.
func (r *Registry) callAdapter(ctx context.Context, reg *registered, req *http.Request) (out *http.Request, err error, panicked any) {
	p := reg.plugin.(ProviderAdapterPlugin)
	defer func() {
		if v := recover(); v != nil {
			out, err = nil, nil
			panicked = v
			r.bump(reg, HookProviderAdapter, func(s *hookStats) { s.Panics++ })
		}
	}()
	r.bump(reg, HookProviderAdapter, func(s *hookStats) { s.Calls++ })
	out, err = p.AdaptHTTPRequest(ctx, req)
	if err != nil {
		r.bump(reg, HookProviderAdapter, func(s *hookStats) { s.Errors++ })
	}
	return out, err, nil
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// bump updates one hook's counters for one plugin.
func (r *Registry) bump(reg *registered, h HookKind, f func(*hookStats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := reg.stats[h]; ok {
		f(s)
	}
}

// Snapshot returns the live per-plugin stats in registration order:
// identity, hooks, per-hook counts, and recent response events when
// the plugin reports them. All numbers are real counts from real
// calls since this process started — nothing is estimated.
func (r *Registry) Snapshot() []PluginStats {
	r.mu.Lock()
	regs := make([]*registered, len(r.plugins))
	copy(regs, r.plugins)
	r.mu.Unlock()
	out := make([]PluginStats, 0, len(regs))
	for _, reg := range regs {
		hooks := make([]HookKind, 0, len(reg.stats))
		stats := make(map[HookKind]hookStats, len(reg.stats))
		r.mu.Lock()
		for h, s := range reg.stats {
			hooks = append(hooks, h)
			stats[h] = *s
		}
		r.mu.Unlock()
		sort.Slice(hooks, func(i, j int) bool { return hooks[i] < hooks[j] })
		ps := PluginStats{
			Name:        reg.plugin.Name(),
			Description: reg.plugin.Description(),
			Builtin:     reg.builtin,
			Hooks:       hooks,
			Stats:       stats,
		}
		if ap, ok := reg.plugin.(ProviderAdapterPlugin); ok {
			ps.AdapterFor = ap.ProviderID()
		}
		if rr, ok := reg.plugin.(RecentResponder); ok {
			ps.Recent = rr.RecentResponses()
		}
		out = append(out, ps)
	}
	return out
}
