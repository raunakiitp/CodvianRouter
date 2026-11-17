# Install Codvian Router

> **Status (2026-11-17):** Phase 1 MVP is built and verified (133/133 tests
> in the coordinator's gate; 207/207 including the Phase 2/3 packages in a
> docs-time re-run — see [CHANGELOG.md](../CHANGELOG.md)). There is **no
> release binary, no published npm package, and no published Docker image
> yet**. The only install path that works today is **build from source**.
> Everything else on this page is labelled **planned** until a release is
> actually published.

## 1. Build from source (works today)

### Prerequisites

- **Go 1.22 or newer.** The module declares `go 1.22`; the verification
  gate ran on Go 1.27.4. (`go 1.22` is the minimum; newer is fine.)
- **git**
- **curl** (to verify the install)
- No other build dependencies. The module uses only two direct
  dependencies (`golang.org/x/crypto v0.24.0`,
  `modernc.org/sqlite v1.34.0`); the SQLite driver is pure Go, so no C
  toolchain is needed (`CGO_ENABLED=0` builds work).

### Steps

```sh
git clone https://github.com/raunakiitp/CodvianRouter
cd CodvianRouter
go build -o codvian-router ./cmd/codvian-router
./codvian-router init      # creates config + your first gateway key (shown ONCE)
./codvian-router serve     # listens on 127.0.0.1:20128, auth ON
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
| **Windows** | Same build commands from PowerShell or `cmd` (use `go build -o codvian-router.exe ./cmd/codvian-router`). Config: `%AppData%\codvian-router\config.json`; data dir: `%USERPROFILE%\.local\share\codvian-router`. Binary name: `codvian-router.exe`. Run `./codvian-router.exe init` / `serve` the same way. The project cross-compiles for Windows (see `.goreleaser.yml`), but the verification gate ran on Linux only — treat Windows and macOS as built-but-untested-by-the-gate until a gate run exists for them. |

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
- Slow provider (2,000 ms) hedged at 150 ms: **203.9–204.4 ms**
- Startup to healthz-ready: **73 ms**; idle memory: **~32 MB**;
  single binary: **16.9 MB**

## 3. Docker — files exist, image NOT published (planned)

A `Dockerfile` and `docker-compose.yml` exist at the repo root, so you can
build the image locally. **No image has been published to any registry
yet** — `docker pull` will not find one.

```sh
docker build -t codvian-router:local .
docker run --rm \
  -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
  -v codvian-router-config:/home/nonroot/.config/codvian-router \
  codvian-router:local init        # first run only; key printed ONCE
docker compose up -d --build       # or: docker run -d --network host codvian-router:local
curl http://127.0.0.1:20128/healthz
```

**Read this before expecting the port to work:** the image is a
multi-stage build (Go 1.22 builder, distroless nonroot runtime, healthcheck
that GETs `/healthz`), but the router binds 127.0.0.1 **only**. Docker's
published-port forwarding (`-p 127.0.0.1:20128:20128`, and the `ports:`
mapping in `docker-compose.yml`) delivers traffic to the container's eth0
address, which a loopback-only listener never sees — so the port mapping
**does not reach the server** as the binary stands today. The documented
workaround is `--network host` (or `network_mode: host` in compose),
which shares the host's loopback. Do not weaken the router's loopback-only
bind to fix this; a container-aware listen option is a future change, not
a config tweak.

The two named volumes matter: the data volume holds the ledger and the
vault master secret — losing it loses the vault.

## 4. npm — package files exist, NOT published (planned)

`package.json` (name `codvian-router`, version `0.1.0`, Node `>=18`) and
`bin/codvian-router.js` exist in the repo. The wrapper downloads the
matching prebuilt binary from the project's GitHub releases **on first
run** (cached under `bin/.cache/`), verifies it is executable, then execs
it, forwarding arguments and signals. Nothing is downloaded at install
time.

**The package is not published to the npm registry, and no GitHub release
exists yet — so `npm install -g codvian-router` does not work today, and
even a local install would fail at first run because there is no release
binary to download.** The intended usage, once published, is:

```sh
npm install -g codvian-router   # PLANNED — not published yet
codvian-router init
codvian-router serve
```

Overrides for testing/local builds (not normal use):
`CODVIAN_ROUTER_BINARY=<path>`, `CODVIAN_ROUTER_VERSION=<x.y.z>`,
`CODVIAN_ROUTER_PLATFORM=<os>-<arch>`.

## 5. Install script — file exists, release NOT published (planned)

`scripts/install.sh` exists in the repo. It downloads the matching
prebuilt release tarball + `checksums.txt` from
`https://github.com/raunakiitp/CodvianRouter/releases`, verifies SHA-256,
and installs to `/usr/local/bin` (or `$PREFIX/bin`). It supports
Linux/macOS on amd64/arm64, requires `curl`, `tar`, and `sha256sum`
(`shasum` on macOS), and refuses to run piped from curl — download it
first, then run it.

**It cannot work yet: there is no GitHub release to download.** The
intended usage, once a release exists, is:

```sh
curl -fsSLO https://raw.githubusercontent.com/raunakiitp/CodvianRouter/main/scripts/install.sh
sh install.sh            # or: sh install.sh v0.1.0
```

## 6. Release binaries — planned, not yet published

`.goreleaser.yml` is configured to produce static, cross-compiled
binaries for Linux/macOS/Windows × amd64/arm64
(`codvian-router_<version>_<os>_<arch>.tar.gz`, `.zip` on Windows) plus
`checksums.txt` (SHA-256) as an ordinary GitHub release. **No release has
been cut yet** — the first release (v0.1.0) is pending the coordinator's
release decision. Until then, build from source (§1).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `init` says "config already exists" | A config is already at that path. Use `--config` to pick another path, or delete the old one (see the vault note in §1). |
| `401` on every request | Missing or wrong gateway key. The key is `crk_` + 32 characters, issued by `init` and shown once. `keygen` prints a key that is **not stored** — it will 401 until added to the config. |
| `curl` to `/v1/chat/completions` returns a clean JSON error about an unknown model | The model ID is not offered by any enabled provider. Check `GET /v1/models` with your key, or add the provider that offers it. |
| `docker run -p ...` never connects | Expected — see §3. Use `--network host`. |
| `npm` / `install.sh` cannot find a release | Expected — see §4/§5. No release is published yet. |
| Router starts but `providers: 0 configured` | No providers in the config yet. Add a provider (base URL + API key) — the registry tests the key inline and auto-fetches the model list on add. |
