package degrade

// CHECKLIST 243: the degrade wrapper is what the server actually
// holds (cmd/codvian-router wires server.New with the wrapped
// engine), so the wrapper must forward the engine's failover-cluster
// stats — otherwise the dashboard guardrails feed sees nothing even
// though the engine recorded the escapes. Same decorator rule as
// RefusalChainStats.

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

type failoverStatsEngine struct {
	stats router.Failover243Stats
}

func (e *failoverStatsEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi"}, nil
}

func (e *failoverStatsEngine) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi"}, nil
}

func (e *failoverStatsEngine) Failover243Stats() router.Failover243Stats { return e.stats }

type plainEngine struct{}

func (e *plainEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi"}, nil
}

func (e *plainEngine) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi"}, nil
}

func TestStaleEngineForwardsFailover243Stats(t *testing.T) {
	want := router.Failover243Stats{
		PinEscapes:            3,
		LastPin:               "a",
		LastEscapedTo:         "b",
		LastEscapeAt:          time.Now(),
		ToolReplaysSuppressed: 2,
	}
	w := &StaleEngine{engine: &failoverStatsEngine{stats: want}}
	got := w.Failover243Stats()
	if got.PinEscapes != want.PinEscapes || got.LastPin != want.LastPin ||
		got.LastEscapedTo != want.LastEscapedTo || got.ToolReplaysSuppressed != want.ToolReplaysSuppressed {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !got.LastEscapeAt.Equal(want.LastEscapeAt) {
		t.Fatalf("LastEscapeAt = %v, want %v", got.LastEscapeAt, want.LastEscapeAt)
	}
}

func TestStaleEngineFailover243StatsZeroWithoutInner(t *testing.T) {
	w := &StaleEngine{engine: &plainEngine{}}
	got := w.Failover243Stats()
	if got != (router.Failover243Stats{}) {
		t.Fatalf("got %+v, want zero stats for an inner engine that reports none", got)
	}
}
