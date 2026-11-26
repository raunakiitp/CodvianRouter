# CI proof specification — multi-arch Docker image (CHECKLIST 241–242)

Date: 2026-11-18. Status: **no release exists yet.** This document is the
specification for what the first real tag's CI run must prove about the
multi-arch image, written against the actual pipeline files as they stand
today. Every claim below quotes the real workflow (`.github/workflows/
release.yml`) and the real GoReleaser config (`.goreleaser.yml`) and
`Dockerfile.release`. Where the workflow does **not** prove something listed
here, that is recorded as a numbered gap (G1–G9) — not claimed.

## 1. What the pipeline is (as configured today)

Trigger (`.github/workflows/release.yml`, `on.push.tags`): only a pushed
`v*.*.*` tag runs this workflow. No slice, cron, or workflow creates a tag.

GoReleaser config (`.goreleaser.yml`, `dockers_v2`):

```yaml
dockers_v2:
  - images:
      - ghcr.io/raunakiitp/codvian-router
    tags:
      - "{{ .Tag }}"
      - latest
    platforms:
      - linux/amd64
      - linux/arm64
    dockerfile: Dockerfile.release
    ids:
      - codvian-router
```

So one multi-arch manifest per tag is published to
`ghcr.io/raunakiitp/codvian-router:<tag>` and `:latest`, built from
`Dockerfile.release` out of the pre-built binaries — the image carries the
exact binary that ships in the release archives (same `builds` entry,
`CGO_ENABLED=0`, `goos: linux/darwin/windows`, `goarch: amd64/arm64`,
`ldflags: -s -w`).

Signing (`.goreleaser.yml`, `docker_signs`): `cosign sign
${artifact}@${digest} --yes`, no `--key` flag — the ambient GitHub OIDC
identity (the workflow grants `id-token: write`), the same keyless identity
as the release blobs. `output: true` keeps cosign's output in the CI log.

`Dockerfile.release` (what actually gets built per platform):

- Healthcheck builder: `FROM --platform=$BUILDPLATFORM
  golang:1.27.4-bookworm`, cross-compiles the tiny healthcheck helper with
  `CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath
  -ldflags "-s -w"`. The RUN step executes on the build platform and
  cross-compiles — it never executes a foreign-architecture binary.
- Runtime: `FROM gcr.io/distroless/static-debian12:nonroot`; copies the
  CA bundle, `COPY $TARGETPLATFORM/codvian-router
  /usr/local/bin/codvian-router`, and the healthcheck binary;
  `ENV HOME=/home/nonroot`; `VOLUME ["/home/nonroot/.local/share/
  codvian-router"]`; `USER nonroot:nonroot`; `EXPOSE 20128`;
  `HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3
  CMD ["/usr/local/bin/codvian-router-healthcheck"]`;
  `ENTRYPOINT ["/usr/local/bin/codvian-router"]`, `CMD ["serve"]`.

Release job (`release`, `runs-on: ubuntu-24.04`), steps in order:

1. `actions/checkout` v4.4.0, `fetch-depth: 0`.
2. `actions/setup-go` v5.6.0, `go-version: "1.27.4"`, `cache: true`.
3. `goreleaser/goreleaser-action` v6.4.0, `args: check` — validates
   `.goreleaser.yml` before anything is built (added by items 241–242;
   see G2).
4. `sigstore/cosign-installer` v3.10.1.
5. `anchore/sbom-action/download-syft` v0.24.3 (GoReleaser's SBOM step
   shells out to syft).
6. `docker/setup-qemu-action` v3.7.0 — registers the arm64 binfmt
   handlers (added by items 241–242; see G1).
7. `docker/setup-buildx-action` v3.12.0 — creates a buildx builder that
   supports multi-platform builds (added by items 241–242; see G1).
8. `docker/login-action` v3.7.0, `registry: ghcr.io`,
   `username: ${{ github.actor }}`, `password: ${{ secrets.GITHUB_TOKEN }}`
   (`packages: write` permission).
9. `goreleaser/goreleaser-action` v6.4.0, `version: v2.20.2`,
   `args: release --clean`.
10. `Get published image digest` — a python3 step that reads
    `dist/artifacts.json`, finds the entry with `type` in
    (`"Docker Image"`, `"Published Docker Image"`) and `name ==
    "ghcr.io/raunakiitp/codvian-router:<tag>"`, and exports its
    `extra.Digest` as `digest`. Exits 1 if no such entry exists.
11. `Attest build provenance` (`actions/attest-build-provenance` v2.4.0)
    over `dist/*.tar.gz`, `dist/*.zip`, `dist/checksums.txt`,
    `dist/*.sbom.json`.
12. `Attest image build provenance` (same action) with
    `subject-name: ghcr.io/raunakiitp/codvian-router`,
    `subject-digest: ${{ steps.image-digest.outputs.digest }}`,
    `push-to-registry: true`.

Verify job (`verify`, `needs: release`, `runs-on: ubuntu-24.04`):

1. Checkout (for the verify script only), cosign-installer v3.10.1,
   GHCR login (same as above).
2. `Verify the published release as a user would` — `sh
   scripts/verify-release.sh "<tag>"`, retried up to 6 times with 30 s
   sleeps (signature → checksums → SBOM → attestation).
3. `Verify the published Docker image as a user would` — `sh
   scripts/verify-image.sh "<tag>" && docker pull
   "ghcr.io/raunakiitp/codvian-router:<tag>" && docker run --rm
   "ghcr.io/raunakiitp/codvian-router:<tag>" help`, retried the same way.

All `uses:` entries are pinned to full 40-char commit SHAs with the
human-readable version in a comment (items 223–224 pinning policy).
After the items 241–242 additions the workflow has 14 `uses:` entries,
all SHA-pinned.

## 2. What the first real tag's CI run must prove

Each item names the exact evidence. "Proven by the workflow as written"
means a green run already establishes it; "manual after the tag" means
someone must run the command and record the output — the workflow does
not do it (see §3).

- **P1 — Both platform binaries are built.** Evidence: the release job's
  GoReleaser log shows the `codvian-router` build for `linux/amd64` and
  `linux/arm64` (and the other archive platforms). Proven by the workflow
  as written (the `builds` entry lists both arches; a build failure fails
  the job).
- **P2 — The buildx build runs for both platforms.** Evidence: the
  release job's GoReleaser log shows the `dockers_v2` build executing for
  `linux/amd64` and `linux/arm64` (GoReleaser `dockers_v2` uses `docker
  buildx` under the hood). Proven by the workflow as written — but only
  if the log is actually read: nothing in the workflow asserts on the
  per-platform build lines. The reviewer of the first tag's run must
  confirm both platforms appear in the log.
- **P3 — The published manifest lists both architectures.**
  Evidence (manual after the tag until G4 is fixed):

  ```sh
  docker buildx imagetools inspect ghcr.io/raunakiitp/codvian-router:<tag>
  ```

  must list `linux/amd64` and `linux/arm64` with two distinct digests.
  **Not** proven by the workflow as written (see G4).
- **P4 — Each per-arch image carries the right binary and config.**
  Evidence (manual after the tag): for each digest from P3,
  `docker buildx imagetools inspect --raw <image>@<digest>` (or `crane
  config`) must show `architecture` = `amd64` / `arm64`,
  `config.Entrypoint = ["/usr/local/bin/codvian-router"]`,
  `config.Cmd = ["serve"]`, `config.ExposedPorts` containing `20128`,
  `config.User = "nonroot"` (uid 65532), and a `Healthcheck` stanza.
  Not proven by the workflow as written.
- **P5 — The cosign keyless signature verifies for the published image.**
  Evidence: the verify job's `scripts/verify-image.sh "<tag>"` step
  passes — it runs `cosign verify` with the certificate identity pinned
  to `https://github.com/raunakiitp/CodvianRouter/.github/workflows/
  release.yml@refs/tags/<tag>` and issuer
  `https://token.actions.githubusercontent.com`, then (when `gh` is
  present) `gh attestation verify oci://...`. Proven by the workflow as
  written, for the manifest (tag) reference. Per-arch-digest
  verification (`cosign verify <image>@sha256:<amd64-digest>` and
  `@sha256:<arm64-digest>`) is manual after the tag.
- **P6 — The build-provenance attestation verifies.** Evidence: the
  verify job's `verify-image.sh` attestation step passes, or manually:

  ```sh
  gh attestation verify oci://ghcr.io/raunakiitp/codvian-router:<tag> --repo raunakiitp/CodvianRouter
  ```

  Proven by the workflow as written (via the script), provided `gh` is
  installed on the runner — the script skips the attestation check with
  a note when `gh` is absent, so the reviewer must confirm the step
  actually ran it rather than skipping it.
- **P7 — `docker run --rm <image> help` exits 0 on amd64.** Evidence:
  the verify job's `docker run --rm "ghcr.io/raunakiitp/codvian-
  router:<tag>" help` step passes. Proven by the workflow as written —
  on the runner's native architecture only (ubuntu-24.04 is amd64).
- **P8 — `docker run --rm <image> help` exits 0 on arm64.** Evidence
  (manual after the tag): on an arm64 host, or under QEMU on amd64,

  ```sh
  docker run --rm --platform linux/arm64 ghcr.io/raunakiitp/codvian-router:<tag> help
  ```

  must exit 0. **Not** proven by the workflow as written (see G3).
  `help` exiting 0 proves the arm64 binary starts and prints help; it
  does not prove `serve` works — see P9.
- **P9 — The image serves: `serve` starts and `/healthz` answers 200.**
  Evidence (manual after the tag until G9 is fixed):

  ```sh
  docker run -d --name cr-test -p 20128:20128 ghcr.io/raunakiitp/codvian-router:<tag> serve
  curl -sf http://127.0.0.1:20128/healthz   # expect {"status":"ok"}
  docker stop cr-test && docker rm cr-test
  ```

  (Serve binds loopback by default; on a remote host use `--network
  host` or `--listen 0.0.0.0:20128` with a gateway key, per
  `docs/INSTALL.md` §3.) **Not** proven by the workflow as written —
  the verify job never starts the server.
- **P10 — The image SBOM exists.** Evidence (manual after the tag):
  GoReleaser attaches an SBOM to the image by default (syft + cosign,
  both installed by the workflow — see the `.goreleaser.yml` comment
  under `dockers_v2`); confirm it is attached to the published image
  (e.g. `cosign verify-attestation` / the registry's referrers API).
  **Not** proven by the workflow as written (see G5).
- **P11 — The image-digest lookup works.** Evidence: the release job's
  `Get published image digest` step prints `image digest: sha256:…`
  and the image-attestation step succeeds. Proven by the workflow as
  written — the step exits 1 if the `dist/artifacts.json` entry is
  absent, so a failure is loud, not silent (see G6).
- **P12 — `:latest` points at the same manifest as the tag.** Evidence
  (manual after the tag): `docker buildx imagetools inspect
  ghcr.io/raunakiitp/codvian-router:latest` shows the same manifest
  digest as the tag. `:latest` moves on every release and must never be
  pinned in production — pin a version tag or the digest.

## 3. Gaps found in the workflow (checked against the real file)

- **G1 — FIXED in this slice (items 241–242): no buildx / QEMU setup.**
  Before this slice the workflow had no `docker/setup-qemu-action` and
  no `docker/setup-buildx-action` step (verified: no `buildx`, `qemu`,
  or `setup-docker` string anywhere in `release.yml`). The multi-arch
  build therefore relied entirely on the runner's default Docker setup.
  Docker's own docs say "The default Docker setup for GitHub Actions
  runners supports building and pushing multi-platform images to
  registries", so it may well have worked — but GoReleaser's
  `dockers_v2` uses `docker buildx` under the hood and `docker buildx`
  requires a multi-platform-capable builder, and GoReleaser's own
  GitHub Actions example lists `setup-qemu-action` and
  `setup-buildx-action` as the steps needed "only … if using docker".
  The workflow now creates the builder explicitly instead of relying
  on runner defaults: `docker/setup-qemu-action` v3.7.0
  (`c7c53464625b32c7a7e944ae62b3e17d2b600130`) and
  `docker/setup-buildx-action` v3.12.0
  (`8d2750c68a42422c14e847fe6c8ac0403b4cbd6f`), both SHA-pinned per the
  223–224 policy (SHAs resolved with `git ls-remote … refs/tags/v3.7.0`
  / `v3.12.0`; both tags are lightweight, so the SHA is the commit —
  no `^{}` dereference exists for them). The steps cannot be executed
  in this VM (no Docker at all — see §4), so they are validated by YAML
  parse + pin resolution only, and remain unproven until the first tag.
- **G2 — FIXED in this slice (items 241–242): no `goreleaser check`
  step.** Before this slice nothing in CI validated `.goreleaser.yml`
  before the release ran. The 221–222 slice found the hard way that a
  broken config (two `signs` entries both defaulting to id `default`)
  makes GoReleaser refuse to run at all — that bug would only have
  surfaced at tag time. The workflow now runs `goreleaser check`
  (same pinned `goreleaser-action` v6.4.0, `args: check`) right after
  `Set up Go`, before cosign/syft/login. `goreleaser check` is validated
  in this VM: green on the real GoReleaser v2.20.2 (see §4). Note what
  it does **not** prove: it validates config schema/fields only — it
  does not build anything, does not execute `Dockerfile.release`, and
  does not touch Docker.
- **G3 — OPEN: the verify job never runs the arm64 image.** The only
  `docker run` in the workflow is `docker run --rm
  "ghcr.io/raunakiitp/codvian-router:<tag>" help` on the ubuntu-24.04
  (amd64) runner, which pulls and runs the amd64 variant. The arm64
  variant is built and signed but never executed anywhere in CI. To
  close it, add to the verify job (proposed step text, **not** added
  here — it cannot be validated in this VM, and an untested step in the
  release pipeline would only fail at release time):

  ```yaml
  - name: Run the published image on arm64 (QEMU)
    run: |
      docker run --rm --platform linux/arm64 \
        "ghcr.io/raunakiitp/codvian-router:${{ github.ref_name }}" help
  ```

  (Requires the setup-qemu step from G1; QEMU emulation makes this
  slow but it is a `help` invocation, not a load test. An arm64 runner
  would be the faster alternative.)
- **G4 — OPEN: nothing in CI inspects the manifest.** No step runs
  `docker buildx imagetools inspect` or otherwise asserts the published
  manifest lists both arches with distinct digests. This is the
  sharpest gap: if the build silently produced an amd64-only image
  (e.g. a default-driver build ignoring `--platform`), the verify job
  would still pass — `docker pull` on the amd64 runner fetches the
  amd64 variant and `help` exits 0. To close it, add to the verify job
  (proposed step text, **not** added here — same reason as G3):

  ```yaml
  - name: Assert the published manifest lists both architectures
    run: |
      docker buildx imagetools inspect \
        "ghcr.io/raunakiitp/codvian-router:${{ github.ref_name }}" \
        | tee /tmp/manifest.txt
      grep -q "linux/amd64" /tmp/manifest.txt
      grep -q "linux/arm64" /tmp/manifest.txt
  ```

  Until that step lands, P3 must be checked manually after the first tag.
- **G5 — OPEN: the image SBOM is never verified.** GoReleaser attaches
  an SBOM to the image by default, but neither the release job nor
  `scripts/verify-image.sh` checks it — the script verifies the cosign
  signature and (when `gh` is present) the provenance attestation only.
  The archive SBOMs are verified by `verify-release.sh`; the image SBOM
  has no equivalent check.
- **G6 — OPEN (fail-safe): the `dist/artifacts.json` digest lookup is
  unproven until CI runs.** The image-attestation step depends on the
  lookup finding a `Docker Image` / `Published Docker Image` entry named
  `ghcr.io/raunakiitp/codvian-router:<tag>` with `extra.Digest`. If the
  entry is absent the step exits 1 and fails the release loudly — a
  failed release, not a silent gap — but the lookup code has never run
  against a real GoReleaser output.
- **G7 — OPEN: the release Dockerfile's base images float.**
  `Dockerfile.release` uses `golang:1.27.4-bookworm` and
  `gcr.io/distroless/static-debian12:nonroot` with no digest pin, so a
  later rebuild can pick up different base layers. The healthcheck
  builder also runs Go 1.27.4 while the main binary is built with Go
  1.27.4 — no drift. Not fixed here: digest pinning needs
  resolved digests and a policy decision about when to bump them.
- **G8 — OPEN (upstream): `dockers_v2` is provisional/alpha** (GoReleaser's
  own marking; it becomes `dockers` in v3). If it breaks in a future
  GoReleaser release, the fallback is the deprecated `dockers` +
  `docker_manifests` pair with the same `Dockerfile.release`.
- **G9 — OPEN: the verify job never starts the server.** `help` proves
  the binary starts; it does not prove `serve` binds, `/healthz`
  answers, or the healthcheck binary works inside the image. See P9 for
  the manual check and the proposed CI step shape (detached run +
  `curl /healthz`).

## 4. What is proven in this VM (and what is not)

Proven in this VM, 2026-11-18, GoReleaser v2.20.2 (the version pinned in
the workflow):

- `goreleaser check` — green, exit 0, on the real `.goreleaser.yml`
  (run in a scratch clone with a remote, because `goreleaser check`
  refuses a repo with no remote and this worktree's local git repo has
  no remote by design — see the 221–222 report for the same caveat).
- Workflow YAML parses (PyYAML); all 14 `uses:` entries are pinned to
  full 40-char SHAs; the two new pins were resolved live with
  `git ls-remote`.
- `docker` is not installed in this VM at all (`docker: command not
  found`, checked 2026-11-18) — there is no daemon to start, no buildx
  to invoke, and no image to run. That is the reason every item in §2
  that needs a build, a pull, or a run stays unproven until the first
  real tag's CI run. The 221–222 slice's local proof (a crane-assembled
  equivalent image, signed with a throwaway keypair, tamper proof 17/17
  on cosign v2.4.3 and 18/18 on v3.1.4) proves the *verify* path and the
  digest-binding of signatures — it does not prove the buildx build,
  the manifest, or the keyless path.

## 5. First-tag runbook (for the reviewer of the first release)

Precondition: the repo must be **public** at push time — private-repo CI
minutes are already exhausted, and the maintainer flips it public
himself before the push (see the repo-visibility plan in memory). Then:

1. Push the tag (`v*.*.*`). Watch the `release` job: confirm the
   GoReleaser log shows both `linux/amd64` and `linux/arm64` builds
   (P1, P2) and the `Get published image digest` step prints a digest
   (P11).
2. Watch the `verify` job: both verify steps green (P5, P7).
3. Run the manual checks P3, P4, P8, P9, P10, P12 from §2 and record
   the outputs (paste the `imagetools inspect` output and the arm64
   `help` exit code into the release notes or a follow-up doc).
4. If any of P1–P12 fails, the release is failed even if the job is
   green in part: a green release job with a red verify job is a failed
   release, and a manifest missing an arch is a failed release even if
   the verify job is green (G4).
5. After the first green tag, file the G3/G4/G5/G9 proposed steps as a
   follow-up slice so the second release proves them in CI instead of
   by hand.
