package webexec

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ExecuteRequest is one web-UI execution: the prompt (already the
// user-visible text — the executor turns it into the surface's chat
// shape), an optional model selector, and whether the execution should
// ride a temporary-chat (no history) surface where the provider
// supports one.
type ExecuteRequest struct {
	Prompt        string
	Model         string
	TemporaryChat bool
}

// ExecuteResult is the surface's answer. StatusCode is the surface's
// HTTP status (200 on success) for callers that log it.
type ExecuteResult struct {
	Content       string
	Surface       string
	Model         string
	StatusCode    int
	TemporaryChat bool
}

// Executor executes one prompt against one catalog WebCookie provider's
// web surface using stored session state. Implementations must be safe
// for concurrent use.
type Executor interface {
	// Execute runs the prompt against the surface and returns its
	// answer. Errors classify with errors.Is: ErrNoSession /
	// ErrStaleSession (401 semantics), ErrQuotaExceeded (429
	// passthrough), ErrExecutorDisabled, ErrEndpointUndocumented,
	// ErrNoBrowser.
	Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error)
	// CatalogID is the surface this executor serves.
	CatalogID() string
	// HasSession reports whether stored session state exists.
	HasSession() bool
}

// ---------------------------------------------------------------------------
// Pluggable headless-browser seam
// ---------------------------------------------------------------------------

// BrowserSession is one live headless-browser session the harness may
// use to CAPTURE session state: the operator logs into the provider in
// the driven browser, then ImportStorageState reads the cookies and
// localStorage back as a SessionState. Capture only — execution always
// goes through the surface executor, never a screen scraper.
type BrowserSession interface {
	// ImportStorageState captures the current storage state for an
	// origin (the operator is expected to be logged in already).
	ImportStorageState(ctx context.Context, origin string) (*SessionState, error)
	// Close terminates the browser session.
	Close() error
}

// BrowserSessionFactory launches a headless browser. name selects the
// registered implementation ("chromedp", "playwright-go", ...).
type BrowserSessionFactory func(ctx context.Context) (BrowserSession, error)

var (
	browserMu       sync.Mutex
	browserRegistry = map[string]BrowserSessionFactory{}
)

// RegisterBrowserFactory plugs a headless-browser implementation into
// the harness. This is THE seam for a future chromedp/playwright-go
// driver: none ships by default (the repo has no Go headless-browser
// dependency and Go-test CI has no Chrome — see doc.go), so capture
// answers ErrNoBrowser until one is registered.
func RegisterBrowserFactory(name string, f BrowserSessionFactory) {
	browserMu.Lock()
	defer browserMu.Unlock()
	browserRegistry[name] = f
}

// BrowserFactory returns a registered factory by name.
func BrowserFactory(name string) (BrowserSessionFactory, bool) {
	browserMu.Lock()
	defer browserMu.Unlock()
	f, ok := browserRegistry[name]
	return f, ok
}

// CaptureSessionState launches the named registered browser, lets the
// operator log in (the callback decides when the login is done), then
// imports the storage state for the surface's web origin. The returned
// state is exactly what Save expects. With no factory registered under
// name, the error wraps ErrNoBrowser — capture is never faked.
func CaptureSessionState(ctx context.Context, name, catalogID string, waitLoggedIn func(bs BrowserSession) error) (*SessionState, error) {
	surface, err := Surface(catalogID)
	if err != nil {
		return nil, err
	}
	f, ok := BrowserFactory(name)
	if !ok {
		return nil, fmt.Errorf("webexec: %w: %q is not registered (available: none by default)", ErrNoBrowser, name)
	}
	bs, err := f(ctx)
	if err != nil {
		return nil, fmt.Errorf("webexec: launch browser %q: %w", name, err)
	}
	defer bs.Close()
	if waitLoggedIn != nil {
		if err := waitLoggedIn(bs); err != nil {
			return nil, fmt.Errorf("webexec: operator login did not complete: %w", err)
		}
	}
	state, err := bs.ImportStorageState(ctx, surface.WebOrigin)
	if err != nil {
		return nil, fmt.Errorf("webexec: capture storage state for %s: %w", catalogID, err)
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return state, nil
}

// ---------------------------------------------------------------------------
// Executor gate
// ---------------------------------------------------------------------------

// defaultExecutorTimeout bounds one web execution: web UIs are slow,
// but an unbounded executor hangs a router request.
const defaultExecutorTimeout = 90 * time.Second
