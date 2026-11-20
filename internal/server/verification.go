package server

import (
	"net/http"
)

// ---------------------------------------------------------------------------
// Verification-status feed (CHECKLIST 241-242).
// ---------------------------------------------------------------------------
//
// GET /dashboard/verification serves the honest verification status: what
// is proven in this VM vs what still needs CI on the first tag, a live
// browser, a second host, or a real Docker host. The table is static —
// the statuses are facts about this repo's verification state, recorded
// in docs/live-verification-backlog.md — but it is served live (not
// baked into the dashboard HTML) so the dashboard and the doc cannot
// drift apart silently: the feed and the doc carry the same item ids
// (LV-01..LV-12, PV-01..PV-06), and if an item clears, both must be
// updated in the same slice.
//
// Statuses: "proven-in-vm" (observed here) or "needs-ci" /
// "needs-first-tag" / "needs-live-browser" / "needs-second-host" /
// "needs-docker" (not yet observed, with the blocker named). Nothing
// unproven is ever reported as proven: the dashboard renders unproven
// items with the grey unverified badge and no green tick.

// verificationStatus is one row of the verification-status feed.
type verificationStatus struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Needs    string `json:"needs"`
	Evidence string `json:"evidence"`
	Doc      string `json:"doc"`
}

// verificationFeed is the GET /dashboard/verification payload.
type verificationFeed struct {
	Items    []verificationStatus `json:"items"`
	Proven   int                  `json:"proven"`
	Unproven int                  `json:"unproven"`
	Note     string               `json:"note"`
}

// verificationItems is the committed status table. Keep it in sync with
// docs/live-verification-backlog.md (same ids, same statuses) — the test
// in verification_test.go pins the counts and the status vocabulary, but
// only a human updating both files keeps the words true.
var verificationItems = []verificationStatus{
	// Proven in this VM.
	{ID: "PV-01", Title: "goreleaser check green on the pinned GoReleaser", Status: "proven-in-vm", Needs: "—", Evidence: "Real GoReleaser v2.20.2, exit 0, on the real .goreleaser.yml (scratch clone with a remote; this worktree's local repo has no remote by design).", Doc: "docs/live-verification-backlog.md"},
	{ID: "PV-02", Title: "Local image sign/verify + tamper proof", Status: "proven-in-vm", Needs: "—", Evidence: "Real cosign + crane-assembled image, throwaway keypair: untampered verifies, tampered-under-the-same-tag / wrong-key rejected — 17/17 (cosign v2.4.3), 18/18 (v3.1.4). Proves the verify path and digest-binding, not the buildx build or the keyless path.", Doc: "docs/live-verification-backlog.md"},
	{ID: "PV-03", Title: "Provider-add / DB import / DB wipe / DB export endpoints at server level", Status: "proven-in-vm", Needs: "—", Evidence: "Real server + tests + curl smoke. The endpoints work; only the dashboard form click-throughs (LV-08–LV-11) are pending.", Doc: "docs/live-verification-backlog.md"},
	{ID: "PV-04", Title: "Loopback serving (dashboard + API on loopback)", Status: "proven-in-vm", Needs: "—", Evidence: "Every live smoke serves the dashboard and API on loopback. This is the loopback-allow half of the guard; the non-loopback refuse half is LV-12.", Doc: "docs/live-verification-backlog.md"},
	{ID: "PV-05", Title: "Release workflow static checks", Status: "proven-in-vm", Needs: "—", Evidence: "Workflow YAML parses; all 14 uses: entries SHA-pinned (incl. the two new buildx steps, pins resolved live via git ls-remote); goreleaser check step present.", Doc: "docs/ci-proof-multarch.md"},
	{ID: "PV-06", Title: "Healthcheck helper parity with Dockerfile", Status: "proven-in-vm", Needs: "—", Evidence: "Inline healthcheck source in Dockerfile.release is byte-identical to Dockerfile's; runtime directives (ENTRYPOINT/CMD/EXPOSE/HEALTHCHECK/USER) match (221–222 gate).", Doc: "docs/live-verification-backlog.md"},
	// Still unproven.
	{ID: "LV-01", Title: "Multi-arch docker buildx build (amd64 + arm64)", Status: "needs-ci", Needs: "CI on the first real tag", Evidence: "Dockerfile.release has never been executed — docker is not installed in this VM (docker: command not found). Validated by goreleaser check only.", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-02", Title: "Published manifest lists both architectures", Status: "needs-ci", Needs: "CI on the first real tag + manual inspect", Evidence: "No published image exists and no CI step asserts the manifest (gap G4). A single-arch publish would still pass the verify job.", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-03", Title: "Cosign keyless signature over the published image", Status: "needs-first-tag", Needs: "First real tag", Evidence: "Keyless identity is minted by GitHub Actions OIDC and only exists inside a CI run. Keypair-mode proof only (PV-02).", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-04", Title: "Image build-provenance attestation (gh attestation verify)", Status: "needs-first-tag", Needs: "First real tag", Evidence: "Attestations live in GitHub's attestation store — nothing to fetch until a release attests. The digest lookup it depends on is itself unproven (gap G6).", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-05", Title: "docker run --rm <image> help on amd64", Status: "needs-ci", Needs: "CI on the first real tag", Evidence: "The verify job runs it, but the job has never run. The binary's help path is proven directly (live smoke), never from inside the published image.", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-06", Title: "docker run --rm <image> help on arm64", Status: "needs-docker", Needs: "Arm64 host or QEMU in CI", Evidence: "The verify job runs on amd64 only, so even CI does not cover the arm64 variant today (gap G3). The arm64 image has never been executed anywhere.", Doc: "docs/ci-proof-multarch.md"},
	{ID: "LV-07", Title: "First literal docker run of the local image", Status: "needs-docker", Needs: "Real Docker host", Evidence: "No daemon; the VM cannot run containers (no effective CAP_SYS_ADMIN; setns: operation not permitted, slice 4). Proven only via chroot of the imported rootfs.", Doc: "docs/live-verification-backlog.md"},
	{ID: "LV-08", Title: "Provider-add form click-through", Status: "needs-live-browser", Needs: "Live browser", Evidence: "POST /api/providers proven at server level (PV-03). The dashboard form's fetch, auth header and error rendering were never clicked in a real browser.", Doc: "docs/live-verification-backlog.md"},
	{ID: "LV-09", Title: "DB import form click-through", Status: "needs-live-browser", Needs: "Live browser", Evidence: "POST /admin/db/import proven at server level (PV-03). The form's file-picker + phrase + result rendering never clicked.", Doc: "docs/live-verification-backlog.md"},
	{ID: "LV-10", Title: "DB wipe form click-through", Status: "needs-live-browser", Needs: "Live browser + throwaway data dir", Evidence: "POST /admin/db/wipe proven at server level (PV-03), incl. the arm-token and phrase rules. The form flow never clicked; the action is irreversible.", Doc: "docs/live-verification-backlog.md"},
	{ID: "LV-11", Title: "DB export form click-through", Status: "needs-live-browser", Needs: "Live browser", Evidence: "POST /admin/db/export proven at server level (PV-03). The form + download never clicked.", Doc: "docs/live-verification-backlog.md"},
	{ID: "LV-12", Title: "Non-loopback 403 (Tier 1 refused off-loopback)", Status: "needs-second-host", Needs: "Second host", Evidence: "The guard judges by real TCP RemoteAddr. This VM's only non-loopback address (host0 198.19.0.2/30) is unreachable — a probe on 2026-11-18 timed out — so no non-loopback peer can ever arrive here. Refusal branch is unit-tested only.", Doc: "docs/live-verification-backlog.md"},
}

// handleVerification serves GET /dashboard/verification.
func (s *Server) handleVerification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	feed := verificationFeed{
		Items: verificationItems,
		Note:  "Statuses are facts about this repo's verification state, recorded in docs/live-verification-backlog.md (same item ids). Proven means observed — in this VM, in CI, or in a live browser — never assumed. Unproven items stay unproven until the check named in Needs is actually run and recorded.",
	}
	for _, it := range verificationItems {
		if it.Status == "proven-in-vm" {
			feed.Proven++
		} else {
			feed.Unproven++
		}
	}
	writeJSON(w, http.StatusOK, feed)
}
