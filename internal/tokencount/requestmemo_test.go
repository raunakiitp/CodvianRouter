// AUD-PERF-004 regression: a request's prompt must be counted once and
// the result reused across the per-hop budget gate and the other
// estimate sites, instead of re-joining, re-hashing and re-marshalling
// the prompt on every call. The tests drive the package-level
// promptWireComputes counter (incremented only when CountPromptWire
// actually recomputes) so the reuse is asserted as a count, never a
// timing.
package tokencount

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// memoTestRequest builds a request big enough that re-joining + hashing
// the prompt is real work, with a tool definition so the tools-JSON
// marshal path is exercised too.
func memoTestRequest() provider.ChatRequest {
	msgs := []provider.Message{
		{Role: "system", Content: "You are a precise assistant."},
		{Role: "user", Content: strings.Repeat("Explain the warm-pool Little's Law derivation step by step. ", 30)},
	}
	var tool provider.ToolDefinition
	tool.Type = "function"
	tool.Function.Name = "lookup"
	tool.Function.Description = "Look something up by query."
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	return provider.ChatRequest{Model: "gpt-4o-mini", Messages: msgs, Tools: []provider.ToolDefinition{tool}}
}

func TestRequestMemoCountsPromptWireOncePerRequest(t *testing.T) {
	req := memoTestRequest()
	req.SetTokenCountMemo(NewRequestMemo())
	before := promptWireComputes.Load()
	var first TokenCount
	for i := 0; i < 5; i++ {
		got, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
		if err != nil {
			t.Fatalf("CountPromptWire: %v", err)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Errorf("call %d = %+v, want %+v (a memo hit must be exact)", i, got, first)
		}
	}
	if delta := promptWireComputes.Load() - before; delta != 1 {
		t.Errorf("CountPromptWire recomputed %d times across 5 calls with one per-request memo, want 1 (AUD-PERF-004)", delta)
	}
	// CountPromptFull is CountPromptWire(openai): it must reuse the
	// same entry, not add a compute.
	before = promptWireComputes.Load()
	if _, err := CountPromptFull(req.Model, req); err != nil {
		t.Fatalf("CountPromptFull: %v", err)
	}
	if delta := promptWireComputes.Load() - before; delta != 0 {
		t.Errorf("CountPromptFull skipped the memo: %d recomputes, want 0", delta)
	}
}

func TestCountPromptWireWithoutMemoRecomputes(t *testing.T) {
	req := memoTestRequest() // no memo attached
	before := promptWireComputes.Load()
	for i := 0; i < 5; i++ {
		if _, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req); err != nil {
			t.Fatalf("CountPromptWire: %v", err)
		}
	}
	if delta := promptWireComputes.Load() - before; delta != 5 {
		t.Errorf("with no memo attached, 5 calls recomputed %d times, want 5", delta)
	}
}

func TestRequestMemoKeyedByModelAndWire(t *testing.T) {
	req := memoTestRequest()
	req.SetTokenCountMemo(NewRequestMemo())
	before := promptWireComputes.Load()
	openai, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := CountPromptWire(req.Model, provider.WireFormatAnthropic, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
	if err != nil {
		t.Fatal(err)
	}
	if delta := promptWireComputes.Load() - before; delta != 2 {
		t.Errorf("two distinct wires recomputed %d times, want 2 (wire must be part of the key)", delta)
	}
	if again != openai {
		t.Errorf("openai re-lookup = %+v, want %+v", again, openai)
	}
	// OpenAI framing adds 3 per message + 3; native wires add 1 per
	// message — the counts must differ, proving the key separates wires.
	if openai.Tokens == anthropic.Tokens {
		t.Errorf("openai and anthropic counts are both %d; the wire is not in the key", openai.Tokens)
	}
}

func TestRequestMemoIsPerRequest(t *testing.T) {
	reqA := memoTestRequest()
	reqA.SetTokenCountMemo(NewRequestMemo())
	reqB := memoTestRequest()
	reqB.SetTokenCountMemo(NewRequestMemo())
	before := promptWireComputes.Load()
	a, err := CountPromptWire(reqA.Model, provider.WireFormatOpenAI, reqA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CountPromptWire(reqB.Model, provider.WireFormatOpenAI, reqB)
	if err != nil {
		t.Fatal(err)
	}
	if delta := promptWireComputes.Load() - before; delta != 2 {
		t.Errorf("two separate per-request memos recomputed %d times, want 2 (memos must not be shared across requests)", delta)
	}
	if a != b {
		t.Errorf("identical requests produced %+v vs %+v", a, b)
	}
}

func TestRequestMemoMessageGuardNotStale(t *testing.T) {
	req := memoTestRequest()
	req.SetTokenCountMemo(NewRequestMemo())
	first, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the message set with a superset: same model + wire, more
	// messages. The memo must NOT hand back the old count.
	req.Messages = append(req.Messages, provider.Message{Role: "user", Content: "And a follow-up question that adds tokens."})
	again, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tokens <= first.Tokens {
		t.Errorf("count did not grow after messages were appended: first=%d again=%d (stale memo hit)", first.Tokens, again.Tokens)
	}
}

func TestRequestMemoConcurrentReuse(t *testing.T) {
	req := memoTestRequest()
	req.SetTokenCountMemo(NewRequestMemo())
	want, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req) // warm the memo
	if err != nil {
		t.Fatal(err)
	}
	before := promptWireComputes.Load()
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[TokenCount]int{}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req)
			if err != nil {
				t.Errorf("CountPromptWire: %v", err)
				return
			}
			mu.Lock()
			seen[got]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Errorf("concurrent counts disagree: %v", seen)
	}
	for got := range seen {
		if got != want {
			t.Errorf("concurrent count = %+v, want %+v", got, want)
		}
	}
	if delta := promptWireComputes.Load() - before; delta != 0 {
		t.Errorf("64 concurrent hits on a warm memo recomputed %d times, want 0 (memo must be concurrency-safe and shared)", delta)
	}
}

func TestRequestMemoBounded(t *testing.T) {
	m := NewRequestMemo()
	for i := 0; i < maxRequestMemoEntries+16; i++ {
		m.store(fmt.Sprintf("model-%d", i), provider.WireFormatOpenAI, 1, 0, TokenCount{Tokens: i})
	}
	m.mu.Lock()
	n := len(m.counts)
	m.mu.Unlock()
	if n > maxRequestMemoEntries {
		t.Errorf("memo grew to %d entries, want <= %d", n, maxRequestMemoEntries)
	}
}

func benchmarkCountPromptWire(b *testing.B, withMemo bool) {
	req := memoTestRequest()
	if withMemo {
		req.SetTokenCountMemo(NewRequestMemo())
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CountPromptWire(req.Model, provider.WireFormatOpenAI, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCountPromptWireNoMemo is the pre-AUD-PERF-004 hot path: every
// call re-joins the prompt, SHA-256s it and re-marshals the tools JSON.
func BenchmarkCountPromptWireNoMemo(b *testing.B) { benchmarkCountPromptWire(b, false) }

// BenchmarkCountPromptWireRequestMemo is the fixed hot path: after the
// first count, every call is a mutex-guarded map lookup.
func BenchmarkCountPromptWireRequestMemo(b *testing.B) { benchmarkCountPromptWire(b, true) }
