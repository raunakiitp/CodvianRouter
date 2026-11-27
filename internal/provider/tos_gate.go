package provider

// ToS risk-acknowledgement gate (A3 item 6).
//
// The catalog's ToSGrey marking (web-cookie / subscription-auth
// providers) is the gate flag. Connecting such a provider means
// handing over the operator's own web session or subscription
// account by their own choice: the provider's terms of service may
// forbid this kind of use and the connected account can be suspended
// or banned. The former hard refusals — at add time, at config load
// and per request — become gate checks: they pass only when the
// operator has explicitly acknowledged that risk for the provider.
// The acknowledgement itself is recorded by the server's
// risk-acknowledgement endpoint (POST /api/providers/{id}/risk-
// acknowledgement) and surfaced as a plain-words notice in the
// dashboard. No proxying trickery is involved anywhere: the user
// hands over their own session, knowingly.

import (
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// ToSGateError is the typed refusal for a ToS-grey catalog provider
// whose risk has not been acknowledged. Callers distinguish it from
// downstream auth failures with errors.As; the message states the
// substance of the risk in plain words and tells the operator where
// to acknowledge it. Layer names which gate check refused ("add",
// "config-load" or "request").
type ToSGateError struct {
	CatalogID string
	Name      string
	Layer     string
}

func (e *ToSGateError) Error() string {
	return fmt.Sprintf(
		"catalog provider %q (%s) is ToS-grey (web-cookie / subscription auth): its terms of service may forbid this kind of use and the account you connect can be suspended or banned. Nothing has been sent. If you accept this risk, acknowledge it in the dashboard (Providers — risk acknowledgement) or POST /api/providers/%s/risk-acknowledgement, then retry (refused at the %s gate)",
		e.CatalogID, e.Name, e.CatalogID, e.Layer)
}

// WithToSAcknowledgement wires the acknowledgement source into the
// registry. fn reports whether the operator has acknowledged the ToS
// risk for a ToS-grey catalog provider ID; production wires this to
// the server's acknowledgement store (tos_gate.go). With no function
// wired (the default), nothing is acknowledged and the gate stays
// closed for every ToS-grey provider — the former hard refusal.
func WithToSAcknowledgement(fn func(catalogProviderID string) bool) RegistryOption {
	return func(r *Registry) { r.tosAckFn = fn }
}

// SetToSAcknowledgement sets the acknowledgement source after
// construction (production builds the registry before the server that
// owns the acknowledgement store, so it wires the gate once the
// server exists). Safe to call before any client is built.
func (r *Registry) SetToSAcknowledgement(fn func(catalogProviderID string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tosAckFn = fn
}

// tosAcknowledged reports whether the ToS risk for a ToS-grey catalog
// provider has been acknowledged. A nil acknowledgement source means
// no: the gate refuses rather than waving anything through.
func (r *Registry) tosAcknowledged(catalogProviderID string) bool {
	r.mu.RLock()
	fn := r.tosAckFn
	r.mu.RUnlock()
	return fn != nil && fn(catalogProviderID)
}

// tosGateRefusal is the gate check shared by the three layers. It
// returns the typed plain-words refusal for an unacknowledged
// ToS-grey provider, wrapped with the layer that refused.
func (r *Registry) tosGateRefusal(entryID, entryName, layer string) error {
	return &ToSGateError{CatalogID: entryID, Name: entryName, Layer: layer}
}

// webCookieHeadersFor resolves the static header for a web-cookie
// surface the catalog refuses to resolve (ToS-grey, auth.go): the
// credential is the operator's own session cookie, sent verbatim as
// the surface's Cookie header. The caller checks tosAcknowledged —
// this must never run for an unacknowledged provider. ok is false
// when the credential is empty: an empty cookie is never turned into
// a header.
func webCookieHeadersFor(entry catalog.Provider, key string) (map[string]string, string, bool) {
	if strings.TrimSpace(key) == "" {
		return nil, "", false
	}
	return map[string]string{"Cookie": key}, string(entry.DefaultWireFormat()), true
}
