// P1.8 — context/output guardrails: the embeddings/rerank context gate.
//
// The chat surfaces already had a pre-dispatch context-fit gate
// (internal/router/contextfit.go). The embeddings and rerank surfaces
// resolve their chain and dial directly, so an input larger than every
// known context window used to be sent upstream — and failed there,
// after a provider call. These tests pin the gate: refuse before any
// provider is dispatched, and never refuse on an UNKNOWN window.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// p18LongEmbedInput is an input comfortably larger than the tiny window
// the tests configure.
const p18LongEmbedInput = "this input is deliberately far longer than the tiny context window these tests configure, so the fit gate has something to refuse"

func p18EmbedReq(input string) provider.EmbeddingRequest {
	return provider.EmbeddingRequest{Model: "emb-model", Input: json.RawMessage(`"` + input + `"`)}
}

// TestP18EmbeddingContextGateRefusesBeforeDispatch: an input that fits
// no known window is refused with the honest error and ZERO provider
// calls.
func TestP18EmbeddingContextGateRefusesBeforeDispatch(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		ContextWindow: func(providerID, model string) (int, bool) { return 5, true },
	})
	_, err := eng.Embed(context.Background(), p18EmbedReq(p18LongEmbedInput))
	var tooLarge *EmbeddingContextTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want EmbeddingContextTooLarge", err)
	}
	if tooLarge.PromptTokens <= 5 {
		t.Fatalf("prompt tokens = %d, want more than the 5-token window", tooLarge.PromptTokens)
	}
	if tooLarge.LargestWindow != 5 {
		t.Fatalf("largest window = %d, want 5", tooLarge.LargestWindow)
	}
	if !strings.Contains(tooLarge.Error(), "no provider was called") {
		t.Fatalf("error = %q, want it to say no provider was called", tooLarge.Error())
	}
	if got := a.embedCalls(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 (the gate refuses before dispatch)", got)
	}
}

// TestP18EmbeddingContextGateUnknownWindowDoesNotRefuse: an unknown
// window is not proof the model is too small, so the request is tried —
// exactly like the chat gate.
func TestP18EmbeddingContextGateUnknownWindowDoesNotRefuse(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		ContextWindow: func(providerID, model string) (int, bool) { return 0, false },
	})
	if _, err := eng.Embed(context.Background(), p18EmbedReq(p18LongEmbedInput)); err != nil {
		t.Fatalf("unknown window must not refuse: %v", err)
	}
	if got := a.embedCalls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// TestP18EmbeddingContextGateFitsWithinWindow: an input that fits is
// served normally.
func TestP18EmbeddingContextGateFitsWithinWindow(t *testing.T) {
	a := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		ContextWindow: func(providerID, model string) (int, bool) { return 100000, true },
	})
	if _, err := eng.Embed(context.Background(), p18EmbedReq(p18LongEmbedInput)); err != nil {
		t.Fatalf("a fitting input must be served: %v", err)
	}
	if got := a.embedCalls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// TestP18RerankContextGateRefusesBeforeDispatch: the same gate on the
// rerank surface.
func TestP18RerankContextGateRefusesBeforeDispatch(t *testing.T) {
	r := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"r": r}, nil, Config{
		ContextWindow: func(providerID, model string) (int, bool) { return 5, true },
	})
	_, err := eng.Rerank(context.Background(), provider.RerankRequest{
		Model:     "rerank-model",
		Query:     p18LongEmbedInput,
		Documents: []json.RawMessage{json.RawMessage(`"doc one"`)},
	})
	var tooLarge *EmbeddingContextTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want EmbeddingContextTooLarge", err)
	}
	if got := r.rerankCalls(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

// TestP18EmbeddingGateDropsOnlyTheTooSmallAttempt: a chain with one
// too-small and one fitting target serves the request on the fitting
// one — the too-small attempt is never dialled.
func TestP18EmbeddingGateDropsOnlyTheTooSmallAttempt(t *testing.T) {
	small := &embedFakeClient{fakeClient: &fakeClient{}}
	big := &embedFakeClient{fakeClient: &fakeClient{}}
	eng := NewEngine(map[string]provider.Client{"small": small, "big": big}, nil, Config{
		ContextWindow: func(providerID, model string) (int, bool) {
			if providerID == "small" {
				return 5, true
			}
			return 100000, true
		},
	})
	if _, err := eng.Embed(context.Background(), p18EmbedReq(p18LongEmbedInput)); err != nil {
		t.Fatalf("a chain with a fitting target must serve: %v", err)
	}
	if got := small.embedCalls(); got != 0 {
		t.Fatalf("too-small provider calls = %d, want 0", got)
	}
	if got := big.embedCalls(); got != 1 {
		t.Fatalf("fitting provider calls = %d, want 1", got)
	}
}
