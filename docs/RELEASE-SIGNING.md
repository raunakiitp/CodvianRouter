# Release signing — Codvian Router

> **Status:** the pipeline is configured and verified locally (items
> 209–210 for the release blobs, items 221–222 for the Docker image),
> but **no release has been published yet**. The first release
> happens only when the router is 10x ahead of OmniRoute, on the
> maintainer's call. Until a tag is pushed, nothing here can be
> exercised against a live release — the tamper proofs below used
> local test files and a local test image, not a GitHub release. Items 223–224 (2026-11-18) closed
> the two honest limitations from 209–210: the npm wrapper now
> verifies the signature + checksums before installing (same rule as
> `install.sh`), and every release-tool version in the workflow is
> pinned to an exact version / commit SHA — see "Pinned release
> toolchain" below. The Docker image signing (signed GHCR image +
> `verify-image.sh`) landed in the workflow from a concurrent slice;
> it is described here only where it touches verification, not owned
> by this slice.

## Pinned release toolchain (items 223–224)

No `latest`, no floating major tag, no floating runner image anywhere
in `.github/workflows/release.yml`. Every `uses:` entry is pinned to
the full 40-char commit SHA of a tagged release (the SHA is the
checksum for the action's code — GitHub checks out exactly that
commit), with the version in a comment next to it. The majors are the
ones this workflow was built and reviewed against; a major bump is a
deliberate, reviewed change, not a side effect of a re-run.

| Tool | Pinned to | Commit SHA |
|---|---|---|
| `actions/checkout` | v4.4.0 | `11d5960a326750d5838078e36cf38b85af677262` |
| `actions/setup-go` | v5.6.0 | `40f1582b2485089dde7abd97c1529aa768e1baff` |
| Go (via setup-go) | 1.27.4 (exact patch) | — |
| `sigstore/cosign-installer` | v3.10.1 | `7e8b541eb2e61bf99390e1afd4be13a184e9ebc5` |
| `anchore/sbom-action/download-syft` | v0.24.3 | `66cbf4bc1f1c0d2edc94016e65bc221b6bb0ad6c` |
| `goreleaser/goreleaser-action` | v6.4.0 | `e435ccd777264be153ace6237001ef4d979d3a7a` |
| GoReleaser (via the action) | v2.20.2 (exact tag) | `1bf59683fabedd7b446d089ec31b1e343b472ce6` |
| `actions/attest-build-provenance` | v2.4.0 | `e8998f949152b193b063cb0ec769d69d929409be` |
| `docker/login-action` | v3.7.0 | `c94ce9fb468520275223c153574b00df6fe4bcc9` |
| Runner | `ubuntu-24.04` (both jobs) | — |

All SHAs were verified against the upstream repos twice
(`api.github.com/repos/<owner>/<repo>/commits/<tag>` and
`git ls-remote ... refs/tags/<tag>`; annotated tags dereferenced
with `^{}` to the commit). To bump a pin, resolve the new tag's
commit the same way and update the SHA + the version comment
together; the how-to lives in the workflow header.

Residual, stated honestly: the actions download their tool binaries
(GoReleaser, cosign, syft) over HTTPS from GitHub releases, and none
of those actions documents an independent checksum check of the
download — the pin covers the action code and the exact tool
version, verified in source for `goreleaser-action` v6.4.0
(`src/goreleaser.ts`, `src/github.ts` contain no checksum/sha256
check). The release job's own `verify` job is the tripwire: it
re-verifies the published artifacts exactly as a user would, and a
green build with a red verify is a failed release.

## Why: the supply-chain lesson

Two 2026 incidents are the reason this pipeline exists from the first
release instead of being bolted on later (research-log entry 7):

- **LiteLLM, Oct 2026 (v1.107.0):** a credential-stealing supply-chain
  compromise shipped through PyPI wheels and public container images.
  Users who installed the published artifact got the attacker's code.
  A checksum file alone would not have stopped it — the attacker
  controlled the published files, so they could publish matching
  checksums too.
- **Bifrost, Sep 2026 (CVE-2026-90898, CVSS 9.8):** an admin API
  unauthenticated by default plus a Docker image binding all
  interfaces. Different bug, same lesson: defaults decide security,
  and "signed, pinned releases from day one" is the fix the incident
  write-ups point at.

The rule that follows: **a release is not trustworthy because it is on
GitHub — it is trustworthy because a signature proves the project's own
CI built it, and the user checked that signature before installing.**

## The choice: Sigstore cosign, keyless

| Option | How it works | Verdict |
|---|---|---|
| **Cosign keyless (chosen)** | GitHub Actions mints a short-lived OIDC identity for the release workflow; Sigstore issues a certificate bound to `https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/vX.Y.Z` and logs the signature in the public Rekor transparency log. No long-term key exists anywhere. | **Chosen.** Nothing to steal, nothing to rotate, identity is pinned to the exact workflow + tag, and a rogue signature is publicly visible in Rekor. Native GoReleaser support. |
| Minisign | One long-term Ed25519 keypair; the private key lives in a GitHub Secret, the public key is published in the repo/docs. | Rejected. The private key becomes the single stealable secret — exactly the LiteLLM failure mode (stolen release credentials) — and rotation is manual. No transparency log, so a leaked key can sign quietly. |
| GPG | Long-term key, same key-management problem as minisign, plus keyserver/Web-of-Trust friction for users. | Rejected for the same reason. |
| Checksums alone | `checksums.txt` with no signature. | Rejected as the *only* control: it detects corruption, not substitution. (It is still published — it is the second layer, not the first.) |

## What is signed

Every release (tag `v*.*.*`) publishes, for each of the 6 platform
archives (linux/darwin/windows × amd64/arm64):

| File | What it is | How it is authenticated |
|---|---|---|
| `codvian-router_<ver>_<os>_<arch>.tar.gz` (`.zip` on Windows) | The release archive | Its SHA-256 is in `checksums.txt` |
| `checksums.txt` | SHA-256 of every archive | Cosign keyless signature `checksums.txt.sigstore.json` |
| `<archive>.sbom.json` | SPDX-JSON SBOM for that archive (syft) | Its own cosign keyless signature `<archive>.sbom.json.sigstore.json` |
| (attestation, not a file) | SLSA build provenance: in-toto attestation stating this workflow built these files from this commit | `gh attestation verify <artifact> --repo raunakiitp/CodvianRouter` |
| `ghcr.io/raunakiitp/codvian-router:<tag>` (+ `:latest`) | Multi-arch Docker image (linux/amd64 + linux/arm64), built by GoReleaser from `Dockerfile.release` out of the pre-built binaries — the image carries the exact binary that ships in the archives | Cosign keyless signature over the manifest digest (`docker_signs` in `.goreleaser.yml`), **the same workflow + tag identity as the blobs**; SLSA provenance attestation pushed to the registry alongside the image |

Signing only `checksums.txt` would leave the SBOMs unauthenticated,
so each SBOM carries its own signature (second `signs` entry in
`.goreleaser.yml`). The Docker image is signed the same way as the
blobs — one identity for the whole release, whether the user installs
the binary or pulls the image (items 221–222; see "Verifying the
Docker image" below).

## How the pipeline runs

1. The maintainer pushes a tag `vX.Y.Z` — pushing the tag *is* the
   release decision; no workflow, cron, or slice creates tags.
2. `.github/workflows/release.yml` runs: GoReleaser builds, archives,
   generates SBOMs (syft), signs `checksums.txt` + each SBOM with
   cosign keyless, and publishes the GitHub release. The same run
   builds the multi-arch Docker image from `Dockerfile.release`,
   pushes `ghcr.io/raunakiitp/codvian-router:<tag>` and `:latest`
   (the workflow logs in to GHCR first — `packages: write`), and
   signs the image with the same cosign keyless identity
   (`docker_signs`, pinned to the manifest digest).
3. Right after GoReleaser builds, the same job runs the router's
   own verifier against the `dist/` it just produced
   (`release verify <tag> --dir dist --skip-image
   --skip-attestation`, items 289-290): all six archives
   (including both Windows zips) + SBOMs + Sigstore bundles,
   with cosign (installed earlier in the job; the tool fails
   closed when cosign is missing). A failure fails the job, which
   skips the attestation steps below and blocks the `verify` job
   — a red release is never recorded as verified. Two honest
   limits: GoReleaser builds AND publishes in one step, so this
   runs after the release is already published — it is a
   tripwire, not a pre-publish gate, and it cannot un-publish;
   and it never executes a binary, so Windows-binary execution
   stays unproven until a CI run runs the exe. Items 307-308
   (2026-11-18) proved the tripwire is the final design, not a
   placeholder: a true pre-publish gate (build-only → verify →
   publish the same `dist/`) needs GoReleaser's `release
   --prepare` + `goreleaser publish`, which are GoReleaser
   Pro-only — the pinned OSS v2.20.2 binary has no `publish`
   command and rejects `--prepare` as an unknown flag (both
   proven locally against that exact binary, GitVersion 2.20.2,
   commit `1bf59683fabedd7b446d089ec31b1e343b472ce6`). A second
   `release` run cannot publish the verified `dist/` instead:
   `build` is not a skippable step and a run without `--clean`
   refuses an existing `dist/`, so it would wipe and rebuild —
   the published artifacts would be a rebuild, not the verified
   bits, re-signed with fresh per-run keyless certificates.
   Measured locally 2026-11-18: two consecutive snapshot builds
   of the same committed tree produced different SHA-256 for all
   12 archives + SBOMs, so a rebuild's bytes are provably not
   the verified bytes.
   Workarounds evaluated and rejected: (a) a draft release
   (`--draft`) gates only the GitHub blobs — the Docker image is
   still built, pushed, signed and `:latest`-tagged in the same
   run before any verify, and a failed verify cannot un-push it,
   plus undrafting needs the unpinned `gh` CLI and a cleanup
   path for the pushed image; (b) a manual re-publish (`gh
   release create` + upload + `docker push` + `cosign sign`)
   re-implements GoReleaser's publish pipe by hand (changelog
   generation, image signing/SBOM, asset upload) with unpinned
   tooling and no local proof for the registry half — a missed
   asset would ship an incomplete release, caught only by the
   same post-publish verify job. Signing still happens before
   publishing in a `--skip=publish` run (`sign` and `publish`
   are independent skips; checksums + signing are the integrity
   phase that precedes the publishing phase), so a build-only
   `dist/` in CI would be signed and verifiable — the blocker is
   purely that OSS GoReleaser has no publish-the-built-dist
   step. Follow-up, not this slice: a GoReleaser Pro licence
   would unlock `--prepare`/`publish` — the maintainer's call.
   The local dry-run of this exact command against the snapshot
   `dist/` is
   recorded in `docs/release-verify-2026-11-18-snapshot.json`
   (checksums 12/12, archives 6/6, SBOM 6/6; bundles/signatures
   fail as expected for an unsigned snapshot).
4. The same workflow attests build provenance for every artifact
   (`actions/attest-build-provenance`) — files by `subject-path`,
   and the image by `subject-name` + the manifest digest from
   `dist/artifacts.json`, pushed to the registry with
   `push-to-registry: true`.
5. A second job downloads the published release exactly as a user
   would and runs `scripts/verify-release.sh`, then verifies the
   published image exactly as a user would
   (`scripts/verify-image.sh` + `docker pull` + `docker run ...
   help`). A green build with a red verify means the pipeline
   published something users cannot verify — treat it as a failed
   release. (It runs only when the release job passed — which
   already includes the in-job `release verify` step in 3 above,
   so a dist/ that fails verification never reaches this job.)

Local snapshot builds (`goreleaser release --snapshot`) produce
**unsigned** artifacts — no OIDC identity exists outside CI. Never
distribute snapshot output as a release.

## Verifying a release — do this FIRST, before installing

### The one command (recommended)

```sh
sh scripts/verify-release.sh v0.1.0
```

It downloads the release for your machine and checks, in order:
signature over `checksums.txt` → SHA-256 of the archive → signature +
JSON validity of the SBOM → build-provenance attestation (when `gh`
is installed). It exits 0 and prints `VERIFIED` only when every
applicable check passed. **On any failure, do not install the files**
— report it at <https://github.com/raunakiitp/CodvianRouter/issues>.

### The manual commands (what the script does)

```sh
# 1. Download the release files for your platform.
TAG=v0.1.0
BASE=https://github.com/raunakiitp/CodvianRouter/releases/download/$TAG
ARCHIVE=codvian-router_0.1.0_linux_amd64.tar.gz   # match your OS/arch
curl -fsSLO $BASE/$ARCHIVE
curl -fsSLO $BASE/checksums.txt
curl -fsSLO $BASE/checksums.txt.sigstore.json
curl -fsSLO $BASE/$ARCHIVE.sbom.json
curl -fsSLO $BASE/$ARCHIVE.sbom.json.sigstore.json

# 2. Verify the signature over checksums.txt (keyless, identity pinned
#    to the release workflow + this exact tag). This is the step that
#    matters: it proves the manifest came from the project.
cosign verify-blob \
  --certificate-identity "https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --bundle checksums.txt.sigstore.json checksums.txt

# 3. Verify the archive against the (now trusted) manifest.
sha256sum --check --ignore-missing checksums.txt

# 4. Verify the SBOM's signature the same way, then check it parses.
cosign verify-blob \
  --certificate-identity "https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  --bundle $ARCHIVE.sbom.json.sigstore.json $ARCHIVE.sbom.json
python3 -m json.tool $ARCHIVE.sbom.json > /dev/null

# 5. Verify the build provenance (optional but recommended).
gh attestation verify $ARCHIVE --repo raunakiitp/CodvianRouter
```

Only after all of that passes, install:

```sh
sh scripts/install.sh $TAG        # re-checks the signature itself
# or: tar -xzf $ARCHIVE && sudo cp codvian-router /usr/local/bin/
```

### Installing via npm — the wrapper verifies too (items 223–224)

`bin/codvian-router.js` (the npm launcher) now verifies before it
installs, with the same rule as `install.sh`: on first run it
downloads the archive + `checksums.txt`, verifies the Sigstore
keyless signature over `checksums.txt` when `cosign` is installed
(identity pinned to the release workflow + this exact tag) and
**refuses on any signature failure**, then verifies the archive's
SHA-256 against the manifest and **refuses on mismatch or on a
missing entry**. A failed verification deletes the downloaded archive
and aborts before anything is extracted or executed. The tamper
proof is in `bin/codvian-router.test.js` (16 tests, all green — see
the slice report): a tampered archive and a tampered checksums entry
are both refused by the wrapper, and a tampered archive is refused
by `install.sh` too.

No-cosign behaviour (honest, mirrors `install.sh`): without cosign
the wrapper warns loudly and continues with checksum-only
verification — unless `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` is set,
which refuses instead. Checksum-only proves the download matches
the manifest; it does **not** prove the manifest came from the
project. Install cosign, or verify first with
`scripts/verify-release.sh` (which always requires cosign).

Honest limits of the wrapper path (same as `install.sh`):

- If `checksums.txt` itself cannot be downloaded, the wrapper
  retries the download (3 attempts, backoff) and then **refuses to
  install** — a release whose manifest cannot be checked is never
  installed unverified. A transient GitHub error is absorbed by the
  retries; the signature is checked whenever the manifest downloads.
- A cached binary (`bin/.cache/`) is **re-verified on every run**
  against the SHA-256 sidecar (`<binary>.sha256`) written when it
  was first cached. A tampered binary, or a cache with no sidecar,
  is discarded and re-downloaded — which re-runs the full
  signature + checksum verification. A good cache is never
  re-downloaded: the re-check is local only.
- The wrapper checks the binary's authentication (signed
  `checksums.txt` → SHA-256), not the SBOM signature — auditors
  verify the SBOM with `scripts/verify-release.sh`.
- `CODVIAN_ROUTER_BINARY` bypasses download **and** verification
  entirely: it is the operator's own binary, their responsibility.

Testing-only overrides (used by `bin/codvian-router.test.js`, not
normal use): `CODVIAN_ROUTER_DOWNLOAD_BASE_URL` (download release
files from a local server instead of GitHub) and
`CODVIAN_ROUTER_CACHE_DIR` (cache the binary elsewhere). Whatever
they point at is still signature- + checksum-verified exactly as a
GitHub download would be.

### Offline checksum check (no cosign)

If you already have the binary (e.g. built from source) and a
`checksums.txt` you trust, check integrity without cosign:

```sh
codvian-router verify-release <directory-containing-checksums.txt>
```

This proves the files match the manifest. It does **not** prove the
manifest came from the project — that needs the cosign step above.
Never treat a checksum-only check as a substitute for the signature.

## Verifying the Docker image — do this FIRST, before `docker run`

The published image is `ghcr.io/raunakiitp/codvian-router:<tag>`
(multi-arch: linux/amd64 + linux/arm64; `:latest` moves on every
release, so pin a version tag — or better, the digest — in
production). It is signed with the **same** cosign keyless identity
as the release blobs: the release workflow + the exact tag. A tag is
just a label — anyone who can push to the registry can point it at
any bytes — so verify the signature before the image ever runs. A
tampered image re-pushed under the same tag does **not** verify:
cosign resolves the tag to its digest and checks the signature for
that digest.

### The one command (recommended)

```sh
sh scripts/verify-image.sh v0.1.0
```

It checks, in order: the cosign keyless signature over the image
(identity pinned to the release workflow + tag) → the SLSA
build-provenance attestation (`gh attestation verify oci://...`,
when `gh` is installed). It exits 0 and prints `VERIFIED` only
when every applicable check passed. **On any failure, do not run
the image** — report it at
<https://github.com/raunakiitp/CodvianRouter/issues>.

### The manual commands (what the script does)

```sh
TAG=v0.1.0
IMAGE=ghcr.io/raunakiitp/codvian-router:$TAG

# 1. Verify the signature over the image (keyless, identity pinned
#    to the release workflow + this exact tag). This is the step that
#    matters: it proves the image was published by the project's
#    release workflow for this tag.
cosign verify \
  --certificate-identity "https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  $IMAGE

# 2. Verify the build provenance (optional but recommended).
gh attestation verify oci://$IMAGE --repo raunakiitp/CodvianRouter
```

Only after all of that passes, run it:

```sh
docker pull $IMAGE
docker run --rm \
  -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
  -v codvian-router-config:/home/nonroot/.config/codvian-router \
  $IMAGE init        # first run only; key printed ONCE
docker run -d --network host $IMAGE
```

Pinning the exact bytes instead of the tag is stronger still —
after step 1, `cosign verify` prints the digest, or take it from
`crane digest $IMAGE`, then run `$IMAGE@sha256:<digest>`.

### Local development image

`docker build -t codvian-router:local .` (from `Dockerfile`) builds
an **unsigned** local image from source. That is fine for local
development — it never leaves your machine — but never distribute
it or confuse it with a release: only the GoReleaser-published
`ghcr.io` image carries a signature, and only that image should
ever be pulled from a registry.

## The SBOM

- **Release SBOMs** are SPDX-JSON, one per archive, generated by
  [syft](https://github.com/anchore/syft) inside GoReleaser
  (`sboms: - artifacts: archive` in `.goreleaser.yml`).
- **Local SBOMs** use the dependency-free generator in this repo:
  `sh scripts/release-sbom.sh [VERSION] [OUT]` (CycloneDX 1.6,
  rendered from `go list -m -json all` by `cmd/release-sbom` /
  `internal/release`). Use it to inspect what a release SBOM will
  contain, or as the fallback if syft is unavailable. The two formats
  describe the same module graph; syft's is richer (it also catalogs
  files inside the archives), which is why CI prefers it.
- An SBOM lists dependencies, not safety: it lets *you* (or a scanner
  like Grype/Trivy) check the release against vulnerability databases.
  It is not a claim that the release has no vulnerabilities.

## If something goes wrong

- **Signature fails, checksums pass:** the files are intact but did
  not come from the release workflow. Do not install. This is the
  LiteLLM scenario — treat it as a compromise until proven otherwise.
- **Checksums fail, signature passes:** the manifest is genuine but
  the download is corrupt or was swapped after signing. Re-download;
  if it persists, report it.
- **The release workflow file moves or is renamed:** the pinned
  identity changes with it. Old releases keep verifying against the
  old workflow path (the certificate records it); new releases verify
  against the new path. The verify script and `install.sh` must be
  updated in the same commit as any workflow rename, and the new
  identity published in the release notes.
- **Image signature fails:** the image is intact as bytes (the
  registry served what it has) but did not come from the release
  workflow — most likely a tag re-pointed at other bytes, or a
  different registry. Do not run it. This is the LiteLLM scenario
  for containers — treat it as a compromise until proven otherwise.
  (A tampered image re-pushed under the same tag fails exactly this
  way; the local tamper proof is in the 221–222 slice report.)
- **A signing identity is abused:** keyless certificates are
  short-lived (minutes) and bound to one workflow run, so there is no
  long-term key to revoke. The response is: revoke the compromised
  workflow run's permissions, fix the workflow, cut a new release, and
  publish a security advisory naming the affected tags. Rekor's public
  log is the audit trail of what was signed.

## Limitations (honest)

- **No release exists yet.** The pipeline is configured, the
  verification scripts are proven against local test files (blobs:
  including a tampered artifact — see the 209–210 slice report)
  and a local signed test image (image: untampered verifies,
  tampered-under-the-same-tag / wrong-key rejected — see the
  221–222 slice report), but no real GitHub release has run through
  it. The first release is the first live proof; the workflow's
  `verify` job is the tripwire if it fails — and since items
  289-290 the release job itself fails first, on the in-job
  `release verify` step, if the just-built `dist/` does not
  verify (that step is also unproven until the first tag: its
  exact command is proven only against the unsigned snapshot
  `dist/`, where it fails correctly — see
  `docs/release-verify-2026-11-18-snapshot.json`).
- **The in-job `release verify` is a post-publish tripwire by
  final design, not a pre-publish gate (items 307-308).** A true
  pre-publish gate (build-only → verify → publish the same
  `dist/`) needs GoReleaser Pro (`release --prepare` +
  `goreleaser publish`); the pinned OSS v2.20.2 has neither, a
  second `release` run would rebuild instead of publishing the
  verified bits, and the draft-release / manual re-publish
  workarounds were evaluated and rejected — see "How the
  pipeline runs", step 3, for the proof and the reasons. The
  tripwire still fails the job, skips attestations and blocks
  the `verify` job, but it cannot un-publish a bad release;
  pulling one means deleting the GitHub release and the pushed
  image by hand, per the release checklist. A GoReleaser Pro
  licence (maintainer's call) is the only clean route to a
  pre-publish gate.
- **Cosign verification needs the network** the first time (Sigstore
  trust root + Rekor). A fully offline verify is only the checksum
  step, which is not a substitute for the signature.
- **The Docker image is published + signed from the first release**
  (items 221–222): GoReleaser publishes the multi-arch image to
  GHCR and signs it with the same cosign keyless identity as the
  blobs (`docker_signs`), with its own provenance attestation and a
  `verify` job that checks the published image as a user would.
  What is still unproven until that first release: the keyless
  signing of the image in CI (this VM cannot mint the OIDC identity
  or reach Sigstore's write path), the multi-arch `docker buildx`
  build itself (no Docker daemon on this VM — `Dockerfile.release`
  is validated by `goreleaser check` only), and the
  `dist/artifacts.json` digest lookup the image-attestation step
  depends on (the lookup code is written against GoReleaser's
  documented `extra.Digest` field and fails the release loudly if
  the entry is absent).
- **The published Docker image also carries a `:latest` tag** (this
  slice's decision, items 221–222). `:latest` moves on every
  release and must never be pinned in production — pin a version
  tag, or better the digest (`...@sha256:...`). It is a convenience
  tag for trying the current release, not a stability promise.
- **The npm wrapper downloads the same signed archives**, so it
  inherits the signature chain only if it verifies them — today it
  does not (it relies on `install.sh`'s checksum step). Wiring the
  npm installer to the same verification is a follow-up. *(Closed by
  items 223–224: the wrapper now verifies the signature when cosign
  is present and always verifies the checksum, refusing on failure —
  see "Installing via npm" above.)*
- **`install.sh` without cosign installs checksum-only** (loud
  warning, unless `CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses).
  That is a deliberate usability trade-off, not a claim that
  checksum-only is sufficient — the docs tell users to run
  `verify-release.sh` first. (The npm wrapper has the identical
  trade-off, same override, since items 223–224.)
- **GoReleaser itself is not pinned** (`version: latest` in the
  workflow). A future GoReleaser major could change the `signs` /
  `sboms` schema; the workflow's `verify` job catches a break at
  release time, but pinning to a v2 minor is a reasonable follow-up.
  *(Closed by items 223–224: GoReleaser is pinned to v2.20.2 and
  every action in the workflow to a full commit SHA — see "Pinned
  release toolchain" above. The remaining residual is that the
  actions' own tool-binary downloads carry no independent checksum
  check; the `verify` job is the tripwire.)*

## Files

| File | Role |
|---|---|
| `.goreleaser.yml` (`sboms`, `signs`, `dockers_v2`, `docker_signs`) | SBOM + cosign keyless signing config + signed multi-arch Docker image |
| `Dockerfile.release` | Release image definition for GoReleaser (pre-built binary + inline healthcheck build; `Dockerfile` stays the local source-build image) |
| `.github/workflows/release.yml` | Tag-triggered release + image publish to GHCR + provenance (files + image) + self-verify (release + image) |
| `scripts/verify-release.sh` | The user-facing verify path (signature → checksums → SBOM → attestation) |
| `scripts/verify-image.sh` | The user-facing image verify path (signature → attestation) — run BEFORE `docker run` |
| `scripts/test-image-signing.sh` | Local proof: build + sign + verify a test image, tampered image fails verification |
| `scripts/install.sh` | Installer; verifies the cosign signature when cosign is present |
| `bin/codvian-router.js` | npm launcher; verifies signature (when cosign is present) + checksum before installing, refuses on failure |
| `bin/codvian-router.test.js` | `node:test` suite for the wrapper + installer (local-server tamper proof, 16 tests) |
| `scripts/release-sbom.sh`, `cmd/release-sbom` | Local CycloneDX SBOM generator (syft fallback) |
| `internal/release` | Checksum manifest generate/verify + CycloneDX generation (stdlib only, tested incl. tamper rejection) |
| `cmd/codvian-router/release_verify.go` | `codvian-router verify-release [DIR]` — offline checksum check |
| `cmd/codvian-router/release.go` | `codvian-router release verify <tag> [--dir DIR] [--key PUBKEY] [--skip-image] [--skip-attestation]` — published-release verifier (GitHub download + cosign signature over checksums.txt and SBOMs + SHA-256 + SBOM shape + provenance attestation + Docker image signature; engine in `internal/release`) |
| `docs/slice-209-210-release-signing-report.md` | Gate evidence for this slice (incl. the tamper proof) |
| `docs/slice-221-222-docker-image-signing-report.md` | Gate evidence for items 221–222 (Docker image signing, incl. the local sign/verify + tamper proof) |
| `docs/slice-223-224-npm-verify-pinning-report.md` | Gate evidence for items 223–224 (npm verification + pinning, incl. the tamper proof) |
