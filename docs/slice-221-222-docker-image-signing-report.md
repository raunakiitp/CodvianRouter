# Slice 221–222 — Docker image signing: gate report

Date: 2026-11-18. Go 1.27.4. GoReleaser 2.20.2 (the version pinned in
the workflow). cosign v2.4.3 and v3.1.4 (both exercised — see below).
No tag or release was created. Nothing was pushed.

## What was built

- **Choice: same Sigstore cosign keyless scheme as items 209–210**
  (no second scheme). The image signature is minted by the same
  release workflow, bound to the same workflow + tag identity, and
  logged in the same Rekor log as the blob signatures. Minisign/GPG
  were already rejected for the blobs (long-term key = the LiteLLM
  failure mode) and nothing about the image changes that.
- **`.goreleaser.yml`**: `dockers_v2` (since v2.12 — the current
  recommended way; the old `dockers` + `docker_manifests` pair is
  deprecated and will be removed in GoReleaser v3) publishes one
  multi-arch manifest (linux/amd64 + linux/arm64) per tag to
  `ghcr.io/raunakiitp/codvian-router:<tag>` and `:latest`, built
  from the new `Dockerfile.release` out of the pre-built binaries —
  the image carries the exact binary that ships in the release
  archives. `docker_signs` signs it keyless (`cosign sign
  ${artifact}@${digest} --yes`, no `--key` flag → ambient GitHub
  OIDC). GoReleaser attaches an SBOM to the image by default (syft
  + cosign, both installed by the workflow).
- **`Dockerfile.release`** (new): the release image definition.
  GoReleaser's dockers_v2 context contains only pre-built binaries
  (no source), so `Dockerfile`'s multi-stage `go build` cannot work
  there — this file copies `$TARGETPLATFORM/codvian-router` and
  builds only the tiny healthcheck helper itself (inline source,
  byte-identical to `Dockerfile`'s — verified by diff in the gate).
  Runtime shape is identical to `Dockerfile`: distroless nonroot,
  CA certs, same ENTRYPOINT/CMD/EXPOSE/HEALTHCHECK/USER.
- **`.github/workflows/release.yml`**: GHCR login
  (`docker/login-action` v3.7.0, SHA-pinned per the 223–224 policy)
  + `packages: write` (new permission — pushing to GHCR needs it);
  image provenance attestation (`actions/attest-build-provenance`,
  `subject-name` + the manifest digest read from
  `dist/artifacts.json` `extra.Digest`, `push-to-registry: true`);
  the `verify` job also verifies the published image
  (`scripts/verify-image.sh` + `docker pull` + `docker run --rm
  <image> help`, retried 6× — `help` exits 0 by design).
- **`scripts/verify-image.sh`** (new): the user-facing verify path
  for the image — cosign keyless verify with the workflow + tag
  identity pinned, then `gh attestation verify oci://...` when `gh`
  is present. `--image REF` (other tag / digest pin), `--key PUBKEY`
  keypair mode for the pipeline's own self-test
  (`--insecure-ignore-tlog` in keypair mode only — the keyless path
  never passes it), `--allow-insecure-registry` only for loopback
  refs.
- **`scripts/test-image-signing.sh`** (new): the local proof (see
  below). Self-contained: installs crane + the go-containerregistry
  test registry via `go install` (pinned v0.21.3) if they are not on
  PATH; takes `COSIGN_BIN` (default `cosign`).
- **Docs**: `docs/RELEASE-SIGNING.md` (signed-image table row,
  pipeline steps, "Verifying the Docker image" section with the
  exact `cosign verify` command, limitations corrected, Files
  table); `docs/INSTALL.md` §3 (verify-first flow, digest pinning,
  `:latest` warning); README.md + README.v2.md status line +
  install-table Docker row.
- **Dashboard**: no new UI (releases are not served by the gateway)
  — the README note + docs are the status note, per the task.

## Real bug found by this slice's validation (not in the task)

The 209–210 `.goreleaser.yml` **failed `goreleaser check`**: both
`signs` entries defaulted to id `default` ("found 2 signs with the
ID 'default'"). The release as configured could never have run —
GoReleaser refuses the config. Fixed in-slice: both `signs` entries
now carry explicit ids (`checksum`, `sbom`), `docker_signs` carries
`docker-image`, and the pre-existing deprecated `archives.format`
was renamed to `formats` (deprecated since GoReleaser v2.6). The
check is now fully green on the real GoReleaser v2.20.2 (validated
in a scratch clone with a remote, because the worktree's local git
repo has no remote and `goreleaser check` refuses that — see
Limitations). The 209–210 slice had only YAML-parsed the file, so
this never surfaced.

## Gate (measured by this run)

- `go build ./...` — OK; `go vet ./...` — clean. (No Go files were
  touched by this slice.)
- `goreleaser check` — **green, exit 0, "1 configuration file(s)
  validated"** (GoReleaser 2.20.2, the pinned workflow version).
- YAML: `.goreleaser.yml` + `.github/workflows/release.yml` parse
  OK (PyYAML). `sh -n` clean on `verify-image.sh`,
  `test-image-signing.sh`.
- `go test -count=1 -v ./...` — **exit 0: 1,998 PASS events
  (1,585 top-level + 413 subtests), 0 FAIL, 39 packages ok**
  (log `$HOME/.tmp/fulltest-221-222-verbose.log`). The suite was
  red mid-gate on the concurrent 229–230 slice's in-flight
  token-count changes (10 tests in `internal/router` /
  `internal/server`, all expecting the old message-text-only token
  counts — e.g. `TestRequestBudgetPreSend402`, `TestMeStatusAfterTraffic`,
  `TestReconcileUsageDivergence` — plus a build break they caused
  and fixed themselves: `CountPromptFull` called with a
  `*provider.ChatRequest` where a value was expected). The suite
  went green on re-run after they landed their fixes; no file of
  this slice was involved at any point.
- Healthcheck source in `Dockerfile.release` is byte-identical to
  `Dockerfile`'s (diff clean); runtime directives match
  (ENTRYPOINT/CMD/EXPOSE/HEALTHCHECK/USER).

## Tamper proof (real cosign, real image, local registry)

Fixture: a real image assembled the way `Dockerfile.release`
describes — real distroless base (`gcr.io/distroless/static-
debian12:nonroot`, pulled with crane), the router binary built
with the Dockerfile's exact flags (`CGO_ENABLED=0`, `-trimpath`,
`-s -w`, linux/amd64), the healthcheck helper extracted from
`Dockerfile`'s inline source, CA certs — pushed to a throwaway
local registry (`localhost:5000/codvian-router:test`). Signatures
made with a throwaway local cosign keypair (test-only password,
deleted with the work directory). Script:
`scripts/test-image-signing.sh`. Full logs:
`$HOME/.tmp/image-signing-proof-v2.log` (v2.4.3),
`$HOME/.tmp/image-signing-proof-v3.log` (v3.1.4).

- **v2.4.3 — 17/17 checks passed.** Untampered verifies
  (`verify-image.sh --key` exit 0 + direct `cosign verify` exit 0);
  tampered image (one string appended to the binary) pushed under
  the SAME tag → new digest
  (`sha256:46c3acd5…` → `sha256:9d40915c…`), verification REJECTED
  by both the script and cosign; the ORIGINAL digest still verifies
  after the tamper (signature is digest-bound, not tag-bound); a
  WRONG key fails.
- **v3.1.4 — 18/18 checks passed** (same 17, plus the signing-config
  step). Note: cosign v3 **removed `--tlog-upload=false`** — it
  errors out and demands a signing config with no transparency-log
  service. The script builds one from Sigstore's public signing
  config with `rekorTlogUrls` + `tsaUrls` deleted (python3, no jq
  needed). `cosign verify --key --insecure-ignore-tlog` still works
  in v3, so `verify-image.sh --key` needed no change.
- One script bug caught by the first run (fixed in-slice, not a
  product bug): the image-config assertion expected `20128/tcp`
  in `ExposedPorts`, but crane records the port as `20128`
  (Docker normalises it to `20128/tcp`); the check now accepts
  either, and the unconditional PASS line after it is now a proper
  if/else.

## Limitations (honest)

- **No live release exists.** The keyless path (GitHub OIDC
  identity, Rekor entry, `gh attestation verify`) is proven only by
  construction — the documented GoReleaser/cosign flags, a green
  `goreleaser check` on the pinned GoReleaser version, and the
  workflow's own `verify` job. The first real tag is the first live
  proof. The same holds for the multi-arch `docker buildx` build
  itself: this VM has no Docker daemon, so `Dockerfile.release`
  has never been executed — only validated by `goreleaser check`
  and by the crane-assembled equivalent image in the tamper proof.
- **The `dist/artifacts.json` digest lookup is unproven until CI
  runs.** The image-attestation step reads the Docker Image
  entry's `extra.Digest` from GoReleaser's documented
  `artifacts.json` schema; if the entry is absent the step fails
  the release loudly (exit 1) instead of attesting the wrong
  digest — a failed release, not a silent gap.
- **`dockers_v2` is provisional/alpha** (GoReleaser's own marking;
  it becomes `dockers` in v3). If it breaks in a future GoReleaser
  release, the fallback is the deprecated `dockers` +
  `docker_manifests` pair with the same `Dockerfile.release`.
- **`goreleaser check` needs a git remote.** It fails in the
  worktree ("no remote configured to list refs from") because the
  local repo has no remote by design (local commits only, never
  push). The green check above was run in a scratch clone with a
  remote; no remote was added to the real repo.
- **Keypair mode is test-only.** It verifies a raw Ed25519
  signature; there is no Rekor entry for a local key. Real releases
  always use the keyless signatures.
- **The local proof image omits the HEALTHCHECK instruction.**
  crane cannot express a healthcheck in image config; the
  instruction lives in `Dockerfile.release` and is verified by
  inspection (same text as `Dockerfile`), not by the proof. The
  healthcheck *binary* is in the proof image and the binary runs.
- **`:latest` is a floating tag.** It moves on every release; pin a
  version tag or the digest in production. Documented in the
  install docs and the signing docs.
- **Concurrent-edit notes (other slices, not this one):** while this
  slice ran, the 223–224 slice pinned the whole release workflow
  (their `docker/login-action` v3.7.0 SHA `c94ce9fb…` independently
  verifies the pin this slice added — confirmed via the GitHub API
  `/git/ref/tags/v3.7.0`, lightweight tag, object type `commit`);
  the 229–230 slice's token-count work broke the build once and the
  suite twice mid-gate (both fixed by them on re-run); the
  223–224 slice also annotated `docs/RELEASE-SIGNING.md`'s
  limitations with this slice's in-progress state (the stale
  "image is not signed" bullet and the duplicate `:latest` note
  were consolidated by this slice into the current-state bullets).
  Nothing was pushed to GitHub.

## Files changed (this slice only)

- New: `Dockerfile.release`, `scripts/verify-image.sh`,
  `scripts/test-image-signing.sh`,
  `docs/slice-221-222-docker-image-signing-report.md`
- Edited: `.goreleaser.yml` (header, `signs` ids, `archives.format`
  → `formats`, `dockers_v2`, `docker_signs`),
  `.github/workflows/release.yml` (header, `packages: write`, GHCR
  login, image-digest step, image attestation, image verify job),
  `.gitignore` (`/dist/`), `docs/RELEASE-SIGNING.md`,
  `docs/INSTALL.md`, `README.md`, `README.v2.md`,
  `CHECKLIST.md` (rows 221–222).
- Shared-file note: `.goreleaser.yml`,
  `.github/workflows/release.yml`, `docs/RELEASE-SIGNING.md`,
  `docs/INSTALL.md`, `README.md`, `README.v2.md`, `CHECKLIST.md`,
  `.gitignore` are shared with concurrent slices; edits were
  minimal and confined to this slice's concerns (one concurrent
  slice's in-flight files were never edited — the one build break
  encountered was fixed by its owner while this slice waited). No
  other slice's files were touched.
- **No commit made by this slice.** The coordinator owns local
  commits; the tree carries many concurrent slices' uncommitted
  work, so committing here would have swept in other slices'
  in-flight changes.
