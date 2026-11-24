// AUD-PERF-004: a per-request prompt-token memo.
//
// CountPromptWire (and its CountPromptFull alias) rebuilds the joined
// prompt, SHA-256s it and re-marshals the tools JSON on every call. The
// Count-level memo below removes the BPE re-encode but not that O(n)
// join + hash + marshal — and the same request is counted by several
// gates (the server's pre-send budget checks, the context-fit gate) and
// again per attempt by the engine's per-hop budget gate. A RequestMemo
// caches the finished CountPromptWire result for one request, so after
// the first count every later gate for the same (model, wire) is a map
// lookup.
//
// Scope and safety: a RequestMemo is meant to live on ONE request (the
// engine attaches it, via provider.ChatRequest.SetTokenCountMemo, only
// after the request's Messages and Tools are final) and is discarded
// with the request. That is what makes it correct without a content
// hash: within a request the counted inputs do not change, and two
// requests never share a memo, so a result can be neither stale nor
// bleed across tenants. The key still carries the message and tool
// counts as a cheap guard, so a request whose messages are replaced
// (a different slice length) is a miss, never a stale hit. Growth is
// bounded by maxRequestMemoEntries — a request's chain names a handful
// of (model, wire) pairs — and the map is mutex-guarded because hedged
// attempts count concurrently.
package tokencount

import "sync"

// maxRequestMemoEntries bounds one RequestMemo. A request's routing
// chain names a handful of (model, wire) pairs at most; the cap is a
// guard against a pathological chain, not a working-set size. Past the
// cap an entry is simply not cached (recomputed), never wrong.
const maxRequestMemoEntries = 64

// RequestMemo is a per-request cache for CountPromptWire / CountPromptFull
// results, keyed by (model, wire, message count, tools count). It is
// safe for concurrent use. Create one with NewRequestMemo and attach it
// to a request's ChatRequest via SetTokenCountMemo.
type RequestMemo struct {
	mu     sync.Mutex
	counts map[requestMemoKey]TokenCount
}

// requestMemoKey identifies a cached count. model implies the encoding
// and fallback flag; wire implies the framing and tool reshaping. The
// message and tool COUNTS are a cheap guard against a request whose
// messages were replaced (a stale hit would need the same model, wire
// AND both counts, which the per-request lifecycle plus this guard rule
// out).
type requestMemoKey struct {
	model    string
	wire     string
	messages int
	tools    int
}

// NewRequestMemo returns an empty per-request token-count memo.
func NewRequestMemo() *RequestMemo {
	return &RequestMemo{counts: make(map[requestMemoKey]TokenCount, 4)}
}

// lookup returns the memoized count for (model, wire, messages, tools).
func (m *RequestMemo) lookup(model, wire string, messages, tools int) (TokenCount, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	got, ok := m.counts[requestMemoKey{model: model, wire: wire, messages: messages, tools: tools}]
	return got, ok
}

// store records count for (model, wire, messages, tools). Past the cap
// the entry is dropped (the caller recomputes next time) — the memo is
// a pure-function cache, so dropping only costs work, never correctness.
func (m *RequestMemo) store(model, wire string, messages, tools int, count TokenCount) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.counts) >= maxRequestMemoEntries {
		return
	}
	m.counts[requestMemoKey{model: model, wire: wire, messages: messages, tools: tools}] = count
}
