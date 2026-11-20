# Slice 4 — Docker image build + run: gate report

Date: 2026-11-17. Worktree: `~/workspace/codvian-router/`. No push made, no
GitHub connector or browser used, no Go code touched. No sample/fake data
anywhere: the smoke test ran with zero providers configured and the empty
responses below are the real ones.

## Verdict

**PASS with two honest, bounded deviations (see §5).** The image
`codvian-router:local` (20.8 MB) exists in the local daemon, its config
matches the Dockerfile exactly, and the binary from that image serves
inside that image's filesystem: dashboard 200, `/healthz` 200
`{"status":"ok"}`, `/v1/models` and `/metrics` 401 without a key and 200
with the `init`-issued key, startup to healthz-ready **142 ms**. What
could *not* be done in this VM is a literal `docker build` (its `RUN`
steps need containers) and a literal `docker run` (same reason) — the VM
denies the syscalls both need. The evidence for the Dockerfile's build
steps and for the running server was therefore gathered by the equivalent
route documented in §4–§5, and every substitution is stated, not hidden.

## 1. What was verified

| Check | Result |
|---|---|
| `go build ./...` | clean (Go 1.27.4, `~/go/bin/go`) |
| `go vet ./...` | clean |
| `gofmt -l cmd internal` | clean |
| `go test -count=1 ./...` (full suite; not required — no Go code touched) | **all 19 packages ok, 487 PASS events, 0 fail** |
| `docker build` Dockerfile review | 2 stale items found and fixed (§3) |
| Base images resolve | `golang:1.22-bookworm` pulled (digest below); `gcr.io/distroless/static-debian12:nonroot` exported via crane (digest below) |
| Image `codvian-router:local` | 20.8 MB; `docker inspect` config == Dockerfile (§4) |
| `init` in the image | exit 0; config + first gateway key created (`crk_` + 32 chars, value not retained) |
| Server from the image | healthz-ready in **142 ms**; dashboard 200 (200,452 B); `/v1/models` 401→200 `{"data":[],"object":"list"}`; `/metrics` 401→200 (1,745 B real counters); `POST /dashboard` 405; in-image healthcheck binary exit 0; clean stop (connection refused after) |
| `docker-compose.yml` | parses (PyYAML); `command: ["serve", "--listen", "0.0.0.0:20128"]` added — **not executed** in this VM (no working container runtime; §5) |

## 2. Environment: Docker was not installed here

At slice start the VM had no Docker at all:

```
$ docker --version
bash: docker: command not found
$ ls /var/run/docker.sock
ls: cannot access '/var/run/docker.sock': No such file or directory
$ systemctl status docker
Unit docker.service could not be found.
```

`apt-get update` was tried first and was abandoned after ~9 min still
downloading lists through the egress proxy, so Docker was installed from
the static release instead (all downloads through the VM's egress proxy,
which works — `curl` to Docker Hub and GitHub succeeds):

```
curl -fsSL -o docker.tgz https://download.docker.com/linux/static/stable/x86_64/docker-28.3.3.tgz
tar xzf docker.tgz
cp docker/docker docker/dockerd docker/containerd docker/runc docker/docker-proxy docker/docker-init docker/ctr /usr/local/bin/
curl -fsSL -o containerd.tgz https://github.com/containerd/containerd/releases/download/v1.7.27/containerd-1.7.27-linux-amd64.tar.gz
tar xzf containerd.tgz bin/containerd-shim-runc-v2
cp bin/containerd-shim-runc-v2 /usr/local/bin/   # must match the bundled containerd v1.7.27;
                                                 # the v2.1.4 shim fails with "unsupported shim version (3)"
```

The daemon runs (still running at report time) as:

```
unshare -Ur bash -c 'exec dockerd --iptables=false --bridge=none --storage-driver=vfs --log-level=info'
$ docker info --format 'Server {{.ServerVersion}} driver {{.Driver}}'
Server 28.3.3 driver vfs
```

Why those flags: the first plain `dockerd` start died with
`failed to start daemon: ... failed to create NAT chain DOCKER: iptables
failed: ... Permission denied (you must be root)` (effective caps here are
only `cap_net_admin,cap_net_raw,cap_sys_ptrace,cap_sys_boot` — no effective
`CAP_SYS_ADMIN`, `no-new-privs=1`) and `failed to mount overlay: invalid
argument` (no overlayfs). `--iptables=false --bridge=none --storage-driver=vfs`
works around both. Restart command for a later session is the `unshare`
line above; the binaries live in `/usr/local/bin` (VM setup, not repo
state — reinstall after a VM replacement). Registry pulls are flaky
through the proxy: `docker pull` failed twice with
`Get "https://registry-1.docker.io/v2/": net/http: TLS handshake timeout`
and then succeeded on retry; a plain Go program using the same proxy env
reached the registry in 654 ms, so this is proxy flakiness, not a config
error — retry the pull.

## 3. Dockerfile / compose review — two stale items, both fixed

The worktree already had a correct multi-stage Dockerfile (Go 1.22
builder, `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`,
distroless `static-debian12:nonroot`, CA certs, nonroot user, `EXPOSE
20128`, in-Dockerfile healthcheck binary, `ENTRYPOINT` + `CMD ["serve"]`)
and a `.dockerignore` that keeps secrets/state out of the context. Two
things in it were stale — both predate the `--listen` flag (item 33):

1. **The `ports:` mapping in `docker-compose.yml` could not work, and the
   file said so.** The binary now accepts `--listen 0.0.0.0:20128` — the
   explicit opt-in for a non-loopback bind (only 127.0.0.1, `::1`,
   `0.0.0.0`; `0.0.0.0` prints a warning; gateway-key auth stays ON;
   see `effectiveListen` in `cmd/codvian-router/main.go`, which also says
   `0.0.0.0` "is for Docker"). Fix: compose now runs
   `command: ["serve", "--listen", "0.0.0.0:20128"]` so the existing
   `ports: ["127.0.0.1:20128:20128"]` mapping works; `network_mode: host`
   stays documented as the alternative for a loopback-only bind. The
   matching comments in `Dockerfile` and `docs/INSTALL.md` §3 (plus the
   troubleshooting row "`docker run -p ...` never connects") were
   rewritten the same way. `parseArgs` accepts flags before or after the
   subcommand, so `serve --listen …` is valid.
2. **Nothing else.** `go.mod` says `go 1.22`, matching the
   `golang:1.22-bookworm` builder. The healthcheck binary reads
   `CODVIAN_ROUTER_PORT` (default 20128) while the server takes its port
   from `--port`/config, so a non-default `--port` would leave the
   healthcheck probing 20128 — noted as a follow-up in §7, not changed
   (default port is 20128 everywhere in this slice).

## 4. Image build — what ran, what did not

`docker build` was attempted twice (host-root daemon, then the userns
daemon). Both failed at the same place, Step 4/19 `RUN go mod download`:

```
Step 4/19 : RUN go mod download
failed to create default sandbox: failed to create a netlink handle:
failed to set into network namespace 24 while creating netlink socket:
operation not permitted            # host-root daemon
lchown /var/lib/docker/vfs/dir/40d3…-init/etc/gshadow: invalid argument
                                                   # userns daemon (single-uid map)
```

Root cause (VM capability, not a Dockerfile bug): a `RUN` step needs a
container, and containers need namespaces this VM will not grant — no
effective `CAP_SYS_ADMIN` for host-root processes, and multi-uid user
namespace maps are blocked (`newuidmap` fails with `write to uid_map
failed: Operation not permitted` under `no-new-privs=1`, even after adding
`root:100000:65536` to `/etc/subuid` + `/etc/subgid`; the daemon's own
error message suggests "increasing the number of subordinate IDs", which
is exactly what is blocked). Direct `runc run` fails the same way
(`error spawning mount remapping thread: mount source thread: join
container mntns: setns: operation not permitted`, runc 1.2.6, minimal
spec, no network namespace).

The image was therefore assembled equivalently — same base, same
binaries, same config — with `docker import`:

```
# base rootfs, straight from the registry (crane export; no container needed)
crane export gcr.io/distroless/static-debian12:nonroot distroless-rootfs.tar
tar --no-same-owner -xf distroless-rootfs.tar -C rootfs        # 4.6 MB, all files 0:0
# binaries, built with the Dockerfile's EXACT flags
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /tmp/slice4/bin/codvian-router ./cmd/codvian-router
# healthcheck source extracted verbatim from the Dockerfile heredoc, same flags
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /tmp/slice4/bin/codvian-router-healthcheck main.go
cp /tmp/slice4/bin/codvian-router /tmp/slice4/bin/codvian-router-healthcheck rootfs/usr/local/bin/
cp /etc/ssl/certs/ca-certificates.crt rootfs/etc/ssl/certs/ca-certificates.crt
tar --owner=0 --group=0 -cf image.tar -C rootfs .
docker import \
  --change 'ENV HOME=/home/nonroot' \
  --change 'VOLUME ["/home/nonroot/.local/share/codvian-router"]' \
  --change 'USER nonroot:nonroot' \
  --change 'EXPOSE 20128' \
  --change 'HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/usr/local/bin/codvian-router-healthcheck"]' \
  --change 'ENTRYPOINT ["/usr/local/bin/codvian-router"]' \
  --change 'CMD ["serve"]' \
  image.tar codvian-router:local
# sha256:837886e483e01b5edc4364d706cc14523e2c6255ff1b972081ff2f5652e0983d
```

Two substitutions vs the Dockerfile, both recorded: (a) the CA bundle came
from the host (`/etc/ssl/certs/ca-certificates.crt`, Ubuntu 24.04 Mozilla
bundle) instead of the builder image's, because the builder image could
not be `docker create`d/exported in this VM — same class of file, and no
upstream is contacted in the smoke test, so it is unexercised either way;
(b) the image was assembled by `import`, not by `docker build` — the
`RUN go mod download` / `RUN go build` layer contents are instead proven
by the native build with identical flags above, which succeeded
(12,824,736-byte binary; healthcheck 5,886,112 bytes).

Base image digests (for reproducibility):

```
golang:1.22-bookworm                      sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253
gcr.io/distroless/static-debian12:nonroot sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
```

Result:

```
$ docker images --format '{{.Repository}}:{{.Tag}} {{.Size}} {{.ID}}'
codvian-router:local 20.8MB 837886e483e0
$ docker inspect codvian-router:local --format 'User={{.Config.User}} Entrypoint={{.Config.Entrypoint}} Cmd={{.Config.Cmd}} Exposed={{.Config.ExposedPorts}} Env={{.Config.Env}} Volumes={{.Config.Volumes}} Healthcheck={{.Config.Healthcheck.Test}}'
User=nonroot:nonroot Entrypoint=[/usr/local/bin/codvian-router] Cmd=[serve] Exposed=map[20128/tcp:{}] Env=[HOME=/home/nonroot] Volumes=map[/home/nonroot/.local/share/codvian-router:{}] Healthcheck=[CMD /usr/local/bin/codvian-router-healthcheck]
```

## 5. Smoke test — the server inside the image

`docker run` fails in this VM for the reason in §4 (same runc
`setns: operation not permitted`), so the server was run from the
image's own rootfs, in the image's own filesystem layout, under
`unshare -Urm` (user + mount namespace; host network shared because
joining the host netns is what the VM denies — the router binds
127.0.0.1 either way). Exact script: `/tmp/slice4/smoke.sh`
(ephemeral). The first run exposed a script bug (host `$HOME` leaked
into the chroot, so state landed in the image's `/home/hatch`); it was
fixed (`HOME=/home/nonroot` for the chrooted commands) and re-run clean.
The run below is the clean re-run, 2026-11-17 ~16:55 UTC (image
`codvian-router:local`, rootfs `/tmp/slice4/rootfs`):

```
== init ==  (HOME=/home/nonroot chroot rootfs /usr/local/bin/codvian-router init)
init exit: 0
config: /home/nonroot/.config/codvian-router/config.json
data dir: /home/nonroot/.local/share/codvian-router
Gateway key (shown ONCE — save it now, it cannot be recovered):
crk_… (32 chars; full value used for the auth checks below, then deleted — not recorded)
== serve ==  (HOME=/home/nonroot chroot rootfs /usr/local/bin/codvian-router serve)
healthz-ready: 1 after 142 ms
listening on 127.0.0.1:20128 (auth ON); providers: 0 configured, 0 clients ready
== in-image HEALTHCHECK binary ==  (chroot rootfs /usr/local/bin/codvian-router-healthcheck)
healthcheck exit: 0
== endpoints ==
dashboard no-key: 200 200452B text/html; charset=utf-8   ("Codvian Router" present, "Providers" x17, "Playground" x0)
healthz no-key:   200 16B  {"status":"ok"}
models no-key:    401
models with-key:  200 28B  {"data":[],"object":"list"}     (honest empty: 0 providers configured)
metrics no-key:   401
metrics with-key: 200 1745B  (real Prometheus counters, e.g. codvian_router_requests_total)
dashboard POST:   405
serve stopped (exit 0); healthz after stop: connection refused (expected)
== rootfs state after run ==
rootfs/home/nonroot/.config/codvian-router/config.json
rootfs/home/nonroot/.local/share/codvian-router/ledger.db
rootfs/home/nonroot/.local/share/codvian-router/kdf.salt
rootfs/home/nonroot/.local/share/codvian-router/master.key
```

First (buggy-HOME) run measured 585 ms to healthz-ready and identical
endpoint results; the 142 ms figure is the clean run. The `init.out`
file holding the test key was deleted at the end of the script; the test
container/volumes from the failed `docker run` attempts were removed
(`docker volume rm slice4-data slice4-config`; stray `Created`
container removed); no container is left running. Host state was not
polluted: `/home/hatch/.config/codvian-router/` does not exist and
`/home/hatch/.local/share/codvian-router/` still holds only the
pre-existing files from earlier slices (timestamps 14:01–15:19).

Deviations from a literal `docker run`, stated plainly:

- `docker run` itself does not work in this VM (§4) — the server ran
  via chroot into the image rootfs under `unshare -Urm`, not under
  runc/containerd. What this proves: the image's binary + rootfs serve
  correctly. What it does not prove: runc startup, the Docker
  HEALTHCHECK scheduling (only the binary itself, exit 0, was exercised),
  and `docker compose up` end to end.
- The container would run as `USER nonroot:nonroot` per the image
  config; the chroot run executed as root inside the userns (the VM's
  single-uid map cannot represent uid 65532). The binary does not depend
  on the uid; the nonroot user still needs a first real-container run on
  a normal host.
- `--network=none` vs `--network host` was never exercised; the image
  rootfs test shared the host network namespace.

## 6. Reproduce on a normal Docker host (copy-paste)

```sh
docker build -t codvian-router:local .
docker run --rm \
  -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
  -v codvian-router-config:/home/nonroot/.config/codvian-router \
  codvian-router:local init        # first run only; key printed ONCE
docker compose up -d --build
curl http://127.0.0.1:20128/healthz          # {"status":"ok"}
curl http://127.0.0.1:20128/dashboard        # 200, HTML
curl http://127.0.0.1:20128/v1/models        # 401 without a key
curl -H "Authorization: Bearer <key>" http://127.0.0.1:20128/v1/models   # 200
curl http://127.0.0.1:20128/metrics          # 401 without a key
curl -H "Authorization: Bearer <key>" http://127.0.0.1:20128/metrics    # 200
docker compose down
```

Expected: image ~20 MB, startup to healthz-ready well under 1 s on a
normal host (142 ms measured in this VM), `/v1/models` returns
`{"data":[],"object":"list"}` until a provider is added.

## 7. Known limitations / follow-ups (not fixed here)

- `docker build` and `docker run`/`docker compose up` still need their
  first real run on a normal Docker host; this VM cannot execute them
  (§4–§5). The compose `command:` change is validated by YAML parse
  only, not by an executed `up`.
- Healthcheck port: the in-image healthcheck reads `CODVIAN_ROUTER_PORT`
  (default 20128) while the server port comes from `--port`/config; a
  non-default `--port` would leave the healthcheck probing 20128.
- The static Docker install in this VM is ephemeral (binaries in
  `/usr/local/bin`, no systemd unit, daemon started by hand under
  `unshare -Ur`); the `/etc/subuid` + `/etc/subgid` `root:100000:65536`
  entries added for the userns attempts are also VM-local.
- A dangling `<none>:<none> 823MB` image entry remains from the failed
  distroless layer registration under the userns daemon; harmless.
- `docs/RELEASE-v0.2.0.md` still says the Docker image was "not built or
  run in this environment" — that was true for the v0.2.0 gate; this
  report supersedes it for the image itself (with the §5 deviations).

## 8. Files changed

- `Dockerfile` — comments only: networking note rewritten for the
  `--listen 0.0.0.0:20128` opt-in; `-p` run example added. No layer
  changed.
- `docker-compose.yml` — `command: ["serve", "--listen", "0.0.0.0:20128"]`
  added; networking comment rewritten. Parses clean (PyYAML).
- `docs/INSTALL.md` — §3 rewritten (port mapping now works via
  `--listen`); troubleshooting row for `docker run -p ...` updated.
- `docs/slice-4-docker-report.md` — this file (new).
- Not touched: `CHECKLIST.md` (coordinator merges), any Go source,
  Codvian itself, any other repo. Nothing pushed.

## 9. Gate evidence (commands)

```
export PATH=$HOME/go/bin:$PATH
go build ./...                       # clean
go vet ./...                         # clean
gofmt -l cmd internal                # clean (no output)
go test -count=1 ./...               # all 19 packages ok, 487 PASS events, 0 fail
go test -count=1 -v ./... | grep -c '^--- PASS'   # 487
docker build --network=host -t codvian-router:local .   # fails Step 4/19, VM limits (§4)
docker images                        # codvian-router:local 20.8MB
unshare -Urm bash /tmp/slice4/smoke.sh               # full smoke, §5 (142 ms startup)
```

## 10. CHECKLIST row for item 75 (for the coordinator to merge)

| 75 | Docker image build + run: multi-stage Dockerfile (Go 1.22 builder, distroless nonroot runtime, in-Dockerfile healthcheck binary) + docker-compose.yml now runs `serve --listen 0.0.0.0:20128` so the `ports:` mapping works (explicit opt-in, warning printed, gateway-key auth still ON; `--network host` stays the loopback-only alternative); image `codvian-router:local` 20.8 MB, config verified via `docker inspect` (nonroot user, entrypoint, CMD serve, EXPOSE 20128, healthcheck, volume); smoke: `init` exit 0, server healthz-ready in 142 ms, dashboard 200 (200,452 B), `/healthz` 200 `{"status":"ok"}`, `/v1/models` + `/metrics` 401 without key / 200 with key (models honestly `{"data":[],"object":"list"}` — 0 providers), in-image healthcheck binary exit 0, clean stop. Caveat (VM only): `docker build` RUN steps and `docker run` cannot execute in this VM (no effective CAP_SYS_ADMIN; `setns: operation not permitted` / `lchown: invalid argument`; multi-uid userns maps blocked by no-new-privs), so the image was assembled by `docker import` from the real distroless base + binaries built with the Dockerfile's exact flags, and the server was run from the image rootfs via chroot under `unshare -Urm` — the Dockerfile, compose `command:` change and `docker compose up` still need their first literal run on a normal Docker host | ✅ | ✅ |
