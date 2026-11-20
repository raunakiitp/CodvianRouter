// Package mcp — per-client identity (M9.1, CHECKLIST 382).
//
// A ClientIdentity is who is calling a tool: what kind of caller,
// a stable id for the audit log, and the scopes that caller was
// granted. The scope check in the dispatch path consumes only this
// type — it never sees a raw key, a password, or a session token —
// so a future transport (M9.2 remote transports) can populate an
// identity from a gateway key without changing the scope or audit
// API: it calls GatewayKeyClient(keyID, scopes) per request and
// passes it to HandleWithIdentity.
//
// # Stdio trust decision (design, not accident)
//
// The stdio transport has exactly one caller: the local process
// that spawned the server — the operator, running on the same
// machine, under the same user, with the same filesystem access as
// the router itself. That caller can already read the config file
// (which holds provider keys), the data dir, and the binary. There
// is no privilege boundary between the stdio client and the
// operator to enforce: any scope restriction on stdio would be
// theatre, because the restricted caller could simply read the
// secrets the restriction pretends to protect, or restart the
// server without it. So the stdio construction explicitly opts
// into the local-operator identity with the full grant (`*`) via
// WithClientIdentity; Serve/Handle themselves default to
// Anonymous() (deny-all) since the M9.1 B1 fix, and a server that
// never opts in denies every tool call.
//
// This is safe ONLY because stdio is same-trust as the operator.
// It is NOT a precedent for remote transports: M9.2's SSE and
// Streamable HTTP endpoints are reachable over the network by
// callers who do NOT have local filesystem access, so there the
// identity MUST be the gateway key id plus that key's scopes, and
// a narrow key must be denied tools outside its scopes. A remote
// transport that defaulted to the local-operator identity would
// be a privilege-escalation hole; the reviewer should verify M9.2
// never constructs LocalOperator for a network caller. The
// Kind field exists so the audit log makes the distinction
// visible: `local-operator` entries are the operator, `gateway-key`
// entries are remote keys, and any future `anonymous` entry is a
// caller with no credentials at all (denied everything).
//
// The identity's Scopes are a defensive copy on construction, so
// a caller cannot widen its own grant by mutating a shared slice
// after the identity was built.
package mcp

// ClientKind distinguishes how a caller was identified.
type ClientKind string

const (
	// KindLocalOperator is the stdio caller: the local operator,
	// same trust as the router process itself (see above).
	KindLocalOperator ClientKind = "local-operator"
	// KindGatewayKey is a remote caller identified by a gateway
	// key id (M9.2). The ID is the key's id, never the key.
	KindGatewayKey ClientKind = "gateway-key"
	// KindAnonymous is a caller with no credentials. It holds no
	// scopes and is denied every tool.
	KindAnonymous ClientKind = "anonymous"
)

// ClientIdentity is one tool caller: kind, stable audit id, and
// granted scopes. The zero value is an anonymous caller with no
// scopes (deny everything) — there is no zero value that
// accidentally grants access.
type ClientIdentity struct {
	Kind   ClientKind `json:"kind"`
	ID     string     `json:"id"`
	Scopes []string   `json:"scopes"`
}

// LocalOperator returns the stdio identity: the local operator
// with the full grant. See the stdio trust decision above — this
// must only ever be used for the stdio transport, never for a
// network caller.
func LocalOperator() ClientIdentity {
	return ClientIdentity{Kind: KindLocalOperator, ID: "local-operator", Scopes: []string{"*"}}
}

// GatewayKeyClient returns the identity for a remote caller
// authenticated by a gateway key. keyID is the key's stable id
// (never the key material itself); scopes are that key's granted
// MCP scopes, copied. An empty scopes list means the key may call
// no tool — narrow keys are denied, never widened to a default.
func GatewayKeyClient(keyID string, scopes []string) ClientIdentity {
	cp := make([]string, len(scopes))
	copy(cp, scopes)
	return ClientIdentity{Kind: KindGatewayKey, ID: keyID, Scopes: cp}
}

// Anonymous returns the identity for a caller with no
// credentials: no scopes, denied every tool.
func Anonymous() ClientIdentity {
	return ClientIdentity{Kind: KindAnonymous, ID: "anonymous"}
}

// Check evaluates this identity against a tool's required scopes.
// It is the same decision the dispatch path makes (via
// CheckScopes), exposed so M9.2/M9.7 can pre-check without
// dispatching. An unknown tool is denied with reason
// "unknown tool: <name>" — the caller cannot probe for a tool's
// existence through the scope check; the dispatcher still answers
// -32602 for it.
func (id ClientIdentity) Check(tool string) ScopeCheckResult {
	required, known := RequiredScopes(tool)
	if !known {
		return ScopeCheckResult{
			Allowed: false,
			Granted: append([]string(nil), id.Scopes...),
			Reason:  "unknown tool: " + tool,
		}
	}
	return CheckScopes(id.Scopes, required)
}

// HasScope reports whether this identity holds one scope
// (wildcard-aware). It is a convenience for callers that need a
// single-scope question; the dispatch path uses Check, not this.
func (id ClientIdentity) HasScope(scope string) bool {
	for _, g := range id.Scopes {
		if ScopeMatches(g, scope) {
			return true
		}
	}
	return false
}
