// Per-key guardrail scoping (CHECKLIST 277 (d), LiteLLM #51080
// follow-up): a gateway key with a non-empty guardrails list runs
// only its listed guardrails — the lists 245 validated are now
// enforced. An empty list (or no key, or a key the config does
// not know) means the global blocking config, exactly as before.
package server

import (
	"net/http"
)

// keyGuardrailScope returns the guardrail scope for the caller's
// key: nil means "run the global set" (no per-key list, no
// verifiable key, or a key the config does not know — a static
// verifier key has no operator-set list); a non-nil list names
// exactly the guardrails that may run for this key (built-in
// kinds pii/secret/injection and configured external names —
// the names were validated at write/load time, so an unknown name
// can never arrive here). The returned slice is a copy: the
// caller's key record must never be mutated through it.
func (s *Server) keyGuardrailScope(r *http.Request) []string {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return nil
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok || len(key.Guardrails) == 0 {
		return nil
	}
	return append([]string(nil), key.Guardrails...)
}

// keyScopeRow is one scoped key in the guardrails feed: the key's
// id + display name (never a hash or a raw key — the keys feed
// already publishes these) and the guardrails it runs.
type keyScopeRow struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Guardrails []string `json:"guardrails"`
}

// keyScopingFeed is the per-key scoping block in GET
// /dashboard/guardrails: how many keys run a scoped list, how
// many keys exist at all, how many request-stage evaluations ran
// under a scope since this process started, and the scoped keys
// themselves. Empty (non-nil Keys) when no key scopes a list —
// an honest empty state, never an invented scope.
type keyScopingFeed struct {
	ScopedKeys  int           `json:"scoped_keys"`
	TotalKeys   int           `json:"total_keys"`
	ScopedEvals uint64        `json:"scoped_evals"`
	Keys        []keyScopeRow `json:"keys"`
}

// keyScoping builds the live scoping block from the loaded
// config: every gateway key with a non-empty guardrails list,
// in config order, plus the scoped-evaluation counter.
func (s *Server) keyScoping() keyScopingFeed {
	feed := keyScopingFeed{Keys: []keyScopeRow{}, ScopedEvals: s.scopedGuardrailEvals.Load()}
	if s.cfg == nil {
		return feed
	}
	feed.TotalKeys = len(s.cfg.GatewayKeys)
	for _, k := range s.cfg.GatewayKeys {
		if len(k.Guardrails) == 0 {
			continue
		}
		feed.ScopedKeys++
		feed.Keys = append(feed.Keys, keyScopeRow{
			ID:         k.ID,
			Name:       k.Name,
			Guardrails: append([]string(nil), k.Guardrails...),
		})
	}
	return feed
}
