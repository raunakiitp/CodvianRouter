# Codvian Router — container image.
#
# Multi-stage build: Go toolchain compiles a static binary, and the
# runtime image is distroless (no shell, no package manager) holding
# only the router binary, a tiny healthcheck helper, and CA certs.
#
# Build:
#   docker build -t codvian-router:local .
#
# First run (creates config + first gateway key; the key is printed once):
#   docker run --rm -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
#     -v codvian-router-config:/home/nonroot/.config/codvian-router \
#     codvian-router:local init
# Then serve (see docker-compose.yml for the composed form):
#   docker run -d --network host \
#     -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
#     -v codvian-router-config:/home/nonroot/.config/codvian-router \
#     codvian-router:local
# or with a published port (binds 0.0.0.0 via the binary's explicit opt-in;
# gateway-key auth stays ON, a warning is printed on startup):
#   docker run -d -p 127.0.0.1:20128:20128 \
#     -v codvian-router-data:/home/nonroot/.local/share/codvian-router \
#     -v codvian-router-config:/home/nonroot/.config/codvian-router \
#     codvian-router:local serve --listen 0.0.0.0:20128
#
# Both volumes are required on EVERY run: the data volume holds the
# ledger + vault secret, the config volume holds config.json with the
# gateway key created by `init`. Omitting the config volume makes
# `serve` LoadOrCreate a fresh empty config (bootstrap PENDING) and
# the key printed by `init` is dead (PKG-1). The VOLUME declaration
# below also persists both dirs as anonymous volumes if -v is omitted.
#
# NETWORKING NOTE — read this before using `-p` port publishing:
# the router binds 127.0.0.1 by default. A process bound to a container's
# loopback cannot be reached through Docker's published-port forwarding,
# which delivers traffic to the container's eth0 address — so
# `-p 127.0.0.1:20128:20128` does NOT reach the server with the default
# `serve`. Either run with `--network host` (the container shares the
# host's loopback), or pass `--listen 0.0.0.0:20128` (the only non-loopback
# bind the binary accepts; see effectiveListen in cmd/codvian-router).
# The EXPOSE below is documentation only; the healthcheck runs inside the
# container against 127.0.0.1 and works in either mode.

# ---------------------------------------------------------------------------
# Builder
# ---------------------------------------------------------------------------
FROM golang:1.27.4-bookworm@sha256:ce59b41415e2524ae99926fe1f37ce48ba8c194dcbd2a56a65436cdcefa2bd34 AS builder

WORKDIR /src

# Download modules first so the layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary: CGO_ENABLED=0 (the module uses modernc.org/sqlite, a pure-Go
# SQLite implementation, so no C toolchain is needed). -trimpath and -s -w
# keep the binary small and reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/codvian-router ./cmd/codvian-router

# Tiny healthcheck helper. Distroless has no shell, curl, or wget, so the
# HEALTHCHECK needs a real binary. It GETs http://127.0.0.1:20128/readyz
# (unauthenticated; readiness, not mere liveness — AUD-CFG-004) and exits
# 0 only on HTTP 200. It lives in this
# Dockerfile — not in the repo — so it never touches Go source ownership.
RUN mkdir -p /tmp/healthcheck && cat > /tmp/healthcheck/main.go <<'EOF'
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("CODVIAN_ROUTER_PORT")
	if port == "" {
		port = "20128"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	// /readyz, not /healthz (AUD-CFG-004): the container healthcheck
	// must gate on READINESS — the ledger (when wired) actually
	// answering — not mere process liveness. 503 = unready, and the
	// retries give the process time to become ready.
	resp, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: unexpected status %s\n", resp.Status)
		os.Exit(1)
	}
}
EOF
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/codvian-router-healthcheck /tmp/healthcheck/main.go

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# CA certificates (copied from the builder; distroless/static has no
# package manager) so the router can reach upstream providers over TLS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /out/codvian-router /usr/local/bin/codvian-router
COPY --from=builder /out/codvian-router-healthcheck /usr/local/bin/codvian-router-healthcheck

# The nonroot user (uid 65532) in distroless has HOME /home/nonroot.
# The router's default data dir is $HOME/.local/share/codvian-router
# (ledger.db, vault master secret); the config lives in
# $HOME/.config/codvian-router/config.json. Persist both with volumes —
# losing the data dir loses the vault secret and the ledger; losing the
# config dir loses the gateway key created by `init` (PKG-1).
ENV HOME=/home/nonroot
VOLUME ["/home/nonroot/.local/share/codvian-router", "/home/nonroot/.config/codvian-router"]

USER nonroot:nonroot

EXPOSE 20128

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/codvian-router-healthcheck"]

# ENTRYPOINT + CMD so `docker run <image> init` / `keygen` work too;
# the default is `serve` on 127.0.0.1:20128 with auth ON.
ENTRYPOINT ["/usr/local/bin/codvian-router"]
CMD ["serve"]
