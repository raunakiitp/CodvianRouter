# Install Codvian Router

> **Status (2026-11-17):** Phase 1 MVP is built and verified. The repository
> contains 55 Go packages (as of 2026-11-26); test counts from earlier eras
> (133/133, 207/207) reflect different package snapshots and are not
> comparable to the current tree. There is **no
> release binary, no published npm package, and no published Docker image
> yet**. The only install path that works today is **build from source**.
> Everything else on this page is labelled **planned** until a release is
> actually published. The install happy paths themselves were proven
> locally on 2026-11-18 against a fixture release served on this VM
> (see §4/§5) — that proves the installer and wrapper code, not the
> real release download, which cannot exist until the first release is
> cut. Release signing is configured (Sigstore cosign keyless,
> per-archive SBOM + build-provenance attestation — see
> [RELEASE-SIGNING.md](RELEASE-SIGNING.md)) — and the first release will
> also publish a signed multi-arch Docker image to
> `ghcr.io/raunakiitp/codvian-router`, verifiable with
> `scripts/verify-image.sh` before `docker run` — but no release has been
> cut yet, so nothing here is verifiable against a live release.

## 1. Build from source (works today)

### Prerequisites

- **Go 1.26.0 or newer.** The module declares `go 1.26.0` with `toolchain go1.27.4`; the verification gate ran on Go 1.27.4. (`go 1.26.0` is the minimum; newer is fine.)
- **git**
- **curl** (to verify the install)
- No other build dependencies. The module uses six direct
  dependencies (`github.com/gorilla/websocket`,
  `github.com/pkoukk/tiktoken-go`, `github.com/redis/go-redis/v9`,
  `golang.org/x/crypto v0.59.0`, `golang.org/x/sys`, `modernc.org/sqlite`);
  the SQLite driver is pure Go, so no C toolchain is needed
  (`CGO_ENABLED=0` builds work).

### Steps

```sh
git clone https://github.com/raunakiitp/CodvianRouter
cd CodvianRouter
go build -o codvian-router ./cmd/codvian-router
./codvian-router init      # creates config + your first gateway key (shown ONCE)
./codvian-router serve     # listens on 127.0.0.1:20128/dashboard, auth ON
```

What `init` prints (save the key — it cannot be recovered):

```text
codvian-router v0.1.0 initialised
config: /home/<you>/.config/codvian-router/config.json
data dir: /home/<you>/.local/share/codvian-router
listening on 127.0.0.1:20128 (auth ON)

Gateway key (shown ONCE — save it now, it cannot be recovered):
crk_<32 random characters>

Use it as:  Authorization: Bearer crk_<32 random characters>
```

What `serve` prints:

```text
codvian-router v0.1.0 listening on 127.0.0.1:20128 (auth ON)
config: /home/<you>/.config/codvian-router/config.json
providers: 0 configured, 0 clients ready
```

Notes:

- `init` **refuses** if a config already exists at that path. Delete the
  old config first if you really want to start over (deleting it also
  orphans the vault secret in the data dir — see
  [ARCHITECTURE.md](ARCHITECTURE.md#crypto--key-storage)).
- `serve` is the default subcommand: bare `./codvian-router` serves.
  Other subcommands: `keygen` (prints a fresh gateway key that is **not**
  stored), `help`.
- Flags: `--config PATH` (default
  `~/.config/codvian-router/config.json`) and `--port PORT` (overrides the
  port in the config; the bind is always loopback-only — see below).
- The router **binds 127.0.0.1 only** and **auth is always ON**. Config
  validation rejects any non-loopback listen address. There is no flag to
  expose it on a public interface; that is deliberate, not a missing
  feature.
- The data dir (`~/.local/share/codvian-router` by default) holds the
  SQLite ledger (`ledger.db`) and the vault master secret. Back it up:
  losing it loses the vault secret and the ledger history.

### Per-OS notes

| OS | Notes |
|---|---|
| **Linux** | Paths above are the Linux defaults. Config: `~/.config/codvian-router/config.json` (from Go's `os.UserConfigDir`). Data: `~/.local/share/codvian-router`. Binary name: `codvian-router`. |
| **macOS** | Same build commands. Config lives under Go's `os.UserConfigDir`, i.e. `~/Library/Application Support/codvian-router/config.json`; the data dir is still `~/.local/share/codvian-router` (home-relative, same on every OS). Binary name: `codvian-router`. Requires a Go install (e.g. via the Go website or Homebrew) — Go is not preinstalled. |
| **Windows** | Same build commands from PowerShell or `cmd` (use `go build -o codvian-router.exe ./cmd/codvian-router`). Config: `%AppData%\codvian-router\config.json`; data dir: `%USERPROFILE%\.local\share\codvian-router`. Binary name: `codvian-router.exe`. Run `./codvian-router.exe init` / `serve` the same way. The project cross-compiles for Windows (see `.goreleaser.yml`) — items 267–268 fixed the release-blocking Windows build failure and a snapshot build now produces both Windows archives — but the Windows binary has not been executed by the gate (no Wine in the VM), so treat Windows as built-but-not-run until CI executes it. macOS is built-but-untested-by-the-gate. |

## 2. Verify the install

With the router running:

```sh
# 1. Health — no auth needed, must return {"status":"ok"}
curl http://127.0.0.1:20128/healthz
# {"status":"ok"}

# 2. Without a key you must get 401
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:20128/v1/models
# 401

# 3. With your gateway key, list models (empty until you add a provider)
curl -H "Authorization: Bearer crk_<your key>" http://127.0.0.1:20128/v1/models
```

Then add at least one provider (base URL + API key) to the config file and
try a real request:

```sh
curl http://127.0.0.1:20128/v1/chat/completions \
  -H "Authorization: Bearer crk_<your key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "<a model ID one of your providers offers>",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Expected: an OpenAI-compatible `chat.completion` JSON object with
`choices[0].message.content` and a `usage` block
(`prompt_tokens` / `completion_tokens` / `total_tokens`).

> The model name in the request must be a real model ID that one of your
> providers offers. The `auto` model name (smart routing modes) is
> **planned** (Phase 2) and does not work yet — today the router routes a
> requested model ID across the providers that offer it.

For a full end-to-end check with failover, see
[DEMO.md](DEMO.md) — it runs two local mock upstreams and proves the 429
failover and the 150 ms hedge against the real binary.

Measured performance (coordinator's gate, 2026-11-17, localhost mocks,
upstream answering in 50 ms — **not** a public benchmark, real-world
numbers depend on upstream latency):

- 429 failover: **52.4–55.8 ms** (~1–2 ms router overhead vs 51.2 ms direct)
- Failover switch time (dead-429 response → fallback dispatch, NOT end-to-end): **0.6–1.0 ms** (measured 2026-11-20, VM localhost mocks, 3 runs, TestChatCompletionsFailoverSwitchTime — parity tracker Area 12 row 18 evidence)
- Slow provider (2,000 ms) hedged at 150 ms: **203.9–204.4 ms**
- Startup to healthz-ready: **73 ms**; idle memory: **~32 MB**;
  single binary: **16.9 MB**

## 3. Docker — signed image on first release (planned until then)

A `Dockerfile` and `docker-compose.yml` exist at the repo root, so you can
build the image locally. **No image has been published to any registry
yet** — `docker pull` will not find one — but the first release will
publish a signed multi-arch image
(`ghcr.io/raunakiitp/codvian-router:<tag>`, linux/amd64 + linux/arm64)
from `Dockerfile.release`, signed with the same cosign keyless identity
as the release binaries. **Verify it before running it** (a tag is just
a label; the signature is what proves the image came from the release
workflow):

```sh
sh scripts/verify-image.sh v0.1.0   # cosign keyless signature + provenance attestation
docker pull ghcr.io/raunakiitp/codvian-router:v0.1.0
```

Only after that prints `VERIFIED`, run it (first run only creates the
config + first gateway key; the key is printed once):

```sh
docker run --rm \
  -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
  -v codvian-router-config:/home/nonroot/.config/codvian-router \
  ghcr.io/raunakiitp/codvian-router:v0.1.0 init
docker run -d --network host ghcr.io/raunakiitp/codvian-router:v0.1.0
curl http://127.0.0.1:20128/healthz
```

Pin a version tag — or better, the digest
(`ghcr.io/raunakiitp/codvian-router@sha256:...`) — in production;
`:latest` moves on every release. The full verify commands (including
the manual `cosign verify` with the pinned workflow + tag identity)
are in [RELEASE-SIGNING.md](RELEASE-SIGNING.md#verifying-the-docker-image--do-this-first-before-docker-run).

Local build (unsigned — fine for local development, never distribute):

```sh
docker build -t codvian-router:local .
docker run --rm \
  -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
  -v codvian-router-config:/home/nonroot/.config/codvian-router \
  codvian-router:local init        # first run only; key printed ONCE
docker compose up -d --build       # or: docker run -d --network host codvian-router:local
curl http://127.0.0.1:20128/healthz
```

**How the port gets through:** the image is a multi-stage build (Go 1.27.4
builder, distroless nonroot runtime, healthcheck that GETs `/healthz`),
but the router binds 127.0.0.1 **by default**, which Docker's
published-port forwarding cannot reach (it delivers traffic to the
container's eth0 address). The compose file therefore runs serve with
`--listen 0.0.0.0:20128` — the binary's explicit opt-in for a
non-loopback bind (only 127.0.0.1, `::1` and `0.0.0.0` are accepted;
`0.0.0.0` prints a warning and gateway-key auth stays ON) — so the
`ports:` mapping works and the router answers on the host's
127.0.0.1:20128. The alternative is `--network host` (or
`network_mode: host` in compose), which shares the host's loopback and
keeps the loopback-only bind. Do not weaken the bind any other way.

The two named volumes matter: the data volume holds the ledger and the
vault master secret — losing it loses the vault.

## 4. npm — package files exist, NOT published (planned)

`package.json` (name `codvian-router`, version `0.1.0`, Node `>=18`) and
`bin/codvian-router.js` exist in the repo. The wrapper downloads the
matching prebuilt binary from the project's GitHub releases **on first
run** (cached under `bin/.cache/`), verifies it, then execs it,
forwarding arguments and signals. Nothing is downloaded at install
time. Verification is verification-first, the same rule as
`install.sh` (§5): the wrapper downloads `checksums.txt`, verifies
the Sigstore cosign keyless signature over it when `cosign` is
installed (identity pinned to the release workflow + tag) and refuses
on any signature failure, then verifies the archive's SHA-256 against
the manifest and refuses on mismatch or a missing entry — before
anything is extracted or executed. Without cosign it warns loudly
and continues checksum-only, unless
`CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` refuses instead. An unreachable
manifest is retried (3 attempts, backoff) and then refused, and a
cached binary is re-verified against its stored SHA-256 on every
run. Full behaviour and honest limits (SBOM not checked here):
`docs/RELEASE-SIGNING.md`, "Installing via npm". Tamper proof:
`bin/codvian-router.test.js` (`npm test`, 16 tests).

**The package is not published to the npm registry, and no GitHub release
exists yet — so `npm install -g codvian-router` does not work today, and
even a local install would fail at first run because there is no release
binary to download.** The intended usage, once published, is:

```sh
npm install -g codvian-router   # PLANNED — not published yet
codvian-router init
codvian-router serve
```

**Proven on this VM (2026-11-18, items 321–322 — local fixture, not a
real release):** the real binary (`go build ./cmd/codvian-router`,
v0.1.0) was tarred as the GoReleaser asset
`codvian-router_0.1.0_linux_amd64.tar.gz` with a real `checksums.txt`,
served from a local HTTP server, and `npm pack` + `npm install -g` from
that tarball was exercised end to end in an isolated HOME: first run
downloaded the asset from the local server, printed the no-cosign
warning, verified the SHA-256, cached the binary, then
`codvian-router --version` printed `codvian-router v0.1.0` (exit 0),
`init --key-file` created the config + gateway key, `provider add`
against a local mock upstream reported 1 model, and `serve` answered
`{"status":"ok"}` on `/healthz`, 401 on `/v1/models` without a key, and
a relayed `mock hello` chat completion with the mock's usage block. A
second `--version` reused the cache (no re-download). This flow is now
a permanent test: `npm test` (12/12, incl. the 9 verification-path
tests and the 3 new happy-path tests in `bin/codvian-router.test.js`).

**Not proven:** the download from the real GitHub releases URL, `npm
publish` itself, the cosign signature branch (this VM has no cosign —
only the stub-cosign tests in `bin/codvian-router.test.js` exercise
that path), and macOS/Windows execution of the wrapper. `version` and
`--version` (also `-v`) are new in this slice — before it, both exited
2 (unknown flag / unknown subcommand); the version printed is the same
`Version` constant the startup lines use.

Overrides for testing/local builds (not normal use):
`CODVIAN_ROUTER_BINARY=<path>`, `CODVIAN_ROUTER_VERSION=<x.y.z>`,
`CODVIAN_ROUTER_PLATFORM=<os>-<arch>`,
`CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` (refuse without cosign instead
of checksum-only), and testing-only `CODVIAN_ROUTER_DOWNLOAD_BASE_URL`
/ `CODVIAN_ROUTER_CACHE_DIR` (used by the test suite).

## 5. Install script — file exists, release NOT published (planned)

`scripts/install.sh` exists in the repo. It downloads the matching
prebuilt release tarball + `checksums.txt` from
`https://github.com/raunakiitp/CodvianRouter/releases`, verifies the
Sigstore cosign keyless signature over `checksums.txt` when `cosign`
is installed (and refuses the install when that check fails), then
verifies the tarball's SHA-256, and installs to `/usr/local/bin` (or
`$PREFIX/bin`). It supports Linux/macOS on amd64/arm64, requires
`curl`, `tar`, and `sha256sum` (`shasum` on macOS), and refuses to run
piped from curl — download it first, then run it.

**Verify the release BEFORE installing — signature first, install
second.** The full check is one command (see
[RELEASE-SIGNING.md](RELEASE-SIGNING.md) for what it proves):

```sh
sh scripts/verify-release.sh v0.1.0   # signature -> checksums -> SBOM -> provenance
```

`install.sh` re-checks the signature itself when `cosign` is present;
without `cosign` it installs checksum-only with a loud warning (set
`CODVIAN_ROUTER_REQUIRE_SIGNATURE=1` to refuse instead). The
`verify-release.sh` path always requires `cosign` — that is the
recommended path.

**It cannot work yet: there is no GitHub release to download.** The
intended usage, once a release exists, is:

```sh
curl -fsSLO https://raw.githubusercontent.com/raunakiitp/CodvianRouter/main/scripts/install.sh
sh install.sh            # or: sh install.sh v0.1.0
```

**Proven on this VM (2026-11-18, items 321–322 — local fixture, not a
real release):** the same real-binary fixture as §4, served locally,
was installed with `PREFIX=<isolated dir> sh scripts/install.sh
v0.1.0`: download + SHA-256 verification + install to `$PREFIX/bin`,
exit 0, then the installed binary ran the full happy path in an
isolated HOME — `--version` → `codvian-router v0.1.0`, `init
--key-file`, `provider add` (mock, 1 model), `serve` smoke (healthz
`{"status":"ok"}`, 401 without key, relayed mock chat completion).
Also proven: piped stdin is refused (`cat install.sh | sh` exits
non-zero with the pipe-guard message), and with no version argument
the script fails cleanly at latest-release resolution — GitHub's API
returns 404 because no release exists yet (exit 1, "could not resolve
the latest release … Pass a version explicitly"). Permanent tests:
`npm test` (12/12, `bin/codvian-router.test.js`).

**Not proven:** the download from the real GitHub releases URL, the
cosign signature branch on a real release (no cosign on this VM), and
macOS/Windows execution.

## 6. Release binaries — planned, not yet published

`.goreleaser.yml` is configured to produce static, cross-compiled
binaries for Linux/macOS/Windows × amd64/arm64
(`codvian-router_<version>_<os>_<arch>.tar.gz`, `.zip` on Windows) plus
`checksums.txt` (SHA-256) as an ordinary GitHub release — and every
release is signed: `checksums.txt` and each archive's SBOM carry
Sigstore cosign keyless signatures (`.sigstore.json` bundles, identity
pinned to the release workflow + tag), one SPDX-JSON SBOM per archive,
and SLSA build-provenance attestations verifiable with
`gh attestation verify`. The pipeline (`.github/workflows/release.yml`)
runs only on a `v*.*.*` tag and self-verifies the published release
with `scripts/verify-release.sh`. Full design + verify commands:
[RELEASE-SIGNING.md](RELEASE-SIGNING.md). **No release has
been cut yet** — the first release (v0.1.0) is pending the coordinator's
release decision. Until then, build from source (§1).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `init` says "config already exists" | A config is already at that path. Use `--config` to pick another path, or delete the old one (see the vault note in §1). |
| `401` on every request | Missing or wrong gateway key. The key is `crk_` + 32 characters, issued by `init` and shown once. `keygen` prints a key that is **not stored** — it will 401 until added to the config. |
| `curl` to `/v1/chat/completions` returns a clean JSON error about an unknown model | The model ID is not offered by any enabled provider. Check `GET /v1/models` with your key, or add the provider that offers it. |
| `docker run -p ...` never connects | The container is binding 127.0.0.1 (the default). Serve with `--listen 0.0.0.0:20128` (see §3) or use `--network host`. |
| `npm` / `install.sh` cannot find a release | Expected — see §4/§5. No release is published yet. |
| Router starts but `providers: 0 configured` | No providers in the config yet. Add a provider (base URL + API key) — the registry tests the key inline and auto-fetches the model list on add. |
