package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// Trace endpoint for /v1/messages (CHECKLIST 211): a translated
// messages request stamps "messages" on its route trace —
// non-streaming, streaming, and rejected-contract paths alike —
// so the dashboard's Live Routing view can tell it from a chat
// trace. Chat keeps "" (rendered as chat), and every other
// endpoint is untouched here.

func traceEndpoint(tr *routetrace.RouteTrace) string {
	if tr == nil {
		return "<nil trace>"
	}
	return tr.Endpoint
}

func TestTraceEndpointMessagesNonStreaming(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	req := contractReq(&provider.RequestRouting{Only: []string{"mid"}})
	req.Endpoint = provider.EndpointMessages
	_, trace, err := e.ChatWithChain(context.Background(), contractChain(), req)
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if trace.Endpoint != "messages" {
		t.Errorf("trace.Endpoint = %q, want messages", trace.Endpoint)
	}
	if trace.Contract == nil || len(trace.Contract.Only) != 1 {
		t.Errorf("trace.Contract = %+v, want the applied only=[mid] contract", trace.Contract)
	}
}

func TestTraceEndpointMessagesStreaming(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	req := contractReq(&provider.RequestRouting{Only: []string{"mid"}})
	req.Endpoint = provider.EndpointMessages
	ch, err := e.ChatStreamWithChain(context.Background(), contractChain(), req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	for range ch {
	}
	if got := traceEndpoint(e.LastTrace()); got != "messages" {
		t.Errorf("LastTrace().Endpoint = %q, want messages", got)
	}
}

func TestTraceEndpointMessagesRejectedContract(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	// Unknown provider: the contract is rejected before any
	// attempt, on both paths — the published trace still names
	// the surface, so the dashboard shows the rejection as a
	// messages rejection, not a stale chat trace.
	req := contractReq(&provider.RequestRouting{Only: []string{"ghost"}})
	req.Endpoint = provider.EndpointMessages
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), req)
	if err == nil {
		t.Fatal("unknown provider should be rejected")
	}
	if got := e.LastTrace(); got == nil || got.Endpoint != "messages" || got.Contract == nil || got.Contract.Error == "" {
		t.Errorf("LastTrace = %+v, want endpoint messages + a contract rejection", got)
	}
	_, err = e.ChatStreamWithChain(context.Background(), contractChain(), req)
	if err == nil {
		t.Fatal("unknown provider should be rejected on the stream path too")
	}
	if got := e.LastTrace(); got == nil || got.Endpoint != "messages" || got.Contract == nil || got.Contract.Error == "" {
		t.Errorf("LastTrace (stream) = %+v, want endpoint messages + a contract rejection", got)
	}
}

func TestTraceEndpointChatStaysEmpty(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	// Chat completions set EndpointChatCompletions explicitly in
	// the server handler — the trace keeps "" there, which the
	// dashboard renders as chat. A zero Endpoint behaves the same.
	for _, ep := range []provider.EndpointKind{provider.EndpointChatCompletions, ""} {
		req := contractReq(nil)
		req.Endpoint = ep
		_, trace, err := e.ChatWithChain(context.Background(), contractChain(), req)
		if err != nil {
			t.Fatalf("ChatWithChain (endpoint %q): %v", ep, err)
		}
		if trace.Endpoint != "" {
			t.Errorf("endpoint %q: trace.Endpoint = %q, want empty", ep, trace.Endpoint)
		}
	}
}
