#!/bin/sh
# test-image-signing.sh — local proof for Docker image signing (items 221-222).
#
# What it proves, end to end, with real tools on this machine:
#   1. A real image of the router can be assembled the way
#      Dockerfile.release describes (distroless base + the router
#      binary built with the Dockerfile's exact flags + healthcheck
#      helper + CA certs + entrypoint/cmd/user config) and pushed to
#      a registry — here a throwaway local registry, since this VM
#      has no Docker daemon.
#   2. cosign signs that image (throwaway local keypair — the same
#      test-key mode the 209-210 slice used for the release blobs).
#   3. scripts/verify-image.sh --key verifies the untampered image
#      (exit 0, VERIFIED).
#   4. A TAMPERED image pushed under the SAME tag FAILS verification
#      (the signature is bound to the image digest, not the tag).
#   5. The ORIGINAL digest still verifies after the tamper (proving
#      the signature is digest-bound), and a WRONG key fails.
#
# What it does NOT prove (honest limitation, same as items 209-210):
# the KEYLESS path — cosign keyless needs GitHub Actions OIDC
# (id-token: write) and Sigstore's write path (Rekor/timestamp), and
# Sigstore POSTs hang from this VM. The keyless signing config is
# validated by `goreleaser check` and by the release workflow's own
# verify job; the first real tag is the first live proof of it.
#
# Usage:
#   sh scripts/test-image-signing.sh [KEEP=1] [COSIGN_BIN=...] [GO_BIN=...]
# Environment:
#   COSIGN_BIN   cosign binary (default: cosign on PATH). v2 and v3
#                both work: v2 signs a keypair with --tlog-upload=false;
#                v3 removed that flag, so the script signs with a
#                signing config stripped of Rekor + timestamp services
#                (built from Sigstore's public config — keypair
#                signing needs neither service). This VM has cosign
#                v2.4.3 at $HOME/.tmp/bin/cosign-v2 and v3.1.4 at
#                $HOME/.tmp/bin/cosign; both are exercised by the gate.
#   CRANE_BIN / REGISTRY_BIN
#                crane / registry binaries (default: crane / registry
#                on PATH; if missing, installed with `go install`
#                pinned to go-containerregistry v0.21.3).
#   GO_BIN       go binary (default: go on PATH).
#   KEEP=1       keep the work directory (default: removed on exit).
#   WORKDIR      work directory (default: mktemp under $TMPDIR).
#
# The throwaway keypair is generated in the work directory with the
# test-only password "codvian-test-only" and deleted with it. Never
# use these keys for anything real.
#
# Exit status is 0 only when every check behaved as expected
# (untampered verifies, tampered / wrong-key do not).

set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMPBASE="${TMPDIR:-$HOME/.tmp}"
WORKDIR="${WORKDIR:-$(mktemp -d "$TMPBASE/image-signing-proof-XXXXXX")}"
KEEP="${KEEP:-0}"

GO_BIN="${GO_BIN:-go}"
CRANE_BIN="${CRANE_BIN:-crane}"
REGISTRY_BIN="${REGISTRY_BIN:-registry}"
COSIGN_BIN="${COSIGN_BIN:-cosign}"

REGISTRY_PORT="${REGISTRY_PORT:-5000}"
IMAGE="localhost:$REGISTRY_PORT/codvian-router:test"
BASE_IMAGE="gcr.io/distroless/static-debian12:nonroot"
BASE_REF="localhost:$REGISTRY_PORT/codvian-router-base:nonroot"

FAILURES=0

pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

# expect_ok <label> <command...> — the command must exit 0.
expect_ok() {
    label="$1"; shift
    if "$@" >/dev/null 2>&1; then pass "$label"; else fail "$label (expected success, got failure)"; fi
}
# expect_fail <label> <command...> — the command must exit non-zero.
expect_fail() {
    label="$1"; shift
    if "$@" >/dev/null 2>&1; then fail "$label (expected failure, got success)"; else pass "$label"; fi
}

cleanup() {
    if [ -n "${REGISTRY_PID:-}" ]; then kill "$REGISTRY_PID" 2>/dev/null || true; fi
    if [ "$KEEP" = "1" ]; then
        printf 'work directory kept: %s\n' "$WORKDIR"
    else
        rm -rf "$WORKDIR"
    fi
}
trap cleanup EXIT INT TERM

command -v "$GO_BIN" >/dev/null 2>&1 || { printf 'FAIL: go not found (GO_BIN=%s)\n' "$GO_BIN"; exit 1; }
command -v "$COSIGN_BIN" >/dev/null 2>&1 || { printf 'FAIL: cosign not found (COSIGN_BIN=%s) — install it: https://docs.sigstore.dev/cosign/installation/\n' "$COSIGN_BIN"; exit 1; }
command -v curl >/dev/null 2>&1 || { printf 'FAIL: curl is required\n'; exit 1; }

# crane + registry: install pinned versions if they are not on PATH.
if ! command -v "$CRANE_BIN" >/dev/null 2>&1; then
    printf 'crane not found — installing go-containerregistry v0.21.3 ...\n'
    GOPATH_BIN="$("$GO_BIN" env GOPATH)/bin"
    "$GO_BIN" install github.com/google/go-containerregistry/cmd/crane@v0.21.3 \
        || { printf 'FAIL: could not install crane\n'; exit 1; }
    CRANE_BIN="$GOPATH_BIN/crane"
fi
if ! command -v "$REGISTRY_BIN" >/dev/null 2>&1; then
    printf 'registry not found — installing go-containerregistry v0.21.3 ...\n'
    GOPATH_BIN="$("$GO_BIN" env GOPATH)/bin"
    "$GO_BIN" install github.com/google/go-containerregistry/cmd/registry@v0.21.3 \
        || { printf 'FAIL: could not install registry\n'; exit 1; }
    REGISTRY_BIN="$GOPATH_BIN/registry"
fi

mkdir -p "$WORKDIR/layer/usr/local/bin" "$WORKDIR/layer/etc/ssl/certs"
cd "$WORKDIR" || exit 1

printf 'work directory: %s\n' "$WORKDIR"
printf 'cosign: %s\n' "$("$COSIGN_BIN" version 2>&1 | grep -i 'GitVersion' | head -1)"
printf 'crane: %s\n' "$("$CRANE_BIN" version 2>&1 | head -1)"

# --- A. build the binaries with the Dockerfile's exact flags -------------
printf '\n-- building router binary (Dockerfile flags) --\n'
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build \
    -trimpath -ldflags "-s -w" \
    -o "$WORKDIR/layer/usr/local/bin/codvian-router" ./cmd/codvian-router) \
    || { fail "router binary build"; exit 1; }
pass "router binary built (linux/amd64, CGO_ENABLED=0, -trimpath -s -w)"

# Healthcheck helper: source is inline in Dockerfile — extract it so
# there is exactly one source of truth, then build it the same way.
sed -n "/cat > \/tmp\/healthcheck\/main.go <<'EOF'/,/^EOF$/p" "$REPO_ROOT/Dockerfile" \
    | sed '1d;$d' > "$WORKDIR/healthcheck.go"
[ -s "$WORKDIR/healthcheck.go" ] || { fail "healthcheck source extraction from Dockerfile"; exit 1; }
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO_BIN" build -trimpath -ldflags "-s -w" \
    -o "$WORKDIR/layer/usr/local/bin/codvian-router-healthcheck" "$WORKDIR/healthcheck.go" \
    || { fail "healthcheck binary build"; exit 1; }
pass "healthcheck binary built from Dockerfile inline source"

# The binary in the image is the real router: it must answer `help`.
expect_ok "router binary answers 'help' (exit 0)" "$WORKDIR/layer/usr/local/bin/codvian-router" help

cp /etc/ssl/certs/ca-certificates.crt "$WORKDIR/layer/etc/ssl/certs/ca-certificates.crt" \
    || { fail "CA certificates copy"; exit 1; }
chmod 0755 "$WORKDIR/layer/usr/local/bin/codvian-router" "$WORKDIR/layer/usr/local/bin/codvian-router-healthcheck"
chmod 0644 "$WORKDIR/layer/etc/ssl/certs/ca-certificates.crt"
(cd "$WORKDIR/layer" && tar --sort=name --owner=0 --group=0 --numeric-owner \
    -cf "$WORKDIR/layer.tar" .) || { fail "layer tarball"; exit 1; }
pass "layer tarball built (router + healthcheck + CA certs)"

# --- B. assemble + push the image on a local registry --------------------
printf '\n-- starting local registry on :%s --\n' "$REGISTRY_PORT"
"$REGISTRY_BIN" -port "$REGISTRY_PORT" >"$WORKDIR/registry.log" 2>&1 &
REGISTRY_PID=$!
ready=0
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -fsSL "http://127.0.0.1:$REGISTRY_PORT/v2/" >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
done
[ "$ready" = "1" ] || { fail "local registry did not answer on :$REGISTRY_PORT"; cat "$WORKDIR/registry.log"; exit 1; }
pass "local registry up on :$REGISTRY_PORT"

printf '\n-- pulling distroless base --\n'
"$CRANE_BIN" pull --platform linux/amd64 "$BASE_IMAGE" "$WORKDIR/base.tar" \
    || { fail "crane pull $BASE_IMAGE"; exit 1; }
pass "distroless base pulled ($BASE_IMAGE)"
"$CRANE_BIN" push "$WORKDIR/base.tar" "$BASE_REF" --insecure \
    || { fail "crane push base to local registry"; exit 1; }

"$CRANE_BIN" append -b "$BASE_REF" -f "$WORKDIR/layer.tar" -t "$IMAGE" --insecure \
    || { fail "crane append layer"; exit 1; }
# Same config as Dockerfile/Dockerfile.release (minus HEALTHCHECK,
# which crane cannot express — the instruction lives in the
# Dockerfiles and is verified by inspection, not by this proof).
"$CRANE_BIN" mutate --entrypoint /usr/local/bin/codvian-router --cmd serve \
    --user 65532:65532 --env HOME=/home/nonroot --exposed-ports 20128 \
    -t "$IMAGE" --insecure "$IMAGE" \
    || { fail "crane mutate config"; exit 1; }
pass "image assembled + pushed to $IMAGE"

DIGEST1="$("$CRANE_BIN" digest "$IMAGE" --insecure)" \
    || { fail "crane digest"; exit 1; }
printf 'original digest: %s\n' "$DIGEST1"

# The pushed image really is the router image: entrypoint + user + port.
# (crane records the exposed port as "20128", Docker normalises it to
# "20128/tcp" — accept either.)
"$CRANE_BIN" config "$IMAGE" --insecure > "$WORKDIR/config.json" \
    || { fail "crane config"; exit 1; }
if python3 - "$WORKDIR/config.json" <<'EOF'
import json, sys
cfg = json.load(open(sys.argv[1]))
assert cfg["config"]["Entrypoint"] == ["/usr/local/bin/codvian-router"], cfg["config"].get("Entrypoint")
assert cfg["config"]["Cmd"] == ["serve"], cfg["config"].get("Cmd")
assert cfg["config"]["User"] == "65532:65532", cfg["config"].get("User")
ports = cfg["config"].get("ExposedPorts") or {}
assert "20128" in ports or "20128/tcp" in ports, ports
EOF
then
    pass "image config: entrypoint=/usr/local/bin/codvian-router, cmd=serve, user=65532:65532, port 20128"
else
    fail "image config is not the router image (entrypoint/cmd/user/port)"
fi

# --- C. throwaway keypair + sign ------------------------------------------
printf '\n-- signing with a throwaway local keypair --\n'
export COSIGN_PASSWORD="codvian-test-only"
"$COSIGN_BIN" generate-key-pair --output-key-prefix "$WORKDIR/cosign" >/dev/null 2>&1 \
    || { fail "cosign generate-key-pair"; exit 1; }
pass "throwaway keypair generated (test-only, deleted with the work directory)"

# cosign v3 removed --tlog-upload: keypair signing there needs a
# signing config with no transparency-log / timestamp services. The
# public Sigstore config minus rekorTlogUrls + tsaUrls is exactly
# that (keypair signing mints no certificate, so Fulcio/OIDC are
# never contacted). v2 keeps the --tlog-upload=false form.
COSIGN_MAJOR="$("$COSIGN_BIN" version 2>/dev/null | grep -i 'GitVersion' | head -1 | sed 's/.*[vV]\([0-9][0-9]*\)\..*/\1/')"
SIGN_ARGS="--tlog-upload=false"
if [ "${COSIGN_MAJOR:-0}" -ge 3 ] 2>/dev/null; then
    curl -fsSL https://raw.githubusercontent.com/sigstore/root-signing/refs/heads/main/targets/signing_config.v0.2.json \
        -o "$WORKDIR/signing-config-public.json" \
        || { fail "download of Sigstore public signing config (needed for cosign v3 keypair signing)"; exit 1; }
    python3 - "$WORKDIR/signing-config-public.json" "$WORKDIR/signing-config.json" <<'EOF' \
        || { fail "strip Rekor + timestamp services from signing config"; exit 1; }
import json, sys
with open(sys.argv[1]) as f:
    cfg = json.load(f)
cfg.pop("rekorTlogUrls", None)
cfg.pop("tsaUrls", None)
with open(sys.argv[2], "w") as f:
    json.dump(cfg, f)
EOF
    SIGN_ARGS="--signing-config $WORKDIR/signing-config.json"
    pass "cosign v$COSIGN_MAJOR: signing config built without Rekor/timestamp services"
fi

# shellcheck disable=SC2086
"$COSIGN_BIN" sign --key "$WORKDIR/cosign.key" $SIGN_ARGS \
    --allow-insecure-registry "$IMAGE" >/dev/null 2>&1 \
    || { fail "cosign sign (keypair mode)"; exit 1; }
pass "image signed (keypair mode)"

# --- D. verify untampered --------------------------------------------------
printf '\n-- verifying untampered image --\n'
expect_ok "verify-image.sh --key verifies untampered image" \
    sh "$REPO_ROOT/scripts/verify-image.sh" --image "$IMAGE" --key "$WORKDIR/cosign.pub"
expect_ok "cosign verify --key verifies untampered image" \
    "$COSIGN_BIN" verify --key "$WORKDIR/cosign.pub" --insecure-ignore-tlog \
    --allow-insecure-registry "$IMAGE"

# --- E. tamper: same tag, different bytes ---------------------------------
printf '\n-- tampering: rebuilding the layer with a modified binary --\n'
printf 'TAMPERED' >> "$WORKDIR/layer/usr/local/bin/codvian-router"
(cd "$WORKDIR/layer" && tar --sort=name --owner=0 --group=0 --numeric-owner \
    -cf "$WORKDIR/layer-tampered.tar" .) || { fail "tampered layer tarball"; exit 1; }
"$CRANE_BIN" append -b "$BASE_REF" -f "$WORKDIR/layer-tampered.tar" -t "$IMAGE" --insecure \
    || { fail "crane append tampered layer"; exit 1; }
"$CRANE_BIN" mutate --entrypoint /usr/local/bin/codvian-router --cmd serve \
    --user 65532:65532 --env HOME=/home/nonroot --exposed-ports 20128 \
    -t "$IMAGE" --insecure "$IMAGE" \
    || { fail "crane mutate tampered image"; exit 1; }
DIGEST2="$("$CRANE_BIN" digest "$IMAGE" --insecure)" \
    || { fail "crane digest (tampered)"; exit 1; }
printf 'tampered digest: %s\n' "$DIGEST2"
[ "$DIGEST1" != "$DIGEST2" ] || { fail "tampered image has the same digest as the original (tamper did not change the bytes?)"; exit 1; }
pass "tampered image has a different digest ($DIGEST1 -> $DIGEST2)"

expect_fail "verify-image.sh --key REJECTS tampered image under the same tag" \
    sh "$REPO_ROOT/scripts/verify-image.sh" --image "$IMAGE" --key "$WORKDIR/cosign.pub"
expect_fail "cosign verify --key REJECTS tampered image under the same tag" \
    "$COSIGN_BIN" verify --key "$WORKDIR/cosign.pub" --insecure-ignore-tlog \
    --allow-insecure-registry "$IMAGE"

# --- F. the original digest still verifies (signature is digest-bound) -----
expect_ok "cosign verify --key still verifies the ORIGINAL digest after the tamper" \
    "$COSIGN_BIN" verify --key "$WORKDIR/cosign.pub" --insecure-ignore-tlog \
    --allow-insecure-registry "$IMAGE@$DIGEST1"

# --- G. wrong key fails ----------------------------------------------------
"$COSIGN_BIN" generate-key-pair --output-key-prefix "$WORKDIR/other" >/dev/null 2>&1 \
    || { fail "second keypair generation"; exit 1; }
expect_fail "cosign verify with a WRONG key fails" \
    "$COSIGN_BIN" verify --key "$WORKDIR/other.pub" --insecure-ignore-tlog \
    --allow-insecure-registry "$IMAGE@$DIGEST1"

# --- summary ---------------------------------------------------------------
printf '\n'
if [ "$FAILURES" = "0" ]; then
    printf 'ALL IMAGE-SIGNING CHECKS PASSED\n'
    printf '  original digest: %s\n  tampered digest: %s\n' "$DIGEST1" "$DIGEST2"
    exit 0
else
    printf '%s CHECK(S) FAILED\n' "$FAILURES"
    exit 1
fi
