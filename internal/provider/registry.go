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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
type ClientFactory func(baseURL, apiKey string) (Client, error)

// Registry is the in-memory provider registry, persisted through a Store.
// It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*Provider
	order     []string // insertion order (List order)
	store     Store
	cipher    KeyCipher
	newClient ClientFactory
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

// NewRegistry returns an empty Registry backed by store (nil store means
// memory-only, no persistence). Use Load to populate it from the store.
// With no WithCipher option the registry uses an ephemeral in-memory
// Vault-backed cipher (see defaultCipher): fine for memory-only use and
// tests, but a registry backed by a persistent Store must be given a
// Vault-backed cipher via WithCipher, or sealed keys will not survive a
// restart.
func NewRegistry(store Store, opts ...RegistryOption) *Registry {
	r := &Registry{
		providers: make(map[string]*Provider),
		store:     store,
		newClient: func(baseURL, apiKey string) (Client, error) {
			return NewClient(baseURL, apiKey)
		},
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
	normalized, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("provider API key is empty")
	}
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

	encrypted, err := r.encryptKey(apiKey)
	if err != nil {
		return nil, err
	}

	client, err := r.newClient(normalized, apiKey)
	if err != nil {
		return nil, err
	}

	// 1. Inline key test.
	testErr := client.TestKey(ctx)
	wireFormat := WireFormatOpenAI
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
		detected, probeErr := DetectWireFormat(ctx, normalized, apiKey, r.newClient)
		if probeErr != nil {
			return nil, fmt.Errorf("key test failed for %s: %w", normalized, testErr)
		}
		wireFormat = detected
		if detected == WireFormatUnknown {
			return nil, fmt.Errorf("key test failed for %s: %w", normalized, testErr)
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
	return r.newClient(p.BaseURL, key)
}

// DetectWireFormat probes an upstream and reports its wire format.
// It tries GET {base}/v1/models first; on 404 it POSTs a minimal
// /v1/chat/completions probe. Any non-404 chat response — or a 404 whose
// body mentions the model (endpoint exists, probe model unknown) — means
// "openai". When both endpoints 404, the result is "unknown".
//
// This is a heuristic for Phase 1 (OpenAI-compatible first); non-OpenAI
// wire formats (Anthropic native, Gemini native, ...) are future work.
func DetectWireFormat(ctx context.Context, baseURL, apiKey string, factory ClientFactory) (string, error) {
	if factory == nil {
		factory = func(baseURL, apiKey string) (Client, error) {
			return NewClient(baseURL, apiKey)
		}
	}
	client, err := factory(baseURL, apiKey)
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
