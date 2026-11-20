# Release checklist — Codvian Router

> **Status:** no release has been published yet. This checklist is
> carved in stone **before** the bulk push and the first public
> release (CHECKLIST items 261–262, research-log entries 233 +
> 235). The first release happens only when the router is 10x ahead
> of the reference router, on the maintainer's call — and when it happens, it
> happens by this checklist, not by memory.
>
> The lesson it encodes is LiteLLM v1.107.0 (Oct 2026): a key
> committed to a public repo was deleted but never revoked; four
> months later it was used to steal a GitHub org owner's CLI OAuth
> token, redirect the private release workflow to build from a
> lookalike org (`Berri-AI/litellm` — one hyphen), approve the
> production gate with a second stolen token, and ship poisoned
> PyPI wheels + Docker images that stole cloud credentials from
> every Python process that imported them. The malicious images
> carried a **valid signature from the old signing key** — a
> signature alone proves nothing after a key compromise. The full
> chain is in research-log entry 233.

## The rules (non-negotiable)

These are the six rules from entry 233, plus the repo-visibility
rule from the bulk-push plan. A release that breaks any of them is
not announced — it is pulled and re-cut.

### 1. Revoke, don't just delete

- [ ] Any key, token, or credential that **ever touched a public
  repo** — even for minutes, even if already deleted from the file
  — is **revoked immediately**, no exceptions. Deleting is not
  revoking: the LiteLLM key stayed valid for 4+ months after
  deletion.
- [ ] This covers GitHub tokens (personal, OAuth, fine-grained),
  cloud keys, npm/PyPI tokens, registry credentials, and any
  secret that appeared in a commit, a gist, a log, or a screenshot.
- [ ] After a revoke, check the audit log for use of that
  credential between its exposure and its revocation. If it was
  used, treat it as a compromise (rule 6), not a cleanup.

### 2. One dedicated release-approval account

- [ ] Release approval sits with **one dedicated account**, tied
  to a single maintainer device — never with an owner token usable
  via `gh` or any CLI.
- [ ] Owner work moves to **separate accounts that cannot be used
  through tokens or the `gh` CLI** at all.
- [ ] Owner tokens **must not be able to override branch
  protection on release workflows**. (In the LiteLLM incident,
  branch protection rejected one push — and the owner token
  overrode it.)
- [ ] The release workflow (`.github/workflows/release.yml`) runs
  **only** on a pushed `v*.*.*` tag, and only the maintainer
  pushes tags. No slice, cron, workflow, or agent creates a tag
  or a release.

### 3. Pin the release workflow to the exact repo

- [ ] The release workflow builds **only** from this exact repo
  (`raunakiitp/CodvianRouter`). A build from a lookalike fork must
  be **impossible, not just unlikely**: the workflow checks out
  this repo by name, the signing identity is pinned to the
  workflow file + tag ref, and the verifier rejects any other
  identity (see "Verification" below).
- [ ] Any rename of the workflow file changes the pinned signing
  identity. The verify paths (`scripts/verify-release.sh`,
  `scripts/verify-image.sh`, `codvian-router release verify`,
  `scripts/install.sh`, `bin/codvian-router.js`) must be updated
  in the **same commit** as the rename, and the new identity
  published in the release notes.
- [ ] Release-tool versions stay pinned to exact versions / commit
  SHAs (see `docs/RELEASE-SIGNING.md`, "Pinned release
  toolchain"). A major bump is a deliberate, reviewed change.

### 4. Verify provenance *change* between releases, not just that a signature exists

- [ ] Before announcing, compare the new release's provenance
  against the **previous release's**: builder (workflow + ref),
  source repo, signing identity, and commit. Any change blocks
  the announce until it is explained in the release notes.
- [ ] A valid signature from a compromised or rotated key is
  worthless — the LiteLLM 1.107.0 images were validly signed with
  the old key. Signature presence alone never passes this check.
- [ ] Fetch each release's attestation and record it:
  `gh attestation verify <archive> --repo raunakiitp/CodvianRouter`
  and `gh attestation verify oci://ghcr.io/raunakiitp/codvian-router:<tag> --repo raunakiitp/CodvianRouter`.
  (`codvian-router release verify` runs these for you when `gh`
  is installed; the cross-release *comparison* is still a manual
  step — the tool verifies each attestation's validity, it does
  not diff two releases. That diff is this checklist item.)
- [ ] A release whose provenance changed without an explanation in
  the notes is treated as compromised until proven otherwise.

### 5. Sign releases and publish SBOM + build provenance from day one

- [ ] Every release ships: 6 platform archives, `checksums.txt`
  (SHA-256 of every archive **and** every SBOM — verified against
  a real GoReleaser v2.20.2 snapshot build, items 261–262),
  `checksums.txt.sigstore.json`, one SPDX-JSON SBOM per archive
  with its own `<archive>.sbom.json.sigstore.json`, SLSA
  build-provenance attestations (files + image), and the signed
  multi-arch Docker image — all under the **same** cosign keyless
  identity (release workflow + exact tag). No long-term signing
  key exists anywhere.
- [ ] Users verify **before** installing or running anything (see
  "Verification" below). The installer and the npm wrapper
  re-check the signature themselves.
- [ ] A snapshot build (`goreleaser release --snapshot`) is
  **unsigned by design** and must never be distributed as a
  release. `release verify` refuses it (missing bundles).

### 6. Audit-log alerting on any change to release workflows

- [ ] GitHub audit logs feed alerting. Any change to
  `.github/workflows/release.yml`, to branch protection, to
  release-related secrets, or to the repo's visibility is an
  alert the maintainer reads — not a log line nobody watches.
- [ ] Any unexpected release-workflow run (a run not triggered by
  the maintainer's tag push) is investigated before anything it
  published is announced.

### 7. Repo visibility (the bulk-push rule)

- [ ] The repo is **private** while nothing is pushed. Before the
  bulk push, **Raunak himself flips it to public** — private-repo
  CI minutes are already exhausted, so the push needs the repo
  public — and **only after he confirms it is public** does the
  push happen (via his logged-in browser, PR + merge).
- [ ] Emergency exception (overnight only, and only if a push is
  genuinely *necessary*, not convenient): the assistant may flip
  the repo public via the logged-in browser, push properly, then
  flip it back to **private** immediately. If GitHub asks for a
  sudo-mode verification code during the flip, stop — codes only
  come from Raunak dictating them.
- [ ] No push of any kind happens while the repo is private
  (nothing would be visible to CI), and no release is cut while
  the repo is private.

## Release-day procedure

Run these in order. Stop at the first failure — a failed step is a
failed release, not a warning to work around.

### Pre-push

- [ ] `goreleaser check` green (validates `.goreleaser.yml`).
  Note: it fails in the push-rule worktree itself — the local git
  repo has **no remote by design**, and the check needs one
  ("scm releases: no remote configured to list refs from"). Run
  it in CI (where checkout provides the remote) or in a scratch
  clone with a remote; the config is what is being validated, not
  the worktree. Recorded as a known gap, not a config defect.
- [ ] The tree cross-compiles for **all six** release platforms,
  including Windows. (Known gap at items 261–262: HEAD does not
  cross-compile to Windows — `internal/tray/supervisor.go` uses
  `syscall.Kill`/`Setpgid` and `internal/provider/tfo.go` uses a
  Unix-only socket option, with no Windows build guard. A real
  `goreleaser release` fails at the Windows build step. This must
  be fixed before the first tag; it is not this slice's code to
  fix.)
- [ ] `go build ./...`, `go vet`, `gofmt -l` clean; the full test
  suite green; `go test -race` clean on the touched packages.
- [ ] README.md + README.v2.md updated in the same push as the
  code (features, numbers, status match what actually shipped).

### Tag + publish

- [ ] The maintainer (only the maintainer) pushes the tag
  `vX.Y.Z`. Pushing the tag *is* the release decision.
- [ ] The release workflow runs: build → SBOMs → cosign keyless
  signatures → the in-job `release verify` step checks the
  just-built `dist/` (all six archives incl. both Windows zips +
  SBOMs + Sigstore bundles; a failure fails the job before any
  provenance attestation is recorded) → GitHub release + GHCR
  image → provenance attestations → the workflow's own `verify`
  job re-verifies the published artifacts as a user would. A
  green build with a red verify job is a **failed release**.
  Honest limit: the in-job step runs after GoReleaser publishes
  (one step builds + publishes), so it is a tripwire, not a
  pre-publish gate — and it never executes a binary, so Windows
  execution stays unproven until a CI run runs the exe. Items
  307–308 proved the tripwire is the final design with the pinned
  OSS GoReleaser v2.20.2 (a true pre-publish gate needs Pro-only
  `--prepare`/`publish`; the draft / manual re-publish workarounds
  were evaluated and rejected — see docs/RELEASE-SIGNING.md,
  "How the pipeline runs", step 3). A failed release is pulled
  by deleting the GitHub release and the pushed image by hand —
  the workflow cannot un-publish it.

### Verify before announcing (nothing is announced first)

- [ ] Run the built-in verifier against the published release:
  ```sh
  codvian-router release verify v0.1.0
  ```
  It downloads the release for this machine and checks, in
  order: cosign keyless signature over `checksums.txt` → SHA-256
  of the archive → cosign signature + JSON validity of the SBOM →
  build-provenance attestation (when `gh` is installed) → cosign
  signature + attestation of the published Docker image. It
  exits 0 and prints `VERIFIED` only when every applicable check
  passed. **On any failure, do not announce** — pull the release
  and investigate.
- [ ] Or use the shell equivalents: `sh scripts/verify-release.sh
  v0.1.0` (blobs) and `sh scripts/verify-image.sh v0.1.0`
  (image), both before install / `docker run`.
- [ ] Verify a local `dist/` the same way before it is ever
  published: `codvian-router release verify v0.1.0 --dir dist/`
  (expects all six platform archives; attestations are skipped
  in local mode — they live in GitHub's attestation store).
- [ ] **Independent check of published packages:** a second
  person — or at minimum a second, clean machine that has never
  seen the build — downloads the published release + image and
  runs the same verification (`release verify` / the two shell
  scripts) from scratch, and confirms the result in writing
  before the announce. The workflow's own `verify` job does not
  count as the independent check: it runs on the same runner
  that published. The independent check also re-runs rule 4
  (provenance *change* vs the previous release).

### Announce

- [ ] Only after the verifier passes AND the independent check
  passes AND the provenance comparison (rule 4) is clean: publish
  the release notes, including the new signing identity if the
  workflow moved, and the verification commands users should run.

## Verification — what the tool checks, and what it refuses to do

`codvian-router release verify <tag>` (engine:
`internal/release`, front end: `cmd/codvian-router/release.go`)
checks, in order:

1. **Checksums** — `checksums.txt` parses and every listed file
   hashes correctly. Remote mode checks the downloaded subset
   (the `--ignore-missing` rule); local mode requires the whole
   manifest.
2. **Archives** — every expected archive is present on disk AND
   listed in `checksums.txt`. An archive present but unmanifested
   is unauthenticated and fails.
3. **SBOM** — every archive has a `<archive>.sbom.json`, valid
   JSON, SPDX-JSON (what syft produces) or CycloneDX (what the
   local generator produces). A missing or malformed SBOM fails.
4. **Bundle (offline structural check)** — every
   `<file>.sigstore.json` exists, is a DSSE bundle, its
   certificate's identity (URI SAN) is exactly the release
   workflow + this tag, its OIDC issuer is GitHub Actions, and
   its in-toto payload digest is the artifact's own SHA-256.
   This is the lookalike-repo tripwire: a bundle from
   `Berri-AI/litellm` fails here, before any network call.
5. **Signature (cosign)** — `cosign verify-blob` for
   `checksums.txt` and every SBOM (keyless identity pinned to
   workflow + tag; `--key` keypair mode is for local test
   fixtures only), and `cosign verify` for the published image.
6. **Provenance** — `gh attestation verify` for the archive and
   the image.

Fail-closed rules (the LiteLLM lesson — a check that passes by
skipping is worse than no check):

- A missing bundle, SBOM, or archive FAILS. There is no
  "optional" artifact.
- If `cosign` is not available, the signature checks FAIL — they
  are never silently skipped. (The offline structural check is a
  pre-check, not a substitute: it does not verify the signature
  cryptographically, the Rekor log entry, or the Fulcio chain,
  and it deliberately does not check certificate expiry — Fulcio
  certificates live ~10 minutes, so a genuine release's
  certificate is expired by verification time; cosign accepts it
  because the Rekor entry proves it was valid when logged.)
- Attestation is the one skippable step, matching
  `scripts/verify-release.sh`: when `gh` is absent it is reported
  SKIP (loudly), never OK — unless `--require-attestation` is
  set, in which case its absence FAILS.

Testing-only overrides (never normal use):
`--download-base-url` / `CODVIAN_ROUTER_RELEASE_DOWNLOAD_BASE_URL`
(download release files from a local server instead of GitHub —
mirrors the npm wrapper's override; whatever it points at is
still signature- + checksum-verified exactly as a GitHub download
would be). `--key` keypair mode is for local test fixtures only.

## If something goes wrong

- **Signature fails, checksums pass:** the files are intact but
  did not come from the release workflow. Do not install. This is
  the LiteLLM scenario — treat it as a compromise until proven
  otherwise.
- **Checksums fail, signature passes:** the manifest is genuine
  but the download is corrupt or was swapped after signing.
  Re-download; if it persists, report it.
- **A signing identity is abused:** keyless certificates are
  short-lived (minutes) and bound to one workflow run, so there
  is no long-term key to revoke. The response is: revoke the
  compromised workflow run's permissions, fix the workflow, cut a
  new release, and publish a security advisory naming the
  affected tags. Rekor's public log is the audit trail of what
  was signed.
- **Image signature fails:** the image is intact as bytes but did
  not come from the release workflow — most likely a tag
  re-pointed at other bytes, or a different registry. Do not run
  it. This is the LiteLLM scenario for containers.
- **The release workflow file moves or is renamed:** the pinned
  identity changes with it (rule 3).

## Honest limitations (recorded by the 261–262 slice)

- **No release exists yet.** The keyless/Sigstore live path is
  unproven until the first tag: no OIDC identity can be minted
  outside CI, so no real keyless bundle has ever been verified
  by this tool. The tool is proven against crafted bundles (the
  offline identity/digest checks, including a lookalike-repo
  bundle that must fail), keypair fixtures, and a real GoReleaser
  snapshot layout — not against a live Sigstore signature. The
  workflow's `verify` job is the tripwire at first release.
  (Items 307–308: the in-job tripwire is the final design — a
  pre-publish gate needs GoReleaser Pro; see
  docs/RELEASE-SIGNING.md, "How the pipeline runs", step 3.)
- **The Windows cross-compile is broken at HEAD** (see
  Pre-push). A real release fails at the Windows build step
  until it is fixed.
- **`goreleaser check` cannot run green in the push-rule
  worktree** (no remote by design). CI runs it where the remote
  exists.
- **Real-cosign sign/verify end-to-end is unproven in this VM:**
  cosign v3.1.4's `sign-blob` needs a signing config for offline
  use (`--tlog-upload` is deprecated and rejected) and its
  timestamp service (`timestamp.sigstore.dev`) times out from
  this VM. The engine's exact cosign/gh invocations are proven
  by stub tests (`TestExecToolVerifierArgv`); the live
  keypair sign → verify round trip is not. The shell scripts'
  tamper proofs (slices 209–210, 221–222) are the existing live
  evidence for cosign keypair sign/verify.
- **Provenance-*change* comparison is a manual checklist step**
  (rule 4). The tool verifies each attestation's validity; it
  does not diff two releases.
- **The npm wrapper / `install.sh` without cosign install
  checksum-only** (loud warning, unless
  `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses). That is a
  deliberate usability trade-off, documented in
  `docs/RELEASE-SIGNING.md` — not a claim that checksum-only is
  sufficient. Run `release verify` first.
- **Unrelated pre-existing gap found while wiring the command:**
  the global arg parser rejects every subcommand-specific flag,
  so `catalog audit --out-dir` and `eval-feedback
  --feedback-db` are both rejected as "unknown flag" today.
  This slice registered its own flags globally (the provider
  add-catalog precedent) so `release verify` works; fixing those
  two is a follow-up for their owners.

## Files

| File | Role |
|---|---|
| `docs/RELEASE-CHECKLIST.md` (this file) | The release-day checklist: the LiteLLM rules, the procedure, the fail-closed verification rules |
| `docs/RELEASE-SIGNING.md` | The signing design: why cosign keyless, what is signed, the manual verify commands, the pinned toolchain |
| `internal/release/verify.go` | The verification engine: layout expectations, SBOM shape, Sigstore bundle structural checks, cosign/gh orchestration |
| `internal/release/verify_test.go` | Engine tests incl. tampered archive/SBOM, missing bundle/SBOM, lookalike identity, unsigned snapshot, real-snapshot-layout, exec argv contract, real-cosign integration (skipped where cosign cannot sign) |
| `cmd/codvian-router/release.go` | `codvian-router release verify` — flag parsing, GitHub download, report printing |
| `cmd/codvian-router/release_test.go` | Command tests incl. local/remote (httptest) paths, tampered download, missing release, cosign rejection |
| `cmd/codvian-router/release_verify.go` | `codvian-router verify-release [DIR]` — offline checksum check only (the pre-existing command; unchanged by this slice) |
| `scripts/verify-release.sh` | The user-facing shell verify path (blobs) |
| `scripts/verify-image.sh` | The user-facing shell verify path (image) |
| `.goreleaser.yml` | SBOM + cosign keyless signing config + signed multi-arch Docker image |
| `.github/workflows/release.yml` | Tag-triggered release + provenance + self-verify (release + image) |
