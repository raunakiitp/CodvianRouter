package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeEngine is a controllable Engine for tests: it answers from canned
// responses, returns a canned error, or sleeps (respecting ctx) to
// simulate a slow engine.
type fakeEngine struct {
	// respond, when set, decides the response for a request.
	respond func(req provider.ChatRequest) (*provider.ChatResponse, error)
	// delay, when > 0, is slept (or until ctx is done) before responding.
	delay time.Duration
}

func (f *fakeEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.respond != nil {
		return f.respond(req)
	}
	return &provider.ChatResponse{Content: "hello", Model: req.Model}, nil
}

// canned returns an engine that always answers with content.
func canned(content string) *fakeEngine {
	return &fakeEngine{respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: content, Model: req.Model}, nil
	}}
}

func TestRunAllPass(t *testing.T) {
	h := &Harness{Engine: canned("Hello! Paris is the capital of France."), Timeout: 5 * time.Second}
	cases := []Case{
		{ID: "greeting", Prompt: "Hello", Model: "test-model", ExpectContains: []string{"hello"}},
		{ID: "capital", Prompt: "Capital of France?", Model: "test-model", ExpectContains: []string{"PARIS", "capital"}},
		{ID: "no-expectations", Prompt: "Anything", Model: "test-model"},
	}
	rep := h.Run(context.Background(), cases)
	if rep.Total != 3 || rep.Passed != 3 || rep.Failed != 0 {
		t.Fatalf("want 3/3 passed, got total=%d passed=%d failed=%d", rep.Total, rep.Passed, rep.Failed)
	}
	if rep.PassRate != 1.0 {
		t.Fatalf("want PassRate 1.0, got %v", rep.PassRate)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(rep.Results))
	}
	for i, r := range rep.Results {
		if !r.OK {
			t.Errorf("case %d (%s): want OK, got error %q missing %v", i, r.CaseID, r.Error, r.Missing)
		}
		if r.CaseID != cases[i].ID {
			t.Errorf("result %d: want CaseID %q, got %q", i, cases[i].ID, r.CaseID)
		}
		if r.LatencyMs < 0 {
			t.Errorf("case %s: negative latency %d", r.CaseID, r.LatencyMs)
		}
		if r.Error != "" || len(r.Missing) != 0 {
			t.Errorf("case %s: passing result should have no error/missing, got %q %v", r.CaseID, r.Error, r.Missing)
		}
	}
	if rep.FinishedAt.Before(rep.StartedAt) {
		t.Errorf("FinishedAt %v before StartedAt %v", rep.FinishedAt, rep.StartedAt)
	}
}

func TestRunMissingSubstring(t *testing.T) {
	h := &Harness{Engine: canned("Paris is the capital of France."), Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), []Case{
		{ID: "wrong-capital", Prompt: "Capital of France?", Model: "m", ExpectContains: []string{"Paris", "Berlin"}},
	})
	if rep.Total != 1 || rep.Passed != 0 || rep.Failed != 1 {
		t.Fatalf("want 0/1 passed, got total=%d passed=%d failed=%d", rep.Total, rep.Passed, rep.Failed)
	}
	if rep.PassRate != 0.0 {
		t.Fatalf("want PassRate 0, got %v", rep.PassRate)
	}
	r := rep.Results[0]
	if r.OK {
		t.Fatal("want OK=false for missing substring")
	}
	if len(r.Missing) != 1 || r.Missing[0] != "Berlin" {
		t.Fatalf("want Missing=[Berlin], got %v", r.Missing)
	}
	if r.Response == "" {
		t.Error("response should still be recorded on a missing-substring failure")
	}
	// A missing substring is a scoring failure, not an engine error.
	if r.Error != "" {
		t.Errorf("want empty Error on missing-substring failure, got %q", r.Error)
	}
}

func TestRunEngineError(t *testing.T) {
	boom := errors.New("upstream unavailable")
	h := &Harness{Engine: &fakeEngine{respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, boom
	}}, Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), []Case{
		{ID: "err", Prompt: "Hello", Model: "m", ExpectContains: []string{"hello"}},
	})
	if rep.Passed != 0 || rep.Failed != 1 {
		t.Fatalf("want 0/1 passed, got passed=%d failed=%d", rep.Passed, rep.Failed)
	}
	r := rep.Results[0]
	if r.OK {
		t.Fatal("want OK=false on engine error")
	}
	if !strings.Contains(r.Error, "upstream unavailable") {
		t.Fatalf("want Error to carry the engine error, got %q", r.Error)
	}
	if r.Response != "" {
		t.Errorf("want empty Response on engine error, got %q", r.Response)
	}
}

func TestRunTimeout(t *testing.T) {
	// Engine sleeps 500ms and respects ctx; harness allows only 50ms.
	h := &Harness{Engine: &fakeEngine{delay: 500 * time.Millisecond, respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "hello"}, nil
	}}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	rep := h.Run(context.Background(), []Case{
		{ID: "slow", Prompt: "Hello", Model: "m", ExpectContains: []string{"hello"}},
	})
	elapsed := time.Since(start)
	if rep.Passed != 0 || rep.Failed != 1 {
		t.Fatalf("want 0/1 passed, got passed=%d failed=%d", rep.Passed, rep.Failed)
	}
	r := rep.Results[0]
	if r.OK {
		t.Fatal("want OK=false on timeout")
	}
	if !strings.Contains(strings.ToLower(r.Error), "timed out") {
		t.Fatalf("want Error to say timed out, got %q", r.Error)
	}
	// The run must actually stop near the timeout, not wait out the engine.
	if elapsed > 400*time.Millisecond {
		t.Errorf("run took %v; per-case timeout did not cut the slow engine short", elapsed)
	}
}

func TestRunEmptyCaseList(t *testing.T) {
	h := &Harness{Engine: canned("hello"), Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), nil)
	if rep.Total != 0 || rep.Passed != 0 || rep.Failed != 0 {
		t.Fatalf("want 0/0, got total=%d passed=%d failed=%d", rep.Total, rep.Passed, rep.Failed)
	}
	if rep.PassRate != 0 {
		t.Fatalf("want PassRate 0 for an empty run, got %v", rep.PassRate)
	}
	if rep.Results == nil || len(rep.Results) != 0 {
		t.Fatalf("want empty non-nil Results, got %v", rep.Results)
	}
	if rep.FinishedAt.Before(rep.StartedAt) {
		t.Errorf("FinishedAt %v before StartedAt %v", rep.FinishedAt, rep.StartedAt)
	}
}

func TestRunLatencyBudget(t *testing.T) {
	h := &Harness{Engine: &fakeEngine{delay: 60 * time.Millisecond, respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "hello"}, nil
	}}, Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), []Case{
		{ID: "too-slow", Prompt: "Hello", Model: "m", ExpectContains: []string{"hello"}, MaxLatencyMs: 10},
	})
	if rep.Passed != 0 {
		t.Fatal("want failure when latency exceeds MaxLatencyMs")
	}
	if !strings.Contains(rep.Results[0].Error, "latency") {
		t.Fatalf("want latency error, got %q", rep.Results[0].Error)
	}
}

func TestRunNilEngineAndNilResponseDoNotPanic(t *testing.T) {
	// No engine at all: every case fails cleanly, no panic.
	h := &Harness{Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), []Case{{ID: "x", Prompt: "hi", Model: "m"}})
	if rep.Passed != 0 || rep.Results[0].Error == "" {
		t.Fatalf("nil engine should fail the case with an error, got %+v", rep.Results[0])
	}

	// Engine returning (nil, nil): likewise a recorded failure, not a panic.
	h2 := &Harness{Engine: &fakeEngine{respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, nil
	}}}
	rep2 := h2.Run(context.Background(), []Case{{ID: "x", Prompt: "hi", Model: "m"}})
	if rep2.Passed != 0 || rep2.Results[0].Error == "" {
		t.Fatalf("nil response should fail the case with an error, got %+v", rep2.Results[0])
	}

	// Engine that panics: recorded as a failure, harness survives.
	h3 := &Harness{Engine: &fakeEngine{respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		panic("boom")
	}}}
	rep3 := h3.Run(context.Background(), []Case{{ID: "x", Prompt: "hi", Model: "m"}})
	if rep3.Passed != 0 || !strings.Contains(rep3.Results[0].Error, "panicked") {
		t.Fatalf("engine panic should be a recorded failure, got %+v", rep3.Results[0])
	}
}

func TestSmokeCases(t *testing.T) {
	cases := SmokeCases("test-model")
	if len(cases) != 3 {
		t.Fatalf("want 3 smoke cases, got %d", len(cases))
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("smoke cases need distinct non-empty IDs, got %q", c.ID)
		}
		seen[c.ID] = true
		if c.Model != "test-model" {
			t.Errorf("case %s: want model test-model, got %q", c.ID, c.Model)
		}
	}
	if cases[2].Prompt != "" {
		t.Errorf("third smoke case should be the empty-prompt edge case, got prompt %q", cases[2].Prompt)
	}
	if len(cases[2].ExpectContains) != 0 {
		t.Errorf("empty-prompt case should have no ExpectContains, got %v", cases[2].ExpectContains)
	}

	// The empty-prompt case against a strict engine (one that errors on
	// an empty prompt) must be a recorded failure, never a harness panic.
	// The other two cases pass against a canned engine that answers both.
	engine := &fakeEngine{respond: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if len(req.Messages) == 0 || req.Messages[0].Content == "" {
			return nil, errors.New("empty prompt rejected")
		}
		return &provider.ChatResponse{Content: "Hello! Paris is the capital of France."}, nil
	}}
	h := &Harness{Engine: engine, Timeout: 5 * time.Second}
	rep := h.Run(context.Background(), cases)
	if rep.Total != 3 || rep.Passed != 2 || rep.Failed != 1 {
		t.Fatalf("want 2/3 passed (empty-prompt case failing cleanly), got %+v", rep)
	}
	empty := rep.Results[2]
	if empty.OK || empty.Error == "" {
		t.Fatalf("empty-prompt case should fail with Error recorded, got %+v", empty)
	}
	if !strings.Contains(empty.Error, "empty prompt rejected") {
		t.Fatalf("want the engine error recorded, got %q", empty.Error)
	}
}

func TestDefaultTimeout(t *testing.T) {
	h := &Harness{Engine: canned("hello")}
	if h.timeout() != DefaultTimeout {
		t.Fatalf("zero Timeout should mean DefaultTimeout %v, got %v", DefaultTimeout, h.timeout())
	}
	if DefaultTimeout != 30*time.Second {
		t.Fatalf("want DefaultTimeout 30s, got %v", DefaultTimeout)
	}
}
