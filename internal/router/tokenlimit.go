// Per-key token limits, engine side (M5.3).
//
// The server pre-call gate (internal/server/keybudgets.go
// checkKeyTokenLimits) covers the global + model scopes for the
// model the request NAMES — but the provider is only known per
// attempt, once the engine resolves the chain, and an auto/*
// request's concrete model is only known then too. So the engine
// re-checks before each attempt, through Config.TokenLimitCheck
// (wired in main.go to the ledger's CheckKeyTokenLimits): a
// breached target is skipped with trace outcome
// "skipped_token_limit" and the next target serves; when every
// target is breached the first breach is returned as the request
// error, which the server maps to 429 (see handleEngineError).
// A TokenLimitCheck error (the ledger cannot be read) fails
// closed: the chain stops with that error — a limit that cannot
// be read must not be silently waived. A nil TokenLimitCheck
// means no limits are configured and the check is skipped.
//
// The hedged second attempt bypasses this check, the same way it
// bypasses the breaker/cooldown/lockout loop-top skips today
// (hedgedChat/hedgedStream dial the second target directly): a
// hedged request may still be served by a breached target when
// the hedge fires. The ledger still counts its tokens, so the
// NEXT request is refused.
package router

import (
	"context"
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TokenLimitBreach is one breached per-key token limit, as the
// engine reports it: the scope that fired, the configured limit,
// what the window already holds, and when the window resets (so
// the server can set an honest Retry-After).
type TokenLimitBreach struct {
	Scope      string
	ScopeValue string
	Limit      int
	Used       int
	NextReset  time.Time
}

func (b *TokenLimitBreach) Error() string {
	scope := "global"
	if b.Scope != "" && b.Scope != "global" {
		scope = b.Scope + " " + b.ScopeValue
	}
	return fmt.Sprintf("key token limit exceeded: %s limit of %d tokens reached (%d used in this window); resets at %s",
		scope, b.Limit, b.Used, b.NextReset.UTC().Format(time.RFC3339))
}

// tokenLimitBreach consults Config.TokenLimitCheck for one
// attempt's (provider, model). Nil check or an anonymous request
// (no CallerID — auth-relaxed mode has no key to limit) means
// no breach.
func (e *Engine) tokenLimitBreach(ctx context.Context, req provider.ChatRequest, att Attempt) (*TokenLimitBreach, error) {
	if e.cfg.TokenLimitCheck == nil || req.CallerID == "" {
		return nil, nil
	}
	return e.cfg.TokenLimitCheck(ctx, req.CallerID, att.ProviderID, att.Model)
}
