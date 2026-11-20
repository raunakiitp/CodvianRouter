// GET /v1/auto-combo/{channel}/candidates — handler logic (M3 Phase C,
// builder C5). PURE FUNCTION ONLY: the coordinator registers the route
// and supplies the candidate pool; this file never touches server.go,
// globals, the DB, or the network.
//
// Shape (open-sse/handlers/autoComboCandidates.ts,
// AutoComboCandidatesResult / AutoComboCandidateView):
//
//	{ "channel": "auto/coding", "candidates": [
//	  { "provider": ..., "connectionId": ..., "model": ...,
//	    "modelStr": ..., "excluded": false, "reachable": true,
//	    "breakerState": "CLOSED", "connectionCooldown": false,
//	    "modelLocked": false, "freeAccessExclusion": null } ] }
//
// Rules carried over from the reference:
//   - `channel` is the suffix after "auto/", or the literal "auto"
//     for the base channel; the result's `channel` is the full id
//     ("auto" or "auto/<suffix>").
//   - Unknown channels are an error (the route layer maps it to
//     404, as isUnknownAutoChannelError does there); the channel
//     must also match the route's ^[a-zA-Z0-9:_-]+$ / max-128 rule.
//   - A candidate the routing path would currently skip (model
//     locked, cooled down, breaker open) is NEVER dropped from this
//     listing: it is surfaced with reachable:false and the specific
//     reason field set. The listing is read-only transparency, not
//     a preview of the dispatch pool.
//   - Reachability reuses the resilience reads, never raw breaker
//     state: breaker OPEN is unreachable (HALF_OPEN can execute —
//     it is a probe, not a block); a missing/unknown breaker state
//     reads as reachable, matching the reference's CLOSED default.
//   - One logical candidate with an allowedConnectionIds allowlist
//     and no single connectionId expands to one row per connection
//     (account-oriented listing); a candidate with neither produces
//     no row.
//   - `excluded` comes from the caller's per-API-key exclusion set
//     (operator-managed auto-candidate-overrides.json, loaded by
//     autoCandidateOverrideStore in autocombo_route.go); a nil set
//     means no exclusions.
//   - A failed or missing lookup is fail-open at the caller's layer:
//     this function takes already-resolved inputs and never fails
//     the listing for one bad row.
package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
)

// ErrUnknownAutoChannel is returned when the channel is not a
// recognised built-in auto/* id. The route layer maps it to 404.
var ErrUnknownAutoChannel = errors.New("unknown auto channel")

// ErrInvalidAutoChannel is returned when the channel fails the
// route's shape rule (1-128 chars of [a-zA-Z0-9:_-]). The route
// layer maps it to 400.
var ErrInvalidAutoChannel = errors.New("invalid auto channel")

var autoChannelPattern = regexp.MustCompile(`^[a-zA-Z0-9:_-]+$`)

// AutoComboCandidateInput is one raw candidate row as the
// coordinator supplies it: the autorouter's candidate-pool entry
// plus the already-resolved resilience reads for it. The handler
// only derives `reachable` from these three reads; it never
// re-reads breaker, cooldown or lockout state itself.
type AutoComboCandidateInput struct {
	// Provider is the provider id.
	Provider string
	// ConnectionID is the connection serving this candidate, "" when
	// the candidate instead carries an allowlist.
	ConnectionID string
	// AllowedConnectionIDs is the candidate's connection allowlist;
	// used only when ConnectionID is "". One output row is
	// produced per entry.
	AllowedConnectionIDs []string
	// Model is the full "provider/model" id.
	Model string
	// ModelStr is the model string as advertised; "" defaults to
	// Model (the reference emits the same value in both fields).
	ModelStr string
	// BreakerState is the provider breaker state ("CLOSED",
	// "DEGRADED", "OPEN", "HALF_OPEN"). "" reads as CLOSED.
	BreakerState string
	// ConnectionCooldown reports a live connection cooldown
	// (rateLimitedUntil in the future, or testStatus unavailable).
	ConnectionCooldown bool
	// ModelLocked reports a model lockout for
	// (provider, connection, bare model).
	ModelLocked bool
	// FreeAccessExclusion names why strict-zero-cost would
	// exclude this candidate from dispatch, or nil when it would
	// not. Reported, never enforced: the row is listed either way.
	FreeAccessExclusion *string
}

// AutoComboCandidatesInput is the handler's whole input: the
// channel plus the candidate pool. Nothing is read from globals.
type AutoComboCandidatesInput struct {
	// Channel is the URL segment: "auto" for the base channel, or
	// the suffix after "auto/" (e.g. "coding:free", "best-coding").
	Channel string
	// Pool is the channel's unfiltered candidate pool.
	Pool []AutoComboCandidateInput
	// ExcludedConnectionIDs is the caller's per-API-key exclusion
	// set (connection ids excluded for this key). Nil means none.
	ExcludedConnectionIDs map[string]bool
}

// AutoComboCandidate is one row of the response, field-for-field
// the reference AutoComboCandidateView (camelCase names kept so
// the shape matches the documented endpoint exactly).
type AutoComboCandidate struct {
	Provider            string  `json:"provider"`
	ConnectionID        string  `json:"connectionId"`
	Model               string  `json:"model"`
	ModelStr            string  `json:"modelStr"`
	Excluded            bool    `json:"excluded"`
	Reachable           bool    `json:"reachable"`
	BreakerState        string  `json:"breakerState"`
	ConnectionCooldown  bool    `json:"connectionCooldown"`
	ModelLocked         bool    `json:"modelLocked"`
	FreeAccessExclusion *string `json:"freeAccessExclusion"`
}

// AutoComboCandidatesResult is the response body:
// { "channel": "auto/<suffix>", "candidates": [...] }.
type AutoComboCandidatesResult struct {
	Channel    string               `json:"channel"`
	Candidates []AutoComboCandidate `json:"candidates"`
}

// ListAutoComboCandidates builds the candidate listing for one
// auto/* channel from the given pool. It is a pure function: no
// globals, no DB, no network. Unknown channels error with
// ErrUnknownAutoChannel (route layer -> 404); malformed channels
// error with ErrInvalidAutoChannel (route layer -> 400).
func ListAutoComboCandidates(in AutoComboCandidatesInput) (AutoComboCandidatesResult, error) {
	channel := in.Channel
	if len(channel) < 1 || len(channel) > 128 || !autoChannelPattern.MatchString(channel) {
		return AutoComboCandidatesResult{}, fmt.Errorf("%w: %q (want 1-128 chars of [a-zA-Z0-9:_-])", ErrInvalidAutoChannel, channel)
	}
	modelStr := channel
	if channel != "auto" {
		modelStr = "auto/" + channel
	}
	if !autorouter.IsKnownAutoID(modelStr) {
		return AutoComboCandidatesResult{}, fmt.Errorf("%w: %q", ErrUnknownAutoChannel, modelStr)
	}
	candidates := []AutoComboCandidate{}
	for _, raw := range in.Pool {
		// Expand an allowlist candidate to one row per
		// connection; a candidate with neither a connection nor
		// an allowlist produces no row.
		connIDs := []string{raw.ConnectionID}
		if raw.ConnectionID == "" {
			connIDs = raw.AllowedConnectionIDs
		}
		for _, connID := range connIDs {
			if connID == "" {
				continue
			}
			modelStr := raw.ModelStr
			if modelStr == "" {
				modelStr = raw.Model
			}
			breakerState := raw.BreakerState
			if breakerState == "" {
				breakerState = "CLOSED"
			}
			// Breaker reachability: OPEN cannot execute; every
			// other state (including HALF_OPEN probes) can.
			breakerReachable := !strings.EqualFold(breakerState, "OPEN")
			candidates = append(candidates, AutoComboCandidate{
				Provider:            raw.Provider,
				ConnectionID:        connID,
				Model:               raw.Model,
				ModelStr:            modelStr,
				Excluded:            in.ExcludedConnectionIDs != nil && in.ExcludedConnectionIDs[connID],
				Reachable:           breakerReachable && !raw.ConnectionCooldown && !raw.ModelLocked,
				BreakerState:        breakerState,
				ConnectionCooldown:  raw.ConnectionCooldown,
				ModelLocked:         raw.ModelLocked,
				FreeAccessExclusion: raw.FreeAccessExclusion,
			})
		}
	}
	return AutoComboCandidatesResult{Channel: modelStr, Candidates: candidates}, nil
}
