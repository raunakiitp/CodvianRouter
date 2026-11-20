package plugins

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ---------------------------------------------------------------------------
// Built-in plugins (CHECKLIST 201): proof the hook points work end
// to end, shipped in the binary so the system is demonstrable
// without any operator-written plugin. Both are deliberately small
// and boring: a header adder and a response logger. They are the
// reference implementations a future plugin author copies.
// ---------------------------------------------------------------------------

// HeaderAddName is the built-in header-adding request plugin's name.
const HeaderAddName = "header-add"

// ResponseLogName is the built-in response-logging plugin's name.
const ResponseLogName = "response-log"

// headerAddPlugin is the built-in pre-request plugin: it adds one
// fixed outbound header to every upstream request. The header is
// proof a pre-request transform reaches the provider: a mock
// upstream can assert it arrived. The value is fixed at
// construction (operator-configured via NewHeaderAddPlugin), never
// derived from prompt content — a header must never carry prompt
// text.
type headerAddPlugin struct {
	header string
	value  string
}

// NewHeaderAddPlugin builds the header-add plugin with one header
// name/value pair. Empty header or value is a construction error:
// a plugin that adds nothing is a misconfiguration, not a plugin.
func NewHeaderAddPlugin(header, value string) (Plugin, error) {
	if header == "" {
		return nil, fmt.Errorf("plugins: header-add needs a header name")
	}
	if value == "" {
		return nil, fmt.Errorf("plugins: header-add needs a header value")
	}
	return &headerAddPlugin{header: header, value: value}, nil
}

func (p *headerAddPlugin) Name() string { return HeaderAddName }
func (p *headerAddPlugin) Description() string {
	return "Adds a fixed header (" + p.header + ") to every upstream provider request"
}
func (p *headerAddPlugin) Hooks() []HookKind { return []HookKind{HookPreRequest} }

// PreRequest adds the header to the request's outbound header set.
// It never denies and never errors: adding a header cannot fail.
func (p *headerAddPlugin) PreRequest(_ context.Context, req *Request) (bool, string, error) {
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	req.Headers[p.header] = p.value
	return false, "", nil
}

// ---------------------------------------------------------------------------
// Response-log plugin
// ---------------------------------------------------------------------------

// maxRecentResponses is how many response observations the
// response-log plugin keeps: shapes only, never content, so the
// ring is bounded and cannot grow into a log of prompts.
const maxRecentResponses = 20

// responseLogPlugin is the built-in post-response plugin: it records
// one observation per delivered response — model, finish reason,
// content length, token counts — and keeps the most recent 20 for
// the dashboard. It never stores prompt or response content, and
// it never fails: PostResponse always returns nil, because a
// logging plugin must never fail a response open or closed.
type responseLogPlugin struct {
	mu     sync.Mutex
	recent []ResponseEvent
}

// NewResponseLogPlugin builds the response-log plugin.
func NewResponseLogPlugin() Plugin { return &responseLogPlugin{} }

func (p *responseLogPlugin) Name() string { return ResponseLogName }
func (p *responseLogPlugin) Description() string {
	return "Records model, finish reason, content length and token counts for each delivered response (never content)"
}
func (p *responseLogPlugin) Hooks() []HookKind { return []HookKind{HookPostResponse} }

// PostResponse records the response shape. A nil response is
// ignored (defensive: the server only calls it with a real
// response, but a nil must never panic a logging hook).
func (p *responseLogPlugin) PostResponse(_ context.Context, req *Request, resp *provider.ChatResponse) error {
	if resp == nil {
		return nil
	}
	ev := ResponseEvent{
		Time:         time.Now(),
		Model:        resp.Model,
		FinishReason: resp.FinishReason,
		ContentChars: len(resp.Content),
	}
	ev.PromptTokens = resp.Usage.PromptTokens
	ev.CompletionTokens = resp.Usage.CompletionTokens
	if ev.Model == "" && req != nil && req.Chat != nil {
		ev.Model = req.Chat.Model
	}
	if ev.FinishReason == "" {
		ev.FinishReason = "stop"
	}
	p.mu.Lock()
	p.recent = append(p.recent, ev)
	if len(p.recent) > maxRecentResponses {
		p.recent = append([]ResponseEvent(nil), p.recent[len(p.recent)-maxRecentResponses:]...)
	}
	p.mu.Unlock()
	return nil
}

// RecentResponses returns the kept observations, newest last.
func (p *responseLogPlugin) RecentResponses() []ResponseEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ResponseEvent, len(p.recent))
	copy(out, p.recent)
	return out
}

// ---------------------------------------------------------------------------
// Built-in registry
// ---------------------------------------------------------------------------

// BuiltinFactory builds one built-in plugin by name. The header-add
// plugin's header/value are fixed here (X-Codvian-Plugin:
// header-add): an operator who wants a different header writes
// their own plugin — config does not become a way to smuggle
// arbitrary headers into upstream requests from a file edit.
type BuiltinFactory func() (Plugin, error)

// Builtins maps built-in plugin names to their factories. It is the
// single source of truth for config validation: a name in
// `plugins.enabled` that is not in this map fails config load with
// the name in the error.
//
// The cost-cap factory builds the plugin UNLIMITED (cap 0, no price
// lookup): the bare factory cannot know the operator's cap or
// prices, and an unlimited cost-cap counts its calls and never
// denies, so it is safe as a default. The configured build — cap
// from the config file, prices from the operator's price book —
// comes through LoadBuiltinsConfigured, which main calls with the
// real values; a positive cap with no lookup is a startup error
// there, not a silent pass-through.
func Builtins() map[string]BuiltinFactory {
	return map[string]BuiltinFactory{
		HeaderAddName: func() (Plugin, error) {
			return NewHeaderAddPlugin("X-Codvian-Plugin", "header-add")
		},
		ResponseLogName: func() (Plugin, error) {
			return NewResponseLogPlugin(), nil
		},
		CostCapName: func() (Plugin, error) {
			return NewCostCapPlugin(0, nil)
		},
	}
}

// IsBuiltinName reports whether name is a built-in plugin name.
func IsBuiltinName(name string) bool {
	_, ok := Builtins()[name]
	return ok
}

// LoadBuiltins registers the named built-ins, in order, into r.
// An unknown name is an error naming it; a duplicate name is an
// error from Register. Order matters: it is the hook execution
// order, so the config list is the pipeline order.
func LoadBuiltins(r *Registry, names []string) error {
	return LoadBuiltinsConfigured(r, names, BuiltinOptions{})
}

// BuiltinOptions carries the operator configuration that the bare
// factories cannot know: the cost-cap plugin's per-request cap in
// USD (0 = unlimited) and its model-level price lookup. A positive
// cap with a nil lookup fails the load: every request would fail
// closed as unpriced, so the misconfiguration must fail at
// startup.
type BuiltinOptions struct {
	CostCapUSD    float64
	CostCapPrices PriceLookup
}

// LoadBuiltinsConfigured is LoadBuiltins with operator options: the
// cost-cap entry is built with the configured cap and price lookup
// instead of the unlimited default. Every other name uses its bare
// factory.
func LoadBuiltinsConfigured(r *Registry, names []string, opts BuiltinOptions) error {
	factories := Builtins()
	for _, name := range names {
		f, ok := factories[name]
		if !ok {
			return fmt.Errorf("plugins: unknown built-in plugin %q (known: header-add, response-log, cost-cap)", name)
		}
		var p Plugin
		var err error
		if name == CostCapName {
			p, err = NewCostCapPlugin(opts.CostCapUSD, opts.CostCapPrices)
		} else {
			p, err = f()
		}
		if err != nil {
			return fmt.Errorf("plugins: building %q: %w", name, err)
		}
		if err := r.Register(p, true); err != nil {
			return err
		}
	}
	return nil
}
