// Package config — gateway keys.
//
// A gateway key is the credential a local client presents to the router
// (e.g. as "Authorization: Bearer <key>"). The router stores only the
// SHA-256 hash of the key, never the key itself; the raw key is shown
// exactly once, when it is issued.
//
// Raw key format: "crk_" followed by 32 characters drawn from
// [A-Za-z0-9] using crypto/rand. Record IDs look like "gk_<16 hex chars>".
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// GatewayKey is one stored gateway credential. KeyHash is the hex-encoded
// SHA-256 of the raw key; the raw key itself is never persisted.
//
// ExpiresAt is the operator-set expiry (CHECKLIST 195): nil means the
// key never expires. RequestsPerMinute / TokensPerMinute are the
// operator-set per-key traffic limits (CHECKLIST 195): 0 means no
// limit on that axis. Traffic is counted on locally estimated prompt
// tokens at admission (see internal/server/keytraffic.go) — completion
// tokens are not counted, and the counters live in memory (since
// server start, not across restarts).
//
// M5.2: RequestsPerDay caps requests per UTC calendar day (0 =
// unlimited); RateLimits are generic sliding-window rules
// (OmniRoute apiKeys.ts rateLimits[] / rateLimiter.ts: {limit,
// window_seconds}) — at most Limit requests in any sliding
// WindowSeconds window, checked alongside RPM/TPM/RPD.
type RateLimitRule struct {
	Limit         int `json:"limit"`
	WindowSeconds int `json:"window_seconds"`
}

// TokenLimit is one per-key token budget (M5.3, OmniRoute
// lib/db/tokenLimits.ts): at most Limit TOTAL tokens (prompt +
// completion, as recorded in the ledger) inside one reset window,
// scoped to a model, a provider, or globally. ScopeValue names
// the model/provider for model/provider scopes and must be empty
// for global. ResetInterval is daily/weekly/monthly ("" = daily),
// ResetTime "HH:MM" ("" = "00:00"), both UTC — the same window
// math as the calendar USD budgets (M5.1, ledger.BudgetWindowFor).
// Enabled nil = enabled.
type TokenLimit struct {
	Scope         string `json:"scope"`
	ScopeValue    string `json:"scope_value,omitempty"`
	Limit         int    `json:"limit"`
	ResetInterval string `json:"reset_interval,omitempty"`
	ResetTime     string `json:"reset_time,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

// IsEnabled reports whether the limit applies (nil = enabled).
func (t TokenLimit) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

type GatewayKey struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	KeyHash           string          `json:"key_hash"`
	Scopes            []string        `json:"scopes"`
	BudgetUSD         float64         `json:"budget_usd"`
	CreatedAt         time.Time       `json:"created_at"`
	ExpiresAt         *time.Time      `json:"expires_at,omitempty"`
	RequestsPerMinute int             `json:"requests_per_minute,omitempty"`
	TokensPerMinute   int             `json:"tokens_per_minute,omitempty"`
	RequestsPerDay    int             `json:"requests_per_day,omitempty"`
	RateLimits        []RateLimitRule `json:"rate_limits,omitempty"`
	TokenLimits       []TokenLimit    `json:"token_limits,omitempty"`
	// Guardrails names the guardrails that must guard this key's
	// requests (CHECKLIST 245, LiteLLM #51080): each name must
	// be registered — a built-in kind (pii, secret, injection)
	// or a configured external guardrail name. An unknown name
	// fails loudly at write time (key update) and at config
	// load, naming the name, instead of silently leaving the
	// key believing it is guarded by a guardrail that does not
	// exist. An empty list names no guardrail: the key is
	// guarded by the global blocking config exactly like a key
	// without the field.
	Guardrails []string `json:"guardrails,omitempty"`
	// Disabled is the per-key kill switch (M1.3): a disabled key
	// never verifies — VerifyGatewayKey skips it — so it stops
	// authenticating on the very next request, with no restart
	// and no expiry involved. It is reversible (clear the flag
	// via POST /admin/keys/{id}); the permanent removal is
	// DELETE /admin/keys/{id}. A disabled key keeps its scopes,
	// budget and limits, so re-enabling restores exactly the
	// key that was disabled. Rotation inherits the flag, so
	// rotating a disabled key cannot silently re-enable it.
	Disabled bool `json:"disabled,omitempty"`
	// AllowUsageCommand gates GET /api/usage/om-usage (M5.7,
	// OmniRoute's allowUsageCommand): the terminal-friendly
	// usage command answers only for keys the operator
	// explicitly enabled. Default false — a key that never
	// asked for the command gets a 403, never usage data.
	AllowUsageCommand bool `json:"allow_usage_command,omitempty"`
	// AccessSchedule is the per-key allowed-hours window (M5.10,
	// OmniRoute's accessSchedule: {enabled, from, until, days,
	// tz}). Outside the window the key is refused with a 403,
	// before any budget is touched. Nil = no schedule (the key
	// is usable at any hour).
	AccessSchedule *AccessScheduleConfig `json:"access_schedule,omitempty"`
	// ThrottleDelayMs is the per-request artificial delay (M5.10,
	// OmniRoute's throttleDelayMs): every request the key makes
	// waits this long before the engine is called. 0 = none.
	// DIVERGENCE (documented): OmniRoute stores the field but
	// does not enforce it in the current checkout; we enforce
	// it, because a stored throttle that never throttles would
	// be fake configuration.
	ThrottleDelayMs int `json:"throttle_delay_ms,omitempty"`
	// MaxSessions is the concurrent in-flight request cap for
	// the key (M5.10, OmniRoute's maxSessions): a request past
	// the cap is refused with a 429. 0 = no cap. DIVERGENCE
	// (documented): OmniRoute stores the field but enforces a
	// max-sessions cap only for VNC browser-login sessions,
	// never for gateway keys; our cap counts concurrent gateway
	// requests, the only reading that means anything here.
	MaxSessions int `json:"max_sessions,omitempty"`
	// G15 Area 4 — per-key network + model policy (OmniRoute
	// ApiKeyMetadata apiKeys.ts:108-157). Every field here is
	// ENFORCED on the request path (see AllowedIP /
	// ModelAllowed / EndpointAllowed and the server's
	// checkKeyTrafficModel): a field with no enforcement point
	// is not added. Empty-list semantics match OmniRoute's
	// keyTemplates contract: an empty IP allowlist, model
	// allowlist (in "all" mode), blocked list or endpoint list
	// means "no restriction" on that axis — except a
	// "restricted" model mode with an empty allowlist, which
	// denies every model (the same rule OmniRoute documents).
	//
	// IPAllowlist / IPBlocklist are exact IPs ("10.0.0.5") or
	// CIDRs ("10.0.0.0/8"). The blocklist always wins over the
	// allowlist. The peer IP is the real TCP peer
	// (r.RemoteAddr) — Host / X-Forwarded-For are never
	// consulted, the same rule the route guard uses.
	IPAllowlist []string `json:"ip_allowlist,omitempty"`
	IPBlocklist []string `json:"ip_blocklist,omitempty"`
	// ModelAccessMode is "" or "all" (every model allowed
	// unless blocked) or "restricted" (only AllowedModels).
	// BlockedModels always wins over AllowedModels.
	ModelAccessMode string   `json:"model_access_mode,omitempty"`
	AllowedModels   []string `json:"allowed_models,omitempty"`
	BlockedModels   []string `json:"blocked_models,omitempty"`
	// AllowedEndpoints restricts which request paths the key
	// may call (exact paths, or a trailing "*" prefix such as
	// "/v1/*"). Empty = every endpoint.
	AllowedEndpoints []string `json:"allowed_endpoints,omitempty"`
	// KeyGroups names the key groups this key belongs to (AK-1,
	// GAP-TRACK §14): group IDs, resolved against the config's
	// `key_groups` records (see KeyGroups in keygroups.go). The
	// group layer is an additional restriction intersected with
	// the per-key policy above: a request must pass BOTH. A key
	// in no groups is unaffected. A key whose memberships resolve
	// to no existing group, or only to disabled groups, is
	// refused for every model (fail closed, never unrestricted).
	// NOTE: Config.Clone shares this slice like the other policy
	// lists (config.go Clone deep-copies only Scopes, Guardrails,
	// limits and the access schedule) — treat it as immutable
	// after load; a future config slice should deep-copy all
	// policy lists in Clone.
	KeyGroups []string `json:"key_groups,omitempty"`
	// ForcedModel pins the key to one model: a request naming
	// any other model is refused with a 403 naming the pin.
	// DIVERGENCE (documented): OmniRoute silently routes a
	// forced-model key to the pinned model; we refuse instead,
	// so a caller is never served a different model than the
	// one it asked for without knowing.
	ForcedModel string `json:"forced_model,omitempty"`
}

// AccessScheduleConfig is one key's allowed-hours window
// (OmniRoute AccessSchedule): From/Until are "HH:MM" in TZ
// (IANA; "" = server local), Days are JS getDay() values
// (0 = Sunday). Until <= From means an overnight window
// (e.g. 22:00–06:00).
type AccessScheduleConfig struct {
	Enabled bool   `json:"enabled"`
	From    string `json:"from"`
	Until   string `json:"until"`
	Days    []int  `json:"days,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

const (
	// GatewayKeyPrefix is prepended to every raw gateway key.
	GatewayKeyPrefix = "crk_"
	// gatewayKeyRandomLen is the number of random characters after the prefix.
	gatewayKeyRandomLen = 32
)

var gatewayKeyAlphabet = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

// HashKey returns the hex-encoded SHA-256 of a raw gateway key.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// randomString returns n characters drawn uniformly from gatewayKeyAlphabet
// using crypto/rand (rejection sampling, so no modulo bias).
func randomString(n int) (string, error) {
	out := make([]rune, n)
	max := big.NewInt(int64(len(gatewayKeyAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("config: cannot generate random key material: %w", err)
		}
		out[i] = gatewayKeyAlphabet[v.Int64()]
	}
	return string(out), nil
}

// IssueKey creates a new gateway key record and returns it together with
// the raw key. The raw key is returned exactly once — callers must show or
// store it now, because only the hash is kept in the returned record.
// name is a human label (e.g. "default"); scopes is a list of permission
// strings (e.g. "chat", "models"); budgetUSD is this key's own spend cap
// in USD (0 = no per-key cap; the global Config.BudgetUSD still applies).
func IssueKey(name string, scopes []string, budgetUSD float64) (GatewayKey, string, error) {
	if name == "" {
		return GatewayKey{}, "", errors.New("config: gateway key name must not be empty")
	}
	if budgetUSD < 0 {
		return GatewayKey{}, "", fmt.Errorf("config: gateway key budget must not be negative (got %v)", budgetUSD)
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return GatewayKey{}, "", fmt.Errorf("config: cannot generate key id: %w", err)
	}
	secret, err := randomString(gatewayKeyRandomLen)
	if err != nil {
		return GatewayKey{}, "", err
	}
	raw := GatewayKeyPrefix + secret
	key := GatewayKey{
		ID:        "gk_" + hex.EncodeToString(idBytes),
		Name:      name,
		KeyHash:   HashKey(raw),
		Scopes:    append([]string(nil), scopes...),
		BudgetUSD: budgetUSD,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	return key, raw, nil
}

// Verify reports whether raw is the key this record was issued for, using
// a constant-time comparison of the SHA-256 hashes.
func (k GatewayKey) Verify(raw string) bool {
	if k.KeyHash == "" || raw == "" {
		return false
	}
	want, err := hex.DecodeString(k.KeyHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(raw))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// Expired reports whether the key's expiry has passed at now. A key
// with no expiry (ExpiresAt nil) never expires. Expiry is inclusive:
// at exactly ExpiresAt the key is already expired.
func (k GatewayKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// HasScope reports whether this key carries the given scope.
func (k GatewayKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// G15 Area 4 — per-key policy enforcement (IP, model, endpoint).
// ---------------------------------------------------------------------------

// KeyPolicy is the enforceable per-key policy bundle shared by key
// creation (template + overrides), key update and key import: one
// shape, one validation, one application — so a policy means the
// same thing whichever endpoint wrote it. It is deliberately the
// enforceable subset of OmniRoute's ~40 ApiKeyMetadata fields
// (apiKeys.ts:108-157): every field here has an enforcement point
// (AllowedIP / ModelAllowed / EndpointAllowed, called from the
// server's checkKeyTrafficModel). Fields with no honest
// enforcement point in a local single-binary router are NOT
// carried — see the not-implemented list on
// KeyPolicyNotImplemented in the server package.
type KeyPolicy struct {
	IPAllowlist      []string `json:"ip_allowlist,omitempty"`
	IPBlocklist      []string `json:"ip_blocklist,omitempty"`
	ModelAccessMode  string   `json:"model_access_mode,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	BlockedModels    []string `json:"blocked_models,omitempty"`
	AllowedEndpoints []string `json:"allowed_endpoints,omitempty"`
	ForcedModel      string   `json:"forced_model,omitempty"`
	// KeyGroups names the key groups this key belongs to (AK-1):
	// group IDs, resolved against the config's `key_groups`
	// records at enforcement time. Membership travels with the
	// policy bundle so create / update / import carry it, and
	// so a key's exported policy round-trips it. Group
	// existence is NOT validated here — there is no group store
	// at this layer — a dangling membership fails closed at
	// enforcement (GroupModelDecision), never at write time.
	KeyGroups []string `json:"key_groups,omitempty"`
}

// PolicyOf returns this key's enforceable policy bundle.
func (k GatewayKey) PolicyOf() KeyPolicy {
	return KeyPolicy{
		IPAllowlist:      append([]string(nil), k.IPAllowlist...),
		IPBlocklist:      append([]string(nil), k.IPBlocklist...),
		ModelAccessMode:  k.ModelAccessMode,
		AllowedModels:    append([]string(nil), k.AllowedModels...),
		BlockedModels:    append([]string(nil), k.BlockedModels...),
		AllowedEndpoints: append([]string(nil), k.AllowedEndpoints...),
		ForcedModel:      k.ForcedModel,
		KeyGroups:        append([]string(nil), k.KeyGroups...),
	}
}

// ApplyPolicy replaces this key's enforceable policy with p
// (deep-copied, so later mutation of p cannot change the key).
func (k *GatewayKey) ApplyPolicy(p KeyPolicy) {
	k.IPAllowlist = append([]string(nil), p.IPAllowlist...)
	k.IPBlocklist = append([]string(nil), p.IPBlocklist...)
	k.ModelAccessMode = p.ModelAccessMode
	k.AllowedModels = append([]string(nil), p.AllowedModels...)
	k.BlockedModels = append([]string(nil), p.BlockedModels...)
	k.AllowedEndpoints = append([]string(nil), p.AllowedEndpoints...)
	k.ForcedModel = p.ForcedModel
	k.KeyGroups = append([]string(nil), p.KeyGroups...)
}

// Validate reports whether p is a well-formed policy: IP entries
// must be an IP or a CIDR, ModelAccessMode must be "", "all" or
// "restricted", and no list may contain an empty entry. Invalid
// policy is refused at write time (create / update / import),
// never stored and silently unenforced.
func (p KeyPolicy) Validate() error {
	for _, e := range p.IPAllowlist {
		if err := ValidateIPEntry(e); err != nil {
			return fmt.Errorf("config: ip_allowlist entry %q: %w", e, err)
		}
	}
	for _, e := range p.IPBlocklist {
		if err := ValidateIPEntry(e); err != nil {
			return fmt.Errorf("config: ip_blocklist entry %q: %w", e, err)
		}
	}
	switch p.ModelAccessMode {
	case "", "all", "restricted":
	default:
		return fmt.Errorf("config: model_access_mode %q must be \"all\" or \"restricted\"", p.ModelAccessMode)
	}
	for _, list := range [][]string{p.AllowedModels, p.BlockedModels, p.AllowedEndpoints, p.KeyGroups} {
		for _, e := range list {
			if strings.TrimSpace(e) == "" {
				return errors.New("config: policy lists must not contain an empty entry")
			}
		}
	}
	// A forced model must be servable under this policy, or the
	// key can never serve any model: it must not be blocklisted,
	// and under "restricted" it must be in the allowlist. A
	// contradictory combination is refused at write time (loud
	// 400), never stored as a key that 403s every request.
	if p.ForcedModel != "" {
		for _, b := range p.BlockedModels {
			if b == p.ForcedModel {
				return fmt.Errorf("config: forced_model %q is blocklisted", p.ForcedModel)
			}
		}
		if p.ModelAccessMode == "restricted" {
			found := false
			for _, a := range p.AllowedModels {
				if a == p.ForcedModel {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("config: forced_model %q is not in allowed_models (restricted mode)", p.ForcedModel)
			}
		}
	}
	return nil
}

// ValidateIPEntry reports whether entry is an IP ("10.0.0.5") or
// a CIDR ("10.0.0.0/8"). Anything else is refused at write time.
func ValidateIPEntry(entry string) error {
	if strings.TrimSpace(entry) == "" {
		return errors.New("empty IP entry")
	}
	if strings.Contains(entry, "/") {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return fmt.Errorf("not a CIDR: %w", err)
		}
		return nil
	}
	if net.ParseIP(entry) == nil {
		return fmt.Errorf("not an IP address: %q", entry)
	}
	return nil
}

// matchPolicyPattern reports whether value matches pattern: an
// exact match, a trailing-"*" prefix match ("/v1/*", "gpt-*"),
// or a bare "*" matching everything. Anything else is exact.
func matchPolicyPattern(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == value
}

// ipListMatch reports whether ip matches any entry of list: a
// CIDR entry contains the IP, a plain entry matches the same IP
// in any spelling (canonicalised — see below). Entries that fail
// to parse never match (they are refused at write time by
// Validate, so this is a belt-and-braces rule).
func ipListMatch(list []string, ip string) bool {
	parsed := net.ParseIP(ip)
	for _, e := range list {
		if strings.Contains(e, "/") {
			_, network, err := net.ParseCIDR(e)
			if err == nil && parsed != nil && network.Contains(parsed) {
				return true
			}
			continue
		}
		// Plain-IP entries compare as parsed IPs, not strings
		// (G15 re-review nit 12): "2001:db8::1" and its expanded
		// form "2001:0db8:0000:0000:0000:0000:0000:0001" are the
		// same peer, and a non-canonical spelling must not evade a
		// blocklist entry. Unparseable entries (e.g. a zone id)
		// fall back to the exact string equality just below.
		if e == ip {
			return true
		}
		if entryIP := net.ParseIP(e); entryIP != nil && parsed != nil && entryIP.Equal(parsed) {
			return true
		}
	}
	return false
}

// AllowedIP reports whether a request from peer ip may use this
// key. The blocklist always wins over the allowlist. A non-empty
// allowlist admits only listed IPs/CIDRs. An unknown peer (empty
// ip) fails closed whenever either list is non-empty — a key with
// an IP policy must never be usable from an unidentifiable peer.
// A key with no lists at all admits every peer, exactly as
// before this policy existed.
func (k GatewayKey) AllowedIP(ip string) bool {
	if len(k.IPAllowlist) == 0 && len(k.IPBlocklist) == 0 {
		return true
	}
	if ip == "" {
		return false
	}
	if ipListMatch(k.IPBlocklist, ip) {
		return false
	}
	if len(k.IPAllowlist) == 0 {
		return true
	}
	return ipListMatch(k.IPAllowlist, ip)
}

// ModelAllowed reports whether model may be served to this key.
// BlockedModels always wins over AllowedModels. In "restricted"
// mode only AllowedModels may be served — and a restricted mode
// with an empty allowlist denies every model (OmniRoute's
// documented rule). In "all" mode ("" or "all") every model is
// allowed unless blocked. An empty model is allowed here: the
// caller skips the check when the request names no model (the
// engine default applies), rather than this method guessing.
func (k GatewayKey) ModelAllowed(model string) bool {
	if model == "" {
		return true
	}
	for _, b := range k.BlockedModels {
		if matchPolicyPattern(b, model) {
			return false
		}
	}
	if k.ModelAccessMode == "restricted" {
		for _, a := range k.AllowedModels {
			if matchPolicyPattern(a, model) {
				return true
			}
		}
		return false
	}
	return true
}

// EndpointAllowed reports whether request path may be called
// with this key. Empty = every endpoint. Otherwise the path
// must match an entry (exact, or a trailing-"*" prefix).
func (k GatewayKey) EndpointAllowed(path string) bool {
	if len(k.AllowedEndpoints) == 0 {
		return true
	}
	for _, e := range k.AllowedEndpoints {
		if matchPolicyPattern(e, path) {
			return true
		}
	}
	return false
}

// NewKeyID mints a fresh gateway-key record ID ("gk_" + 16 hex
// chars), the same shape IssueKey uses — for key import, where
// the caller supplies the secret but not (necessarily) an ID.
func NewKeyID() (string, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("config: cannot generate key id: %w", err)
	}
	return "gk_" + hex.EncodeToString(idBytes), nil
}

// AddGatewayKey appends key to the config's gateway key list, rejecting a
// duplicate ID.
func (c *Config) AddGatewayKey(key GatewayKey) error {
	for _, existing := range c.GatewayKeys {
		if existing.ID == key.ID {
			return fmt.Errorf("config: gateway key id %q already exists", key.ID)
		}
	}
	c.GatewayKeys = append(c.GatewayKeys, key)
	return nil
}

// FindGatewayKey returns the key with the given ID, if present.
func (c *Config) FindGatewayKey(id string) (GatewayKey, bool) {
	for _, k := range c.GatewayKeys {
		if k.ID == id {
			return k, true
		}
	}
	return GatewayKey{}, false
}

// VerifyGatewayKey checks raw against every stored gateway key and returns
// the matching key, if any. This is the auth check the server calls per
// request: auth is ON whenever at least one key exists, and a request with
// no matching key is rejected. A disabled key never verifies (M1.3):
// disabling takes effect on the next request, with no restart.
func (c *Config) VerifyGatewayKey(raw string) (GatewayKey, bool) {
	for _, k := range c.GatewayKeys {
		if k.Disabled {
			continue
		}
		if k.Verify(raw) {
			return k, true
		}
	}
	return GatewayKey{}, false
}

// VerifyGatewayKeyDisabled reports whether raw matches a stored gateway
// key that is disabled, so the server's 401 can name the disable
// instead of a generic auth failure. An unknown raw value, or a
// raw value matching only enabled keys, reports false.
func (c *Config) VerifyGatewayKeyDisabled(raw string) bool {
	for _, k := range c.GatewayKeys {
		if k.Disabled && k.Verify(raw) {
			return true
		}
	}
	return false
}

// VerifyGatewayKeyExpired reports whether raw matches a stored gateway
// key whose expiry has passed, and the expiry time when it has. It is
// the companion to VerifyGatewayKey: the server's auth check rejects
// an expired key, and this names the reason (an honest 401 naming
// expiry, never a generic auth failure). A key with no expiry, or a
// raw value matching no key at all, reports false.
func (c *Config) VerifyGatewayKeyExpired(raw string) (time.Time, bool) {
	for _, k := range c.GatewayKeys {
		if k.Verify(raw) && k.Expired(time.Now()) {
			return *k.ExpiresAt, true
		}
	}
	return time.Time{}, false
}
