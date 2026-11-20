// Structured JSON request logging (M2.3).
//
// One JSON object per finished request, emitted via log/slog. The entry
// type deliberately has NO field for prompt/response content, API keys or
// any other secret: nothing sensitive can be logged because there is
// nowhere to put it.
package observability

import (
	"io"
	"log/slog"
)

// RequestLogEntry is one finished request as measured by the request path.
// Field names are the JSON keys emitted by Logger.LogRequest.
type RequestLogEntry struct {
	RequestID string
	Provider  string
	Model     string
	// Outcome is "ok", "error" or "timeout".
	Outcome string
	// ErrorClass classifies a non-ok outcome ("rate_limited", "timeout", ...).
	ErrorClass        string
	LatencyMs         float64
	UpstreamLatencyMs float64
	TokensIn          int64
	TokensOut         int64
	TokensCached      int64
	CostUSD           float64
	Cache             string // "hit" / "miss" / "" (not consulted)
	CacheLayer        string // "exact" / "semantic" / ...
	FallbackAttempts  int
	FallbackPath      string // e.g. "openai->anthropic"
	BreakerOpen       bool
}

// Logger emits structured JSON log rows. Safe for concurrent use.
type Logger struct {
	inner *slog.Logger
}

// NewLogger returns a Logger writing JSON lines to w. A nil writer drops
// rows silently instead of panicking.
func NewLogger(w io.Writer) *Logger {
	if w == nil {
		w = io.Discard
	}
	return &Logger{inner: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// LogRequest emits one JSON line for a finished request.
func (l *Logger) LogRequest(e RequestLogEntry) {
	if l == nil || l.inner == nil {
		return
	}
	attrs := []any{
		slog.String("request_id", e.RequestID),
		slog.String("provider", e.Provider),
		slog.String("model", e.Model),
		slog.String("outcome", e.Outcome),
		slog.Float64("latency_ms", e.LatencyMs),
	}
	if e.ErrorClass != "" {
		attrs = append(attrs, slog.String("error_class", e.ErrorClass))
	}
	if e.UpstreamLatencyMs != 0 {
		attrs = append(attrs, slog.Float64("upstream_latency_ms", e.UpstreamLatencyMs))
	}
	if e.TokensIn != 0 {
		attrs = append(attrs, slog.Int64("tokens_in", e.TokensIn))
	}
	if e.TokensOut != 0 {
		attrs = append(attrs, slog.Int64("tokens_out", e.TokensOut))
	}
	if e.TokensCached != 0 {
		attrs = append(attrs, slog.Int64("tokens_cached", e.TokensCached))
	}
	if e.CostUSD != 0 {
		attrs = append(attrs, slog.Float64("cost_usd", e.CostUSD))
	}
	if e.Cache != "" {
		attrs = append(attrs, slog.String("cache", e.Cache))
	}
	if e.CacheLayer != "" {
		attrs = append(attrs, slog.String("cache_layer", e.CacheLayer))
	}
	if e.FallbackAttempts != 0 {
		attrs = append(attrs, slog.Int("fallback_attempts", e.FallbackAttempts))
	}
	if e.FallbackPath != "" {
		attrs = append(attrs, slog.String("fallback_path", e.FallbackPath))
	}
	if e.BreakerOpen {
		attrs = append(attrs, slog.Bool("breaker_open", true))
	}
	l.inner.Info("request", attrs...)
}
