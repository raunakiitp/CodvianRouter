package server

import (
	"strings"
	"testing"
)

// G-02 keyboard-accessibility regression: the served dashboard must keep
// the three additions that make the operator UI keyboard-usable, all
// asserted against the SERVED artifact (never a copy):
//   - a skip-to-content link targeting the main content anchor;
//   - a :focus-visible ring and the no-outline-on-mouse rule;
//   - the modal focus helpers and their hooks on both dialogs.
//
// A regression strips these and this test fails on the real served page.
func TestDashboardAccessibilitySurfaceServed(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)

	// Skip link + target + visible-on-focus style.
	for _, want := range []string{
		`class="skip-link"`,
		`Skip to main content`,
		`id="mainContent"`,
		`href="#mainContent"`,
		`.skip-link:focus-visible{`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served dashboard is missing %q", want)
		}
	}

	// :focus-visible ring uses existing tokens (no new palette).
	if !strings.Contains(body, `:focus-visible{outline:2px solid var(--blue)`) {
		t.Errorf("served dashboard is missing the keyboard focus-visible ring")
	}

	// Modal focus helpers + hooks in the served inline script.
	for _, want := range []string{
		`function trapFocusIn(root)`,
		`function untrapFocus(root)`,
		`function openModalWithFocus(root)`,
		`function closeModalReturnFocus(root)`,
		`openModalWithFocus(modal.runner || modal)`,
		`openModalWithFocus(dangerDialog)`,
		`closeModalReturnFocus(dangerDialog)`,
		`closeModalReturnFocus(provTestModalEl)`,
		`e.key === "Escape"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("served dashboard script is missing %q", want)
		}
	}
}
