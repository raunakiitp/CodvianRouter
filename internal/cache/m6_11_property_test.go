// M6.11 — cache correctness property tests, cache layer.
//
// PLAN.md M6.11 (exit gate, correctness half): no cross-tenant
// serves, no temperature leaks, no truncated serves, and —
// per the M6.12 documented divergence — cross-provider SHARING
// for the same model id (NOT isolation). Each test below proves
// its property through REAL store calls (ExactCache.Set,
// LocalSemanticCache.SetTextForModel, ReasoningCache.SetStateForTenant)
// and the real EmbedText path; nothing here hand-waves a fixture.
//
// Toggle evidence: every property has an inversion control in the
// same test — the same setup with the distinguishing input removed
// (same tenant, same model, same temperature, complete finish)
// DOES hit. A test that only ever misses proves nothing; the
// controls prove these tests can tell hit from miss. The two
// documented limits are pinned, not hidden:
//   - the semantic layer records NO tenant (see
//     TestM611SemanticLayerIsTenantBlind): tenant isolation for
//     semantic hits lives in degrade.semanticBlocked, tested in
//     internal/degrade/m6_11_property_test.go — and the
//     cross-tenant PARAPHRASE gap there is a real, failing
//     probe (TestM611CrossTenantSemanticParaphraseGap);
//   - ShouldStore does NOT refuse finish_reason "max_tokens":
//     the wire layer normalises max_tokens -> "length" before
//     store ever sees it (wire/anthropic.go, wire/gemini.go,
//     wire/cohere.go, wire/bedrock.go; wire tests pin the
//     mapping), so "length" is the only truncated spelling this
//     layer must refuse.
package cache

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// m611KeyInput builds a ScopedKey input for one (tenant, model,
// prompt, temperature) combination.
func m611KeyInput(tenant, model, text string, temp float64) KeyInput {
	return KeyInput{
		Tenant:      tenant,
		Model:       model,
		Messages:    []Message{{Role: "user", Content: text}},
		Temperature: temp,
	}
}

func m611ScopedKey(t *testing.T, in KeyInput) string {
	t.Helper()
	k, err := ScopedKey(in)
	if err != nil {
		t.Fatalf("ScopedKey(%+v): %v", in, err)
	}
	return k
}

// TestM611ScopedKeyTenantModelTempMatrix pins the key rule the
// exact layer's tenant/temperature isolation rests on: the key
// differs across tenants, across models, and across temperature
// buckets — and is identical for identical inputs. Parameterised
// over 3 tenants x 2 models x 2 temperatures, so a collision in
// any one dimension fails loudly.
func TestM611ScopedKeyTenantModelTempMatrix(t *testing.T) {
	tenants := []string{"tenant-a", "tenant-b", ""}
	models := []string{"gpt-4o", "claude-sonnet-4"}
	temps := []float64{0, 0.7}
	seen := map[string]KeyInput{}
	for _, tenant := range tenants {
		for _, model := range models {
			for _, temp := range temps {
				in := m611KeyInput(tenant, model, "What is 2+2?", temp)
				k := m611ScopedKey(t, in)
				if prev, dup := seen[k]; dup {
					t.Fatalf("key collision: %+v and %+v share key %q", prev, in, k)
				}
				seen[k] = in
				// Determinism control: the same input keys the same.
				if k2 := m611ScopedKey(t, in); k2 != k {
					t.Fatalf("same input keyed differently: %q vs %q", k, k2)
				}
			}
		}
	}
	// Temperature buckets: 0.704 and 0.700 are the same bucket by
	// design (2-decimal buckets, see TemperatureBucket), 0.70 and
	// 0.71 are different buckets.
	if a, b := m611ScopedKey(t, m611KeyInput("t", "m", "x", 0.704)), m611ScopedKey(t, m611KeyInput("t", "m", "x", 0.700)); a != b {
		t.Fatalf("0.704 and 0.700 keyed differently (%q vs %q): same bucket must share", a, b)
	}
	if a, b := m611ScopedKey(t, m611KeyInput("t", "m", "x", 0.70)), m611ScopedKey(t, m611KeyInput("t", "m", "x", 0.71)); a == b {
		t.Fatalf("0.70 and 0.71 share key %q: different buckets must differ", a)
	}
	// Boundary note: 0.0001 buckets to "0.00", the SAME key as
	// temp 0 — the key does NOT carry the temperature rule. The
	// gate does (see TestM611CacheableTemperatureBoundary).
	if a, b := m611ScopedKey(t, m611KeyInput("t", "m", "x", 0.0001)), m611ScopedKey(t, m611KeyInput("t", "m", "x", 0)); a != b {
		t.Fatalf("0.0001 and 0 keyed differently (%q vs %q): 0.0001 buckets to 0.00", a, b)
	}
}

// TestM611CacheableTemperatureBoundary pins the exact temperature
// rule (cache.CacheableTemperature): 0 is always cacheable, any
// temperature above 0 — including the 0.0001 boundary the key
// bucketing cannot see — is cacheable only with the per-key
// allowance, and a negative temperature is never cacheable.
func TestM611CacheableTemperatureBoundary(t *testing.T) {
	cases := []struct {
		temp    float64
		allow   bool
		want    bool
		comment string
	}{
		{0, false, true, "temp 0, no allowance"},
		{0, true, true, "temp 0, allowance"},
		{0.0001, false, false, "temp 0.0001, no allowance (boundary: >0, same key bucket as 0)"},
		{0.0001, true, true, "temp 0.0001, allowance"},
		{0.1, false, false, "temp 0.1, no allowance"},
		{0.7, false, false, "temp 0.7, no allowance"},
		{0.7, true, true, "temp 0.7, allowance"},
		{1.0, false, false, "temp 1.0, no allowance"},
		{2.0, false, false, "temp 2.0, no allowance"},
		{-0.5, false, false, "negative temp, no allowance"},
		{-0.5, true, false, "negative temp, allowance (still never cacheable)"},
	}
	for _, tc := range cases {
		if got := CacheableTemperature(tc.temp, tc.allow); got != tc.want {
			t.Errorf("CacheableTemperature(%v, allow=%v) = %v, want %v (%s)", tc.temp, tc.allow, got, tc.want, tc.comment)
		}
	}
	// The bucket collision is real and the gate is what saves it:
	// temp 0.0001 shares temp 0's key, yet is not cacheable.
	if TemperatureBucket(0.0001) != TemperatureBucket(0) {
		t.Fatalf("TemperatureBucket(0.0001) = %q, want %q (same bucket as 0)", TemperatureBucket(0.0001), TemperatureBucket(0))
	}
}

// TestM611ShouldStoreMatrix pins the single store-time gate
// (cache.ShouldStore) over finish reason x temperature x
// allowance x tool calls. "max_tokens" is deliberately NOT
// refused here: the wire layer normalises it to "length" before
// store sees it, so refusing it here would double-count a rule
// owned upstream — and "length" in any case is refused.
func TestM611ShouldStoreMatrix(t *testing.T) {
	cases := []struct {
		finish string
		temp   float64
		allow  bool
		tools  bool
		want   bool
	}{
		{"stop", 0, false, false, true},
		{"", 0, false, false, true},           // unknown finish reason passes
		{"tool_calls", 0, false, false, true}, // finish reason alone does not refuse
		{"length", 0, false, false, false},
		{"LENGTH", 0, false, false, false}, // case-insensitive
		{"Length", 0, false, false, false},
		{"length", 0, true, false, false},     // allowance does not rescue truncation
		{"max_tokens", 0, false, false, true}, // NOT refused here: wire normalises to "length"
		{"stop", 0.7, false, false, false},    // hot without allowance
		{"stop", 0.7, true, false, true},      // hot with allowance
		{"stop", 0.0001, false, false, false}, // boundary hot without allowance
		{"stop", 0, false, true, false},       // tool calls never stored
		{"stop", 0, true, true, false},        // allowance does not rescue tool calls
		{"length", 0.7, true, true, false},    // every refusal at once
	}
	for _, tc := range cases {
		if got := ShouldStore(tc.finish, tc.temp, tc.allow, tc.tools); got != tc.want {
			t.Errorf("ShouldStore(%q, temp=%v, allow=%v, tools=%v) = %v, want %v",
				tc.finish, tc.temp, tc.allow, tc.tools, got, tc.want)
		}
	}
}

// TestM611ExactLayerTenantIsolation proves at the layer itself:
// an entry stored under tenant A's scoped key is never returned
// for tenant B's key — same prompt, same model, same temperature
// — while the same tenant hits. Parameterised over tenant pairs
// and models; all entries come from real Set calls.
func TestM611ExactLayerTenantIsolation(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-b"},
		{"gk-1", "gk-2"},
		{"", "gk-named"}, // anonymous ("") never collides with a named tenant
	}
	models := []string{"gpt-4o", "test-model"}
	ctx := context.Background()
	for _, pair := range pairs {
		for _, model := range models {
			c := NewExactCache(100)
			ka := m611ScopedKey(t, m611KeyInput(pair[0], model, "What is 2+2?", 0))
			kb := m611ScopedKey(t, m611KeyInput(pair[1], model, "What is 2+2?", 0))
			c.Set(ctx, ka, CachedResponse{Content: "4", Model: model}, time.Hour)
			// Control: the storing tenant hits its own entry.
			if got, ok := c.Get(ctx, ka); !ok || got.Content != "4" {
				t.Fatalf("tenants %q/%q model %q: storing tenant missed its own entry (found=%v got=%q)", pair[0], pair[1], model, ok, got.Content)
			}
			// Property: the other tenant misses.
			if got, ok := c.Get(ctx, kb); ok {
				t.Fatalf("tenants %q/%q model %q: tenant B served tenant A's entry %q", pair[0], pair[1], model, got.Content)
			}
		}
	}
}

// TestM611SemanticLayerModelScopingAndProviderSharing proves the
// semantic layer's two model rules through real EmbedText vectors
// (SetTextForModel / GetTextForModel):
//   - different MODEL never shares, however similar the text
//     (CHECKLIST 187);
//   - same MODEL shares — including across providers, because the
//     layer records model only, never provider (M6.12 divergence:
//     gpt-4o via OpenAI and via OpenRouter is the same scope).
//
// It also pins that the raw model-less Get never serves a
// model-scoped entry.
func TestM611SemanticLayerModelScopingAndProviderSharing(t *testing.T) {
	ctx := context.Background()
	models := []string{"gpt-4o", "claude-sonnet-4", "deepseek-chat"}
	stored := "What is the capital of France?"
	paraphrase := "What is the capital of France today?"
	for _, model := range models {
		sem := NewLocalSemanticCache()
		sem.SetTextForModel(ctx, model, stored, SemanticEntry{Response: CachedResponse{Content: "Paris", Model: model}}, time.Hour)
		// Control: same model, same text hits.
		if got, ok := sem.GetTextForModel(ctx, model, stored); !ok || got.Response.Content != "Paris" {
			t.Fatalf("model %q: same-model lookup missed its own entry (found=%v)", model, ok)
		}
		// Control: same model, paraphrase hits (real similarity).
		if got, ok := sem.GetTextForModel(ctx, model, paraphrase); !ok || got.Response.Content != "Paris" {
			t.Fatalf("model %q: same-model paraphrase missed (found=%v)", model, ok)
		}
		// Property: every other model misses, same text and paraphrase alike.
		for _, other := range models {
			if other == model {
				continue
			}
			if got, ok := sem.GetTextForModel(ctx, other, stored); ok {
				t.Fatalf("model %q entry served to model %q (same text): %q", model, other, got.Response.Content)
			}
			if got, ok := sem.GetTextForModel(ctx, other, paraphrase); ok {
				t.Fatalf("model %q entry served to model %q (paraphrase): %q", model, other, got.Response.Content)
			}
		}
		// Property: the raw model-less Get never serves a
		// model-scoped entry (unattributed-only rule).
		if got, ok := sem.GetText(ctx, stored); ok {
			t.Fatalf("model %q entry served by raw model-less Get: %q", model, got.Response.Content)
		}
	}
	// Cross-provider sharing, stated as the layer sees it: there
	// is no provider dimension to vary, so the entry stored for
	// "gpt-4o" (which routed to provider A) is served to the
	// identical "gpt-4o" lookup (which would route to provider B).
	// The key identity behind it is pinned in
	// TestM612SameModelSharedAcrossProviders; here the serve is
	// proven through both layers.
	sem := NewLocalSemanticCache()
	sem.SetTextForModel(ctx, "gpt-4o", stored, SemanticEntry{Response: CachedResponse{Content: "Paris", Model: "gpt-4o"}}, time.Hour)
	if got, ok := sem.GetTextForModel(ctx, "gpt-4o", stored); !ok || got.Response.Content != "Paris" {
		t.Fatalf("same-model semantic lookup missed the shared entry (found=%v)", ok)
	}
	exact := NewExactCache(10)
	k := m611ScopedKey(t, m611KeyInput("tenant-a", "gpt-4o", stored, 0))
	exact.Set(ctx, k, CachedResponse{Content: "Paris", Model: "gpt-4o"}, time.Hour)
	if got, ok := exact.Get(ctx, k); !ok || got.Content != "Paris" {
		t.Fatalf("same-model exact lookup missed the shared entry (found=%v)", ok)
	}
}

// TestM611SemanticLayerIsTenantBlind pins the layer-level fact
// the engine tests rely on: LocalSemanticCache records NO tenant,
// so it cannot distinguish callers by itself. A stored entry is
// retrievable by any caller naming the same model — tenant
// isolation for this layer is enforced one level up, by
// degrade.semanticBlocked (proven in
// internal/degrade/m6_11_property_test.go). This test exists so
// the layer's blindness is a pinned fact, not an assumption: if
// the store ever gains tenant recording, this test must change
// with it.
func TestM611SemanticLayerIsTenantBlind(t *testing.T) {
	ctx := context.Background()
	sem := NewLocalSemanticCache()
	sem.SetTextForModel(ctx, "test-model", "What is the capital of France?",
		SemanticEntry{Response: CachedResponse{Content: "Paris", Model: "test-model"}}, time.Hour)
	// There is no tenant parameter on GetTextForModel: the same
	// lookup serves any tenant naming this model. That is the
	// documented division of labour, not a pass of property 1 —
	// the wrapper-level gate is what keeps tenant B out.
	if got, ok := sem.GetTextForModel(ctx, "test-model", "What is the capital of France?"); !ok || got.Response.Content != "Paris" {
		t.Fatalf("semantic lookup missed (found=%v got=%q): layer must serve same-model lookups", ok, got.Response.Content)
	}
}

// TestM611ReasoningKeysTenantIsolated proves the reasoning replay
// keys carry the tenant: ToolCallKey and ConversationKey differ
// across tenants for identical ids/turns, and a full state stored
// under tenant A's key is never returned for tenant B's key —
// while tenant A retrieves exactly what was stored (clone, full
// text). Parameterised over tenant pairs and tool-call ids
// (clients reuse ids like call_0 across tenants).
func TestM611ReasoningKeysTenantIsolated(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-b"},
		{"gk-1", "gk-2"},
		{"", "gk-named"},
	}
	callIDs := []string{"call_0", "call_1", "call_42"}
	turns := []ReasoningTurn{
		{Role: "user", Text: "what is 2+2?"},
		{Role: "assistant", Text: "calling calc"},
	}
	ctx := context.Background()
	for _, pair := range pairs {
		for _, callID := range callIDs {
			ka, kb := ToolCallKey(pair[0], callID), ToolCallKey(pair[1], callID)
			if ka == "" || kb == "" {
				t.Fatalf("tenants %q/%q call %q: empty tool-call key", pair[0], pair[1], callID)
			}
			if ka == kb {
				t.Fatalf("tenants %q/%q call %q: same tool-call key %q", pair[0], pair[1], callID, ka)
			}
			ca, cb := ConversationKey(pair[0], turns), ConversationKey(pair[1], turns)
			if ca == "" || cb == "" {
				t.Fatalf("tenants %q/%q: empty conversation key", pair[0], pair[1])
			}
			if ca == cb {
				t.Fatalf("tenants %q/%q: same conversation key %q", pair[0], pair[1], ca)
			}
			rc := NewReasoningCache()
			st := reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "thinking about math"}}}
			rc.SetStateForTenant(ctx, ka, pair[0], "", "deepseek-chat", st)
			// Control: the storing tenant retrieves the full state.
			if got, ok := rc.GetState(ctx, ka); !ok || got.ReasoningText() != "thinking about math" {
				t.Fatalf("tenants %q/%q call %q: storing tenant missed its own reasoning (found=%v)", pair[0], pair[1], callID, ok)
			}
			// Property: the other tenant's key misses.
			if got, ok := rc.GetState(ctx, kb); ok {
				t.Fatalf("tenants %q/%q call %q: tenant B served tenant A's reasoning %q", pair[0], pair[1], callID, got.ReasoningText())
			}
			// Same for the conversation key.
			rc.SetStateForTenant(ctx, ca, pair[0], "", "deepseek-chat", st)
			if _, ok := rc.GetState(ctx, cb); ok {
				t.Fatalf("tenants %q/%q: tenant B served tenant A's conversation-keyed reasoning", pair[0], pair[1])
			}
		}
	}
	// Empty tool-call id yields no key at all (never stored under it).
	if k := ToolCallKey("tenant-a", ""); k != "" {
		t.Fatalf("ToolCallKey with empty id = %q, want empty", k)
	}
}
