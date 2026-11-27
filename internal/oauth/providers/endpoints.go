package providers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"testing"
)

// newPKCE generates an RFC 7636 code_verifier and its S256
// code_challenge.
func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("providers: pkce entropy: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// newState generates a random 32-byte state for CSRF protection on
// authorization flows.
func newState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("providers: state entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// endpointOverrides lets TESTS point a handler's network endpoints at
// a local mock. The map is package-private and only the exported
// SetEndpointsForTest writes it (with the test's cleanup); production
// code never touches it and every production endpoint is the
// documented one verbatim.
var endpointOverrides atomic.Pointer[map[string]EndpointOverrides]

// EndpointOverrides redirects named endpoints of one handler. The keys
// are the per-handler endpoint names ("auth", "token", "device",
// "exchange", "register", "refresh", "deviceauth"); an absent or empty
// value keeps the documented endpoint.
type EndpointOverrides struct {
	URLs map[string]string
}

// SetEndpointsForTest points the named handler at test-supplied
// endpoints for the duration of the test. It is exported ONLY for the
// bridge tests in internal/server, which drive the real handlers
// against local mocks.
func SetEndpointsForTest(t testing.TB, id string, urls map[string]string) {
	t.Helper()
	m := map[string]EndpointOverrides{}
	if cur := endpointOverrides.Load(); cur != nil {
		for k, v := range *cur {
			m[k] = v
		}
	}
	m[id] = EndpointOverrides{URLs: urls}
	endpointOverrides.Store(&m)
	t.Cleanup(func() {
		cur := endpointOverrides.Load()
		if cur == nil {
			return
		}
		next := map[string]EndpointOverrides{}
		for k, v := range *cur {
			if k != id {
				next[k] = v
			}
		}
		endpointOverrides.Store(&next)
	})
}

// endpoint returns the effective URL for one named endpoint of a
// handler: the test override when set, the documented URL otherwise.
func endpoint(id, key, documented string) string {
	if cur := endpointOverrides.Load(); cur != nil {
		if o, ok := (*cur)[id]; ok {
			if u := o.URLs[key]; u != "" {
				return u
			}
		}
	}
	return documented
}
