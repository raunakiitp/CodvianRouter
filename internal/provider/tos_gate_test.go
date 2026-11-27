package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// A3 item 6 — ToS risk-acknowledgement gate.
//
// The catalog's ToSGrey marking (web-cookie / subscription-auth
// providers) is the gate flag. The three former hard refusals — add
// time (AddCatalogWithOptions), config load (resolveCatalogForLoad)
// and request time (resolveAuthHeadersUncached) — become gate checks:
// an unacknowledged ToS-grey provider is refused with a plain-words
// message telling the operator to acknowledge the risk; an
// acknowledged one proceeds. Every test here fails when the
// acknowledgement check is removed from the corresponding layer.

// tosGateAcks is the seam the server wires to its acknowledgement
// store (SetToSAcknowledgement in production). Tests flip it to seed
// "acknowledged" exactly the way the real store answers.
type tosGateAcks map[string]bool

func (a tosGateAcks) fn() func(string) bool {
	return func(id string) bool { return a[id] }
}

func newTosGateTestRegistry(acks tosGateAcks, opts ...RegistryOption) *Registry {
	opts = append([]RegistryOption{WithRegistryAllowPrivateDestinations(true),
		WithToSAcknowledgement(acks.fn())}, opts...)
	return NewRegistry(nil, opts...)
}

// tosGreyProbeUpstream is a minimal OpenAI-compatible upstream for the
// add-time pair: /v1/models answers the model list so the live key
// test passes once the gate lets the add through.
func tosGreyProbeUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"web-model-a"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

// assertToSGateError asserts err is the typed, plain-words gate
// refusal: it names the substance (terms may forbid, account can be
// suspended or banned) and tells the operator where to acknowledge.
func assertToSGateError(t *testing.T, err error, catalogID string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected ToS risk-acknowledgement refusal for %q, got nil", catalogID)
	}
	var gate *ToSGateError
	if !errors.As(err, &gate) {
		t.Fatalf("error %v is not a *ToSGateError", err)
	}
	if gate.CatalogID != catalogID {
		t.Errorf("gate error names catalog %q, want %q", gate.CatalogID, catalogID)
	}
	msg := gate.Error()
	for _, want := range []string{
		"ToS-grey",
		"terms",
		"suspended or banned",
		"acknowledge",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("gate message %q does not contain %q", msg, want)
		}
	}
}

// TestToSGateAddTimeUnacknowledged — layer 1: an unacknowledged
// ToS-grey catalog provider is refused at add time with the guidance
// message and nothing is stored.
func TestToSGateAddTimeUnacknowledged(t *testing.T) {
	upstream := httptest.NewServer(tosGreyProbeUpstream())
	defer upstream.Close()
	reg := newTosGateTestRegistry(tosGateAcks{})

	p, err := reg.AddCatalogWithOptions(context.Background(), "chatgpt-web", "session-cookie-value", CatalogAddOptions{BaseURL: upstream.URL})
	if err == nil {
		t.Fatalf("unacknowledged ToS-grey add succeeded (%+v), want refusal", p)
	}
	assertToSGateError(t, err, "chatgpt-web")
	if got := reg.List(); len(got) != 0 {
		t.Errorf("unacknowledged ToS-grey provider was stored (%d providers), want 0", len(got))
	}
}

// TestToSGateAddTimeAcknowledged — layer 1 positive: after the risk is
// acknowledged the same add proceeds like any catalog provider.
func TestToSGateAddTimeAcknowledged(t *testing.T) {
	upstream := httptest.NewServer(tosGreyProbeUpstream())
	defer upstream.Close()
	reg := newTosGateTestRegistry(tosGateAcks{"chatgpt-web": true})

	p, err := reg.AddCatalogWithOptions(context.Background(), "chatgpt-web", "session-cookie-value", CatalogAddOptions{BaseURL: upstream.URL})
	if err != nil {
		t.Fatalf("acknowledged ToS-grey add: %v (want success past the gate)", err)
	}
	if p == nil || p.CatalogID != "chatgpt-web" {
		t.Errorf("added provider = %+v, want catalog chatgpt-web", p)
	}
}

// TestToSGateLoadTimeUnacknowledged — layer 2: a config file naming an
// unacknowledged ToS-grey provider fails the load with the guidance
// message.
func TestToSGateLoadTimeUnacknowledged(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-tosgate-load!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:        "prov-grey",
		Name:      "Grey",
		CatalogID: "chatgpt-web",
		BaseURL:   "https://chatgpt.example.invalid",
		Enabled:   true,
	}})
	_, err = NewFileRegistry(filepath.Join(dir, filepath.Base(path)), WithCipher(NewVaultCipher(vault)), WithRegistryAllowPrivateDestinations(true))
	assertToSGateError(t, err, "chatgpt-web")
}

// TestToSGateLoadTimeAcknowledged — layer 2 positive: the same config
// file loads once the risk is acknowledged.
func TestToSGateLoadTimeAcknowledged(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-tosgate-load-ok!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:        "prov-grey",
		Name:      "Grey",
		CatalogID: "chatgpt-web",
		BaseURL:   "https://chatgpt.example.invalid",
		Enabled:   true,
	}})
	reg, err := NewFileRegistry(filepath.Join(dir, filepath.Base(path)), WithCipher(NewVaultCipher(vault)), WithRegistryAllowPrivateDestinations(true),
		WithToSAcknowledgement(func(id string) bool { return id == "chatgpt-web" }))
	if err != nil {
		t.Fatalf("acknowledged ToS-grey load: %v (want success past the gate)", err)
	}
	if got := reg.List(); len(got) != 1 {
		t.Errorf("loaded providers = %d, want 1", len(got))
	}
}

// TestToSGateRequestTimeUnacknowledged — layer 3: an unacknowledged
// ToS-grey provider present in the registry is refused on the real
// auth-resolution path (resolveAuthHeadersUncached, which runs before
// the OAuth branch) with the guidance message.
func TestToSGateRequestTimeUnacknowledged(t *testing.T) {
	reg := newTosGateTestRegistry(tosGateAcks{})
	p := &Provider{
		ID:        "p-grey",
		Name:      "Grey",
		CatalogID: "chatgpt-web",
		Enabled:   true,
	}
	_, _, _, err := reg.resolveAuthHeadersUncached(p, "session-cookie-value")
	assertToSGateError(t, err, "chatgpt-web")
}

// TestToSGateRequestTimeAcknowledged — layer 3 positive: with the risk
// acknowledged, the same resolution passes the gate and proceeds down
// the real catalog resolution path. Whatever the downstream outcome
// (the web-cookie surface may honestly refuse a bare credential), the
// refusal must no longer be the gate.
func TestToSGateRequestTimeAcknowledged(t *testing.T) {
	reg := newTosGateTestRegistry(tosGateAcks{"chatgpt-web": true})
	p := &Provider{
		ID:        "p-grey",
		Name:      "Grey",
		CatalogID: "chatgpt-web",
		Enabled:   true,
	}
	headers, wire, _, err := reg.resolveAuthHeadersUncached(p, "session-cookie-value")
	if err != nil {
		var gate *ToSGateError
		if errors.As(err, &gate) {
			t.Fatalf("acknowledged provider still refused by the gate: %v", err)
		}
		// An honest downstream error is acceptable; the gate passed.
		return
	}
	if len(headers) == 0 || wire == "" {
		t.Errorf("gate passed but resolution produced no headers/wire (%v/%q)", headers, wire)
	}
}

// TestToSGateNonGreyUnaffected — control: non-ToS-grey providers are
// untouched by the gate. With NO acknowledgement function wired at all
// (nil — the default), a normal catalog provider still adds, and a
// wired-but-empty acknowledgement store refuses nothing non-grey.
func TestToSGateNonGreyUnaffected(t *testing.T) {
	upstream := httptest.NewServer(tosGreyProbeUpstream())
	defer upstream.Close()

	// Nil ack fn (no gate wiring): the normal add path is unchanged.
	reg := NewRegistry(nil, WithRegistryAllowPrivateDestinations(true))
	p, err := reg.AddCatalogWithOptions(context.Background(), "groq", "sk-groq-test", CatalogAddOptions{BaseURL: upstream.URL})
	if err != nil {
		t.Fatalf("non-grey add without any gate wiring: %v", err)
	}
	if p == nil || p.CatalogID != "groq" {
		t.Errorf("added provider = %+v, want catalog groq", p)
	}

	// A wired-but-empty ack store must not refuse non-grey providers.
	reg2 := newTosGateTestRegistry(tosGateAcks{})
	if _, err := reg2.AddCatalogWithOptions(context.Background(), "groq", "sk-groq-test", CatalogAddOptions{BaseURL: upstream.URL}); err != nil {
		t.Fatalf("non-grey add with an empty ack store: %v", err)
	}
}
