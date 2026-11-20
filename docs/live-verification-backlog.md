# Live-verification backlog (CHECKLIST 241–242)

Date: 2026-11-18. This is the single honest record of everything that is
still pending a live check — a real CI run, a real browser, a second host,
a real Docker host, or the first tag. Nothing in this file is marked
proven unless it was actually observed; the "What is proven in-VM
instead" column states the nearest in-VM evidence without overstating it.

The same items are served live by the dashboard: `GET
/dashboard/verification` (feed) and the Verification view in
`web/index.html`. The feed is a static-but-honest table kept in
`internal/server/verification.go` — its statuses must match this file;
if an item clears, update both in the same slice.

## Pending items

### LV-01 — Multi-arch `docker buildx` build
- **What:** `dockers_v2` builds the image for `linux/amd64` +
  `linux/arm64` from `Dockerfile.release` and pushes one manifest to
  GHCR (`docs/ci-proof-multarch.md` P1–P2).
- **Needs:** CI on the first real tag (real Docker on the runner).
- **Why not in this VM:** `docker` is not installed at all (`docker:
  command not found`, checked 2026-11-18) — no daemon, no buildx, no
  image to build. `Dockerfile.release` has never been executed; it is
  validated by `goreleaser check` only.
- **Proven in-VM instead:** `goreleaser check` green on v2.20.2; the
  workflow now has explicit `setup-qemu-action` + `setup-buildx-action`
  steps (items 241–242, G1).
- **Clear it:** first tag's CI run; read the GoReleaser log for both
  platforms.

### LV-02 — Published manifest lists both architectures
- **What:** `docker buildx imagetools inspect
  ghcr.io/raunakiitp/codvian-router:<tag>` lists `linux/amd64` and
  `linux/arm64` with two distinct digests (`docs/ci-proof-multarch.md`
  P3).
- **Needs:** CI on the first real tag (published image) + a manual
  inspect (no CI step asserts it — gap G4).
- **Why not in this VM:** no published image exists and no Docker to
  inspect one with.
- **Proven in-VM instead:** nothing — the manifest has never existed.
- **Clear it:** run the inspect command after the first tag; record the
  output.

### LV-03 — Cosign keyless signature over the published image
- **What:** `scripts/verify-image.sh <tag>` passes against the real
  GHCR image (signature bound to the release workflow + tag ref, Rekor
  entry checked).
- **Needs:** first real tag.
- **Why not in this VM:** the keyless identity is minted by GitHub
  Actions OIDC and only exists inside a CI run; this VM cannot mint it
  or reach Sigstore's write path, and there is no published image to
  verify.
- **Proven in-VM instead:** the keypair-mode path only — local test
  image signed with a throwaway key, untampered verifies, tampered
  under the same tag / wrong key rejected (17/17 on cosign v2.4.3,
  18/18 on v3.1.4; 221–222 report). Keypair mode verifies a raw Ed25519
  signature; there is no Rekor entry for a local key.
- **Clear it:** first tag's `verify` job (`verify-image.sh` step) green.

### LV-04 — Image build-provenance attestation
- **What:** `gh attestation verify
  oci://ghcr.io/raunakiitp/codvian-router:<tag> --repo
  raunakiitp/CodvianRouter` passes.
- **Needs:** first real tag (published image + GitHub attestation store).
- **Why not in this VM:** attestations live in GitHub's attestation
  store, not in the registry — there is nothing to fetch until a release
  attests.
- **Proven in-VM instead:** nothing (the attestation step's code is
  written; its `dist/artifacts.json` digest lookup is itself unproven
  — gap G6).
- **Clear it:** first tag's `verify` job, or the manual `gh` command
  after it.

### LV-05 — `docker run --rm <image> help` on amd64
- **What:** the verify job's `docker pull` + `docker run --rm
  <image> help` exits 0.
- **Needs:** CI on the first real tag.
- **Why not in this VM:** no Docker, no published image (same as LV-01).
- **Proven in-VM instead:** the binary's `help` path is exercised by
  every live smoke (the real binary, run directly) — but never from
  inside the published image.
- **Clear it:** first tag's `verify` job green.

### LV-06 — `docker run --rm <image> help` on arm64
- **What:** the arm64 variant starts and prints help (gap G3).
- **Needs:** an arm64 host, or QEMU emulation in CI
  (`docker run --rm --platform linux/arm64 … help`).
- **Why not in this VM:** no Docker, no arm64 host, no QEMU; and the
  workflow's verify job runs on amd64 only, so even CI does not cover
  it today.
- **Proven in-VM instead:** nothing — the arm64 binary is built by
  GoReleaser for the archives (cross-compiled) but the arm64 *image*
  has never been executed anywhere.
- **Clear it:** add the proposed G3 step to the verify job, or run the
  command by hand on an arm64 host after the first tag.

### LV-07 — First literal `docker run` of the local image
- **What:** `docker run` / `docker compose up` from `Dockerfile` +
  `docker-compose.yml` on a normal Docker host (the slice-4 image was
  assembled by `docker import` from the real distroless base + binaries
  built with the Dockerfile's exact flags, and the server was run from
  the image rootfs via chroot under `unshare -Urm` — the Dockerfile and
  `docker compose up` still need their first literal run).
- **Needs:** a real Docker host.
- **Why not in this VM:** no Docker daemon, and the VM cannot run
  containers — slice 4 measured `setns: operation not permitted` /
  `lchown: invalid argument` (no effective CAP_SYS_ADMIN; multi-uid
  userns maps blocked by no-new-privs). `docker` is not even installed.
- **Proven in-VM instead:** the chroot smoke from slice 4 (`init` exit
  0, server healthz-ready in 142 ms, dashboard 200, `/v1/models` +
  `/metrics` 401 without key / 200 with key, in-image healthcheck
  binary exit 0) — a rootfs run, not a container run.
- **Clear it:** on a normal Docker host: `docker compose up -d --build`
  (or `docker run -d --network host codvian-router:local`), then
  `curl http://127.0.0.1:20128/healthz`.

### LV-08 — Provider-add form click-through
- **What:** in the dashboard Providers view, fill the add-provider form
  (catalog selector + API key) and submit; the provider appears with its
  verification badge, and errors render in the form.
- **Needs:** a live browser (a real click in the served dashboard).
- **Why not in this VM:** subagents cannot operate the live browser;
  there is no browser session here to click in.
- **Proven in-VM instead:** the endpoint is proven at server level —
  `POST /api/providers` tests the supplied key live against the
  upstream, encrypts and persists it, and adds it to the live registry
  (server tests + live smoke via curl/CLI). What is *not* proven is the
  dashboard form's wiring: the fetch call, the auth header, success /
  error rendering in the page.
- **Clear it:** open the live dashboard in a browser, add a provider
  through the form, confirm it appears; record browser + result.

### LV-09 — DB import form click-through
- **What:** in the dashboard Settings/Guard area, use the DB import form
  (file + typed phrase `IMPORT`) and confirm usage history merges
  (duplicates skipped, corrupt file imports nothing).
- **Needs:** a live browser.
- **Why not in this VM:** same as LV-08 — no live browser here.
- **Proven in-VM instead:** `POST /admin/db/import` proven at server
  level (tests + curl smoke); the form's file-picker + phrase + result
  rendering never clicked.
- **Clear it:** live-browser run against a throwaway data dir; record
  browser + result.

### LV-10 — DB wipe form click-through
- **What:** in the dashboard, arm the wipe (`POST /admin/db/wipe/arm`)
  and confirm it with the typed phrase `WIPE DATABASE` + single-use
  token, and confirm usage history is gone while providers/keys/config
  are preserved.
- **Needs:** a live browser + a throwaway data dir (the action is
  irreversible).
- **Why not in this VM:** same as LV-08 — no live browser here.
- **Proven in-VM instead:** `POST /admin/db/wipe` proven at server
  level (tests + curl smoke, incl. the arm-token and phrase rules); the
  form flow never clicked.
- **Clear it:** live-browser run against a throwaway data dir only;
  record browser + result.

### LV-11 — DB export form click-through
- **What:** in the dashboard, use the DB export form (typed phrase
  `EXPORT`) and confirm the downloaded JSON carries usage history and
  no secrets.
- **Needs:** a live browser.
- **Why not in this VM:** same as LV-08 — no live browser here.
- **Proven in-VM instead:** `POST /admin/db/export` proven at server
  level; the form + download never clicked.
- **Clear it:** live-browser run; confirm the download and its contents.

### LV-12 — Non-loopback 403
- **What:** a request to a Tier 1 (local-only) route from a non-loopback
  TCP peer is refused with `403 LOCAL_ONLY` (guard working as designed),
  while loopback callers are served.
- **Needs:** a second host (or any reachable non-loopback peer).
- **Why not in this VM:** the guard judges by the real TCP peer address
  (`isLoopbackPeer`, `internal/server/guard.go` — `RemoteAddr`, never a
  header). This VM's only non-loopback interface is `host0`
  `198.19.0.2/30`, and a probe on 2026-11-18 (a listener bound to
  `0.0.0.0:20199`, connect to `http://198.19.0.2:20199/`) **timed out**
  — the sandbox does not deliver traffic to the VM's own non-loopback
  address, so no non-loopback peer can ever arrive here. Binding the
  server to `0.0.0.0` and curling the host0 address cannot work while
  the address itself is unreachable.
- **Proven in-VM instead:** the loopback-allow path is proven by every
  live smoke (the dashboard and API serve on loopback); the refusal
  branch is unit-tested (`isLoopbackPeer` + guard tests) but has never
  been observed over a real socket.
- **Clear it:** from a second host on the same network, `curl
  http://<router-host>:20128/dashboard/guard` (or any Tier 1 route)
  with the router listening on `0.0.0.0` — expect `403 LOCAL_ONLY`;
  record the peer address + status.

## Proven in-VM (the other side of the same record)

These are proven — the dashboard shows them with the proven badge — and
exist so the unproven items above are not mistaken for "nothing works":

- **PV-01 — `goreleaser check` green.** Real GoReleaser v2.20.2, exit 0,
  on the real `.goreleaser.yml` (scratch clone with a remote; the
  worktree's local repo has no remote by design).
- **PV-02 — Local image sign/verify + tamper proof.** Real cosign, real
  crane-assembled image, throwaway keypair: untampered verifies,
  tampered-under-the-same-tag / wrong-key rejected — 17/17 (cosign
  v2.4.3), 18/18 (v3.1.4). Proves the verify path and digest-binding,
  not the buildx build or the keyless path.
- **PV-03 — Provider-add / DB import / DB wipe / DB export endpoints
  at server level.** Real server + tests + curl smoke; the *endpoints*
  work — only the dashboard form click-throughs (LV-08–LV-11) are
  pending.
- **PV-04 — Loopback serving.** Every live smoke serves the dashboard
  and API on loopback; the loopback-allow half of the guard is proven.
- **PV-05 — Release workflow static checks.** YAML parses; all 14
  `uses:` entries SHA-pinned (incl. the two new buildx steps, pins
  resolved live via `git ls-remote`); `goreleaser check` step present.
- **PV-06 — Healthcheck helper parity.** The inline healthcheck source
  in `Dockerfile.release` is byte-identical to `Dockerfile`'s, and the
  runtime directives (ENTRYPOINT/CMD/EXPOSE/HEALTHCHECK/USER) match
  (221–222 gate).

## What is NOT in this backlog

- The Geekflare benchmark re-run (items 149–150, 207–208, 225–226):
  measured honestly in-VM whenever the VM is quiet; not a live-check
  item, tracked in `docs/benchmark-*.json` and the Benchmark view.
- The first real release as a whole (binaries, npm package): tracked in
  `docs/RELEASE-SIGNING.md` ("No release exists yet"); its image half
  is LV-01–LV-06 above.
