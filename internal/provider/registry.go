package provider

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// ErrProviderNotFound is returned when no provider with the given ID exists.
var ErrProviderNotFound = errors.New("provider not found")

// ErrProviderExists is returned when a provider with the same normalized
// base URL is already registered.
var ErrProviderExists = errors.New("provider with this base URL already exists")

// ProviderPreset is a known provider the user can pick with one click and
// then paste their own API key into. Only the base URL is prescribed —
// no other endpoint, key, or credential is invented here.
type ProviderPreset struct {
	Name    string
	BaseURL string
}

// CodvianPreset is Codvian as a provider option: base URL only, the user
// pastes their own Codvian API key. Do not invent any other Codvian
// endpoint; everything else goes through the OpenAI-compatible paths.
var CodvianPreset = ProviderPreset{
	Name:    "Codvian",
	BaseURL: "https://api.codvian.com/v1",
}

// KeyCipher encrypts API keys at rest. The production implementation is
// the AES-256-GCM Vault in internal/crypto, adapted by NewVaultCipher.
// Registry only ever talks to this interface, so the cipher is one
// assignment (WithCipher) plus the master secret's data directory.
type KeyCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// vaultCipher adapts a *crypto.Vault to KeyCipher: Seal/Unseal under the
// Vault's master secret (AES-256-GCM, "v1:" + base64). A nil Vault makes
// every call fail with crypto's nil-vault error; it never stores or
// returns plaintext.
type vaultCipher struct {
	v *crypto.Vault
}

// NewVaultCipher returns a KeyCipher backed by v (a Vault created from
// the router's data directory, e.g. via crypto.New). This is the
// production cipher: pass it to NewRegistry with WithCipher.
func NewVaultCipher(v *crypto.Vault) KeyCipher {
	return vaultCipher{v: v}
}

func (c vaultCipher) Encrypt(plaintext string) (string, error) {
	if c.v == nil {
		return "", errors.New("provider: nil vault cipher")
	}
	return c.v.Seal([]byte(plaintext))
}

func (c vaultCipher) Decrypt(ciphertext string) (string, error) {
	if c.v == nil {
		return "", errors.New("provider: nil vault cipher")
	}
	plain, err := c.v.Unseal(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// defaultCipher is the cipher NewRegistry falls back to when no cipher
// was supplied with WithCipher: a Vault over a fresh random in-memory
// master secret. It is ephemeral by design — keys sealed with it cannot
// be unsealed by another Registry instance — so it is only safe for
// memory-only registries (nil store) and tests. Any registry backed by
// a persistent Store MUST be given a Vault-backed cipher via WithCipher
// (rooted at the router's data directory), or keys sealed in one run
// will be undecryptable in the next.
func defaultCipher() KeyCipher {
	var secret [32]byte
	if _, err := cryptorand.Read(secret[:]); err != nil {
		return nil
	}
	v, err := crypto.NewFromSecret(secret[:], "")
	if err != nil {
		return nil
	}
	return NewVaultCipher(v)
}

// Store persists provider configs. The Registry never touches files
// directly; a config-backed Store (wrapping internal/config's Providers
// list once it exists) can replace JSONFileStore without other changes.
type Store interface {
	Load() ([]ProviderConfig, error)
	Save([]ProviderConfig) error
}

// JSONFileStore is a Store backed by a single JSON file holding a
// []ProviderConfig. Writes are atomic (temp file + rename) with 0600
// permissions, because the file holds (encrypted) API keys. A missing file
// loads as an empty list.
type JSONFileStore struct {
	path string
}

// NewJSONFileStore returns a Store for path.
func NewJSONFileStore(path string) *JSONFileStore { return &JSONFileStore{path: path} }

// Path returns the backing file path.
func (s *JSONFileStore) Path() string { return s.path }

func (s *JSONFileStore) Load() ([]ProviderConfig, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading provider store %s: %w", s.path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var configs []ProviderConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		return nil, fmt.Errorf("parsing provider store %s: %w", s.path, err)
	}
	return configs, nil
}

func (s *JSONFileStore) Save(configs []ProviderConfig) error {
	if configs == nil {
		configs = []ProviderConfig{}
	}
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding provider store: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating provider store dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".providers-*.tmp")
	if err != nil {
		return fmt.Errorf("creating provider store temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing provider store: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("securing provider store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing provider store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("saving provider store %s: %w", s.path, err)
	}
	return nil
}

// ClientFactory builds a Client for a base URL + plaintext API key. The
// default is NewClient; tests inject fakes through it.
//
// authHeaders carries the exact auth headers resolved for the provider
// (catalog.ResolveAuth per provider + endpoint); nil means the legacy
// default (`Authorization: Bearer <key>`), a non-nil map — possibly
// empty, for no-auth surfaces — replaces it entirely. Factories must
// honour nil vs non-nil: a no-auth provider must send no credential,
// not a Bearer header it never asked for.
//
// wireFormat is the wire the client must speak ("openai", "anthropic",
// "gemini", ...); "" means the OpenAI default. "anthropic" and
// "gemini" change the client (Messages / generateContent clients);
// other formats fall back to OpenAI wire (see WithWireFormat), so a
// provider added before native transports exist keeps its previous
// behaviour.
type ClientFactory func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (Client, error)

// Registry is the in-memory provider registry, persisted through a Store.
// It is safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	providers  map[string]*Provider
	order      []string // insertion order (List order)
	store      Store
	cipher     KeyCipher
	newClient  ClientFactory
	tfoEnabled bool // TCP Fast Open on clients built by the default factory (default true)
	// allowPrivateDestinations allows the registry to accept loopback,
	// private, link-local or cloud-metadata base URLs. This is an
	// operator override for on-prem models or test mocks; the default
	// (false) rejects such destinations to prevent SSRF (7A).
	allowPrivateDestinations bool
	// pools owns the warm HTTP/2 transports, one per (provider, key)
	// (see pool.go). Every client ClientFor builds shares its pair's
	// pool, so a provider's connections stay warm across requests,
	// health probes and engine clients instead of re-dialing.
	pools *PoolManager
	// httpAdapter is the plugin provider-adapter hook (CHECKLIST
	// 201), set by main from the plugins registry. Nil means no
	// adapter is registered for any provider. Guarded by mu: it
	// is set once at startup before any client is built, and
	// read on every ClientFor.
	httpAdapter HTTPAdapterFunc
	// authCache memoizes resolveAuthHeaders results, one entry per
	// provider ID (see cachedAuth). The resolution walks the whole
	// catalog — catalog.All deep-clones every provider, and
	// ResolveAuthForBaseURL calls it up to twice, with the
	// catalogWireForBaseURL fallback calling it twice more — so an
	// uncached resolve costs ~4 full catalog clones per request
	// for a base-URL provider (benchmark round 2, phase 2 profile:
	// catalog.All 42.7% of all CPU on the request path, via the
	// per-request readiness check in server.checkLocalReadiness).
	// The cached result is keyed by a fingerprint of every input
	// the resolution depends on (see authFingerprint), so a key
	// rotation, base-URL change or catalog relink can never serve
	// a stale header; it only costs a recompute. authMu guards the
	// map only — the (slow) resolution itself runs without it.
	authMu    sync.Mutex
	authCache map[string]cachedAuth
	// oauthTokenFn resolves a stored OAuth access token for a catalog
	// provider ID. Set by main from the server's OAuth connection store
	// (WithOAuthTokenResolver). Nil means OAuth surfaces stay refused:
	// the resolver is the only source of OAuth credentials and it is
	// never faked. Guarded by mu (set once at startup).
	oauthTokenFn func(catalogProviderID string) (string, error)
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithCipher overrides the key cipher. The production cipher is
// NewVaultCipher(crypto.New(dataDir)); tests use it with
// crypto.NewFromSecret, or rely on the ephemeral default (see
// defaultCipher). Pass WithCipher for any registry backed by a
// persistent Store.
func WithCipher(cipher KeyCipher) RegistryOption {
	return func(r *Registry) { r.cipher = cipher }
}

// WithClientFactory overrides how upstream clients are built (tests).
func WithClientFactory(f ClientFactory) RegistryOption {
	return func(r *Registry) { r.newClient = f }
}

// WithOAuthTokenResolver wires the OAuth connection store into auth
// resolution. When a catalog surface's auth kind is OAuth
// (AuthOAuth — device-code or PKCE), the per-request resolver calls
// fn(catalogProviderID) to fetch the stored access token and sends it
// as `Authorization: Bearer <token>`. Production wires this to the
// server's oauthStore.GetAccessToken. With no resolver wired, OAuth
// surfaces stay refused — the token is never fabricated.
func WithOAuthTokenResolver(fn func(catalogProviderID string) (string, error)) RegistryOption {
	return func(r *Registry) { r.oauthTokenFn = fn }
}

// SetOAuthTokenResolver sets the OAuth token resolver after
// construction (production builds the registry before the server that
// owns the OAuth store, so it wires the resolver once the server
// exists). Safe to call before any client is built.
func (r *Registry) SetOAuthTokenResolver(fn func(catalogProviderID string) (string, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oauthTokenFn = fn
}

// isOAuthExchangeKind reports whether an exchange kind is an OAuth
// flow (device code, PKCE, or any kind naming oauth).
func isOAuthExchangeKind(kind string) bool {
	k := strings.ToLower(kind)
	return strings.Contains(k, "oauth")
}

// WithTFOEnabled enables or disables TCP Fast Open on the upstream
// clients the registry builds (default true). Disabling is only for
// measuring the with/without difference (see provider.TFOStatus);
// the dialer falls back gracefully either way, so disabling is
// never needed for correctness.
func WithTFOEnabled(enabled bool) RegistryOption {
	return func(r *Registry) {
		r.tfoEnabled = enabled
		r.pools.SetTFOEnabled(enabled)
	}
}

// WithRegistryAllowPrivateDestinations allows the registry to accept
// loopback, private, link-local or cloud-metadata base URLs when
// adding providers. This is an operator override for on-prem models
// or test mocks; the default (false) rejects such destinations to
// prevent SSRF (7A).
func WithRegistryAllowPrivateDestinations(allow bool) RegistryOption {
	return func(r *Registry) { r.allowPrivateDestinations = allow }
}

// WithPoolConfig sets the warm-pool sizing for the upstream clients
// the registry builds (see PoolConfig in pool.go — Little's Law
// sizing from expected_rps × expected_latency_ms, 0 fields mean
// defaults). Pools created before this option applies keep the size
// they were built with; in serve mode the option is applied at
// construction, before any client is built.
func WithPoolConfig(cfg PoolConfig) RegistryOption {
	return func(r *Registry) { r.pools.SetConfig(cfg) }
}

// SetHTTPAdapter installs the plugin provider-adapter hook
// (CHECKLIST 201). It is called once at startup by main, before
// any client is built; every client built afterwards carries the
// hook and its provider ID, so the adapter sees the outgoing
// request for exactly the provider it registered for.
func (r *Registry) SetHTTPAdapter(fn HTTPAdapterFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpAdapter = fn
}

// httpAdapterSetter is implemented by clients that can carry the
// plugin adapter hook (openAIClient, promoted to every native-wire
// wrapper through the embedded client). Factory-built fakes in
// tests do not implement it and are left alone.
type httpAdapterSetter interface {
	setHTTPAdapter(providerID string, fn HTTPAdapterFunc)
}

// attachHTTPAdapter gives a freshly built client its provider ID
// and the adapter hook, when one is installed. A nil hook means no
// adapter: the client is returned untouched.
func (r *Registry) attachHTTPAdapter(providerID string, client Client) Client {
	r.mu.RLock()
	fn := r.httpAdapter
	r.mu.RUnlock()
	if fn == nil {
		return client
	}
	if s, ok := client.(httpAdapterSetter); ok {
		s.setHTTPAdapter(providerID, fn)
	}
	return client
}

// NewRegistry returns an empty Registry backed by store (nil store means
// memory-only, no persistence). Use Load to populate it from the store.
// With no WithCipher option the registry uses an ephemeral in-memory
// Vault-backed cipher (see defaultCipher): fine for memory-only use and
// tests, but a registry backed by a persistent Store must be given a
// Vault-backed cipher via WithCipher, or sealed keys will not survive a
// restart.
func NewRegistry(store Store, opts ...RegistryOption) *Registry {
	r := &Registry{
		providers:  make(map[string]*Provider),
		store:      store,
		tfoEnabled: true,
		pools:      NewPoolManager(PoolConfig{}, true),
		authCache:  make(map[string]cachedAuth),
	}
	// The default factory reads r.tfoEnabled at call time (not at
	// construction), so WithTFOEnabled applied after NewRegistry
	// still affects clients built later.
	r.newClient = func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (Client, error) {
		return NewClient(baseURL, apiKey, WithAuthHeaders(authHeaders), WithWireFormat(wireFormat), WithTCPFastOpen(r.tfoEnabled), WithAllowPrivateDestinations(r.allowPrivateDestinations))
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	if r.cipher == nil {
		r.cipher = defaultCipher()
	}
	return r
}

// NewFileRegistry opens (or creates) a Registry persisted at path
// (JSON file store) and loads any providers already stored there. Pass a
// Vault-backed cipher via WithCipher (e.g. NewVaultCipher with a Vault
// rooted at the router's data directory) for persisted keys to survive
// restarts; without one the ephemeral default cipher is used (see
// defaultCipher) and sealed keys are undecryptable by the next process.
func NewFileRegistry(path string, opts ...RegistryOption) (*Registry, error) {
	r := NewRegistry(NewJSONFileStore(path), opts...)
	if err := r.Load(); err != nil {
		return nil, err
	}
	return r, nil
}

// Load replaces the in-memory registry with the contents of the store.
// Unknown wire formats / quota hints are preserved as stored.
func (r *Registry) Load() error {
	if r.store == nil {
		return nil
	}
	configs, err := r.store.Load()
	if err != nil {
		return err
	}
	providers := make(map[string]*Provider, len(configs))
	order := make([]string, 0, len(configs))
	for _, cfg := range configs {
		if cfg.ID == "" {
			continue // skip corrupt rows rather than failing the whole load
		}
		if _, dup := providers[cfg.ID]; dup {
			continue
		}
		p := ProviderFromConfig(cfg)
		if p.CatalogID != "" {
			// A catalog-linked provider resolves its base URL, wire
			// format and verification status from the catalog entry.
			// A bad catalog_id is a config error: fail the load
			// loudly rather than starting with a silently wrong
			// provider. Exchange auth is NOT refused here — the
			// client cannot be built for it yet, and ClientFor
			// fails loudly at startup, where the provider is
			// skipped with a warning instead of taking the
			// gateway down.
			if err := r.resolveCatalogForLoad(p); err != nil {
				return err
			}
		} else if rem, ok := IsRemovedBaseURL(p.BaseURL); ok {
			// CT-2: a config-file provider with an explicit base
			// URL and no catalog link bypasses
			// resolveCatalogForLoad entirely, so the removed-
			// domain check has to run here too — otherwise a
			// removed provider comes back through the config
			// file with an explicit base URL.
			return fmt.Errorf("provider %q: %w", p.ID, &catalog.RemovedProviderError{
				Entry:   catalog.Provider{ID: rem.ID, BaseURLs: []string{p.BaseURL}},
				Removed: rem,
			})
		}
		providers[p.ID] = p
		order = append(order, p.ID)
	}
	r.mu.Lock()
	r.providers = providers
	r.order = order
	r.mu.Unlock()
	return nil
}

// saveLocked persists the current registry. Caller must hold r.mu.
func (r *Registry) saveLocked() error {
	if r.store == nil {
		return nil
	}
	configs := make([]ProviderConfig, 0, len(r.order))
	for _, id := range r.order {
		if p, ok := r.providers[id]; ok {
			configs = append(configs, p.ToConfig())
		}
	}
	return r.store.Save(configs)
}

// addTimeout bounds the upstream calls Add makes (key test, model fetch,
// wire-format probe).
const addTimeout = 30 * time.Second

// Add validates baseURL, encrypts apiKey, verifies the key (TestKey),
// auto-fetches the model list (ListModels) and auto-detects the wire
// format, then stores and persists the provider. The display name is
// derived from the base URL host; use AddNamed for an explicit name.
//
// If the upstream's /v1/models endpoint 404s but its chat endpoint
// answers, the provider is still added — with the catalog entry's
// static model list when one is known, else an empty list — and wire
// format "openai" (some gateways only expose chat completions). Any other
// key-test failure (401, 429, 5xx, network error) rejects the add.
func (r *Registry) Add(baseURL, apiKey string) (*Provider, error) {
	return r.AddNamed(DeriveName(baseURL), baseURL, apiKey)
}

// AddNamed is Add with an explicit display name.
func (r *Registry) AddNamed(name, baseURL, apiKey string) (*Provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), addTimeout)
	defer cancel()
	return r.AddContext(ctx, name, baseURL, apiKey)
}

// AddContext is the context-aware form of AddNamed.
func (r *Registry) AddContext(ctx context.Context, name, baseURL, apiKey string) (*Provider, error) {
	return r.addProvider(ctx, addInput{
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
	})
}

// addInput is the shared input for adding a provider, custom or
// catalog-resolved. authHeaders nil means the legacy Bearer default;
// a non-nil map (possibly empty, for no-auth surfaces) is the exact
// auth resolved for the provider's endpoint. wireFormat "" means the
// OpenAI default; catalog adds pass the resolved wire. catalogID ""
// means a fully-custom provider.
type addInput struct {
	name          string
	baseURL       string
	apiKey        string
	authHeaders   map[string]string
	wireFormat    string
	allowEmptyKey bool
	catalogID     string
	verification  string
	authEndpoint  string
	authVariant   string
	// staticModels is the catalog entry's static model list
	// (entry.StaticModels()); catalog adds pass it, custom
	// adds leave it nil. Used only as a fallback when the
	// live /v1/models probe yields no list.
	staticModels []string
}

// addProvider validates baseURL, encrypts apiKey, verifies the key
// (TestKey), auto-fetches the model list (ListModels) and auto-detects
// the wire format, then stores and persists the provider. The display
// name is derived from the base URL host; use AddNamed for an explicit
// name.
//
// If the upstream's /v1/models endpoint 404s but its chat endpoint
// answers, the provider is still added — with the catalog entry's
// static model list when one is known, else an empty list — and wire
// format "openai" (some gateways only expose chat completions). Any other
// key-test failure (401, 429, 5xx, network error) rejects the add.
func (r *Registry) addProvider(ctx context.Context, in addInput) (*Provider, error) {
	normalized, err := ValidateBaseURL(in.baseURL, r.allowPrivateDestinations)
	if err != nil {
		return nil, err
	}
	// Removed-provider enforcement (G19 Area 1, CT-2): a base
	// URL belonging to a removed provider can never be added —
	// not as a custom provider, not via a catalog add with a
	// base-URL override. The catalog-add path additionally
	// checks the entry itself (AddCatalogWithOptions), and
	// config-file loads check at resolve time
	// (resolveCatalogForLoad). The refusal is the typed
	// catalog.RemovedProviderError, so callers can distinguish
	// a removal from a generic validation failure.
	if rem, ok := IsRemovedBaseURL(normalized); ok {
		return nil, &catalog.RemovedProviderError{
			Entry:   catalog.Provider{ID: rem.ID, BaseURLs: []string{normalized}},
			Removed: rem,
		}
	}
	if strings.TrimSpace(in.apiKey) == "" && !in.allowEmptyKey {
		return nil, errors.New("provider API key is empty")
	}
	name := in.name
	if strings.TrimSpace(name) == "" {
		name = DeriveName(normalized)
	}

	// Custom-provider prefix convention (PARITY B4-04): a hand-added
	// provider whose name carries a known prefix (openai-compatible-,
	// anthropic-compatible-, anthropic-compatible-cc-) states the wire
	// format, and the prefix wins — over the base-URL derivation below
	// and over the chat-probe detection. This mirrors ToProvider's
	// ApplyPrefix in internal/catalog/custom.go; the live add path
	// (AddNamed/AddContext, incl. POST /api/providers) never consulted
	// it. Only applies when no wire was already prescribed (catalog
	// adds prescribe their resolved wire, which stands); prescribing
	// in.wireFormat makes the prefix behave exactly like a
	// catalog-prescribed wire downstream.
	if in.wireFormat == "" {
		if _, _, prefixWire, ok := catalog.ParseCustomPrefix(name); ok {
			in.wireFormat = string(prefixWire)
		}
	}

	r.mu.RLock()
	for _, p := range r.providers {
		if p.BaseURL == normalized {
			r.mu.RUnlock()
			return nil, fmt.Errorf("%w: %s", ErrProviderExists, normalized)
		}
	}
	r.mu.RUnlock()

	encrypted, err := r.encryptKey(in.apiKey)
	if err != nil {
		return nil, err
	}

	// A wire format was not prescribed (hand-added provider): derive
	// it from the catalog by base URL, so a hand-added Anthropic base
	// speaks Messages instead of OpenAI chat completions. Catalog adds
	// always prescribe the resolved wire, so this only fires for
	// custom providers. Unknown bases keep the OpenAI default.
	wireFormat := in.wireFormat
	if wireFormat == "" {
		wireFormat = catalogWireForBaseURL(normalized, in.apiKey)
	}

	client, err := r.newClient(normalized, in.apiKey, in.authHeaders, wireFormat)
	if err != nil {
		return nil, err
	}

	// 1. Inline key test.
	testErr := client.TestKey(ctx)
	if wireFormat == "" {
		wireFormat = WireFormatOpenAI
	}
	var models []Model
	// fetchedAt stamps ModelsFetchedAt (CT-7): the add-time
	// ListModels call is a fetch, so a provider added with a
	// live list is stamped now. The chat-only path below
	// (models 404s, no list fetched) leaves it zero — never
	// fetched — rather than claiming a fetch that never
	// happened.
	fetchedAt := time.Time{}
	switch {
	case testErr == nil:
		// 2. Auto-fetch model list (second call, per the Phase 1 contract:
		// TestKey verifies, ListModels fetches).
		ids, err := client.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching model list for %s: %w", normalized, err)
		}
		models = modelsFromIDs(ids)
		fetchedAt = time.Now().UTC()
	case isNotFound(testErr):
		// /v1/models 404s: maybe the gateway only exposes chat. Probe it.
		// The probe only speaks OpenAI wire, so it may only override a
		// wire format that was not prescribed by the catalog: a
		// catalog-resolved Anthropic wire is never overwritten by an
		// OpenAI probe.
		detected, probeErr := DetectWireFormat(ctx, normalized, in.apiKey, in.authHeaders, r.newClient)
		if probeErr != nil {
			return nil, fmt.Errorf("key test failed for %s: %w", normalized, testErr)
		}
		if detected == WireFormatUnknown {
			return nil, fmt.Errorf("key test failed for %s: %w", normalized, testErr)
		}
		if in.wireFormat == "" {
			wireFormat = detected
		}
		// Chat endpoint exists but no model list: fall back to
		// the catalog entry's static list when one is known
		// (B4-02); a custom provider has none, so it keeps an
		// empty list. The static list is not a fetch, so
		// fetchedAt stays zero.
		if len(in.staticModels) > 0 {
			models = modelsFromIDs(in.staticModels)
		} else {
			models = nil
		}
	default:
		return nil, fmt.Errorf("key test failed for %s: %w", normalized, testErr)
	}

	provider := &Provider{
		ID:              NewProviderID(),
		Name:            name,
		BaseURL:         normalized,
		APIKeyEncrypted: encrypted,
		WireFormat:      wireFormat,
		Models:          models,
		ModelsFetchedAt: fetchedAt,
		Enabled:         true,
		QuotaHint:       QuotaUnknown,
		CreatedAt:       time.Now().UTC(),
		CatalogID:       in.catalogID,
		Verification:    in.verification,
		AuthEndpoint:    in.authEndpoint,
		AuthVariant:     in.authVariant,
		// Pool-native from birth: the single key added here is
		// the pool's first preferred key, so later AddKey calls
		// append to this pool instead of orphaning it.
		Keys: []ProviderKey{{
			ID:          NewProviderKeyID(),
			Encrypted:   encrypted,
			Role:        KeyRolePreferred,
			Fingerprint: KeyFingerprint(in.apiKey),
			CreatedAt:   time.Now().UTC(),
		}},
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check duplicates under the write lock.
	for _, p := range r.providers {
		if p.BaseURL == normalized {
			return nil, fmt.Errorf("%w: %s", ErrProviderExists, normalized)
		}
	}
	r.providers[provider.ID] = provider
	r.order = append(r.order, provider.ID)
	if err := r.saveLocked(); err != nil {
		delete(r.providers, provider.ID)
		r.order = r.order[:len(r.order)-1]
		return nil, fmt.Errorf("persisting provider: %w", err)
	}
	return provider.Clone(), nil
}

// CatalogAddOptions selects the catalog surface and base URL for
// AddCatalogWithOptions. All fields are optional:
//
//   - Endpoint: the catalog surface ("chat", "anthropic", "messages",
//     ...); "" means the entry's default surface.
//   - Variant: the surface variant ("alt-header", "api-key", "jwt",
//     ...); "" means the default variant.
//   - BaseURL: override the catalog base URL — a proxy or mirror, a
//     per-resource URL substituted into a templated catalog base
//     (Azure {resource}, Vertex {LOCATION}), or a mock in tests.
//     "" means the catalog base URL.
type CatalogAddOptions struct {
	Endpoint string
	Variant  string
	BaseURL  string
}

// AddCatalog adds a catalog provider by ID: its base URL, wire format
// and auth scheme come from the catalog entry, so the operator only
// supplies the ID and their own API key. The catalog ID may also be an
// alias ("grok" for xAI, "ollama" for local Ollama).
//
// Refusals (loud, with the reason named — never a silently broken
// provider):
//   - unknown catalog ID;
//   - ToS-grey entries (web-cookie / subscription-OAuth): they are
//     catalogued for completeness but are never routing targets;
//   - surfaces needing a credential exchange (Vertex service-account
//     JSON, OpenRouter JWT, Azure Entra): the resolver never fakes a
//     static header for these — add the static-header variant instead
//     (e.g. Azure "api-key"), or wait for exchange support. Bedrock
//     SigV4 and Vertex service-account JSON are the exceptions: the
//     client performs the exchange itself (Bedrock signs each
//     request from the operator's AWS credentials; Vertex mints an
//     OAuth2 token from the service-account JSON), so those
//     providers are allowed when the key is the right credential
//     material; a key that is not stays refused.
//   - entries with no known base URL (name-only reference entries);
//   - templated catalog base URLs (Azure {resource}, ...): pass a
//     concrete BaseURL via AddCatalogWithOptions.
func (r *Registry) AddCatalog(ctx context.Context, catalogID, apiKey string) (*Provider, error) {
	return r.AddCatalogWithOptions(ctx, catalogID, apiKey, CatalogAddOptions{})
}

// AddCatalogWithOptions is AddCatalog with an explicit surface,
// variant, and/or base-URL override (see CatalogAddOptions).
func (r *Registry) AddCatalogWithOptions(ctx context.Context, catalogID, apiKey string, opts CatalogAddOptions) (*Provider, error) {
	// Removed-provider enforcement (G19 Area 1, CT-2): check
	// BEFORE the catalog lookup. The removed providers are not
	// in the catalog, so lookupCatalog would fail first with a
	// generic "unknown catalog provider" that never names the
	// removal. The refusal is the typed
	// catalog.RemovedProviderError, so callers can distinguish
	// a removal from a generic failure.
	if err := refuseRemovedCatalogID(catalogID); err != nil {
		return nil, err
	}
	entry, err := lookupCatalog(catalogID)
	if err != nil {
		return nil, err
	}
	// Removed-provider enforcement (G19 Area 1, CT-2): the
	// entry itself must not be the removed variant. Matches
	// carries the keyless-only nuance — an authenticated
	// opencode entry passes, the keyless "OpenCode Free"
	// surface does not. (This is the second half of the
	// pre-lookup check above: the pre-lookup check can only
	// refuse whole-integration removals, because a keyless-
	// only ID may still name a legitimate authenticated
	// catalog entry.)
	for _, rem := range catalog.RemovedProviders {
		if rem.Matches(entry) {
			return nil, &catalog.RemovedProviderError{Entry: entry, Removed: rem}
		}
	}
	if entry.ToSGrey {
		return nil, fmt.Errorf("catalog provider %q (%s) is ToS-grey (web-cookie / subscription auth) and cannot be added as a routing target", entry.ID, entry.Name)
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = defaultCatalogEndpoint(entry)
	}
	// Resolve auth for this exact (provider, endpoint, variant) pair.
	// Exchange surfaces fail here with a clear error — never a faked
	// static header. The exceptions are the three exchanges the
	// router performs itself: Bedrock SigV4 (the client signs each
	// request), Vertex service-account JSON (the client mints a
	// token per request), and Azure Entra ID client credentials
	// (a Bearer token is minted now and re-minted on expiry by the
	// auth cache). Any other exchange, or an exchange surface whose
	// key is not the right credential material, stays refused.
	res, err := catalog.ResolveAuth(entry, endpoint, opts.Variant, apiKey)
	if err != nil {
		if errors.Is(err, catalog.ErrExchangeRequired) {
			if headers, wire, ok := sigv4Headers(res, apiKey); ok {
				// SigV4 is the one exchange the client performs
				// itself: keep the resolution (BaseURL + Wire are
				// already set on res) with an empty header set.
				res.Headers = headers
				res.Wire = catalog.WireFormat(wire)
			} else if headers, wire, ok := vertexHeaders(res, apiKey); ok {
				// Vertex service-account JSON is the second
				// exchange the client performs itself (CHECKLIST
				// 277 (e)): the client mints the OAuth2 token
				// per request. The wire is "vertex", not the
				// resolution's Gemini wire — see vertexHeaders.
				res.Headers = headers
				res.Wire = catalog.WireFormat(wire)
			} else if headers, wire, _, eerr := entraIDHeaders(res, apiKey); eerr == nil {
				// Azure Entra ID client credentials: mint the
				// Bearer token now (the add-time key test uses
				// it); per-request resolution re-mints on expiry.
				res.Headers = headers
				res.Wire = catalog.WireFormat(wire)
			} else if eerr != nil && !errors.Is(eerr, errEntraNotApplicable) {
				return nil, fmt.Errorf("catalog provider %q (%s): %w", entry.ID, entry.Name, eerr)
			} else {
				return nil, fmt.Errorf("catalog provider %q (%s): %w — credential exchange is not supported yet; use a static-header variant of this provider if one exists", entry.ID, entry.Name, err)
			}
		} else {
			return nil, fmt.Errorf("catalog provider %q (%s): %w", entry.ID, entry.Name, err)
		}
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = res.BaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("catalog provider %q (%s) has no known base URL; add it as a custom provider with an explicit base URL instead", entry.ID, entry.Name)
	}
	if strings.ContainsAny(baseURL, "{}") {
		return nil, fmt.Errorf("catalog provider %q (%s) has a templated base URL %q; supply a concrete base URL (e.g. your resource URL) via the base-URL override", entry.ID, entry.Name, baseURL)
	}
	// The endpoint's auth kind decides whether an empty key is OK
	// (local / no-auth surfaces take no credential at all).
	allowEmptyKey := false
	if ep, ok := entry.Endpoint(endpoint, opts.Variant); ok {
		allowEmptyKey = ep.AuthKind == catalog.AuthNone
	} else {
		allowEmptyKey = entry.DefaultAuthKind() == catalog.AuthNone
	}
	return r.addProvider(ctx, addInput{
		name:          entry.Name,
		baseURL:       baseURL,
		apiKey:        apiKey,
		authHeaders:   res.Headers,
		wireFormat:    string(res.Wire),
		allowEmptyKey: allowEmptyKey,
		catalogID:     entry.ID,
		verification:  string(entry.Verification),
		authEndpoint:  endpoint,
		authVariant:   opts.Variant,
		staticModels:  entry.StaticModels(),
	})
}

// refuseRemovedCatalogID reports whether catalogID (an ID or alias
// as the operator typed it) names a removed provider whose whole
// integration was removed, returning the typed removal error. It
// deliberately does NOT refuse keyless-only removals (opencode):
// that ID may still name the legitimate authenticated catalog
// entry, which only RemovedProvider.Matches can tell apart once
// the entry is looked up — so keyless-only IDs fall through to
// the post-lookup Matches check in AddCatalogWithOptions /
// resolveCatalogForLoad. The check reuses the catalog's own
// 5-entry register (catalog.IsRemoved); it never forks the table.
func refuseRemovedCatalogID(catalogID string) error {
	rem, ok := catalog.IsRemoved(catalogID)
	if !ok || rem.KeylessOnly {
		return nil
	}
	return &catalog.RemovedProviderError{
		Entry:   catalog.Provider{ID: rem.ID, Alias: rem.Alias},
		Removed: rem,
	}
}

// lookupCatalog finds a catalog entry by ID, falling back to alias
// ("grok" → xai, "ollama" → ollama-local). A retired ID is not
// "unknown": RetiredByID names it and its retirement reason, so an
// add/load attempt fails with the reason instead of a generic
// "unknown catalog provider" (CT-3 registry half).
func lookupCatalog(idOrAlias string) (catalog.Provider, error) {
	if entry, ok := catalog.ByID(idOrAlias); ok {
		return entry, nil
	}
	if entry, ok := catalog.ByAlias(idOrAlias); ok {
		return entry, nil
	}
	if rp, ok := catalog.RetiredByID(idOrAlias); ok {
		return catalog.Provider{}, fmt.Errorf("catalog provider %q (%s) is retired and cannot be added: %s", rp.ID, rp.Name, rp.RetiredReason)
	}
	return catalog.Provider{}, fmt.Errorf("unknown catalog provider %q (see the catalog for known IDs)", idOrAlias)
}

// defaultCatalogEndpoint returns the surface ResolveAuth uses when no
// endpoint is named: "chat" when the entry records one, else its first
// recorded surface, else "chat". It mirrors catalog.ResolveAuth's own
// defaulting rule.
func defaultCatalogEndpoint(entry catalog.Provider) string {
	if _, ok := entry.Endpoint(catalog.EndpointChat, ""); ok {
		return catalog.EndpointChat
	}
	if len(entry.Endpoints) > 0 {
		return entry.Endpoints[0].Endpoint
	}
	return catalog.EndpointChat
}

// resolveCatalogForLoad fills a config-file provider's catalog-derived
// fields: base URL (when the file leaves it empty), wire format (when
// empty), verification status (always — the catalog is authoritative),
// and the endpoint name (when empty). The base URL may be an operator
// override in the file; it is validated but never replaced. A bad
// catalog_id or a ToS-grey entry fails the load loudly. Exchange auth
// is deliberately NOT refused here: ClientFor fails loudly for it at
// startup, where the provider is skipped with a warning instead of
// taking the gateway down.
//
// The resolution calls catalog.ResolveAuth with a placeholder
// credential: only the BaseURL and Wire of the resolution are used
// here, and the placeholder is never sent anywhere (no request is
// made). An exchange surface still yields a usable BaseURL/Wire
// alongside its error, which is tolerated here for that reason.
func (r *Registry) resolveCatalogForLoad(p *Provider) error {
	// Removed-provider enforcement (G19 Area 1, CT-2): check
	// BEFORE the catalog lookup, for the same reason as in
	// AddCatalogWithOptions — a removed provider is not in the
	// catalog, so the lookup would fail first with a generic
	// "unknown catalog provider" that never names the removal.
	// The error is wrapped with the config provider's ID so the
	// load failure names the offending row; errors.As still
	// finds the typed catalog.RemovedProviderError underneath.
	if err := refuseRemovedCatalogID(p.CatalogID); err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
	}
	entry, err := lookupCatalog(p.CatalogID)
	if err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
	}
	// Removed-provider enforcement (G19 Area 1, CT-2): a
	// config-file provider whose catalog entry is the removed
	// variant fails the load loudly instead of routing to a
	// provider its operator asked removed.
	for _, rem := range catalog.RemovedProviders {
		if rem.Matches(entry) {
			return fmt.Errorf("provider %q: %w", p.ID, &catalog.RemovedProviderError{Entry: entry, Removed: rem})
		}
	}
	if entry.ToSGrey {
		return fmt.Errorf("provider %q: catalog provider %q (%s) is ToS-grey and cannot be a routing target", p.ID, entry.ID, entry.Name)
	}
	endpoint := p.AuthEndpoint
	if endpoint == "" {
		endpoint = defaultCatalogEndpoint(entry)
	}
	res, err := catalog.ResolveAuth(entry, endpoint, p.AuthVariant, "codvian-router-load-probe")
	if err != nil && !errors.Is(err, catalog.ErrExchangeRequired) {
		return fmt.Errorf("provider %q: catalog provider %q: %w", p.ID, entry.ID, err)
	}
	if p.BaseURL == "" {
		if res.BaseURL == "" {
			return fmt.Errorf("provider %q: catalog provider %q has no known base URL; set base_url in the config", p.ID, entry.ID)
		}
		if strings.ContainsAny(res.BaseURL, "{}") {
			return fmt.Errorf("provider %q: catalog provider %q has a templated base URL %q; set a concrete base_url in the config", p.ID, entry.ID, res.BaseURL)
		}
		p.BaseURL = NormalizeBaseURL(res.BaseURL)
	} else if normalized, err := ValidateBaseURL(p.BaseURL, r.allowPrivateDestinations); err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
	} else {
		// A config-file override keeps its spelling only in the
		// file: the stored value is the canonical form, so a
		// trailing slash or mixed-case scheme/host registers to
		// the same value as the clean spelling.
		p.BaseURL = normalized
	}
	// CT-2: an explicit base URL in the config file — whether it
	// overrides the catalog base or is the only base URL — must
	// also be checked against the removed domains. A catalog-
	// linked provider pointed at a removed host (or a catalog
	// entry whose own base URL sits on a removed domain) fails
	// the load here, at load time, not at first request.
	if rem, ok := IsRemovedBaseURL(p.BaseURL); ok {
		return fmt.Errorf("provider %q: %w", p.ID, &catalog.RemovedProviderError{
			Entry:   catalog.Provider{ID: rem.ID, BaseURLs: []string{p.BaseURL}},
			Removed: rem,
		})
	}
	if p.WireFormat == "" {
		p.WireFormat = string(res.Wire)
	}
	p.Verification = string(entry.Verification)
	p.AuthEndpoint = endpoint
	return nil
}

// isNotFound reports whether err is an UpstreamError with status 404.
func isNotFound(err error) bool {
	var upErr *UpstreamError
	return errors.As(err, &upErr) && upErr.StatusCode == http.StatusNotFound
}

// modelsFromIDs converts model IDs into Model records with unknown
// context window / pricing (upstreams do not report those via /v1/models).
func modelsFromIDs(ids []string) []Model {
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, Model{ID: id, Name: id})
	}
	return models
}

// Remove deletes the provider with id and persists the change.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[id]; !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	delete(r.providers, id)
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	// Drop the warm pool: a removed provider leaves no idle
	// connection behind (the transport's idle sockets are closed).
	r.pools.Drop(id)
	// Drop its memoized auth resolutions too, so the cache cannot
	// grow across add/remove cycles (a re-added provider with the
	// same ID re-resolves under its new fingerprint anyway).
	// Entries are keyed by provider ID + fingerprint (see
	// resolveAuthHeaders), so every entry for this provider goes.
	r.authMu.Lock()
	for k := range r.authCache {
		if strings.HasPrefix(k, id+"\x00") {
			delete(r.authCache, k)
		}
	}
	r.authMu.Unlock()
	if err := r.saveLocked(); err != nil {
		return fmt.Errorf("persisting provider removal: %w", err)
	}
	return nil
}

// Get returns a copy of the provider with id, or false when absent.
func (r *Registry) Get(id string) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	if !ok {
		return nil, false
	}
	return p.Clone(), true
}

// GetByBaseURL returns a copy of the provider with the given normalized
// base URL, or false when absent.
func (r *Registry) GetByBaseURL(baseURL string) (*Provider, bool) {
	normalized, err := ValidateBaseURL(baseURL, r.allowPrivateDestinations)
	if err != nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.BaseURL == normalized {
			return p.Clone(), true
		}
	}
	return nil, false
}

// List returns copies of all providers in insertion order.
func (r *Registry) List() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Provider, 0, len(r.order))
	for _, id := range r.order {
		if p, ok := r.providers[id]; ok {
			out = append(out, p.Clone())
		}
	}
	return out
}

// OffersModel reports whether any enabled provider serves the exact
// model ID. It mirrors the chain builder's offer rule: a provider
// with an empty model list serves any model. Used by the server to
// tell a full nested model ID (e.g. an aggregator's "vendor/model")
// apart from a "provider/model" routing pin — a listed model ID must
// stay addressable verbatim.
func (r *Registry) OffersModel(modelID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, id := range r.order {
		p, ok := r.providers[id]
		if !ok || !p.Enabled {
			continue
		}
		if providerOffersModelLocked(p, modelID) {
			return true
		}
	}
	return false
}

// ProviderOffersModel reports whether the named enabled provider
// serves the exact model ID (empty model list = serves any model).
// The name may be a registry ID, a catalog ID or a display name:
// resolveProviderV1 resolves catalog entries too, and the registry
// stores generated IDs, so a catalog-ID pin must still find its
// provider (same matching the engine's ResolveProvider does).
func (r *Registry) ProviderOffersModel(providerID, modelID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[providerID]
	if !ok {
		for _, id := range r.order {
			c := r.providers[id]
			if c != nil && (strings.EqualFold(c.CatalogID, providerID) || strings.EqualFold(c.Name, providerID)) {
				p, ok = c, true
				break
			}
		}
	}
	if !ok || !p.Enabled {
		return false
	}
	return providerOffersModelLocked(p, modelID)
}

func providerOffersModelLocked(p *Provider, modelID string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

// SetEnabled enables or disables a provider and persists the change.
func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	p.Enabled = enabled
	if err := r.saveLocked(); err != nil {
		return fmt.Errorf("persisting provider update: %w", err)
	}
	return nil
}

// SetQuotaHint records the remaining-quota hint (0..1, or -1 unknown) for
// a provider and persists the change. Used by the router for quota-aware
// routing.
func (r *Registry) SetQuotaHint(id string, hint float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	if err := p.SetQuotaHint(hint); err != nil {
		return err
	}
	if err := r.saveLocked(); err != nil {
		return fmt.Errorf("persisting provider update: %w", err)
	}
	return nil
}

// --- Multi-key pooling (CHECKLIST 175) ---
//
// A provider may hold several API keys. The pool lives on the
// Provider (Keys, in pool order); the legacy APIKeyEncrypted field
// always holds the primary (first preferred) key's ciphertext, so
// older code paths that read it keep working.

// materializeLegacyKeyLocked moves a provider stored before pools
// existed onto an explicit pool: its legacy single key becomes
// Keys[0] (preferred, same ciphertext, derived stable ID — its
// fingerprint is unknown without the plaintext and stays empty).
// Caller must hold r.mu. It is a no-op when Keys is already
// populated or the provider holds no key at all. It never
// persists by itself — callers save after their mutation.
func (r *Registry) materializeLegacyKeyLocked(p *Provider) {
	if len(p.Keys) > 0 || p.APIKeyEncrypted == "" {
		return
	}
	p.Keys = []ProviderKey{{
		ID:        legacyKeyID(p.APIKeyEncrypted),
		Encrypted: p.APIKeyEncrypted,
		Role:      KeyRolePreferred,
		CreatedAt: p.CreatedAt,
	}}
}

// syncPrimaryKeyLocked re-points APIKeyEncrypted at the primary
// (first preferred) key's ciphertext after a pool mutation, so
// the legacy field never names a key that left the pool. Caller
// must hold r.mu.
func (r *Registry) syncPrimaryKeyLocked(p *Provider) {
	if k, ok := p.PrimaryKey(); ok {
		p.APIKeyEncrypted = k.Encrypted
	}
}

// AddKey tests a new API key live against the provider's upstream
// (same contract as Add: TestKey must pass, or the models endpoint
// 404s and the chat probe finds the wire) and appends it to the
// provider's pool with the given role ("preferred" or "reserve";
// "" means preferred). The same credential twice is refused —
// failing over to an identical key would burn an attempt for
// nothing. The key is stored encrypted and the change is
// persisted; the returned key carries its ID, role and fingerprint,
// never the plaintext.
// keyFingerprint returns the fingerprint for a pool key: the
// stored fingerprint when present, else the fingerprint of the
// decrypted credential (CHECKLIST 175). The legacy synthesized
// key carries no stored fingerprint (it is built without
// plaintext), so a duplicate check that only compared stored
// fingerprints would let the same credential in twice — once as
// the legacy key, once as a new key. An undecryptable or empty
// key reports "": it can never match a real fingerprint, so the
// check stays safe rather than refusing a legitimate add.
func (r *Registry) keyFingerprint(k ProviderKey) string {
	if k.Fingerprint != "" {
		return k.Fingerprint
	}
	if k.Encrypted == "" {
		return ""
	}
	plaintext, err := r.decryptKey(k.Encrypted)
	if err != nil {
		return ""
	}
	return KeyFingerprint(plaintext)
}

// testKeyLive tests a candidate API key against the provider's upstream
// without storing anything: TestKey must pass, or the models endpoint
// 404s and the chat probe finds the wire. AddKey and RotateKey share
// it, so a key the upstream rejects can never enter the pool by
// either path.
func (r *Registry) testKeyLive(ctx context.Context, p *Provider, apiKey string) error {
	headers, _, _, err := r.resolveAuthHeadersUncached(p, apiKey)
	if err != nil {
		return fmt.Errorf("resolving auth for provider %q: %w", p.Name, err)
	}
	client, err := r.newClient(p.BaseURL, apiKey, headers, p.WireFormat)
	if err != nil {
		return err
	}
	if err := client.TestKey(ctx); err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("key test failed for %s: %w", p.BaseURL, err)
		}
		// Models endpoint 404s: maybe the gateway only exposes
		// chat — same probe Add runs before accepting the key.
		detected, probeErr := DetectWireFormat(ctx, p.BaseURL, apiKey, headers, r.newClient)
		if probeErr != nil || detected == WireFormatUnknown {
			if probeErr != nil {
				return fmt.Errorf("key test failed for %s: %w", p.BaseURL, probeErr)
			}
			return fmt.Errorf("key test failed for %s: %w", p.BaseURL, err)
		}
	}
	return nil
}

func (r *Registry) AddKey(ctx context.Context, providerID, apiKey, role string) (ProviderKey, error) {
	if strings.TrimSpace(apiKey) == "" {
		return ProviderKey{}, errors.New("provider API key is empty")
	}
	if !IsKeyRole(role) {
		return ProviderKey{}, fmt.Errorf("unknown key role %q (want %q or %q)", role, KeyRolePreferred, KeyRoleReserve)
	}
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return ProviderKey{}, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	fp := KeyFingerprint(apiKey)
	for _, k := range p.EffectiveKeys() {
		if f := r.keyFingerprint(k); f != "" && f == fp {
			return ProviderKey{}, fmt.Errorf("provider %q already holds this key (fingerprint %s)", p.Name, fp)
		}
	}

	// Live key test against this provider's upstream, with its
	// stored wire format and catalog-resolved auth (uncached:
	// the key is not stored yet, so there is no ciphertext to
	// fingerprint a cache entry with).
	if err := r.testKeyLive(ctx, p, apiKey); err != nil {
		return ProviderKey{}, err
	}

	encrypted, err := r.encryptKey(apiKey)
	if err != nil {
		return ProviderKey{}, err
	}
	added := ProviderKey{
		ID:          NewProviderKeyID(),
		Encrypted:   encrypted,
		Role:        NormalizeKeyRole(role),
		Fingerprint: fp,
		CreatedAt:   time.Now().UTC(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok = r.providers[providerID]
	if !ok {
		return ProviderKey{}, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	// Re-check the duplicate under the write lock: a concurrent
	// AddKey could have stored the same credential meanwhile.
	// (Same decrypt-fallback as above: the legacy key's stored
	// fingerprint is empty.)
	for _, k := range p.EffectiveKeys() {
		if f := r.keyFingerprint(k); f != "" && f == fp {
			return ProviderKey{}, fmt.Errorf("provider %q already holds this key (fingerprint %s)", p.Name, fp)
		}
	}
	r.materializeLegacyKeyLocked(p)
	p.Keys = append(p.Keys, added)
	r.syncPrimaryKeyLocked(p)
	if err := r.saveLocked(); err != nil {
		// Roll back the in-memory append so a persist failure
		// never leaves memory and disk disagreeing.
		p.Keys = p.Keys[:len(p.Keys)-1]
		return ProviderKey{}, fmt.Errorf("persisting provider key: %w", err)
	}
	return added.Clone(), nil
}

// RemoveKey deletes one key from a provider's pool and persists
// the change. The last key cannot be removed — a provider with no
// key cannot serve, and removing the provider itself is the way to
// drop it. Removing the primary key re-points the legacy field at
// the new primary. The warm pool for the provider is dropped: its
// idle connections were keyed by the removed key's fingerprint.
func (r *Registry) RemoveKey(providerID, keyID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	r.materializeLegacyKeyLocked(p)
	if len(p.Keys) <= 1 {
		return fmt.Errorf("provider %q holds only one key; remove the provider instead of its last key", p.Name)
	}
	idx := -1
	for i, k := range p.Keys {
		if k.ID == keyID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("provider %q holds no key %q", p.Name, keyID)
	}
	p.Keys = append(p.Keys[:idx], p.Keys[idx+1:]...)
	r.syncPrimaryKeyLocked(p)
	r.pools.Drop(providerID)
	if err := r.saveLocked(); err != nil {
		return fmt.Errorf("persisting provider key removal: %w", err)
	}
	return nil
}

// SetKeyRole changes one pool key's role ("preferred" or
// "reserve") and persists the change. A reserve key is held back
// until every preferred key is exhausted; a preferred key is tried
// first, in pool order.
func (r *Registry) SetKeyRole(providerID, keyID, role string) error {
	if !IsKeyRole(role) {
		return fmt.Errorf("unknown key role %q (want %q or %q)", role, KeyRolePreferred, KeyRoleReserve)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	r.materializeLegacyKeyLocked(p)
	found := false
	for i := range p.Keys {
		if p.Keys[i].ID == keyID {
			p.Keys[i].Role = NormalizeKeyRole(role)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("provider %q holds no key %q", p.Name, keyID)
	}
	r.syncPrimaryKeyLocked(p)
	if err := r.saveLocked(); err != nil {
		return fmt.Errorf("persisting provider key role: %w", err)
	}
	return nil
}

// RotateKey replaces one pool key's credential in place, keeping its
// ID and role: the dashboard's "paste a replacement key" box rotates
// the provider's primary key without changing key identity (unlike
// DELETE + POST-add, which mints a new ID and can strand the provider
// if the add fails after the delete). The new key is tested live
// against the upstream exactly like AddKey tests one (a 401 key is
// refused); the same credential twice is refused, whether it is the
// key being rotated or another key in the pool. The warm pool is
// dropped (idle connections are keyed by fingerprint), the primary
// pointer is re-synced when the rotated key is the primary, and the
// change is persisted. CreatedAt is stamped at rotation: the stored
// credential is new.
func (r *Registry) RotateKey(ctx context.Context, providerID, keyID, apiKey string) (ProviderKey, error) {
	if strings.TrimSpace(apiKey) == "" {
		return ProviderKey{}, errors.New("provider API key is empty")
	}
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return ProviderKey{}, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	// KeyByID covers EffectiveKeys, so a legacy provider's single
	// key is found by its derived ID too.
	if _, found := p.KeyByID(keyID); !found {
		return ProviderKey{}, fmt.Errorf("provider %q holds no key %q", p.Name, keyID)
	}
	fp := KeyFingerprint(apiKey)
	for _, k := range p.EffectiveKeys() {
		if f := r.keyFingerprint(k); f != "" && f == fp {
			return ProviderKey{}, fmt.Errorf("provider %q already holds this key (fingerprint %s)", p.Name, fp)
		}
	}
	if err := r.testKeyLive(ctx, p, apiKey); err != nil {
		return ProviderKey{}, err
	}
	encrypted, err := r.encryptKey(apiKey)
	if err != nil {
		return ProviderKey{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok = r.providers[providerID]
	if !ok {
		return ProviderKey{}, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	r.materializeLegacyKeyLocked(p)
	// Re-check the duplicate under the write lock: a concurrent
	// AddKey could have stored the same credential meanwhile.
	for _, k := range p.EffectiveKeys() {
		if f := r.keyFingerprint(k); f != "" && f == fp {
			return ProviderKey{}, fmt.Errorf("provider %q already holds this key (fingerprint %s)", p.Name, fp)
		}
	}
	idx := -1
	for i := range p.Keys {
		if p.Keys[i].ID == keyID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProviderKey{}, fmt.Errorf("provider %q holds no key %q", p.Name, keyID)
	}
	old := p.Keys[idx]
	p.Keys[idx].Encrypted = encrypted
	p.Keys[idx].Fingerprint = fp
	p.Keys[idx].CreatedAt = time.Now().UTC()
	r.syncPrimaryKeyLocked(p)
	r.pools.Drop(providerID)
	if err := r.saveLocked(); err != nil {
		// Roll back the in-memory replacement so a persist
		// failure never leaves memory and disk disagreeing.
		p.Keys[idx] = old
		r.syncPrimaryKeyLocked(p)
		return ProviderKey{}, fmt.Errorf("persisting provider key: %w", err)
	}
	return p.Keys[idx].Clone(), nil
}

// KeyPool returns display-safe copies of a provider's key pool —
// ID, role, fingerprint and creation time only, never ciphertext
// (and never plaintext). An empty pool (no-auth provider) returns
// an empty non-nil slice.
func (r *Registry) KeyPool(providerID string) ([]ProviderKey, error) {
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	out := make([]ProviderKey, 0, len(p.EffectiveKeys()))
	for _, k := range p.EffectiveKeys() {
		k.Encrypted = "" // display-safe: no ciphertext leaves the registry
		out = append(out, k)
	}
	return out, nil
}

// ApplySyncedVision sets one model's vision capability from the
// opt-in models.dev sync (G19 Area 1), in memory ONLY — the same
// contract as ApplySyncedContextWindow: the sync state file
// (models-dev-sync.json) is the durable record, and the flag is
// re-applied from it at startup. Unlike a window, vision is a
// boolean the sync both sets and clears: a feed entry that
// affirmatively reports a modalities table without image input
// clears a previously synced flag. The provider's other models
// are untouched. known records whether the feed affirmatively
// reported modalities for this model (VisionReported): only a
// known report sets VisionKnown, so the modality bridge can
// tell "the feed said text-only" (known=true, vision=false)
// from "never reported" (known=false) — the bridge reroutes
// away only from the former, never from the latter.
func (r *Registry) ApplySyncedVision(providerID, model string, vision, known bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].Vision = vision
			p.Models[i].VisionKnown = known
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// ApplySyncedAudio sets one model's audio capability from the
// opt-in models.dev sync (B3-24 modality bridges), in memory
// ONLY — the same contract as ApplySyncedVision: the sync state
// file (models-dev-sync.json) is the durable record, and the
// flag is re-applied from it at startup. A feed entry that
// affirmatively reports a modalities table without audio input
// clears a previously synced flag; only a known report sets
// AudioKnown, so the modality bridge can tell "the feed said
// no-audio" (known=true, audio=false) from "never reported"
// (known=false).
func (r *Registry) ApplySyncedAudio(providerID, model string, audio, known bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].Audio = audio
			p.Models[i].AudioKnown = known
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// ApplySyncedVideo sets one model's video capability from the
// opt-in models.dev sync (B3-24 modality bridges), in memory
// ONLY — the same contract as ApplySyncedVision. A feed entry
// that affirmatively reports a modalities table without video
// input clears a previously synced flag; only a known report
// sets VideoKnown.
func (r *Registry) ApplySyncedVideo(providerID, model string, video, known bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].Video = video
			p.Models[i].VideoKnown = known
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// ApplySyncedReasoning sets one model's reasoning capability in memory.
func (r *Registry) ApplySyncedReasoning(providerID, model string, reasoning bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].Reasoning = reasoning
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// ApplySyncedTools sets one model's tools capability in memory.
func (r *Registry) ApplySyncedTools(providerID, model string, tools bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].Tools = &tools
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// ApplySyncedContextWindow sets one model's context window from
// the opt-in models.dev sync (CHECKLIST 179), in memory ONLY —
// it deliberately does NOT persist: the sync state file
// (models-dev-sync.json) is the durable record, and the window is
// re-applied from it at startup. Writing a synced value into the
// provider store would mix fetched data into the operator's config
// and erase the operator-vs-synced distinction (a later sync
// could no longer tell an operator-set window from a synced one).
// A window <= 0 clears nothing and is refused: the sync only ever
// applies a positive window models.dev reported. The provider's
// other models are untouched.
func (r *Registry) ApplySyncedContextWindow(providerID, model string, window int) error {
	if window <= 0 {
		return fmt.Errorf("provider: cannot apply a non-positive synced context window %d for %s/%s", window, providerID, model)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	for i := range p.Models {
		if p.Models[i].ID == model {
			p.Models[i].ContextWindow = window
			return nil
		}
	}
	return fmt.Errorf("provider: model %q not offered by provider %q", model, providerID)
}

// encryptKey seals apiKey with the registry's cipher. It fails clearly
// when no cipher is configured instead of panicking on a nil cipher.
func (r *Registry) encryptKey(apiKey string) (string, error) {
	if r.cipher == nil {
		return "", errors.New("provider: no key cipher configured (pass a Vault-backed cipher via WithCipher)")
	}
	encrypted, err := r.cipher.Encrypt(apiKey)
	if err != nil {
		return "", fmt.Errorf("encrypting provider API key: %w", err)
	}
	return encrypted, nil
}

// decryptKey unseals a stored API key. It fails clearly when no cipher
// is configured instead of panicking on a nil cipher. An empty
// ciphertext means no key was ever stored — a local no-auth provider
// (e.g. Ollama) holds no credential at all — so it unseals to the
// empty key instead of erroring: Vault.Unseal rejects "" as an
// unknown seal format (no "v1:"/"v2:" prefix), which used to make
// ClientFor fail for exactly the providers that need no key
// (CHECKLIST 313; RelayTargetForModel already skipped decryption
// when no key was stored, clientForKey did not). An auth provider
// stored with no key still fails honestly downstream: ResolveAuth
// refuses an empty credential for auth surfaces, and an
// uncatalogued base falls back to a client that 401s upstream.
func (r *Registry) decryptKey(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if r.cipher == nil {
		return "", errors.New("provider: no key cipher configured (pass a Vault-backed cipher via WithCipher)")
	}
	key, err := r.cipher.Decrypt(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypting provider API key: %w", err)
	}
	return key, nil
}

// DecryptedKey returns the plaintext API key for a provider. The router
// uses it (via ClientFor) to build upstream clients; the key must never
// be logged or persisted in plaintext.
func (r *Registry) DecryptedKey(id string) (string, error) {
	r.mu.RLock()
	p, ok := r.providers[id]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	return r.decryptKey(p.APIKeyEncrypted)
}

// ClientFor builds an upstream Client for a registered provider.
//
// The auth headers come from the catalog, resolved per (provider,
// endpoint): a catalog-linked provider uses its stored endpoint and
// variant (Anthropic's messages surface gets Bearer +
// anthropic-version, DeepSeek's Anthropic surface gets x-api-key,
// Fireworks' alt-header variant gets X-Fireworks-Api-Key, ...). A
// provider added by hand with no catalog link is matched against the
// catalog by base URL when possible (a hand-added DeepSeek or
// Fireworks base URL gets the right header too); otherwise it keeps
// the legacy Bearer default. Exchange surfaces (Vertex without a
// service-account JSON key, OpenRouter JWT, Azure Entra) fail here
// with a clear error — the client is never built with a faked static
// header for them. The two exceptions are Bedrock SigV4 and Vertex
// service-account JSON: the exchange is performed by the client
// itself (each Bedrock request is signed from the operator's AWS
// credentials; each Vertex request carries a token minted from the
// service-account JSON), so a SigV4 surface with parseable
// credentials, or a Vertex surface with a parseable service-account
// JSON key, builds its native client instead of failing.
//
// The wire format likewise comes from the catalog resolution: a
// provider whose surface speaks Anthropic gets a Messages client, a
// Gemini surface a generateContent client, a Cohere surface a Chat
// API v2 client, a Bedrock surface a Converse client, a Vertex
// surface a Vertex client, everything else the OpenAI client.
func (r *Registry) ClientFor(id string) (Client, error) {
	r.mu.RLock()
	p, ok := r.providers[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	// The client serves through the provider's primary key — the
	// first preferred key in pool order (see Provider.PrimaryKey).
	// For a key other than the primary, use ClientForKey: each key
	// gets its own client (and its own warm pool), so the router
	// can fail over from one key to the next without rebuilding
	// anything mid-request.
	key, ok := p.PrimaryKey()
	if !ok {
		// No key at all (a no-auth provider whose stored key is
		// an empty seal): build with the legacy field, exactly
		// as before pools existed.
		key = ProviderKey{Encrypted: p.APIKeyEncrypted}
	}
	return r.clientForKey(p, key)
}

// ClientForKey builds an upstream Client for one key of a
// registered provider (CHECKLIST 175): the client the router
// uses for that key's attempts, so a dead key can be abandoned
// without taking the provider's other keys with it. The key's
// ciphertext fingerprints the memoized auth resolution, so two
// keys of one provider never share a cached header set.
func (r *Registry) ClientForKey(providerID, keyID string) (Client, error) {
	r.mu.RLock()
	p, ok := r.providers[providerID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, providerID)
	}
	key, ok := p.KeyByID(keyID)
	if !ok {
		return nil, fmt.Errorf("provider %q holds no key %q", p.Name, keyID)
	}
	return r.clientForKey(p, key)
}

// clientForKey is the shared build behind ClientFor and
// ClientForKey: decrypt this key, resolve auth + wire for it, and
// attach its warm pool.
func (r *Registry) clientForKey(p *Provider, key ProviderKey) (Client, error) {
	plaintext, err := r.decryptKey(key.Encrypted)
	if err != nil {
		return nil, err
	}
	headers, wireFormat, err := r.resolveAuthHeaders(p, plaintext, key.Encrypted)
	if err != nil {
		return nil, err
	}
	wireFormat = resolveWireFormat(p, wireFormat)
	// Warm pool: the client shares its (provider, key) pool with
	// every other client built for the same pair (engine clients,
	// health probes, live-added providers), so its connections stay
	// warm across requests instead of re-dialing. A factory-built
	// fake (tests) has no attachable transport and keeps its own.
	client, err := r.newClient(p.BaseURL, plaintext, headers, wireFormat)
	if err != nil {
		return nil, err
	}
	client = r.attachHTTPAdapter(p.ID, client)
	if at, ok := client.(interface{ attachPoolTransport(http.RoundTripper) }); ok {
		at.attachPoolTransport(r.pools.TransportFor(p.ID, plaintext))
	}
	return client, nil
}

// resolveWireFormat merges the stored and catalog-resolved wire
// formats with the native-wire-wins rule shared by ClientFor and
// RelayTargetForModel: a stored wire from an older config must not
// be silently downgraded by a base-URL lookup that could not see
// it (e.g. a proxy base), and a resolved native wire must not be
// lost to a stale stored "openai".
func resolveWireFormat(p *Provider, wireFormat string) string {
	switch {
	case p.WireFormat == WireFormatAnthropic || wireFormat == WireFormatAnthropic:
		return WireFormatAnthropic
	case p.WireFormat == WireFormatGemini || wireFormat == WireFormatGemini:
		return WireFormatGemini
	case p.WireFormat == WireFormatCohere || wireFormat == WireFormatCohere:
		return WireFormatCohere
	case p.WireFormat == WireFormatBedrock || wireFormat == WireFormatBedrock:
		return WireFormatBedrock
	case p.WireFormat == WireFormatVertex || wireFormat == WireFormatVertex:
		return WireFormatVertex
	case wireFormat == "":
		return p.WireFormat
	default:
		return wireFormat
	}
}

// PoolStats returns the live warm-pool state, one row per
// (provider, key) pool (see PoolStat in pool.go).
func (r *Registry) PoolStats() []PoolStat {
	return r.pools.Stats()
}

// PoolConfig returns the resolved warm-pool sizing config in force.
func (r *Registry) PoolConfig() PoolConfig {
	return r.pools.Config()
}

// PooledTransport returns the warm-pool transport for a relay target's
// (providerID, keyFingerprint) pair — the SAME pool the engine clients
// share for that provider+key, so a relayed request reuses idle
// connections instead of re-dialing TCP+TLS after the two connections
// http.DefaultTransport keeps idle (AUD-PERF-003). keyFP is a
// KeyFingerprint value, never key material. Returns nil for an empty
// provider ID (no pool to address); callers then keep their own client.
func (r *Registry) PooledTransport(providerID, keyFP string) http.RoundTripper {
	if providerID == "" {
		return nil
	}
	return r.pools.TransportForFingerprint(providerID, keyFP)
}

// WarmPools warms every enabled provider's pool with one real
// request (TestKey = GET /v1/models) through the pooled transport,
// so the first user request never pays a cold dial. It is
// best-effort: a provider whose key test fails is reported in the
// returned failures map and skipped — a bad key must not take the
// gateway down at startup. Callers pass a bounded context; each
// provider gets at most 5 s.
func (r *Registry) WarmPools(ctx context.Context) (warmed int, failures map[string]error) {
	failures = make(map[string]error)
	for _, p := range r.List() {
		if !p.Enabled {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		client, err := r.ClientFor(p.ID)
		if err == nil {
			err = client.TestKey(pctx)
		}
		cancel()
		if err != nil {
			failures[p.ID] = err
			continue
		}
		warmed++
	}
	return warmed, failures
}

// RelayTarget is what the byte-for-byte native relay needs to
// forward one request verbatim: the single provider serving the
// model, its native wire format, its base URL, and exactly the auth
// headers the catalog resolves for it (never a guessed header).
// Attempts counts the enabled providers serving the model: >1 means
// a failover chain exists, so the relay must translate instead of
// forwarding verbatim. IsLocal marks a gateway-provided local
// capability (Ollama wire, or a catalog local-category provider):
// those go through the readiness gate and answer 503 + Retry-After
// while their runtime is not ready.
type RelayTarget struct {
	ProviderID    string
	ProviderName  string
	WireFormat    string
	BaseURL       string
	AuthHeaders   map[string]string
	UpstreamModel string
	Attempts      int
	IsLocal       bool
	// KeyFingerprint is the warm-pool key fingerprint for this
	// target's primary key (provider.KeyFingerprint: 12 hex chars of
	// SHA-256(key), never key material — the same value GET
	// /dashboard/pools serves). The native relay uses it to fetch the
	// identical warm transport the engine clients use
	// (Registry.PooledTransport), so a verbatim relay reuses idle
	// connections instead of falling back to
	// http.DefaultTransport's MaxIdleConnsPerHost=2 (AUD-PERF-003).
	KeyFingerprint string
}

// RelayTargetForModel resolves the relay target for one model ID:
// the first enabled provider serving it (insertion order), with the
// attempt count across all enabled providers serving it. It errors
// when no enabled provider serves the model — the caller then falls
// back to the translate path, whose engine reports the unknown
// model honestly, instead of the relay inventing a target.
func (r *Registry) RelayTargetForModel(model string) (*RelayTarget, error) {
	var first *Provider
	attempts := 0
	for _, p := range r.List() {
		if p == nil || !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if m.ID == model {
				attempts++
				if first == nil {
					first = p
				}
				break
			}
		}
	}
	if first == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, model)
	}
	key := ""
	keyCiphertext := ""
	if pk, ok := first.PrimaryKey(); ok {
		keyCiphertext = pk.Encrypted
	} else {
		keyCiphertext = first.APIKeyEncrypted
	}
	if keyCiphertext != "" {
		var err error
		key, err = r.decryptKey(keyCiphertext)
		if err != nil {
			return nil, err
		}
	}
	headers, wireFormat, err := r.resolveAuthHeaders(first, key, keyCiphertext)
	if err != nil {
		return nil, err
	}
	// A native wire wins from either source, exactly as in
	// ClientFor (see resolveWireFormat).
	wireFormat = resolveWireFormat(first, wireFormat)
	isLocal := wireFormat == "ollama" || first.WireFormat == "ollama"
	if !isLocal && first.CatalogID != "" {
		if entry, err := lookupCatalog(first.CatalogID); err == nil && entry.Category == catalog.CategoryLocal {
			isLocal = true
		}
	}
	return &RelayTarget{
		ProviderID:    first.ID,
		ProviderName:  first.Name,
		WireFormat:    wireFormat,
		BaseURL:       first.BaseURL,
		AuthHeaders:   headers,
		UpstreamModel: model,
		Attempts:      attempts,
		IsLocal:       isLocal,
		// The decrypted key is fingerprinted here and the plaintext
		// dropped: the transport pool is keyed by the fingerprint
		// (see PoolManager.TransportForFingerprint), never the key.
		KeyFingerprint: KeyFingerprint(key),
	}, nil
}

// catalogWireForBaseURL derives the wire format for a raw base URL
// from the catalog (default surface): "anthropic" when the matched
// surface speaks Messages, "gemini" when it speaks generateContent,
// "cohere" when it speaks Chat API v2, "bedrock-converse" when the
// host is a Bedrock runtime host (the catalog's templated base URLs
// never match a concrete host, so the host pattern is checked
// directly), "openai" otherwise (unknown base, lookup error, or an
// OpenAI surface). It never errors — callers treat the OpenAI
// default as the safe fallback.
func catalogWireForBaseURL(baseURL, apiKey string) string {
	if isBedrockBaseURL(baseURL) {
		return WireFormatBedrock
	}
	if isVertexBaseURL(baseURL) {
		return WireFormatVertex
	}
	res, found, err := catalog.ResolveAuthForBaseURL(baseURL, "", "", apiKey)
	if err != nil || !found {
		return WireFormatOpenAI
	}
	if res.Wire == catalog.WireAnthropic {
		return WireFormatAnthropic
	}
	if res.Wire == catalog.WireGemini {
		return WireFormatGemini
	}
	if res.Wire == catalog.WireCohere {
		return WireFormatCohere
	}
	if res.Wire == catalog.WireBedrock {
		return WireFormatBedrock
	}
	return WireFormatOpenAI
}

// isBedrockBaseURL reports whether baseURL points at a Bedrock
// runtime host (bedrock-runtime.{region}.amazonaws.com / .api.aws).
// The catalog records that host templated ({region}), so a concrete
// operator URL never matches a catalog entry exactly — the host
// pattern is the honest match.
func isBedrockBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return strings.HasPrefix(host, "bedrock-runtime.") &&
		(strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".api.aws"))
}

// isVertexBaseURL reports whether baseURL points at a Vertex AI
// host ({location}-aiplatform.googleapis.com). The catalog
// records that host templated ({LOCATION}), so a concrete
// operator URL never matches a catalog entry exactly — the host
// pattern is the honest match, mirroring isBedrockBaseURL.
func isVertexBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return false
	}
	return strings.HasSuffix(u.Hostname(), "-aiplatform.googleapis.com")
}

// sigv4Headers handles the one credential exchange the router can
// perform itself: Bedrock SigV4. When the resolution failed only
// because the surface needs a SigV4 exchange and the operator
// supplied parseable AWS credentials, it returns the (empty) header
// set — the client signs each request, so no static header exists —
// plus the wire. Otherwise it reports false and the caller keeps
// the exchange refusal. A key that is not AWS credential material
// stays refused: the exchange is never faked.
func sigv4Headers(res catalog.AuthResolution, key string) (map[string]string, string, bool) {
	if res.Exchange == nil || res.Exchange.Kind != "sigv4" {
		return nil, "", false
	}
	if _, err := parseAWSCredentials(key); err != nil {
		return nil, "", false
	}
	return map[string]string{}, string(res.Wire), true
}

// vertexHeaders handles the second credential exchange the router
// can perform itself: Vertex AI service-account JSON (CHECKLIST
// 277 (e)). When the resolution failed only because the surface
// needs a service-account-json exchange and the operator supplied
// a parseable service-account JSON key, it returns the (empty)
// header set — the client mints an OAuth2 token per request, so
// no static header exists — plus the Vertex wire ("vertex", NOT
// the resolution's Gemini wire: the two surfaces share a shape
// but not an auth exchange). Otherwise it reports false and the
// caller keeps the exchange refusal. A key that is not
// service-account JSON stays refused: the exchange is never
// faked.
func vertexHeaders(res catalog.AuthResolution, key string) (map[string]string, string, bool) {
	if res.Exchange == nil || res.Exchange.Kind != "service-account-json" {
		return nil, "", false
	}
	if _, _, err := parseVertexServiceAccount(key); err != nil {
		return nil, "", false
	}
	return map[string]string{}, WireFormatVertex, true
}

// cachedAuth is one memoized resolveAuthHeaders result. headers is
// owned by the cache: callers always receive a copy, so no caller
// can mutate a cached header set (e.g. by adding a per-request
// header) and poison later requests. expiry bounds minted-token
// resolutions (Entra ID): a cached Bearer token is only served until
// its expiry, then the resolution re-runs and re-mints. Static
// header sets carry the zero time (never expire).
type cachedAuth struct {
	fp      [32]byte // authFingerprint of the provider record resolved
	headers map[string]string
	wire    string
	expiry  time.Time
}

// authFingerprint hashes every input resolveAuthHeaders depends on,
// so a cache hit is only possible when the resolution would return
// exactly the same result. The plaintext key is deliberately NOT an
// input: both call sites (ClientFor, RelayTargetForModel) derive key
// as decryptKey(ciphertext), so the stored ciphertext covers the
// key — a rotation changes the ciphertext (fresh random nonce per
// seal) and therefore the fingerprint, while the secret itself never
// enters a map key or a hash input. keyCiphertext is the ciphertext
// of the key being resolved (CHECKLIST 175): two keys of one
// provider resolve under different fingerprints, so they never
// share a cached header set. Enabled/QuotaHint are not inputs
// either: they never affect auth or wire. The catalog itself is
// compile-time data with no runtime mutation path, so it needs no
// fingerprint component.
func authFingerprint(p *Provider, keyCiphertext string) [32]byte {
	h := sha256.New()
	var lenBuf [8]byte
	for _, s := range []string{p.ID, p.BaseURL, p.CatalogID, p.AuthEndpoint, p.AuthVariant, p.APIKeyEncrypted, keyCiphertext} {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	var fp [32]byte
	copy(fp[:], h.Sum(nil))
	return fp
}

func cloneHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// resolveAuthHeaders resolves the exact auth headers for one provider,
// plus the wire format its surface speaks. It returns ("", nil, nil)
// for the legacy Bearer default with the stored wire format.
//
// The result is memoized per (provider, key) — the cache key is the
// provider ID plus the resolution fingerprint, which covers the key's
// ciphertext (see authFingerprint), so two keys of one provider hold
// separate entries and never share a header set. Only successful
// resolutions are cached — an error (ToS-grey provider, exchange
// required, unknown catalog ID) is re-resolved on every call, so a
// configuration fix takes effect immediately.
func (r *Registry) resolveAuthHeaders(p *Provider, key, keyCiphertext string) (map[string]string, string, error) {
	fp := authFingerprint(p, keyCiphertext)
	cacheKey := p.ID + "\x00" + string(fp[:])
	r.authMu.Lock()
	if ent, ok := r.authCache[cacheKey]; ok && ent.fp == fp && (ent.expiry.IsZero() || time.Now().Before(ent.expiry)) {
		headers, wire := cloneHeaders(ent.headers), ent.wire
		r.authMu.Unlock()
		return headers, wire, nil
	}
	r.authMu.Unlock()

	headers, wire, expiry, err := r.resolveAuthHeadersUncached(p, key)
	if err != nil {
		return nil, "", err
	}
	r.authMu.Lock()
	if r.authCache == nil {
		r.authCache = make(map[string]cachedAuth)
	}
	r.authCache[cacheKey] = cachedAuth{fp: fp, headers: cloneHeaders(headers), wire: wire, expiry: expiry}
	r.authMu.Unlock()
	return headers, wire, nil
}

// resolveAuthHeadersUncached is the uncached resolution behind
// resolveAuthHeaders: catalog lookup for a catalog-linked provider,
// base-URL catalog match (plus the wire fallback) otherwise. The
// returned expiry bounds minted-token header sets (Entra ID); static
// resolutions return the zero time.

// oauthHeadersFor resolves auth for an OAuth catalog surface: it
// fetches the stored access token via the wired resolver and builds
// Authorization: Bearer <token> plus the endpoint's extra headers.
// It never fabricates a credential — no resolver wired, resolver
// error, or empty token all return an honest error. The returned
// expiry is a short TTL so the auth cache re-resolves and picks up
// refreshed/rotated tokens (the cache fingerprint does not include
// the token itself).
func (r *Registry) oauthHeadersFor(entry catalog.Provider, ep catalog.EndpointAuth, p *Provider) (map[string]string, string, time.Time, error) {
	r.mu.RLock()
	fn := r.oauthTokenFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, "", time.Time{}, fmt.Errorf("provider %q (%s): OAuth surface but no OAuth token resolver is wired", p.ID, entry.ID)
	}
	tok, err := fn(entry.ID)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("provider %q (%s): resolve OAuth access token: %w", p.ID, entry.ID, err)
	}
	if strings.TrimSpace(tok) == "" {
		return nil, "", time.Time{}, fmt.Errorf("provider %q (%s): OAuth resolver returned an empty token", p.ID, entry.ID)
	}
	headers := map[string]string{"Authorization": "Bearer " + tok}
	for k, v := range ep.ExtraHeaders {
		headers[k] = v
	}
	wire := entry.DefaultWireFormat()
	if len(entry.WireFormats) > 0 {
		wire = entry.WireFormats[0]
	}
	return headers, string(wire), time.Now().Add(time.Minute), nil
}

func (r *Registry) resolveAuthHeadersUncached(p *Provider, key string) (map[string]string, string, time.Time, error) {
	if p.CatalogID != "" {
		entry, err := lookupCatalog(p.CatalogID)
		if err != nil {
			return nil, "", time.Time{}, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		if entry.ToSGrey {
			return nil, "", time.Time{}, fmt.Errorf("provider %q: catalog provider %q is ToS-grey and cannot be a routing target", p.ID, entry.ID)
		}
		// A2: OAuth surfaces (device-code / PKCE, category-oauth).
		// The stored API key is NOT the credential here — the OAuth
		// connection store is. Resolve the access token via the wired
		// resolver and send it as the Bearer credential. Keyed off the
		// endpoint's declared AuthKind so both endpoint-tabled and
		// endpoint-less OAuth providers are covered. Never fabricated:
		// no resolver or no connection = an honest error.
		ep, epOK := entry.Endpoint(p.AuthEndpoint, p.AuthVariant)
		if !epOK {
			ep = entry.DefaultEndpointAuth()
		}
		if ep.AuthKind == catalog.AuthOAuth {
			return r.oauthHeadersFor(entry, ep, p)
		}
		res, err := catalog.ResolveAuth(entry, p.AuthEndpoint, p.AuthVariant, key)
		if err != nil {
			if headers, wire, ok := sigv4Headers(res, key); ok {
				return headers, wire, time.Time{}, nil
			}
			if headers, wire, ok := vertexHeaders(res, key); ok {
				return headers, wire, time.Time{}, nil
			}
			if headers, wire, expiry, eerr := entraIDHeaders(res, key); eerr == nil {
				return headers, wire, expiry, nil
			} else if !errors.Is(eerr, errEntraNotApplicable) {
				return nil, "", time.Time{}, fmt.Errorf("provider %q (%s): %w", p.ID, entry.ID, eerr)
			}
			return nil, "", time.Time{}, fmt.Errorf("provider %q (%s): %w", p.ID, entry.ID, err)
		}
		return res.Headers, string(res.Wire), time.Time{}, nil
	}
	// No catalog link: a Bedrock runtime host speaks Converse and
	// signs with SigV4 — the catalog's templated base URLs never
	// match a concrete host, so check the host pattern directly.
	if isBedrockBaseURL(p.BaseURL) {
		if _, err := parseAWSCredentials(key); err != nil {
			return nil, "", time.Time{}, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		return map[string]string{}, WireFormatBedrock, time.Time{}, nil
	}
	// No catalog link: a Vertex AI host speaks the Vertex wire
	// and mints its token from service-account JSON — same
	// host-pattern rule as Bedrock above, and a key that is not
	// service-account JSON stays refused.
	if isVertexBaseURL(p.BaseURL) {
		if _, _, err := parseVertexServiceAccount(key); err != nil {
			return nil, "", time.Time{}, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		return map[string]string{}, WireFormatVertex, time.Time{}, nil
	}
	// No catalog link: match by base URL when the catalog knows it.
	// Exchange matches fail loudly; an unknown endpoint on a known
	// provider falls back to the legacy Bearer default (the operator
	// typed this base URL by hand, and Bearer is what Add sent).
	// The chat endpoint is requested for headers: a native-wire base
	// with no chat surface (e.g. https://api.anthropic.com) errors
	// here and keeps the legacy headers — which are correct for
	// Anthropic — while the wire is still derived from the base URL
	// below, so the client speaks Messages.
	res, found, err := catalog.ResolveAuthForBaseURL(p.BaseURL, catalog.EndpointChat, "", key)
	if err != nil {
		if errors.Is(err, catalog.ErrExchangeRequired) {
			if headers, wire, ok := sigv4Headers(res, key); ok {
				return headers, wire, time.Time{}, nil
			}
			if headers, wire, ok := vertexHeaders(res, key); ok {
				return headers, wire, time.Time{}, nil
			}
			if headers, wire, expiry, eerr := entraIDHeaders(res, key); eerr == nil {
				return headers, wire, expiry, nil
			} else if !errors.Is(eerr, errEntraNotApplicable) {
				return nil, "", time.Time{}, fmt.Errorf("provider %q: %w", p.ID, eerr)
			}
			return nil, "", time.Time{}, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		return nil, catalogWireForBaseURL(p.BaseURL, key), time.Time{}, nil
	}
	if !found {
		return nil, catalogWireForBaseURL(p.BaseURL, key), time.Time{}, nil
	}
	return res.Headers, string(res.Wire), time.Time{}, nil
}

// DetectWireFormat probes an upstream and reports its wire format.
// It tries GET {base}/v1/models first; on 404 it POSTs a minimal
// /v1/chat/completions probe. Any non-404 chat response — or a 404 whose
// body mentions the model (endpoint exists, probe model unknown) — means
// "openai". When both endpoints 404, the result is "unknown".
//
// This is a heuristic for Phase 1 (OpenAI-compatible first). Native
// wire formats are derived from the catalog by base URL in
// addProvider (catalogWireForBaseURL), not probed here — this probe
// only ever speaks OpenAI wire, so it can only ever report "openai"
// or "unknown".
func DetectWireFormat(ctx context.Context, baseURL, apiKey string, authHeaders map[string]string, factory ClientFactory) (string, error) {
	if factory == nil {
		factory = func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (Client, error) {
			return NewClient(baseURL, apiKey, WithAuthHeaders(authHeaders), WithWireFormat(wireFormat))
		}
	}
	// The probe only speaks OpenAI wire.
	client, err := factory(baseURL, apiKey, authHeaders, WireFormatOpenAI)
	if err != nil {
		return "", err
	}
	if err := client.TestKey(ctx); err == nil {
		return WireFormatOpenAI, nil
	} else if !isNotFound(err) {
		// Key/auth/network failure tells us nothing about the wire format;
		// surface it rather than guessing.
		return "", err
	}

	// Models endpoint 404s — probe chat completions directly.
	probeClient, ok := client.(*openAIClient)
	if !ok {
		return WireFormatUnknown, nil
	}
	payload := []byte(`{"model":"__codvian_router_probe__","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`)
	httpReq, err := probeClient.newRequest(ctx, http.MethodPost, probeClient.chatURL(), payload)
	if err != nil {
		return "", err
	}
	resp, err := probeClient.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode != http.StatusNotFound {
		return WireFormatOpenAI, nil
	}
	if strings.Contains(strings.ToLower(string(body)), "model") {
		return WireFormatOpenAI, nil // endpoint exists; only the probe model is unknown
	}
	return WireFormatUnknown, nil
}
