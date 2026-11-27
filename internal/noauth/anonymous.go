package noauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AnonymousClient dials a provider's documented keyless endpoint.
// It carries NO user credential: no Authorization header at all,
// or the provider's documented shared anonymous key (AI Horde's
// 0000000000) as a placeholder Bearer — never a stored user key.
// The mechanism's documented per-IP limits are enforced locally
// before the request leaves, and the kill switch (NOAUTH_DISABLED)
// fails the dial closed.
type AnonymousClient struct {
	// HTTP is the client used for dials; nil means a default
	// client with a sane timeout.
	HTTP *http.Client
	// Now is injectable for limiter tests; nil means time.Now.
	Now func() time.Time
	// limiters caches one limiter per mechanism ID.
	limiters map[string]*IPLimiter
}

// Dial sends one anonymous chat request to the mechanism's keyless
// endpoint (OpenAI-compatible chat completions). ip is the caller's
// client address, used only for the documented per-IP budget. The
// response body is returned raw; the caller decodes its own wire.
func (c *AnonymousClient) Dial(ctx context.Context, m Mechanism, ip, model string, messages []map[string]string) ([]byte, error) {
	if disabled() {
		return nil, ErrBridgeDisabled
	}
	if m.Kind != KindAnonymous || len(m.BaseURLs) == 0 {
		return nil, fmt.Errorf("noauth: %s has no documented anonymous endpoint", m.ID)
	}
	if c.limiters == nil {
		c.limiters = map[string]*IPLimiter{}
	}
	lim, ok := c.limiters[m.ID]
	if !ok {
		lim = NewIPLimiter(m.Limits)
		if c.Now != nil {
			lim.now = c.Now
		}
		c.limiters[m.ID] = lim
	}
	if !lim.Allow(ip) {
		return nil, &OverLimitError{ID: m.ID, Limits: m.Limits}
	}

	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": messages,
	})
	if err != nil {
		return nil, fmt.Errorf("noauth: %s: %w", m.ID, err)
	}
	target, err := url.JoinPath(strings.TrimRight(m.BaseURLs[0], "/"), "chat/completions")
	if err != nil {
		return nil, fmt.Errorf("noauth: %s: %w", m.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("noauth: %s: %w", m.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if m.AnonymousKey != "" {
		// The provider documents this shared key for anonymous
		// use (AI Horde: "Use the API key 0000000000"). It is a
		// public constant from the provider's docs, not a stored
		// credential.
		req.Header.Set("Authorization", "Bearer "+m.AnonymousKey)
	}

	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("noauth: %s dial: %w", m.ID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("noauth: %s read: %w", m.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("noauth: %s upstream HTTP %d: %s", m.ID, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// OverLimitError reports that the locally-enforced documented per-IP
// budget is exhausted: the request was refused before dialing.
type OverLimitError struct {
	ID     string
	Limits []Limit
}

func (e *OverLimitError) Error() string {
	parts := make([]string, len(e.Limits))
	for i, l := range e.Limits {
		parts[i] = l.String()
	}
	return fmt.Sprintf("noauth: %s documented per-IP limit exceeded (%s) — request refused locally (upstream answers 429)", e.ID, strings.Join(parts, ", "))
}
