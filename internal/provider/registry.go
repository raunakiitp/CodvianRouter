package provider

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
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

// WithTFOEnabled enables or disables TCP Fast Open on the upstream
// clients the registry builds (default true). Disabling is only for
// measuring the with/without difference (see provider.TFOStatus);
// the dialer falls back gracefully either way, so disabling is
// never needed for correctness.
func WithTFOEnabled(enabled bool) RegistryOption {
	return func(r *Registry) { r.tfoEnabled = enabled }
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
	}
	// The default factory reads r.tfoEnabled at call time (not at
	// construction), so WithTFOEnabled applied after NewRegistry
	// still affects clients built later.
	r.newClient = func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (Client, error) {
		return NewClient(baseURL, apiKey, WithAuthHeaders(authHeaders), WithWireFormat(wireFormat), WithTCPFastOpen(r.tfoEnabled))
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
			if err := resolveCatalogForLoad(p); err != nil {
				return err
			}
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
// answers, the provider is still added with an empty model list and wire
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
}

// addProvider validates baseURL, encrypts apiKey, verifies the key
// (TestKey), auto-fetches the model list (ListModels) and auto-detects
// the wire format, then stores and persists the provider. The display
// name is derived from the base URL host; use AddNamed for an explicit
// name.
//
// If the upstream's /v1/models endpoint 404s but its chat endpoint
// answers, the provider is still added with an empty model list and wire
// format "openai" (some gateways only expose chat completions). Any other
// key-test failure (401, 429, 5xx, network error) rejects the add.
func (r *Registry) addProvider(ctx context.Context, in addInput) (*Provider, error) {
	normalized, err := ValidateBaseURL(in.baseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.apiKey) == "" && !in.allowEmptyKey {
		return nil, errors.New("provider API key is empty")
	}
	name := in.name
	if strings.TrimSpace(name) == "" {
		name = DeriveName(normalized)
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
	switch {
	case testErr == nil:
		// 2. Auto-fetch model list (second call, per the Phase 1 contract:
		// TestKey verifies, ListModels fetches).
		ids, err := client.ListModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching model list for %s: %w", normalized, err)
		}
		models = modelsFromIDs(ids)
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
		// Chat endpoint exists but no model list: add with no models.
		models = nil
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
		Enabled:         true,
		QuotaHint:       QuotaUnknown,
		CreatedAt:       time.Now().UTC(),
		CatalogID:       in.catalogID,
		Verification:    in.verification,
		AuthEndpoint:    in.authEndpoint,
		AuthVariant:     in.authVariant,
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
//     SigV4 is the exception: the client signs each request itself,
//     so a Bedrock provider is allowed when the key is AWS
//     credential material ("ACCESS:SECRET[:TOKEN]"); a key that is
//     not stays refused.
//   - entries with no known base URL (name-only reference entries);
//   - templated catalog base URLs (Azure {resource}, ...): pass a
//     concrete BaseURL via AddCatalogWithOptions.
func (r *Registry) AddCatalog(ctx context.Context, catalogID, apiKey string) (*Provider, error) {
	return r.AddCatalogWithOptions(ctx, catalogID, apiKey, CatalogAddOptions{})
}

// AddCatalogWithOptions is AddCatalog with an explicit surface,
// variant, and/or base-URL override (see CatalogAddOptions).
func (r *Registry) AddCatalogWithOptions(ctx context.Context, catalogID, apiKey string, opts CatalogAddOptions) (*Provider, error) {
	entry, err := lookupCatalog(catalogID)
	if err != nil {
		return nil, err
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
	// static header. The one exception is Bedrock SigV4: the client
	// signs each request itself from the operator's AWS credentials,
	// so a SigV4 surface with parseable credentials is allowed
	// through (with an empty header set — there is no static header
	// to store). Any other exchange, or a SigV4 surface whose key
	// is not AWS credential material, stays refused.
	res, err := catalog.ResolveAuth(entry, endpoint, opts.Variant, apiKey)
	if err != nil {
		if errors.Is(err, catalog.ErrExchangeRequired) {
			if headers, _, ok := sigv4Headers(res, apiKey); ok {
				// SigV4 is the one exchange the client performs
				// itself: keep the resolution (BaseURL + Wire are
				// already set on res) with an empty header set.
				res.Headers = headers
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
	})
}

// lookupCatalog finds a catalog entry by ID, falling back to alias
// ("grok" → xai, "ollama" → ollama-local).
func lookupCatalog(idOrAlias string) (catalog.Provider, error) {
	if entry, ok := catalog.ByID(idOrAlias); ok {
		return entry, nil
	}
	if entry, ok := catalog.ByAlias(idOrAlias); ok {
		return entry, nil
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
func resolveCatalogForLoad(p *Provider) error {
	entry, err := lookupCatalog(p.CatalogID)
	if err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
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
	} else if _, err := ValidateBaseURL(p.BaseURL); err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
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
	normalized, err := ValidateBaseURL(baseURL)
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
// is configured instead of panicking on a nil cipher.
func (r *Registry) decryptKey(ciphertext string) (string, error) {
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
// the legacy Bearer default. Exchange surfaces (Vertex, OpenRouter
// JWT, Azure Entra) fail here with a clear error — the client is
// never built with a faked static header for them. The one exception
// is Bedrock SigV4: the exchange is performed by the client itself
// (each request is signed from the operator's AWS credentials), so a
// SigV4 surface with parseable credentials builds a Converse client
// instead of failing.
//
// The wire format likewise comes from the catalog resolution: a
// provider whose surface speaks Anthropic gets a Messages client, a
// Gemini surface a generateContent client, a Cohere surface a Chat
// API v2 client, a Bedrock surface a Converse client, everything
// else the OpenAI client.
func (r *Registry) ClientFor(id string) (Client, error) {
	r.mu.RLock()
	p, ok := r.providers[id]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	key, err := r.decryptKey(p.APIKeyEncrypted)
	if err != nil {
		return nil, err
	}
	headers, wireFormat, err := r.resolveAuthHeaders(p, key)
	if err != nil {
		return nil, err
	}
	// A native wire wins from either source: a stored wire from an
	// older config must not be silently downgraded by a base-URL
	// lookup that could not see it (e.g. a proxy base), and a
	// resolved native wire must not be lost to a stale stored
	// "openai".
	if p.WireFormat == WireFormatAnthropic || wireFormat == WireFormatAnthropic {
		wireFormat = WireFormatAnthropic
	} else if p.WireFormat == WireFormatGemini || wireFormat == WireFormatGemini {
		wireFormat = WireFormatGemini
	} else if p.WireFormat == WireFormatCohere || wireFormat == WireFormatCohere {
		wireFormat = WireFormatCohere
	} else if p.WireFormat == WireFormatBedrock || wireFormat == WireFormatBedrock {
		wireFormat = WireFormatBedrock
	} else if wireFormat == "" {
		wireFormat = p.WireFormat
	}
	return r.newClient(p.BaseURL, key, headers, wireFormat)
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

// resolveAuthHeaders resolves the exact auth headers for one provider,
// plus the wire format its surface speaks. It returns ("", nil, nil)
// for the legacy Bearer default with the stored wire format.
func (r *Registry) resolveAuthHeaders(p *Provider, key string) (map[string]string, string, error) {
	if p.CatalogID != "" {
		entry, err := lookupCatalog(p.CatalogID)
		if err != nil {
			return nil, "", fmt.Errorf("provider %q: %w", p.ID, err)
		}
		if entry.ToSGrey {
			return nil, "", fmt.Errorf("provider %q: catalog provider %q is ToS-grey and cannot be a routing target", p.ID, entry.ID)
		}
		res, err := catalog.ResolveAuth(entry, p.AuthEndpoint, p.AuthVariant, key)
		if err != nil {
			if headers, wire, ok := sigv4Headers(res, key); ok {
				return headers, wire, nil
			}
			return nil, "", fmt.Errorf("provider %q (%s): %w", p.ID, entry.ID, err)
		}
		return res.Headers, string(res.Wire), nil
	}
	// No catalog link: a Bedrock runtime host speaks Converse and
	// signs with SigV4 — the catalog's templated base URLs never
	// match a concrete host, so check the host pattern directly.
	if isBedrockBaseURL(p.BaseURL) {
		if _, err := parseAWSCredentials(key); err != nil {
			return nil, "", fmt.Errorf("provider %q: %w", p.ID, err)
		}
		return map[string]string{}, WireFormatBedrock, nil
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
				return headers, wire, nil
			}
			return nil, "", fmt.Errorf("provider %q: %w", p.ID, err)
		}
		return nil, catalogWireForBaseURL(p.BaseURL, key), nil
	}
	if !found {
		return nil, catalogWireForBaseURL(p.BaseURL, key), nil
	}
	return res.Headers, string(res.Wire), nil
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
