package webexec

import (
	"encoding/json"
	"fmt"
	"time"
)

// Cookie is one browser cookie inside a captured session state. The
// field set and JSON names mirror Playwright's storageState cookie
// entries so an operator-exported storage state parses verbatim.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // unix seconds; -1 = session cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"` // "Strict" | "Lax" | "None"
}

// LocalStorageEntry is one key/value pair of an origin's localStorage.
type LocalStorageEntry struct {
	Key   string `json:"name"`
	Value string `json:"value"`
}

// OriginStorage is the captured localStorage of one origin.
type OriginStorage struct {
	Origin       string              `json:"origin"`
	LocalStorage []LocalStorageEntry `json:"localStorage,omitempty"`
}

// SessionState is a captured browser session: cookies plus per-origin
// localStorage. It is the unit the operator hands over (their own
// session, by their own choice) and the unit the Vault encrypts at
// rest. CapturedAt records when the state was taken; a state older than
// maxSessionAge is treated as stale on load.
type SessionState struct {
	Cookies    []Cookie        `json:"cookies"`
	Origins    []OriginStorage `json:"origins,omitempty"`
	CapturedAt time.Time       `json:"captured_at"`
}

// maxSessionAge bounds how old a captured state may be before Load
// refuses it as stale. Browser session cookies are typically far
// shorter lived; 30 days is the generous outer bound, not a promise.
const maxSessionAge = 30 * 24 * time.Hour

// Validate reports whether the state carries the minimum needed to
// authenticate a web surface: at least one named cookie (a state with
// zero cookies can never authenticate anything, so storing one would be
// a silent later 401) and non-empty names.
func (s *SessionState) Validate() error {
	if s == nil {
		return fmt.Errorf("webexec: session state is nil")
	}
	if len(s.Cookies) == 0 {
		return fmt.Errorf("webexec: session state carries no cookies")
	}
	for _, c := range s.Cookies {
		if c.Name == "" {
			return fmt.Errorf("webexec: session state has a cookie with an empty name")
		}
	}
	return nil
}

// IsStale reports whether the state is old enough that the provider
// almost certainly invalidated it (treated as stale state → 401
// semantics, not a live refresh attempt: cookies cannot be refreshed
// programmatically without the operator re-capturing).
func (s *SessionState) IsStale(now time.Time) bool {
	if s.CapturedAt.IsZero() {
		return false // imported without a timestamp: let the provider decide
	}
	return now.Sub(s.CapturedAt) > maxSessionAge
}

// RedactedJSON is the audit/log-safe form of a state: cookie and
// localStorage VALUES are replaced with their lengths, so a state can
// be logged without leaking session material.
func (s *SessionState) RedactedJSON() string {
	if s == nil {
		return "{}"
	}
	type redactedCookie struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Len    int    `json:"value_len"`
	}
	type redacted struct {
		Cookies    []redactedCookie `json:"cookies"`
		Origins    int              `json:"origins"`
		CapturedAt time.Time        `json:"captured_at"`
	}
	out := redacted{CapturedAt: s.CapturedAt, Origins: len(s.Origins)}
	for _, c := range s.Cookies {
		out.Cookies = append(out.Cookies, redactedCookie{Name: c.Name, Domain: c.Domain, Len: len(c.Value)})
	}
	b, _ := json.Marshal(out)
	return string(b)
}
