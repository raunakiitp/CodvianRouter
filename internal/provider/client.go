package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ContentPart, ToolCall and ToolDefinition are the OpenAI shapes from
// internal/wire, aliased here so the gateway and the conversion layer
// share one definition (there is exactly one OpenAI wire shape).
// ToolCallFunction is aliased the same way: router tests build tool
// calls through the provider package.
type ContentPart = wire.ContentPart
type ToolCall = wire.ToolCall
type ToolCallFunction = wire.ToolCallFunction
type ToolDefinition = wire.ToolDefinition

// Message is a single chat message. Content is the plain-text form
// (concatenated text parts when the wire sent an array). Parts carries
// multi-part content (text + image_url) when the client sent it as an
// array; ToolCalls carries assistant tool calls, ToolCallID the id a
// "tool" message answers. Content decodes from either a JSON string
// or an array of parts, and encodes back the same way it arrived
// (array when Parts is set, string otherwise); see MarshalJSON /
// UnmarshalJSON below. Reasoning carries the assistant turn's
// captured reasoning state (restored by the router from the replay
// store, or decoded from an upstream reasoning_content field): it
// is json:"-" — router-internal, never serialised into a client
// response, a coalescing key, a cache key, or a trace — and each
// wire client translates it into its own shape (or refuses the hop
// honestly) before sending.
type Message struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Parts      []ContentPart    `json:"-"`
	ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	Reasoning  *reasoning.State `json:"-"`
}

// messageJSON is the decode/encode shape for Message.
// ReasoningContent is decode-only: an upstream (or a
// reasoning-aware client) may send reasoning_content on an
// assistant message, and it decodes into Message.Reasoning as a
// thought block. MarshalJSON never sets it, so reasoning state is
// never echoed back to a client.
type messageJSON struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// UnmarshalJSON decodes a Message, accepting content as a plain string,
// an array of content parts, or null (assistant messages that only
// carry tool calls).
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	m.Name = raw.Name
	m.Parts = nil
	m.Content = ""
	m.Reasoning = nil
	// reasoning_content (OpenAI wire) decodes into a thought block:
	// it is the upstream's reasoning text for this assistant turn,
	// captured so the router can store and replay it. It is never
	// copied into Content — reasoning must never become user-visible
	// content, and it is never re-emitted by MarshalJSON.
	if strings.TrimSpace(raw.ReasoningContent) != "" {
		m.Reasoning = &reasoning.State{
			Blocks:     []reasoning.Block{{Kind: reasoning.KindThought, Text: raw.ReasoningContent}},
			SourceWire: reasoning.WireOpenAI,
		}
	}
	trimmed := strings.TrimSpace(string(raw.Content))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw.Content, &asString); err == nil {
		m.Content = asString
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return fmt.Errorf("provider: message content is neither a string nor a content-part array: %w", err)
	}
	m.Parts = parts
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	m.Content = sb.String()
	return nil
}

// MarshalJSON encodes a Message: content goes out as an array when
// Parts is set, as a string otherwise (null when there is no content
// at all, e.g. an assistant message carrying only tool calls).
func (m Message) MarshalJSON() ([]byte, error) {
	var content any
	switch {
	case len(m.Parts) > 0:
		content = m.Parts
	case m.Content != "":
		content = m.Content
	default:
		content = nil
	}
	return json.Marshal(messageJSON{
		Role:       m.Role,
		Content:    mustJSONRaw(content),
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	})
}

// mustJSONRaw marshals v to its JSON encoding (infallible for the
// values used here: string, []ContentPart, nil).
func mustJSONRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// ChatRequest is a chat completion request. MaxTokens 0 means "provider
// default"; Temperature / TopP 0 means "omit, use provider default".
// Stop accepts a string or an array of strings on decode (it always
// encodes as an array). ToolChoice is kept raw so every OpenAI form
// ("none" | "auto" | "required" | {"type":"function",...}) passes
// through to the wire conversion unchanged.
//
// RequestRouting is the per-request routing contract (CHECKLIST
// 157): the routing controls a client supplies for one request
// only. It is the canonical spelling — a request-body field named
// "routing" on POST /v1/chat/completions. There is no header form:
// the server rejects an X-Codvian-Routing header outright, so a
// client using the wrong transport learns immediately instead of
// being silently ignored. Unknown keys inside "routing" are
// rejected at decode time (see ChatRequest.UnmarshalJSON), never
// ignored.
//
//   - Order: try these providers in exactly this order. Every
//     entry must be a registered provider that serves the model;
//     chain providers not listed are excluded (recorded as
//     not_in_order), never silently reordered behind the list.
//
//   - Only: restrict the chain to these providers.
//
//   - Exclude: never use these providers for this request.
//
//   - Sort: reorder the chain for this request — "price"
//     (cheapest first), "latency" (least latency first) or
//     "reliability" (most reliable first). It overrides the
//     auto:<axis> alias, the chain override and the global
//     strategy for this request only.
//
//   - MaxPrice: per-request price ceiling in USD per 1M tokens
//     (input/output legs, either may be omitted). A pair fits only
//     when every leg it sets has a known operator-configured price
//     at or under the ceiling; an unpriced pair is excluded, never
//     assumed cheap. When nothing fits the request fails with
//     HTTP 402.
//
//   - DataCollection: "deny" routes only to providers the
//     operator has marked as not collecting training data (config
//     no_training_data) or that are local by construction (catalog
//     category local, or a loopback base URL — the data never
//     leaves this machine). "allow" (the default) applies no
//     filter. The router ships no built-in vendor-policy database:
//     a vendor the operator has not marked is treated as unknown
//     and excluded under "deny", never guessed safe.
//
//   - Group: route this request through one operator-named
//     routing group (CHECKLIST 173) — the group's members, in
//     config order, become the fallback chain for the request's
//     model, participating in failover, budgets and the ledger
//     exactly like a chain. The group must be defined under
//     routing.groups in the config file; an unknown group is a
//     400 naming it, never a silent fallback to everything, and
//     a group with no member serving the model is a 400 naming
//     the group and the model. The model may also name a group
//     directly with the "group:<name>/<model>" alias instead —
//     when both spellings are present they must name the same
//     group. Group is recorded on the trace contract, like the
//     other controls.
//
// All controls are validated by the engine (internal/router):
// an unknown provider is a 400 naming it, an empty result set is
// a 400 (402 for max_price), never a silent fallback to
// everything. The applied contract — controls + every exclusion
// with its reason — is recorded on the route trace and served by
// GET /dashboard/trace.
type RequestRouting struct {
	Order          []string  `json:"order,omitempty"`
	Only           []string  `json:"only,omitempty"`
	Exclude        []string  `json:"exclude,omitempty"`
	Sort           string    `json:"sort,omitempty"`
	MaxPrice       *MaxPrice `json:"max_price,omitempty"`
	DataCollection string    `json:"data_collection,omitempty"`
	Group          string    `json:"group,omitempty"`
}

// MaxPrice is the routing.max_price ceiling: USD per 1M tokens per
// leg. Either leg may be omitted; at least one must be set.
type MaxPrice struct {
	Input  *float64 `json:"input,omitempty"`
	Output *float64 `json:"output,omitempty"`
}

// Empty reports whether the contract carries no control at all: a
// nil receiver, no lists, no sort, no price ceiling, no
// data-collection demand and no group. An empty contract is the same as no
// contract — routing proceeds exactly as without it, and the
// trace carries no contract block.
func (r *RequestRouting) Empty() bool {
	if r == nil {
		return true
	}
	return len(r.Order) == 0 && len(r.Only) == 0 && len(r.Exclude) == 0 &&
		r.Sort == "" && r.MaxPrice == nil && r.DataCollection == "" && r.Group == ""
}

// VertexOverride is one client-supplied Vertex location/project
// override found in a request body: the body field name, as sent
// ("vertex_location", "vertex_project", "vertex_ai_location" or
// "vertex_ai_project"), and its value. The server rejects the
// request naming the field unless the admin opted in; the value
// is never logged and never forwarded.
type VertexOverride struct {
	Field string
	Value string
}

// vertexOverrideFields are the request-body field names that
// select a Vertex AI location/project. A client sending any of
// them is asking to choose which cloud project/location the
// request lands in — that choice belongs to the admin's provider
// config, never to the caller.
var vertexOverrideFields = []string{"vertex_location", "vertex_project", "vertex_ai_location", "vertex_ai_project"}

// DetectVertexOverride scans a raw request body for a
// client-supplied Vertex location/project override. It returns
// the first override field found (in vertexOverrideFields order)
// and its value. A field that is absent, null or an empty string
// is not an override — there is nothing to choose. A present
// non-string value is still an override (fail closed: the field
// is named and the raw value carried, so the server can reject
// it rather than guess what the client meant).
func DetectVertexOverride(data []byte) (VertexOverride, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return VertexOverride{}, false
	}
	for _, name := range vertexOverrideFields {
		raw, ok := fields[name]
		if !ok || string(raw) == "null" {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			// Non-string value: still an override, fail
			// closed with the field named.
			return VertexOverride{Field: name, Value: string(raw)}, true
		}
		if value == "" {
			continue
		}
		return VertexOverride{Field: name, Value: value}, true
	}
	return VertexOverride{}, false
}

// prompt_cache_key, when supplied: the key string, or nil with
// PromptCacheKeySet true when the client sent an explicit JSON null
// — a null is a deliberate "no key", never replaced by a generated
// one. CallerID is the gateway-key ID the request arrived under
// (set by the server after auth). All three are json:"-": they are
// router-internal, never serialised, so they can never leak into a
// coalescing key, a cache key, a trace, the ledger, or any
// re-encoded request body.
// CompressionRequest is the per-request lossless-first compression
// opt-in (CHECKLIST 183): the "compression" request-body field on
// POST /v1/chat/completions.
//
//   - Lossless: apply the lossless steps (dedupe repeated
//     system/developer blocks, strip redundant whitespace,
//     canonicalise repeated tool results) before the provider call.
//     Nil means "no per-request opinion" — the operator's
//     per-model config list then decides. An explicit false opts
//     OUT even for a model the operator listed: the client can
//     always refuse compression of its own prompt.
//
//   - LossyMaxPromptTokens: when > 0 the client EXPLICITLY opts in
//     to lossy compression as well — extractive trimming of the
//     prompt down toward this many locally-counted prompt tokens
//     (see internal/compress). Lossy is never enabled by operator
//     config and never implied by Lossless: without this field the
//     router never drops meaning, only duplicates and whitespace.
//     0 means "no lossy step". Negative is a 400.
//
//   - Mode: an optional named compression preset (M7.6 — one of
//     off, lite, standard, aggressive, ultra, rtk, stacked; see
//     internal/compress.ParseMode). The mode selects which lossy
//     engines may run (RTK tool-output filter, Caveman prose
//     condensation, stacked pipeline); it NEVER enables lossy by
//     itself — a mode without LossyMaxPromptTokens runs the
//     lossless steps only, exactly as if no mode were set. An
//     unknown mode is a 400 (validateChatRequest). Empty means
//     "no explicit mode": a lossy request with no mode runs exactly
//     the extractive pair (lossy drop/truncate) unless the caller
//     opts into auto-trigger / budget-dial selection via
//     internal/compress.ApplyOptions.
//
//   - PreserveSystemPromptMode: "always" (default — the system
//     prompt is never compressed), "whenNoCache" (compress it
//     only when there is no cache to protect), or "never"
//     (always compress it, even when it breaks a prompt cache;
//     it is still never DROPPED, only truncated). Empty or
//     unknown means "always". For a caching provider an
//     aggressive/ultra mode is downgraded to standard. See
//     internal/compress/preserve.go.
//
//   - RTKLevel: the RTK tool-output filter level for this
//     request (B4-25 — one of minimal, standard, aggressive; see
//     internal/compress.ParseRTKLevel). It selects how hard the
//     mode=rtk step and the stacked pipeline's RTK stage filter
//     tool output; it NEVER enables lossy by itself — without a
//     lossy opt-in no RTK step runs at all. Empty means
//     "standard". An unknown level is a 400. The
//     X-Codvian-RTK-Level header names the same level per
//     request and wins over this field when present and valid
//     (see internal/server.applyCompressionPrePass).
//
//   - ToolResultMaxChars: an opt-in Lite cap (in runes) on
//     pre-current-turn tool results: a tool result over the cap
//     is truncated head (+tail for multi-line results), the
//     current turn's tool results are never cut. 0 / unset means
//     no cap. Out-of-range values are clamped into [256,
//     1,000,000]. Setting it also opts the request into the
//     lossless pass. See internal/compress/toolcap.go. Negative
//     is treated as unset (no 400 — server validation of this
//     field is coordinator follow-up, not something Apply does).
//
// The field is router-internal after decode, like Routing: the wire
// clients build their own upstream payload from Model/Messages and
// never forward it, so a provider can never see the opt-in — but it
// IS visible on the request the client sent, and the applied result
// is reported back (see CompressionResult, the response headers and
// the route trace), never silently applied.
type CompressionRequest struct {
	Lossless                 *bool  `json:"lossless,omitempty"`
	LossyMaxPromptTokens     int    `json:"lossy_max_prompt_tokens,omitempty"`
	Mode                     string `json:"mode,omitempty"`
	RTKLevel                 string `json:"rtk_level,omitempty"`
	PreserveSystemPromptMode string `json:"preserve_system_prompt_mode,omitempty"`
	ToolResultMaxChars       int    `json:"tool_result_max_chars,omitempty"`
}

// PreCompressionSnapshot is the request as the client sent it: the
// raw messages plus the compression opt-in in force when they were
// sent. The exact-cache key is derived from this snapshot (see
// ChatRequest.PreCompression): identical logical requests must key
// identically regardless of how much state the stateful
// compression engines (CCR, session-dedup) have accumulated
// between them. Messages is a deep copy — later engines replace
// message content in place — and Compression is a copy of the
// opt-in as it stood at snapshot time (the pre-pass itself stashes
// the RTK level onto the live opt-in afterwards, and that must
// not leak into the snapshot).
type PreCompressionSnapshot struct {
	Messages    []Message
	Compression *CompressionRequest
}

// SnapshotPreCompression deep-copies the request's messages and
// compression opt-in as they stand right now. The copy is safe to
// keep while compression engines mutate the live request: message
// structs are copied, Parts and ToolCalls slices are copied
// (including the CacheControl/ImageURL pointees on parts), and the
// compression opt-in is copied (including the Lossless pointee).
// Reasoning state is shared, never copied: it is not part of the
// message JSON the cache key hashes, and compression never
// mutates it. A nil receiver returns nil.
func (req *ChatRequest) SnapshotPreCompression() *PreCompressionSnapshot {
	if req == nil {
		return nil
	}
	msgs := make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = m
		if len(m.Parts) > 0 {
			parts := make([]ContentPart, len(m.Parts))
			for j, p := range m.Parts {
				parts[j] = p
				if p.CacheControl != nil {
					cc := *p.CacheControl
					parts[j].CacheControl = &cc
				}
				if p.ImageURL != nil {
					u := *p.ImageURL
					parts[j].ImageURL = &u
				}
			}
			msgs[i].Parts = parts
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]ToolCall, len(m.ToolCalls))
			copy(calls, m.ToolCalls)
			msgs[i].ToolCalls = calls
		}
	}
	var comp *CompressionRequest
	if req.Compression != nil {
		c := *req.Compression
		if req.Compression.Lossless != nil {
			b := *req.Compression.Lossless
			c.Lossless = &b
		}
		comp = &c
	}
	return &PreCompressionSnapshot{Messages: msgs, Compression: comp}
}

// CompressionResult is the compression actually applied to one
// request, stamped on the request by whoever compressed it (the
// server handler, or the engine for direct callers) and carried to
// the route trace, the ledger row and the response headers. All
// fields are json:"-": the result is router bookkeeping, never
// serialised into a coalescing key, a cache key, or a request
// body — the compressed MESSAGES are what those keys see.
//
// BeforeTokens / AfterTokens are local tiktoken estimates
// (internal/tokencount) of the prompt before and after
// compression, so SavedTokens = BeforeTokens - AfterTokens is an
// estimate too — the same estimate class as the pre-send budget
// gate, never a provider-reported number. Verified says the
// lossless steps passed their equivalence check (see
// internal/compress.VerifyLossless): the compressed form is
// exactly the canonical normal form of the original, so only
// exact duplicates and redundant whitespace were removed. A
// lossy step is by definition NOT verified — its entry in Steps
// names it, and LossyApplied says it ran.
type CompressionResult struct {
	Applied        bool
	Lossless       bool
	LossyRequested bool
	LossyApplied   bool
	Steps          []string
	BeforeTokens   int
	AfterTokens    int
	SavedTokens    int
	LossyMaxTokens int
	Model          string
	Source         string // "request" | "model-config" | "none"
	Verified       bool
	// Mode is the named preset that selected the lossy engines
	// for this request (M7.6; "" when no mode ran — lossless-only
	// or no compression). It names the REQUESTED/selected mode,
	// not a guarantee: a mode whose defining engine does not
	// exist in this build runs its available subset (see
	// internal/compress.Apply), and Steps names what actually
	// ran.
	Mode string `json:"mode,omitempty"`
}

// Reasoning-effort levels (CHECKLIST 255, Magpie v0.1.4405): the
// OpenAI-style reasoning_effort values a request — or one routing
// group entry — can ask for. A group entry is (model, effort), not
// model alone: the same model at different efforts is a distinct
// cost/latency/quality point, so a group may hold the same model
// twice at different efforts. The levels are validated, never
// guessed: an unknown level is a config-load error for a group
// member and a 400 for a client request. "none" means the model
// runs with no reasoning at all (where the provider supports it);
// "" (empty) means "no explicit effort" — the client's own value
// passes through, or the provider default applies.
const (
	// ReasoningEffortNone runs the model with no reasoning.
	ReasoningEffortNone = "none"
	// ReasoningEffortMinimal is the cheapest reasoning pass.
	ReasoningEffortMinimal = "minimal"
	// ReasoningEffortLow is a light reasoning pass.
	ReasoningEffortLow = "low"
	// ReasoningEffortMedium is the default-strength reasoning pass.
	ReasoningEffortMedium = "medium"
	// ReasoningEffortHigh is a deep reasoning pass.
	ReasoningEffortHigh = "high"
	// ReasoningEffortXHigh is the deepest reasoning pass.
	ReasoningEffortXHigh = "xhigh"
)

// ReasoningEfforts lists every valid reasoning-effort level, in
// increasing strength order.
var ReasoningEfforts = []string{
	ReasoningEffortNone,
	ReasoningEffortMinimal,
	ReasoningEffortLow,
	ReasoningEffortMedium,
	ReasoningEffortHigh,
	ReasoningEffortXHigh,
}

// ValidReasoningEffort reports whether s is a known reasoning
// effort level (case-insensitive, surrounding whitespace trimmed).
// "" is NOT valid here — it means "no explicit effort", which is
// a different state from any level; callers that accept "" check
// for it before calling this.
func ValidReasoningEffort(s string) bool {
	norm := strings.ToLower(strings.TrimSpace(s))
	for _, e := range ReasoningEfforts {
		if norm == e {
			return true
		}
	}
	return false
}

// NormalizeReasoningEffort lower-cases and trims an effort level.
// It does not validate: callers validate with ValidReasoningEffort
// first when the value must be known.
func NormalizeReasoningEffort(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// CacheControls is the per-request cache directive set for one
// chat request (M6.4), parsed from HTTP headers by the server
// (internal/server.parseCacheControls) and carried on ChatRequest.
// A nil *CacheControls means "no directives — default caching".
//
// Header mapping (X-Codvian- prefix; see server/cache_controls.go):
//
//   - Bypass: X-Codvian-No-Cache (or the X-Router-No-Cache alias),
//     Cache-Control containing no-cache, Pragma: no-cache.
//   - NoStore: X-Codvian-Cache-No-Store, or Cache-Control containing
//     no-store.
//   - Type: X-Codvian-Cache-Type: exact | semantic | both | none.
//   - KeyOverride: X-Codvian-Cache-Key.
//   - Threshold: X-Codvian-Cache-Threshold, clamped to [0, 1].
//   - TTL: X-Codvian-Cache-TTL (seconds), capped at
//     MaxCacheControlTTL.
//
// Invalid header values are IGNORED by the parser (fail-open to
// default caching), never a 400 — a client that misspells a control
// gets ordinary caching, not a refused request. The only validation
// that happens here is the defensive clamp in EffectiveTTL: a
// directly-constructed CacheControls with an out-of-range TTL is
// still capped, so no caller can widen the store TTL past the cap.
type CacheControls struct {
	// Bypass skips the cache entirely for this request: no
	// lookup, no store, and no stale fallback. The request must
	// never be served a stale answer and must never poison the
	// cache.
	Bypass bool
	// NoStore skips the store only; lookup (and the stale
	// fallback, which is a read) is still allowed.
	NoStore bool
	// Type selects which layers are consulted and stored:
	// "" (unset) or "both" = exact + semantic; "exact" or
	// "semantic" = that layer only; "none" = bypass. Any other
	// value is ignored by the parser and never reaches here.
	Type string
	// KeyOverride replaces the message-derived exact key with a
	// caller-chosen bucket. It is NEVER used raw: the exact key
	// is hash(tenant + model + override), so two tenants (or two
	// models) sharing an override value still never share an
	// entry. An override also disables the semantic layer for
	// the request (see UseSemantic): the semantic layer is keyed
	// on prompt text, which would defeat the caller's explicit
	// bucket. Empty means "derive the key from the request".
	KeyOverride string
	// Threshold overrides the semantic similarity floor for
	// this request only. Nil means the cache's configured floor
	// (0.85). A value below the cache's own floor cannot widen
	// hits — the cache only returns entries at or above its
	// floor, so the override can only narrow (raise) the bar.
	// The parser clamps to [0, 1]; a directly-constructed value
	// outside [0, 1] is treated as unset here.
	Threshold *float32
	// TTL overrides the store TTL for this request only, in
	// whole seconds as a duration. 0 (or negative) means the
	// layer default (1h). Above MaxCacheControlTTL it clamps,
	// never errors.
	TTL time.Duration
}

// MaxCacheControlTTL is the cap on a per-request cache TTL: the
// layer default (degrade.DefaultStoreTTL, cache.DefaultSemanticTTL
// — both 1h). A per-request TTL can only narrow the window, never
// widen it past what an ordinary stored entry already gets.
const MaxCacheControlTTL = time.Hour

// BypassCache reports whether this request skips the cache
// entirely (no lookup, no store, no stale serve). Nil-safe: a
// nil *CacheControls means default caching.
func (c *CacheControls) BypassCache() bool {
	return c != nil && (c.Bypass || c.Type == "none")
}

// AllowStore reports whether this request may store into the
// cache. Nil-safe: default caching stores.
func (c *CacheControls) AllowStore() bool {
	return !c.BypassCache() && (c == nil || !c.NoStore)
}

// UseExact reports whether the exact-match layer is consulted
// and stored for this request.
func (c *CacheControls) UseExact() bool {
	if c.BypassCache() {
		return false
	}
	return c == nil || c.Type == "" || c.Type == "exact" || c.Type == "both"
}

// UseSemantic reports whether the semantic layer is consulted
// and stored for this request. A key override disables it: the
// semantic layer is keyed on prompt text, so consulting it would
// let a request escape the bucket its override pinned it to.
func (c *CacheControls) UseSemantic() bool {
	if c.BypassCache() {
		return false
	}
	if c == nil {
		return true
	}
	if c.KeyOverride != "" {
		return false
	}
	return c.Type == "" || c.Type == "semantic" || c.Type == "both"
}

// ThresholdFloor returns the per-request semantic floor and
// whether one is set. Values outside [0, 1] are treated as
// unset (the parser clamps; this is the backstop for
// directly-constructed values).
func (c *CacheControls) ThresholdFloor() (float32, bool) {
	if c == nil || c.Threshold == nil {
		return 0, false
	}
	v := *c.Threshold
	if v < 0 || v > 1 {
		return 0, false
	}
	return v, true
}

// EffectiveTTL returns the per-request store TTL, capped at
// MaxCacheControlTTL. 0 means "use the layer default".
func (c *CacheControls) EffectiveTTL() time.Duration {
	if c == nil || c.TTL <= 0 {
		return 0
	}
	if c.TTL > MaxCacheControlTTL {
		return MaxCacheControlTTL
	}
	return c.TTL
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	// ReasoningEffort is the client-requested reasoning effort
	// (CHECKLIST 255): one of the ReasoningEffort* levels, or ""
	// for no explicit effort. A routing-group entry's effort
	// overrides it for that attempt (the operator's routing
	// decision is final for the entry); an attempt with no entry
	// effort passes this value through untouched. Since
	// CHECKLIST 269-270 a resolved intent tier's pinned effort
	// also lands here (the engine's resolveAuto path), with the
	// same override precedence as a group entry: the tier is
	// the operator's routing decision, so it wins over a
	// client-supplied value. It is forwarded to the upstream
	// only on wires that natively accept a named effort — today
	// that is the OpenAI wire alone (reasoning_effort). The
	// other wires have no named-effort field, so the effort is
	// NOT forwarded on them as a name (CHECKLIST 269-270,
	// audited — no invented mapping): Anthropic takes a numeric
	// thinking.budget_tokens, Gemini and Vertex a numeric
	// thinkingConfig.thinkingBudget (Vertex uses the Gemini
	// shape), Cohere has no reasoning
	// control, and Bedrock Converse has no effort field. Since
	// CHECKLIST 285-286 the engine resolves the numeric budget
	// for the Anthropic/Gemini wires from the operator-declared
	// effort→budget table (config `reasoning.effort_budgets`)
	// and carries it here as ThinkingBudget; CHECKLIST 303-304
	// extends the same table to Vertex (its own "vertex"
	// entry). With no table
	// (or no entry for the effort) the field stays 0 and
	// nothing is sent. The trace records the effort the
	// attempt ran at, so the gap is visible on the dashboard,
	// never silent.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ThinkingBudget is the numeric thinking-token budget for
	// this attempt (CHECKLIST 285-286, Vertex 303-304), resolved by the engine
	// from the operator-declared effort→budget table for the
	// attempt's effective effort. 0 means unset: no thinking
	// field is sent. Only the Anthropic client (as
	// thinking.budget_tokens), the Gemini client (as
	// thinkingConfig.thinkingBudget) and the Vertex client (as
	// the same thinkingConfig.thinkingBudget) read it; the OpenAI client
	// ignores it (it sends the named reasoning_effort), and no
	// other wire has a thinking-budget field. It is
	// router-internal after decode — the value comes from the
	// operator's table, never from the client — and it is set
	// per attempt by the engine, after any request coalescing
	// (like ReasoningEffort it is not part of the coalescing
	// key: two requests that differ only in effort already
	// share a flight key today, and the budget follows the
	// attempt's own effective effort).
	ThinkingBudget int `json:"thinking_budget,omitempty"`
	// EffortPolicy is the forced-reasoning policy decision for
	// this request (CHECKLIST 311-312), stamped by the engine
	// entry points (Chat/ChatStream) when a policy matched the
	// requested or resolved model: "forced:<effort>:policy:<model>"
	// (the policy won) or "client-override:<effort>:policy:<model>"
	// (the client's value won because the policy allows
	// override). "" when no policy matched. The effective effort
	// rides ReasoningEffort; the chain stamping in
	// ChatWithChainStrategy / ChatStreamWithChainStrategy copies
	// both onto every attempt, so a group-resolved chain (built
	// after the entry points) still honours the policy. json:"-":
	// engine-internal, never on the wire and never part of the
	// coalescing key — a client must not be able to forge it,
	// so the entry points overwrite it unconditionally.
	EffortPolicy string           `json:"-"`
	Stop         []string         `json:"stop,omitempty"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	ToolChoice   json.RawMessage  `json:"tool_choice,omitempty"`
	// Routing is the per-request routing contract (see
	// RequestRouting). It is router-internal after decode: the
	// wire clients build their own upstream payload from the
	// fields above and never forward it, so a contract can never
	// leak to a provider. It IS part of the coalescing key (see
	// internal/coalescing): two requests that differ only in
	// their contract must never share one flight.
	Routing *RequestRouting `json:"routing,omitempty"`
	// Compression is the per-request compression opt-in (see
	// CompressionRequest). Compressed is the result stamped by
	// the server handler or the engine once compression has been
	// decided for this request: a non-nil stamp means "already
	// decided for Compressed.Model" — the engine re-decides
	// only when the model changed since the stamp (an "auto/*"
	// virtual model resolving to a concrete one), and then it
	// keeps the stamp's BeforeTokens so the saving is measured
	// against the original prompt, not the already-compressed
	// one. Both are json:"-" after decode for Compressed; the
	// opt-in itself is part of the client's request body; the
	// stamp (Compressed) is json:"-" and never serialised.
	Compression *CompressionRequest `json:"compression,omitempty"`
	Compressed  *CompressionResult  `json:"-"`
	// PreCompression is the snapshot of the request as the client
	// sent it, taken by the server's applyCompressionPrePass
	// before any compression engine mutates Messages
	// (session-dedup, CCR and Codex in the pre-pass; the lossy
	// trim in applyCompression). The exact-cache key is derived
	// from this snapshot, not from the messages as sent: the CCR
	// store is stateful, so two identical requests reach the
	// engine with different messages (the second one's repeated
	// content is markered), and the lossy trim counts post-CCR
	// token lengths — keying on the messages as sent makes the
	// repeat miss (TestChatCompletionsLossyExactHitStillWorks).
	// The snapshot is router-internal (json:"-"): it never
	// reaches a provider and never enters the coalescing key.
	// A nil snapshot means "no pre-pass ran" — the engine keys
	// on the request as received, exactly as before.
	PreCompression *PreCompressionSnapshot `json:"-"`

	PromptCacheKey    *string `json:"-"`
	PromptCacheKeySet bool    `json:"-"`
	// ClientVertexOverride records a client-supplied Vertex
	// location/project override found in the request body
	// (CHECKLIST 245, LiteLLM #47028): the field name and its
	// value, as sent. It is router-internal (json:"-") and is
	// never forwarded to a provider: the server rejects the
	// request unless the admin opted in
	// (config.AllowClientVertexOverride), because a client must
	// never choose which cloud project/location a request lands
	// in. The value is stored only so the rejection can name
	// the field; it is never logged.
	ClientVertexOverride *VertexOverride `json:"-"`
	CallerID             string          `json:"-"`
	// BudgetUSD is the client-supplied per-request spend cap in
	// USD, from the X-Codvian-Budget header (CHECKLIST 155):
	// nil means "no per-request budget". BudgetStrict is the
	// strict-mode flag from X-Codvian-Budget-Strict (or the
	// reference-style Budget-Fallback: strict alias): in strict
	// mode the engine stops the chain before any attempt whose
	// priced prompt estimate would push cumulative spend over
	// the cap; in non-strict mode the chain continues and the
	// overage is recorded honestly. Both are json:"-" like the
	// other router-internal fields: they never leak into a
	// coalescing key, a cache key, a trace, the ledger, or any
	// re-encoded request body.
	BudgetUSD    *float64 `json:"-"`
	BudgetStrict bool     `json:"-"`

	// StreamIncludeUsage is the client's stream_options.include_usage
	// request (G1): when true, a completed stream emits a final
	// usage chunk before [DONE] — but only when the upstream
	// actually reported usage (a local estimate is never presented
	// as provider-reported). Router-internal (json:"-"): the
	// upstream request carries its own include_usage flag
	// (streamOptionsWire), never the client's block verbatim.
	StreamIncludeUsage bool `json:"-"`

	// CacheControls carries the per-request cache directives parsed
	// from HTTP headers by the server (M6.4): bypass headers and
	// per-request cache controls. It is router-internal (json:"-"):
	// never forwarded to a provider, never logged, never part of
	// the coalescing flight key (internal/coalescing hashes
	// selected fields only — a bypass request may still share an
	// in-flight provider call with an identical normal request,
	// which is a live answer, not a cache serve; each request's
	// own store is gated separately by degrade.store). The only
	// cache-key effect is KeyOverride, which replaces the
	// message-derived exact key under tenant+model scoping (see
	// degrade.scopedKey and server.scopedMessagesKey).
	CacheControls *CacheControls `json:"-"`
	// Endpoint names the API surface this request was made to
	// (chat completions when zero). It is router-internal
	// (json:"-"): the replay exemption (internal/router) keys off
	// it — embeddings, images and rerank must never be replayed
	// onto another target, because replaying them could
	// double-apply stateful upstream work. The server sets it
	// when decoding; a zero value means chat completions.
	// Responses requests (fresh or branched) are replay-safe:
	// the gateway holds the transcript and re-sends it as plain
	// chat messages, so no upstream-held state exists to
	// double-apply (CHECKLIST 237).
	Endpoint EndpointKind `json:"-"`
	// ServiceTier is the client's requested service tier
	// (OpenAI API field "service_tier"): "", "standard",
	// "flex", "batch", "priority" or "auto". It is decoded from
	// the client request, carried through the engine (via
	// router.Attempt — the coordinator threads it there; see
	// the G16 report) into Usage.ServiceTier, and priced by the
	// ledger (flex / batch at 0.5x). It is NOT forwarded to
	// upstreams: no upstream wire-request construction sends a
	// service_tier field today, so a tier the upstream does not
	// know cannot be requested there yet — forwarding it is
	// coordinator follow-up work, not something this field does.
	// "" means standard.
	ServiceTier string `json:"service_tier,omitempty"`
	// PreviousResponseID is the Responses-API continuation id
	// (previous_response_id): the request continues the
	// gateway-held conversation of that response. The engine
	// tries the original provider first (PinProviderID) and
	// fails over to the next target like any other request
	// (CHECKLIST 237).
	PreviousResponseID string `json:"-"`
	// PinProviderID is the provider a Responses continuation
	// prefers (the provider that served the response named by
	// PreviousResponseID, recorded by the server from the route
	// trace). The engine tries that provider first: when it is
	// still in the chain it leads the chain, and a failure there
	// fails over to the rest (CHECKLIST 237); when it no longer
	// serves the model the normal chain applies instead — the
	// transcript is gateway-held, so the conversation still
	// resumes, and the trace shows who served it. Empty means
	// no pin.
	PinProviderID string `json:"-"`
	// DeliveredToolCallIDs names the tool calls the client already
	// received on an earlier stream of this logical request
	// (CHECKLIST 243, LiteLLM #51683): the ids of tool calls
	// delivered before the stream broke. A resumed stream
	// (ChatStreamFrom/Chunks) suppresses deltas for these ids, so
	// a fallback replay never re-delivers — and the client never
	// re-executes — a call it already saw. It is router-internal
	// (json:"-"): the ids are delivery bookkeeping, never part of
	// the upstream request, the coalescing key, or any log. Empty
	// means "no prior delivery": a fresh stream suppresses
	// nothing. A delta with an empty id is never suppressed by
	// this list (there is nothing to match it against) — the
	// text-prefix skip is the only guard for id-less calls.
	DeliveredToolCallIDs []string `json:"-"`
	// WarmUp marks a synthetic warm-up request (connection/cache
	// warming, never a user request): replaying it would only
	// double the upstream load it was sent to avoid.
	WarmUp bool `json:"-"`
	// tokenCountMemo is an opaque per-request scratch slot for the
	// local prompt-token memo (tokencount.RequestMemo). The engine
	// attaches one once the request's Messages/Tools are final, so
	// the per-hop budget gate and the other estimate sites reuse a
	// single CountPromptWire instead of re-joining, re-hashing and
	// re-marshalling the prompt on every hop (AUD-PERF-004). It is
	// unexported and json-ignored: never on the wire, never in a
	// coalescing or cache key, and never part of a re-encoded body.
	// The zero value (nil) means "no memo": every caller then
	// computes fresh, exactly as before this field existed. See
	// TokenCountMemo / SetTokenCountMemo.
	tokenCountMemo any
}

// TokenCountMemo returns the per-request token-count memo attached by
// the engine (an opaque *tokencount.RequestMemo), or nil when none is
// attached. internal/tokencount reads it to reuse a CountPromptWire
// result within one request; nothing else should.
func (r *ChatRequest) TokenCountMemo() any { return r.tokenCountMemo }

// SetTokenCountMemo attaches a per-request token-count memo. Contract:
// it must be set only once this request's Messages and Tools are FINAL
// for the request — the memo caches a CountPromptWire result keyed by
// (model, wire, message count, tools count), so replacing the messages
// afterwards would let a stale count stand. internal/router sets it
// right after its own compression + reasoning restore, which is that
// point. A nil memo leaves every caller computing fresh.
func (r *ChatRequest) SetTokenCountMemo(m any) { r.tokenCountMemo = m }

// chatRequestAlias is ChatRequest without its methods, so decoding
// into chatRequestJSON cannot recurse back into UnmarshalJSON.
type chatRequestAlias ChatRequest

// chatRequestJSON is the decode shape for ChatRequest (Stop may be a
// string or a string array on the wire).
type chatRequestJSON struct {
	chatRequestAlias
	Stop json.RawMessage `json:"stop,omitempty"`
}

// UnmarshalJSON decodes a ChatRequest, accepting Stop as either a
// single string or an array of strings. It also records whether the
// client supplied prompt_cache_key (and its value): the key is
// router-internal after this point and is forwarded to the upstream
// verbatim — including an explicit null — never replaced.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	var raw chatRequestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ChatRequest(raw.chatRequestAlias)
	r.Stop = nil
	r.PromptCacheKey = nil
	r.PromptCacheKeySet = false
	if len(raw.Stop) == 0 {
		// Stop absent: still check for prompt_cache_key below.
	} else {
		var single string
		if err := json.Unmarshal(raw.Stop, &single); err == nil {
			r.Stop = []string{single}
		} else {
			var multi []string
			if err := json.Unmarshal(raw.Stop, &multi); err != nil {
				return fmt.Errorf("provider: invalid stop value: %w", err)
			}
			r.Stop = multi
		}
	}
	// Presence of prompt_cache_key is significant on its own: an
	// explicit null means the client deliberately wants no key.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// Client-supplied Vertex location/project (CHECKLIST 245,
	// LiteLLM #47028): recorded, never silently dropped and
	// never acted on here — the server rejects the request
	// naming the field unless the admin opted in.
	if ov, ok := DetectVertexOverride(data); ok {
		r.ClientVertexOverride = &ov
	}
	if v, ok := fields["prompt_cache_key"]; ok {
		r.PromptCacheKeySet = true
		if string(v) != "null" {
			var key string
			if err := json.Unmarshal(v, &key); err != nil {
				return fmt.Errorf("provider: invalid prompt_cache_key value: %w", err)
			}
			r.PromptCacheKey = &key
		}
	}
	// stream_options (G1): only include_usage is meaningful to the
	// gateway. Decoded leniently — unknown keys inside the block
	// are ignored, not a 400: OpenAI SDKs send only include_usage,
	// and a compatibility surface must not break on a future
	// field it does not act on. A malformed include_usage value
	// is a 400 naming the field.
	if v, ok := fields["stream_options"]; ok && string(v) != "null" {
		var so struct {
			IncludeUsage *bool `json:"include_usage"`
		}
		if err := json.Unmarshal(v, &so); err != nil {
			return fmt.Errorf("provider: invalid stream_options: %w", err)
		}
		if so.IncludeUsage != nil {
			r.StreamIncludeUsage = *so.IncludeUsage
		}
	}
	// The routing contract is decoded strictly: an unknown key
	// inside "routing" is a 400 naming the key (with the known
	// field list), never silently ignored — a client that
	// misspells "exclude" as "excludes" must learn immediately,
	// not discover later that nothing was excluded. The alias
	// decode above already populated r.Routing; this pass only
	// rejects unknown keys and wrong shapes.
	if v, ok := fields["routing"]; ok && string(v) != "null" {
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.DisallowUnknownFields()
		var strict RequestRouting
		if err := dec.Decode(&strict); err != nil {
			return fmt.Errorf("provider: invalid routing: %s (known fields: order, only, exclude, sort, max_price, data_collection)", routingDecodeDetail(err))
		}
		r.Routing = &strict
	}
	// The compression opt-in is decoded strictly the same way:
	// an unknown key inside "compression" is a 400 naming the
	// key, never silently ignored — a client that misspells
	// "lossy_max_prompt_tokens" must learn immediately, not
	// discover later that no lossy step ran. A negative
	// lossy_max_prompt_tokens is a 400: a token budget below
	// zero is meaningless, and 0 already means "no lossy step".
	if v, ok := fields["compression"]; ok && string(v) != "null" {
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.DisallowUnknownFields()
		var strict CompressionRequest
		if err := dec.Decode(&strict); err != nil {
			return fmt.Errorf("provider: invalid compression: %s (known fields: lossless, lossy_max_prompt_tokens, mode, rtk_level)", routingDecodeDetail(err))
		}
		if strict.LossyMaxPromptTokens < 0 {
			return fmt.Errorf("provider: invalid compression: lossy_max_prompt_tokens must be >= 0 (known fields: lossless, lossy_max_prompt_tokens, mode, rtk_level)")
		}
		r.Compression = &strict
	}
	return nil
}

// routingDecodeDetail turns a strict routing-decode error into the
// one detail worth reporting: the unknown field name when that is
// what failed, otherwise the decoder's own message.
func routingDecodeDetail(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, `unknown field "`); i >= 0 {
		rest := msg[i+len(`unknown field "`):]
		if j := strings.Index(rest, `"`); j >= 0 {
			return `unknown field "` + rest[:j] + `"`
		}
	}
	return msg
}

// Usage is token usage reported by the upstream. CachedTokens is
// the provider-reported count of prompt tokens served from the
// provider's prompt cache (OpenAI's
// usage.prompt_tokens_details.cached_tokens, Anthropic's
// cache_read_input_tokens, Gemini's cachedContentTokenCount) — 0
// when the upstream did not report any. It is a subset of
// PromptTokens, never a separate charge; the ledger stores it only
// alongside a known split, and the dashboard counts only these
// provider-reported numbers.
//
// ReasoningTokens is the provider-reported count of completion
// tokens spent on internal reasoning (OpenAI's
// usage.completion_tokens_details.reasoning_tokens, Gemini's
// usageMetadata.thoughtsTokenCount) — a subset of CompletionTokens.
// Anthropic/Cohere/Bedrock do not report a reasoning split, so it
// stays 0 for them: unknown, never zero-filled from a guess.
//
// CacheWriteTokens is the provider-reported count of input tokens
// written into the provider's prompt cache on this call
// (Anthropic's cache_creation_input_tokens). Only Anthropic
// reports it; 0 elsewhere means not reported, not "free".
//
// CacheWriteTokens5m / CacheWriteTokens1h (M6.9b) split that
// total by TTL tier (Anthropic usage.cache_creation
// ephemeral_5m_input_tokens / ephemeral_1h_input_tokens). Both 0
// with a positive CacheWriteTokens means the upstream reported
// only the legacy total — the tier is unknown, never guessed.
// Only the Anthropic client populates them; every other client
// reports at most a total (Bedrock's Converse usage has no cache
// fields at all, OpenAI/Gemini/Cohere/Vertex report no
// cache-creation split in this tree), so their tiers stay 0 and
// the ledger prices their writes at the plain input rate.
//
// LocalEstimate is NOT provider-reported: the router sets it when
// the upstream reported no usage at all and the prompt count is a
// local tiktoken estimate (see internal/tokencount). PromptTokens
// then holds the estimate and TotalTokens equals it. The ledger
// stores such a row with tokens_split_known=0 and local_estimate=1,
// so an estimate can never be mistaken for a reported split.
// These three fields are json:"-" because they are router-internal
// bookkeeping, not part of the OpenAI-compatible usage block the
// client receives.
//
// PromptBeforeCompression / CompressionSavedTokens are the same
// class of router-internal bookkeeping (CHECKLIST 183): the local
// tiktoken estimate of the prompt BEFORE compression, and how many
// of those tokens compression saved — both measured by the local
// counter, never provider-reported. They ride on the usage of the
// successful attempt only (a failed attempt billed nothing, and a
// shared/coalesced row is zeroed like every other token field),
// so the ledger's window totals count each request's saving once.
// 0 / 0 means "no compression applied to this request", never "no
// saving measured".
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`

	ReasoningTokens    int  `json:"-"`
	CacheWriteTokens   int  `json:"-"`
	CacheWriteTokens5m int  `json:"-"`
	CacheWriteTokens1h int  `json:"-"`
	LocalEstimate      bool `json:"-"`
	// LocalEstimatePrompt is the local tiktoken estimate of the
	// prompt AS SENT, for the same request, whenever the router
	// computed one — whether or not the upstream reported usage
	// (see router.Engine.reconcileUsage). It is what lets the
	// ledger record estimate-vs-reported drift per request: on
	// a row with a known split it rides alongside the
	// provider-reported prompt count; on a local-estimate row
	// it equals PromptTokens. 0 means the router never computed
	// one for this attempt (counting failed, or a failed
	// attempt, which records Usage{}). It is router-internal
	// bookkeeping (json:"-"): the client-facing usage block
	// never carries it, and it never feeds cost.
	LocalEstimatePrompt int `json:"-"`
	// Endpoint names the API surface the usage was recorded for
	// (chat completions when zero). It is router-internal
	// (json:"-"): the ledger stores it on the usage row so the
	// dashboard can tell an embeddings/rerank call from a chat
	// call with the same model — the OpenAI usage block the
	// client receives never carries it.
	Endpoint EndpointKind `json:"-"`

	PromptBeforeCompression int `json:"-"`
	CompressionSavedTokens  int `json:"-"`
	// Cost is the EXACT cost the upstream reported for this
	// attempt, in USD — when the upstream reports one at all.
	// Only populated from a real reported value, never invented:
	// xAI reports cost_in_usd_ticks (1e10 ticks per USD, per the
	// xAI cost-tracking docs) and OpenRouter reports usage.cost
	// (bare USD) — both decoded in usageWire below. Nil means
	// "the upstream reported no cost": the ledger falls back to
	// the hand-computed cost, and a reported value wins over it
	// there (see ledger.normalizeUsage). Router-internal
	// (json:"-"): the client-facing usage block never carries
	// it, and it never feeds the coalescing key.
	Cost *float64 `json:"-"`
	// ServiceTier is the request's service tier ("", "standard",
	// "flex", "batch", ...), carried from the request so the
	// ledger can price flex / batch at 0.5x (see
	// ledger.ServiceTierMultiplier). Router-internal
	// (json:"-"): it rides the usage into the ledger, never
	// into the client-facing usage block.
	ServiceTier string `json:"-"`
	// AutoVariant is the auto-routing variant that served this
	// attempt ("auto", "auto/thrifty", "auto/best-code", ...),
	// set by the router from the auto dispatch so the ledger
	// can attribute the row to the variant that chose the
	// model. "" means the request was not auto-routed, or the
	// variant was never recorded. Router-internal (json:"-"):
	// it rides the usage into the ledger, never into the
	// client-facing usage block.
	AutoVariant string `json:"-"`
}

// NonNegative returns u with every token count clamped to >= 0
// (P1-09 security review): a malformed or hostile upstream report
// can carry negative counts, which must never price as negative
// spend or reach a client as negative tokens. Valid reports are
// returned unchanged. Subset-vs-parent clamping stays the ledger's
// job (normalizeUsage); this is the display/telemetry sanitization.
func (u Usage) NonNegative() Usage {
	clamp := func(n int) int {
		if n < 0 {
			return 0
		}
		return n
	}
	u.PromptTokens = clamp(u.PromptTokens)
	u.CompletionTokens = clamp(u.CompletionTokens)
	u.TotalTokens = clamp(u.TotalTokens)
	u.CachedTokens = clamp(u.CachedTokens)
	u.ReasoningTokens = clamp(u.ReasoningTokens)
	u.CacheWriteTokens = clamp(u.CacheWriteTokens)
	u.CacheWriteTokens5m = clamp(u.CacheWriteTokens5m)
	u.CacheWriteTokens1h = clamp(u.CacheWriteTokens1h)
	return u
}

// ChatResponse is a completed (non-streaming or reassembled streaming)
// chat completion. ToolCalls carries the model's tool calls (empty
// for a plain text turn) and FinishReason the upstream finish reason
// ("stop", "length", "tool_calls", ...); both are "" when the upstream
// did not report them. Reasoning carries the reasoning state captured
// from the upstream response, when any was present: router-internal
// (json:"-"), stored by the engine in the replay store so the next
// turn can restore it — never sent to the client.
type ChatResponse struct {
	Content      string           `json:"content"`
	Usage        Usage            `json:"usage"`
	Model        string           `json:"model"`
	ToolCalls    []ToolCall       `json:"tool_calls,omitempty"`
	FinishReason string           `json:"finish_reason,omitempty"`
	Reasoning    *reasoning.State `json:"-"`
	// RateLimit carries the upstream rate-limit headers parsed
	// from this response (M5.13) — nil when the upstream sent no
	// rate-limit headers. The router learns from it (pacing +
	// persistence); it is never serialized to callers.
	RateLimit *adaptive.RateLimitSignals `json:"-"`
}

// ChunkToolCall is one tool-call delta inside a streaming chunk:
// Index identifies the call, ID/Name arrive on its first delta,
// Arguments carries a fragment (streaming) or the complete arguments
// (reassembled response).
type ChunkToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// StreamChunk is one chunk of a streaming chat completion, in
// provider-neutral form: Content is a text delta ("" when the chunk
// carries only tool-call deltas), ToolCalls the tool-call deltas of
// this chunk, FinishReason the upstream finish reason ("" until the
// final chunk), Usage the usage block when the upstream sent one
// (nil otherwise).
type StreamChunk struct {
	Content      string          `json:"content,omitempty"`
	ToolCalls    []ChunkToolCall `json:"tool_calls,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
}

// ChunkStreamer is optionally implemented by Clients that can stream
// chunk-aware: onChunk is called once per upstream chunk, in order,
// with the full chunk (text delta, tool-call deltas, finish reason,
// usage) — not just a text string. The returned ChatResponse holds
// the reassembled result (content, completed tool calls, finish
// reason, usage). Clients that do not implement it stream text-only
// through ChatStream; the router and server prefer this interface
// when present and fall back otherwise.
type ChunkStreamer interface {
	ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error)
}

// RateLimitError is returned when the upstream answers 429. RetryAfter is
// parsed from the Retry-After header (seconds, or an HTTP date).
// RetryAfterPresent tells a present-but-zero hint apart from an absent
// one: it is true when the header was present and parsed (including an
// explicit "0"), false when the header is absent or unparsable —
// callers must then apply their own backoff. The distinction matters
// for the retry rules (internal/router, CHECKLIST 167-168): a present
// Retry-After below 10s is floored to 10s (an explicit 0 is a poisoned
// value, never a licence to retry immediately), while an absent hint
// keeps exponential backoff.
type RateLimitError struct {
	RetryAfter        time.Duration
	RetryAfterPresent bool
	Body              string
	// Signals carries the upstream rate-limit headers parsed from
	// the 429 response (M5.13) — nil when the upstream sent none.
	// The router learns from it the same way it learns from a
	// successful response's headers.
	Signals *adaptive.RateLimitSignals
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("upstream rate limited, retry after %s", e.RetryAfter)
	}
	return "upstream rate limited"
}

// QuotaError is returned when the upstream reports exhausted quota /
// billing problems (HTTP 402, or a 4xx whose body mentions quota,
// insufficient_quota, billing, or "exceeded"). Unlike a rate limit this is
// not transient: retrying the same provider will not help.
type QuotaError struct {
	Message    string
	StatusCode int
	Body       string
}

func (e *QuotaError) Error() string {
	if e == nil {
		return "upstream quota exhausted"
	}
	if e.Message != "" {
		return fmt.Sprintf("upstream quota error: %s", e.Message)
	}
	return "upstream quota exhausted"
}

// UpstreamError is any other non-2xx upstream response. Retryable is true
// for the statuses in the shared taxonomy (internal/wire.RetryableStatus:
// 402, 404, 408, 429 — only when not already mapped to RateLimitError,
// it always is, so this is defensive — and 5xx/529). 405/501 are
// non-retryable on chat completions (retryable only on /v1/responses
// and /v1/embeddings, which this client never calls).
type UpstreamError struct {
	StatusCode int
	Retryable  bool
	Body       string
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "upstream error"
	}
	return fmt.Sprintf("upstream error: HTTP %d%s", e.StatusCode, truncateForError(e.Body))
}

func truncateForError(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return ": " + body
}

// Client is an OpenAI-compatible upstream client. Implementations must be
// safe for concurrent use.
type Client interface {
	// Chat performs a single non-streaming chat completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream performs a streaming chat completion, calling onDelta for
	// each content delta in order. The returned ChatResponse holds the
	// reassembled full content. If onDelta returns an error, streaming
	// stops and that error is returned.
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error)
	// ListModels returns the model IDs from GET {base}/models.
	ListModels(ctx context.Context) ([]string, error)
	// TestKey verifies the API key with a tiny ListModels call.
	TestKey(ctx context.Context) error
}

// ClientOption configures an openAIClient.
type ClientOption func(*openAIClient)

// WithHTTPClient overrides the HTTP client (tests, custom transports).
// A client supplied this way keeps its own Transport: the TFO dialer
// is only installed on the default client built by NewClient.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *openAIClient) { c.httpClient = hc; c.customHTTP = true }
}

// WithTimeout sets the timeout for non-streaming requests. Streaming
// requests always run with no client-level timeout (context controls them).
func WithTimeout(d time.Duration) ClientOption {
	return func(c *openAIClient) { c.httpClient.Timeout = d }
}

// WithAllowPrivateDestinations allows the provider base URL to use
// loopback, private, link-local or cloud-metadata addresses. This is
// an operator override for on-prem models or test mocks; the default
// (false) rejects such destinations to prevent SSRF (7A).
func WithAllowPrivateDestinations(allow bool) ClientOption {
	return func(c *openAIClient) {
		c.allowPrivateDestinations = allow
		c.bypassEgressGuard = allow
	}
}

// WithAuthHeaders sets the exact auth headers sent on every upstream
// request, as resolved by the catalog for one (provider, endpoint)
// pair (see catalog.ResolveAuth). A nil map means "legacy": send
// `Authorization: Bearer <key>`. A non-nil map — possibly empty, for
// no-auth surfaces — replaces the Bearer default entirely, so a
// provider that needs `x-api-key` (DeepSeek's Anthropic surface),
// `api-key` (Azure), `X-Fireworks-Api-Key`, or extra headers like
// `anthropic-version` gets exactly those headers and nothing else.
// Exchange surfaces never reach this option: the resolver returns an
// error instead of headers, so no static header is ever faked for them.
func WithAuthHeaders(headers map[string]string) ClientOption {
	return func(c *openAIClient) { c.authHeaders = headers }
}

// WithWireFormat selects the wire format the client speaks.
// "anthropic" returns a Messages client (internal/wire), "gemini" a
// generateContent client (internal/wire), "cohere" a Chat API v2
// client (internal/wire), "bedrock-converse" a Converse client
// (internal/wire, SigV4-signed); "" and "openai" return the
// OpenAI-compatible client. Any other format (ollama, unknown) also
// returns the OpenAI client for now — those native transports are
// not built yet, and the previous behaviour was to speak OpenAI wire
// to them and fail over on the upstream error, so falling back
// preserves it instead of breaking ClientFor for providers added
// before this option existed.
func WithWireFormat(format string) ClientOption {
	return func(c *openAIClient) { c.wireFormat = format }
}

// openAIClient is the Client implementation for OpenAI-compatible upstreams.
// HTTPAdapterFunc rewrites one outgoing upstream HTTP request for
// a named provider (the plugin provider-adapter hook, CHECKLIST
// 201; see internal/plugins). It is a plain func type, not an
// import of the plugins package, so this package stays a leaf:
// the plugins registry implements it in main. A nil func means no
// adapter. An error fails the attempt closed — the unadapted
// request is never sent — and surfaces as an upstream failure for
// that attempt, so the engine's normal failover may still try the
// next provider.
type HTTPAdapterFunc func(ctx context.Context, providerID string, req *http.Request) (*http.Request, error)

// ctxKeyExtraHeaders carries plugin-added outbound headers (the
// pre-request hook's header set) from the server handler to the
// upstream client through the engine. Values flow with the request
// context; a derived context (timeout, cancel) keeps them.
type ctxKeyExtraHeaders struct{}

// WithExtraHeaders returns a context carrying extra outbound
// headers for the upstream request. The map is copied, so later
// mutation by the caller cannot change what is sent.
func WithExtraHeaders(ctx context.Context, headers map[string]string) context.Context {
	if len(headers) == 0 {
		return ctx
	}
	cp := make(map[string]string, len(headers))
	for k, v := range headers {
		cp[k] = v
	}
	return context.WithValue(ctx, ctxKeyExtraHeaders{}, cp)
}

// extraHeadersFrom returns the plugin-added outbound headers
// carried by ctx, or nil.
func extraHeadersFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(ctxKeyExtraHeaders{}).(map[string]string)
	return h
}

// reservedPluginHeaders are the headers a plugin may never set: the
// credential header, the body headers, and the host. A plugin that
// tries is silently skipped for that header (the adapter hook is
// the escape hatch for a provider that genuinely needs a different
// auth header — and it is explicit, per-provider, and reviewed).
var reservedPluginHeaders = map[string]bool{
	"authorization":  true,
	"content-type":   true,
	"content-length": true,
	"accept":         true,
	"host":           true,
}

type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// customHTTP is true when WithHTTPClient supplied the client: its
	// Transport is the caller's, and NewClient must not replace it
	// with the TFO transport.
	customHTTP               bool
	allowPrivateDestinations bool
	bypassEgressGuard        bool
	// tfoEnabled is whether the default client's dialer attempts TCP
	// Fast Open (default true; see WithTCPFastOpen). tfoSet records
	// that the option was given, so the default stays enabled when
	// it was not.
	tfoEnabled bool
	tfoSet     bool
	// authHeaders, when non-nil, are the exact auth headers to send
	// (resolved per provider + endpoint by the catalog). Nil means the
	// legacy default: Authorization: Bearer <apiKey>.
	authHeaders map[string]string
	// wireFormat selects the wire spoken by NewClient: "" / "openai"
	// stays here, "anthropic" builds a Messages client, "gemini"
	// a generateContent client, "cohere" a cohereClient,
	// "bedrock-converse" a bedrockClient wrapping this one, and
	// "vertex" a vertexClient (Vertex AI Gemini, OAuth2 token
	// minted from the service-account JSON key — see
	// vertex_client.go), anything else falls back to OpenAI wire
	// (see WithWireFormat).
	wireFormat string
	// providerID is the registry ID of the provider this client
	// serves ("" for ad-hoc clients built outside the registry,
	// e.g. probes). It is passed to httpAdapter so one func can
	// serve many providers. Set by the registry only.
	providerID string
	// httpAdapter, when non-nil, rewrites each outgoing request
	// through the plugin adapter for providerID (CHECKLIST 201).
	// It runs after auth + plugin headers are set, so an adapter
	// sees the final request. Set by the registry only.
	httpAdapter HTTPAdapterFunc
}

// setHTTPAdapter attaches a provider-adapter hook to this client
// (registry only). The method is promoted through the embedded
// *openAIClient to every native-wire wrapper, so one type assertion
// in the registry covers all wires.
func (c *openAIClient) setHTTPAdapter(providerID string, fn HTTPAdapterFunc) {
	c.providerID = providerID
	c.httpAdapter = fn
}

// NewClient builds a Client for an upstream. baseURL is the API root: it
// may already end in "/v1" (e.g. https://api.codvian.com/v1) or be a
// bare host root (e.g. https://api.openai.com); the client appends
// "/v1/..." only when the path does not already end in "/v1". The wire
// format comes from WithWireFormat: "anthropic" returns a Messages
// client, "gemini" a generateContent client, "cohere" a Chat API v2
// client, "bedrock-converse" a Converse client (SigV4-signed), and
// "vertex" a Vertex AI client (OAuth2 token minted from the
// service-account JSON key), everything else the OpenAI-compatible
// client.
func NewClient(baseURL, apiKey string, opts ...ClientOption) (Client, error) {
	c := &openAIClient{
		baseURL:    "",
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		tfoEnabled: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	normalized, err := ValidateBaseURL(baseURL, c.allowPrivateDestinations)
	if err != nil {
		return nil, err
	}
	c.baseURL = normalized
	// The default client dials through the TFO transport: the
	// Control hook attempts TCP_FASTOPEN_CONNECT (Linux) and falls
	// back to a normal handshake when the platform or a middlebox
	// refuses it — it never fails a dial. A caller-supplied client
	// (WithHTTPClient) keeps its own Transport untouched.
	if !c.customHTTP {
		c.httpClient.Transport = tfoTransport(c.tfoEnabled, c.bypassEgressGuard)
	}
	if c.wireFormat == WireFormatAnthropic {
		return &anthropicClient{openAIClient: c}, nil
	}
	if c.wireFormat == WireFormatGemini {
		return &geminiClient{openAIClient: c}, nil
	}
	if c.wireFormat == WireFormatCohere {
		return &cohereClient{openAIClient: c}, nil
	}
	if c.wireFormat == WireFormatBedrock {
		return &bedrockClient{openAIClient: c}, nil
	}
	if c.wireFormat == WireFormatVertex {
		return &vertexClient{openAIClient: c}, nil
	}
	return c, nil
}

// attachPoolTransport swaps this client's transport for a warm pooled
// one (see internal/provider/pool.go). It is a no-op for a client
// built with WithHTTPClient: the caller owns that transport and the
// router must not replace it. The method is promoted through the
// embedded *openAIClient to every native-wire wrapper (Anthropic,
// Gemini, Cohere, Bedrock), so the registry can attach a pool to any
// client ClientFor builds with one type assertion.
func (c *openAIClient) attachPoolTransport(rt http.RoundTripper) {
	if c.customHTTP {
		return
	}
	c.httpClient.Transport = rt
}

// joinAPIPath joins base with an API segment, appending "/v1" first when
// the base path does not already end in "/v1".
func joinAPIPath(base, segment string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/" + strings.TrimPrefix(segment, "/")
	}
	return base + "/v1/" + strings.TrimPrefix(segment, "/")
}

// modelsURL is GET {base}/models (with the /v1 rule above).
func (c *openAIClient) modelsURL() string { return joinAPIPath(c.baseURL, "models") }

// chatURL is POST {base}/chat/completions (with the /v1 rule above).
func (c *openAIClient) chatURL() string {
	return joinAPIPath(c.baseURL, "chat/completions")
}

// maxErrorBody is how much of an error body we keep / read.
const maxErrorBody = 64 * 1024

// maxResponseBody caps non-streaming response bodies.
const maxResponseBody = 10 * 1024 * 1024

func (c *openAIClient) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if c.authHeaders != nil {
		// Catalog-resolved auth: exactly these headers, no Bearer
		// default. An empty map means the surface takes no credential
		// (local runtimes); the stored key is simply not sent.
		for k, v := range c.authHeaders {
			req.Header.Set(k, v)
		}
	} else if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Plugin-added outbound headers (CHECKLIST 201): from the
	// request context (set by the server after the pre-request
	// hooks ran). Reserved headers are skipped — a plugin can
	// never override auth or body headers through this path.
	for k, v := range extraHeadersFrom(ctx) {
		if reservedPluginHeaders[strings.ToLower(k)] {
			continue
		}
		req.Header.Set(k, v)
	}
	// Provider-adapter hook (CHECKLIST 201): the registered
	// adapter for this provider rewrites the outgoing request
	// (custom wire). An error fails closed — the unadapted
	// request is never sent.
	if c.httpAdapter != nil {
		adapted, err := c.httpAdapter(ctx, c.providerID, req)
		if err != nil {
			return nil, err
		}
		if adapted == nil {
			return nil, errors.New("provider adapter returned a nil request")
		}
		req = adapted
	}
	return req, nil
}

// parseRetryAfter parses a Retry-After header value: integer seconds, a
// fractional-second value, or an HTTP date. It reports whether the
// header was present and parsed: (0, true) for an explicit "0",
// (0, false) when the header is absent or unparsable. Callers must
// keep the two apart — an explicit 0 is a poisoned hint the router
// floors to 10s, while an absent hint means exponential backoff
// (see RateLimitError).
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

func isQuotaBody(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"insufficient_quota", "insufficient quota", "quota", "billing",
		"exceeded", "out of credits", "no credits",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// mapStatusError converts a non-2xx status + headers + body into the
// typed error for this package, for the chat-completions surface.
func mapStatusError(status int, h http.Header, body []byte) error {
	return mapStatusErrorForEndpoint(status, h, body, "chat/completions")
}

// mapStatusErrorForEndpoint is mapStatusError for one API surface:
// the endpoint decides the 405/501 rule (a target that lacks
// /v1/embeddings or /v1/rerank may still serve the same request on
// another target; on chat completions 405/501 is a client error).
// endpoint is the wire name ("chat/completions", "embeddings",
// "rerank", ...); see wire.RetryableStatusForEndpoint.
func mapStatusErrorForEndpoint(status int, h http.Header, body []byte, endpoint string) error {
	trimmed := string(body)
	if len(trimmed) > maxErrorBody {
		trimmed = trimmed[:maxErrorBody]
	}
	// Log redaction, default-on (entries 150/152): an upstream
	// error body is untrusted text — it can quote the request,
	// including an Authorization header or an api_key field — so
	// key-shaped material is stripped here, at construction, before
	// the body can reach any log line, trace, or error string.
	// (Prompt/response text an upstream echoes verbatim is handled
	// downstream: the server log line for stream failures carries
	// the status code only, never the body.)
	trimmed = guardrails.Redact(trimmed)
	switch {
	case status == http.StatusTooManyRequests:
		ra, raPresent := parseRetryAfter(h)
		if !raPresent {
			// No usable header: fall back to a hint carried in
			// the error body (M4.4 — Gemini RetryInfo,
			// "retry in Xs", ISO, "resets in XhYmZs"). A
			// present, parsable header always wins.
			ra, raPresent = ParseRetryAfterBody(trimmed)
		}
		// Rate-limit learning (M5.13): the 429's own headers
		// carry the upstream's remaining budget — the router
		// learns from them the same way it learns from a
		// successful response.
		signals := adaptive.ParseRateLimitHeaders(h, time.Now())
		return &RateLimitError{RetryAfter: ra, RetryAfterPresent: raPresent, Body: trimmed, Signals: &signals}
	case status == http.StatusPaymentRequired ||
		(status >= 400 && status < 500 && isQuotaBody(trimmed)):
		msg := parseAPIErrorMessage(body)
		if msg == "" {
			msg = strings.TrimSpace(trimmed)
		}
		return &QuotaError{Message: guardrails.Redact(msg), StatusCode: status, Body: trimmed}
	default:
		return &UpstreamError{
			StatusCode: status,
			// Shared retry taxonomy (internal/wire, research-log
			// entry 183): 402/404/408/429/5xx/529 are properties
			// of that target, so another target may serve; the
			// surface keeps 405/501 non-retryable only on chat
			// completions (they are client errors here — only
			// /v1/responses, /v1/embeddings and /v1/rerank
			// treat them as "target lacks the endpoint").
			// Entry 151 adds the 400 body-signal layer on top:
			// a 400 that means "model not found at this target"
			// is retryable even though a plain 400 is not — see
			// ClassifyError / IsModelNotFoundSignal.
			Retryable: wire.RetryableStatusForEndpoint(status, endpoint) ||
				(status == http.StatusBadRequest && IsModelNotFoundSignal(trimmed)),
			Body: trimmed,
		}
	}
}

// apiErrorEnvelope handles both {"error": {"message": ...}} and
// {"error": "plain string"} shapes.
type apiErrorEnvelope struct {
	Error json.RawMessage `json:"error"`
}

type apiErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// parseAPIErrorMessage extracts the human message from an upstream error
// body, or "" when there is none.
func parseAPIErrorMessage(body []byte) string {
	var env apiErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Error) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(env.Error, &asString); err == nil {
		return asString
	}
	var obj apiErrorObject
	if err := json.Unmarshal(env.Error, &obj); err == nil {
		return obj.Message
	}
	return ""
}

// do performs the request and returns the body bytes for a 2xx response,
// or a typed error otherwise. Network errors are returned as-is (the
// caller / router treats them as retryable transport failures).
func (c *openAIClient) do(req *http.Request) ([]byte, http.Header, error) {
	return c.doForEndpoint(req, "chat/completions")
}

// doForEndpoint is do for one API surface: the endpoint decides
// the 405/501 rule in the mapped error (see
// mapStatusErrorForEndpoint). Embeddings and rerank use it with
// their own surface names; chat uses do (chat/completions).
func (c *openAIClient) doForEndpoint(req *http.Request, endpoint string) ([]byte, http.Header, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, nil, fmt.Errorf("reading upstream response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, mapStatusErrorForEndpoint(resp.StatusCode, resp.Header, body, endpoint)
	}
	return body, resp.Header, nil
}

// WireFormat returns the wire format this client speaks
// ("openai", "anthropic", "gemini", "cohere", "bedrock-converse"):
// the router uses it to label reasoning handoffs per hop and to
// decide whether a captured reasoning state is representable on
// the next target. Native clients embed *openAIClient, so they
// inherit this and report the format they were built with.
func (c *openAIClient) WireFormat() string {
	if c == nil || c.wireFormat == "" {
		return WireFormatOpenAI
	}
	return c.wireFormat
}

// chatMessageWire is the wire shape of one message sent to an
// OpenAI-compatible upstream: the gateway Message plus the
// reasoning_content field for assistant messages carrying replay
// state. It exists as a separate type so Message.MarshalJSON never
// emits reasoning_content — client-facing encodings must not leak
// internal reasoning state.
type chatMessageWire struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// chatCompletionRequest is the body of POST /v1/chat/completions
// chatCompletionRequest is the body of POST /v1/chat/completions.
// PromptCacheKey is the client's own key (a JSON string, or an
// explicit JSON null) when one was supplied, else the
// router-generated auto key for OpenAI's own API, else omitted.
type chatCompletionRequest struct {
	Model           string             `json:"model"`
	Messages        []chatMessageWire  `json:"messages"`
	Stream          bool               `json:"stream"`
	StreamOptions   *streamOptionsWire `json:"stream_options,omitempty"`
	MaxTokens       int                `json:"max_tokens,omitempty"`
	Temperature     float64            `json:"temperature,omitempty"`
	TopP            float64            `json:"top_p,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	Stop            []string           `json:"stop,omitempty"`
	Tools           []ToolDefinition   `json:"tools,omitempty"`
	ToolChoice      json.RawMessage    `json:"tool_choice,omitempty"`
	PromptCacheKey  json.RawMessage    `json:"prompt_cache_key,omitempty"`
}

// streamOptionsWire is the OpenAI stream_options block. G1
// (streaming usage accounting): the router always asks for
// include_usage on OpenAI-wire streams so the upstream's final
// usage chunk reaches the ledger as provider-reported tokens
// instead of a local estimate. OpenAI itself omits streaming
// usage without this flag; most OpenAI-compatible providers
// either honor it or ignore the unknown field. The aggregated
// usage is decoded by the same usageWire path as non-streaming
// responses (cached/reasoning splits and reported cost included).
type streamOptionsWire struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAIToolCallDelta is one tool-call delta inside a streaming chunk.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// chatDelta is the delta of one streaming choice. ReasoningContent
// carries a reasoning_content fragment (OpenAI wire) when the
// upstream streams one; it is accumulated into the response's
// reasoning state, never emitted as content.
type chatDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls,omitempty"`
}

// chatCompletionChoice is one choice of a chat completion, streaming
// or not: Message for the full reply, Delta for a chunk.
type chatCompletionChoice struct {
	Message      Message   `json:"message"`
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   usageWire              `json:"usage"`
}

// usageWire is the decode shape for an OpenAI usage block: the flat
// token counts plus the nested prompt_tokens_details carrying the
// cached-token split. OpenAI nests the details inside "usage", not
// at the response top level.
//
// It also decodes the two provider-reported exact-cost shapes
// (G16, Area 11 gateway-reported cost): OpenRouter's usage.cost
// (bare USD) and xAI's cost_in_usd_ticks (1e10 ticks per USD —
// the reference router costCalculator.ts extractExactCostUsd, verified
// against the xAI cost-tracking docs, which state
// TICKS_IN_USD_CENT = 100_000_000, i.e. 1e10 ticks per USD).
// Either one, when present and valid (>= 0, finite), becomes
// Usage.Cost in usage(); a cost field from any other provider is
// taken at face value too — it is the upstream's own billing
// number, and the ledger prefers it over the hand-computed cost.
// cost_in_usd_ticks wins over cost when both are present (it is
// the more specific wire shape); a negative / non-finite value
// in either field is ignored, never trusted.
type usageWire struct {
	Usage
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	Cost           *float64 `json:"cost,omitempty"`
	CostInUSDTicks *float64 `json:"cost_in_usd_ticks,omitempty"`
}

// usdTicksPerDollar is the xAI cost_in_usd_ticks divisor: 1e10
// ticks per USD (xAI docs: TICKS_IN_USD_CENT = 100_000_000).
const usdTicksPerDollar = 10_000_000_000

// usage returns the response's usage with the provider-reported
// cached-token count folded in (0 when the upstream reported none).
func (r chatCompletionResponse) usage() Usage {
	u := r.Usage.Usage
	if r.Usage.PromptTokensDetails != nil {
		u.CachedTokens = r.Usage.PromptTokensDetails.CachedTokens
	}
	if r.Usage.CompletionTokensDetails != nil {
		u.ReasoningTokens = r.Usage.CompletionTokensDetails.ReasoningTokens
	}
	if r.Usage.CostInUSDTicks != nil && *r.Usage.CostInUSDTicks >= 0 && !math.IsNaN(*r.Usage.CostInUSDTicks) && !math.IsInf(*r.Usage.CostInUSDTicks, 0) {
		v := *r.Usage.CostInUSDTicks / usdTicksPerDollar
		u.Cost = &v
	} else if r.Usage.Cost != nil && *r.Usage.Cost >= 0 && !math.IsNaN(*r.Usage.Cost) && !math.IsInf(*r.Usage.Cost, 0) {
		v := *r.Usage.Cost
		u.Cost = &v
	}
	return u
}

// cacheKeyField returns the prompt_cache_key value for the wire
// request: the client's own key (a JSON string, or an explicit null)
// when one was supplied, else the router-generated auto key when
// this client speaks to OpenAI's own API with a known caller and a
// shared prefix, else nil (field omitted). It is computed here, at
// send time, per attempt — never stored on the request, never used
// for routing.
func (c *openAIClient) cacheKeyField(req ChatRequest) json.RawMessage {
	if req.PromptCacheKeySet {
		if req.PromptCacheKey == nil {
			return json.RawMessage("null")
		}
		b, err := json.Marshal(*req.PromptCacheKey)
		if err != nil {
			return nil
		}
		return b
	}
	if !IsOpenAIAPI(c.baseURL) {
		return nil
	}
	key, ok := AutoPromptCacheKey(req.CallerID, CacheKeyEndpointChatCompletions, req)
	if !ok {
		return nil
	}
	b, err := json.Marshal(key)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Prompt-cache markers for OpenRouter (G13, parity tracker Area 6 row
// "Provider prompt-cache markers").
//
// OpenRouter's chat-completions API honours explicit cache_control
// breakpoints on content blocks — but ONLY for the Claude, Qwen
// and Gemini model families (OpenRouter docs,
// openrouter.ai/docs/guides/best-practices/prompt-caching: "Place
// cache_control directly on individual content blocks", 5-minute
// default, 1-hour via "ttl": "1h"). Other families on the same
// endpoint either cache implicitly (OpenAI models) or ignore the
// marker, so annotating blindly would be a fake promise: the
// marker is gated to those families by model id, and to the
// provider id "openrouter" (the registry sets it via
// setHTTPAdapter; ad-hoc clients carry "" and are never marked).
//
// The Anthropic-endpoint variant of OpenRouter is NOT marked here
// and NOT in the Anthropic client either: the model behind that
// endpoint is not guaranteed to be Claude, and OpenRouter's marker
// translation for non-Claude models on the Messages endpoint is
// not verified against primary docs (fail closed — see
// promptCacheMarkerProviders in anthropic_client.go).
//
// Mock-only verification: no live upstream verification exists in
// this repo for any prompt-cache marker (M6.9c); these tests drive
// the mock upstream and assert on the outbound JSON, exactly like
// the Anthropic marker tests.
// ---------------------------------------------------------------------------

// openRouterCacheableModel reports whether the model id names a
// family OpenRouter documents as honouring explicit cache_control
// breakpoints on chat-completions: Claude, Qwen, Gemini.
// OpenRouter model ids carry the vendor prefix
// ("anthropic/claude-...", "qwen/qwen-...", "google/gemini-..."),
// so a substring match on the family name is the documented
// signal — but it IS a heuristic on the id string, not a catalog
// lookup: a future family OpenRouter adds will stay unmarked
// until this list grows, which fails closed (a missed saving,
// never a wrong annotation).
func openRouterCacheableModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "claude") || strings.Contains(m, "qwen") || strings.Contains(m, "gemini")
}

// openRouterPromptCacheEligible is the M6.5 cacheability gate for
// the OpenRouter OpenAI-wire path: provider openrouter, a
// cacheable model family, temperature exactly 0, prompt >= 1024
// estimated tokens, and no in-flight tool calls / tool-role
// messages (a cached prefix ending mid-tool-flow cannot be
// reused by the local cache). Tool DEFINITIONS do not
// disqualify — the last one is marked (see
// applyOpenRouterPromptCacheMarker).
func (c *openAIClient) openRouterPromptCacheEligible(req ChatRequest) bool {
	if c == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(c.providerID)) != "openrouter" {
		return false
	}
	if req.Temperature != 0 {
		return false
	}
	if !openRouterCacheableModel(req.Model) {
		return false
	}
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || m.Role == "tool" {
			return false
		}
	}
	return estimatedPromptTokens(req) >= minPromptCacheTokens
}

// openRouterMarkerCount counts cache_control markers already on
// the wire request — client-supplied ones (preserved verbatim
// through decode) plus any the auto-marker placed. OpenRouter
// documents the same 4-breakpoint maximum as Anthropic.
func openRouterMarkerCount(wireReq *chatCompletionRequest) int {
	n := 0
	for _, m := range wireReq.Messages {
		var parts []ContentPart
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if p.CacheControl != nil {
					n++
				}
			}
		}
	}
	for _, t := range wireReq.Tools {
		if t.CacheControl != nil {
			n++
		}
	}
	return n
}

// markOpenRouterMessageParts marks the last non-empty text part of
// one wire message's content. String-form content is converted to
// a single marked text part (OpenAI-compatible upstreams accept
// both forms); part-array content is marked in place. It reports
// whether a marker was added: a part that already carries a
// client-supplied marker is left untouched and counts as
// already-marked, so the client keeps exactly the breakpoint it
// asked for.
func markOpenRouterMessageParts(content json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return content, false
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		if asString == "" {
			return content, false
		}
		b, err := json.Marshal([]ContentPart{{
			Type: "text", Text: asString,
			CacheControl: &wire.AnthropicCacheControl{Type: "ephemeral"},
		}})
		if err != nil {
			return content, false
		}
		return b, true
	}
	var parts []ContentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return content, false
	}
	for j := len(parts) - 1; j >= 0; j-- {
		if parts[j].Type != "text" || strings.TrimSpace(parts[j].Text) == "" {
			continue
		}
		if parts[j].CacheControl != nil {
			return content, false
		}
		parts[j].CacheControl = &wire.AnthropicCacheControl{Type: "ephemeral"}
		b, err := json.Marshal(parts)
		if err != nil {
			return content, false
		}
		return b, true
	}
	return content, false
}

// applyOpenRouterPromptCacheMarker sets cache_control
// {"type": "ephemeral"} on up to three slots of an
// OpenRouter-bound OpenAI-wire request, mirroring the Anthropic
// client's placement: the last user message's last text part,
// the last system/developer message's last text part, and the
// last tool definition. A slot that already carries a
// client-supplied marker is left as sent, and no auto-marker is
// added once 4 markers are present. No TTL is set (provider
// default 5-minute tier). A no-op when the gate fails.
func (c *openAIClient) applyOpenRouterPromptCacheMarker(wireReq *chatCompletionRequest, req ChatRequest) {
	if wireReq == nil || !c.openRouterPromptCacheEligible(req) {
		return
	}
	const maxOpenRouterMarkers = 4
	marked := openRouterMarkerCount(wireReq)
	// 1. Last user message.
	if marked < maxOpenRouterMarkers {
		for i := len(wireReq.Messages) - 1; i >= 0; i-- {
			if wireReq.Messages[i].Role != "user" {
				continue
			}
			if out, ok := markOpenRouterMessageParts(wireReq.Messages[i].Content); ok {
				wireReq.Messages[i].Content = out
				marked++
			}
			break
		}
	}
	// 2. Last system / developer message.
	if marked < maxOpenRouterMarkers {
		for i := len(wireReq.Messages) - 1; i >= 0; i-- {
			if wireReq.Messages[i].Role != "system" && wireReq.Messages[i].Role != "developer" {
				continue
			}
			if out, ok := markOpenRouterMessageParts(wireReq.Messages[i].Content); ok {
				wireReq.Messages[i].Content = out
				marked++
			}
			break
		}
	}
	// 3. Last tool definition.
	if marked < maxOpenRouterMarkers && len(wireReq.Tools) > 0 {
		last := &wireReq.Tools[len(wireReq.Tools)-1]
		if last.CacheControl == nil {
			last.CacheControl = &wire.AnthropicCacheControl{Type: "ephemeral"}
		}
	}
}

// toChatCompletionRequest converts a provider ChatRequest to the wire shape.
// An assistant message carrying restored reasoning state gets its
// reasoning_content (the state's text, for replay-capable models
// only — see reasoning.ReplayModel); a state the target model
// cannot represent returns *reasoning.UnrepresentableError, so the
// router fails over to a target that can carry it instead of
// silently dropping the reasoning.
func (c *openAIClient) toChatCompletionRequest(req ChatRequest) (chatCompletionRequest, error) {
	messages := make([]chatMessageWire, 0, len(req.Messages))
	for i, m := range req.Messages {
		wm := chatMessageWire{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		switch {
		case len(m.Parts) > 0:
			wm.Content = mustJSONRaw(m.Parts)
		case m.Content != "":
			wm.Content = mustJSONRaw(m.Content)
		default:
			// No content (e.g. an assistant message carrying only
			// tool calls): leave Content nil so it encodes as null.
		}
		if m.Reasoning != nil && !m.Reasoning.Empty() {
			if _, _, err := reasoning.Check(m.Reasoning, reasoning.WireOpenAI, req.Model); err != nil {
				return chatCompletionRequest{}, fmt.Errorf("messages[%d] reasoning: %w", i, err)
			}
			wm.ReasoningContent = m.Reasoning.ReasoningText()
		}
		messages = append(messages, wm)
	}
	out := chatCompletionRequest{
		Model:           req.Model,
		Messages:        messages,
		Stream:          req.Stream,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		ReasoningEffort: req.ReasoningEffort,
		Stop:            req.Stop,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		PromptCacheKey:  c.cacheKeyField(req),
	}
	// G1: ask for the final usage chunk on every OpenAI-wire
	// stream. Non-streaming requests carry usage in the response
	// body already and never set this block.
	if req.Stream {
		out.StreamOptions = &streamOptionsWire{IncludeUsage: true}
	}
	// G13: the OpenRouter prompt-cache marker is applied here, at
	// send time — toChatCompletionRequest is the single funnel for
	// Chat and ChatStream, so both paths are marked identically,
	// and non-OpenRouter providers pass through unmarked (the
	// gate checks providerID).
	c.applyOpenRouterPromptCacheMarker(&out, req)
	return out, nil
}

// MergeToolCallDeltas folds streaming tool-call deltas into the
// accumulated calls (keyed by index): the first delta sets ID/Name,
// later deltas append argument fragments.
func MergeToolCallDeltas(calls []ToolCall, deltas []ChunkToolCall) []ToolCall {
	for _, d := range deltas {
		for len(calls) <= d.Index {
			calls = append(calls, ToolCall{})
		}
		call := &calls[d.Index]
		if d.ID != "" {
			call.ID = d.ID
		}
		if call.Type == "" {
			call.Type = "function"
		}
		if d.Name != "" {
			call.Function.Name = d.Name
		}
		call.Function.Arguments += d.Arguments
	}
	return calls
}

func (c *openAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	r := req
	r.Stream = false // Chat is the non-streaming call, whatever the caller set.
	wireReq, err := c.toChatCompletionRequest(r)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("encoding chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
	if err != nil {
		return nil, err
	}
	// Rate-limit learning (M5.13): the response headers carry the
	// upstream's remaining budget — parse them onto the response
	// so the router can pace the adaptive limiter before the 429.
	body, header, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	signals := adaptive.ParseRateLimitHeaders(header, time.Now())
	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed chat response: " + err.Error()}
	}
	if len(decoded.Choices) == 0 {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "upstream returned no choices"}
	}
	choice := decoded.Choices[0]
	return &ChatResponse{
		Content:      choice.Message.Content,
		Usage:        decoded.usage(),
		Model:        decoded.Model,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Reasoning:    choice.Message.Reasoning,
		RateLimit:    &signals,
	}, nil
}

// ChatStream is the text-only streaming entry point: it drains
// ChatStreamChunks and forwards only the content deltas. Chunk-aware
// callers (router, server) use ChatStreamChunks directly.
func (c *openAIClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return c.ChatStreamChunks(ctx, req, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		return onDelta(chunk.Content)
	})
}

// ChatStreamChunks performs a streaming chat completion, calling
// onChunk once per SSE chunk in order with the full chunk (text delta,
// tool-call deltas, finish reason, usage). The returned ChatResponse
// holds the reassembled result. If onChunk returns an error,
// streaming stops and that error is returned.
func (c *openAIClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	// Reuse the caller's request (forcing Stream) rather than a
	// fresh literal: the router-internal fields (CallerID, the
	// client's own prompt_cache_key) must survive into the wire
	// request — a rebuilt struct would silently drop them, and
	// the auto key would never fire on streams.
	r := req
	r.Stream = true
	wireReq, err := c.toChatCompletionRequest(r)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(wireReq)
	if err != nil {
		return nil, fmt.Errorf("encoding chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
	if err != nil {
		return nil, err
	}
	// No client-level timeout for streaming: a long stream must not be cut
	// off by http.Client.Timeout. The context still bounds the request.
	streamClient := *c.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapStatusError(resp.StatusCode, resp.Header, body)
	}
	// Rate-limit learning (M5.13): the stream-open headers carry
	// the upstream's remaining budget — parse them once and attach
	// them to the reassembled response below, so the router can
	// pace the adaptive limiter from streamed calls too.
	streamSignals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	var (
		full         strings.Builder
		model        string
		usage        Usage
		toolCalls    []ToolCall
		finishReason string
		done         bool
		reasoningBuf strings.Builder
	)
	emit := func(chunk StreamChunk) error {
		// A chunk already buffered by the scanner when the
		// caller cancels must never be delivered: the read
		// loop's next Scan would return buffered bytes without
		// touching the network (and without noticing the
		// cancelled transport), so a stale third chunk could
		// reach the caller after the abort (CHECKLIST 190 —
		// TestOpenAIStreamCancelStopsUpstreamReads flaked on
		// exactly that: "delivered 3 chunks, want 2"). The
		// caller's cancellation is the verdict: return it
		// unwrapped so the router still recognises it.
		if err := ctx.Err(); err != nil {
			return err
		}
		if chunk.Content != "" {
			full.WriteString(chunk.Content)
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = MergeToolCallDeltas(toolCalls, chunk.ToolCalls)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		return onChunk(chunk)
	}
	scanner := bufio.NewScanner(resp.Body)
	// Deltas can be large; allow up to 1 MiB per SSE line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank line or SSE comment
		}
		if strings.HasPrefix(line, "event:") {
			continue // event names carry no content for chat completions
		}
		if !strings.HasPrefix(line, "data:") {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed SSE line: " + guardrails.Redact(line)}
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunkResp chatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunkResp); err != nil {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed SSE data: " + guardrails.Redact(data)}
		}
		if chunkResp.Model != "" {
			model = chunkResp.Model
		}
		// Some providers send usage in the final chunk.
		if chunkResp.Usage.TotalTokens != 0 || chunkResp.Usage.PromptTokens != 0 || chunkResp.Usage.CompletionTokens != 0 {
			usage = chunkResp.usage()
		}
		if len(chunkResp.Choices) == 0 {
			continue
		}
		choice := chunkResp.Choices[0]
		// reasoning_content deltas accumulate into the response's
		// reasoning state (never into content, never emitted).
		if choice.Delta.ReasoningContent != "" {
			reasoningBuf.WriteString(choice.Delta.ReasoningContent)
		}
		chunk := StreamChunk{Content: choice.Delta.Content, FinishReason: choice.FinishReason}
		for _, tc := range choice.Delta.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, ChunkToolCall{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if chunkResp.Usage.TotalTokens != 0 || chunkResp.Usage.PromptTokens != 0 || chunkResp.Usage.CompletionTokens != 0 {
			u := chunkResp.usage()
			chunk.Usage = &u
		}
		if chunk.Content == "" && len(chunk.ToolCalls) == 0 && chunk.FinishReason == "" && chunk.Usage == nil {
			continue
		}
		if err := emit(chunk); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "error reading SSE stream: " + err.Error()}
	}
	if !done && full.Len() == 0 && len(toolCalls) == 0 {
		// A stream that ends without [DONE] and without any content is
		// treated as a truncated upstream response, not a success.
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: true, Body: "SSE stream ended without [DONE]"}
	}
	var state *reasoning.State
	if reasoningBuf.Len() > 0 {
		state = &reasoning.State{
			Blocks:      []reasoning.Block{{Kind: reasoning.KindThought, Text: reasoningBuf.String()}},
			SourceWire:  reasoning.WireOpenAI,
			SourceModel: model,
		}
	}
	return &ChatResponse{Content: full.String(), Usage: usage, Model: model, ToolCalls: toolCalls, FinishReason: finishReason, Reasoning: state, RateLimit: &streamSignals}, nil
}

type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (c *openAIClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, c.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	var decoded modelsListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (c *openAIClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}
