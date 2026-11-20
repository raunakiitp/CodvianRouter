#!/bin/sh
# Codvian Router — installer.
#
# Downloads the matching prebuilt release from GitHub and installs it to
# /usr/local/bin (or $PREFIX/bin). This is a FILE you download and run:
#
#   curl -fsSLO https://raw.githubusercontent.com/raunakiitp/CodvianRouter/main/scripts/install.sh
#   sh install.sh            # or: sh install.sh v0.1.0
#
# It is NOT a pipe-to-shell script: running it as `curl ... | sh` is
# refused on purpose (stdin is not a terminal in that case). Pass --yes
# or set CODVIAN_ROUTER_ALLOW_NON_TTY=1 only if you understand that and
# still want to run it with stdin redirected.
#
# What it does, in order:
#   1. detects OS (linux/darwin) and CPU (amd64/arm64)
#   2. resolves the release version (argument, $CODVIAN_ROUTER_VERSION,
#      or the latest GitHub release)
#   3. downloads the release tarball + checksums.txt from
#      https://github.com/raunakiitp/CodvianRouter/releases
#   4. verifies the Sigstore cosign keyless signature over checksums.txt
#      when cosign is installed (and refuses the install when the
#      signature check fails — a checksums file without a valid signature
#      proves nothing, since anyone who can swap the tarball can swap
#      the checksums too). Without cosign it warns loudly and continues
#      with checksum-only verification, unless
#      CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is set (then it refuses).
#      Verify the release fully BEFORE installing with
#      scripts/verify-release.sh — see docs/RELEASE-SIGNING.md.
#   5. verifies the tarball's SHA-256 against checksums.txt, which is
#      retried (3 attempts, backoff) and then FAILS CLOSED: a release
#      whose manifest cannot be fetched is never installed unverified.
#   6. installs the binary, then prints the next steps.
#
# Requires: curl, tar, and sha256sum (Linux) or shasum (macOS).
# Recommended: cosign (https://docs.sigstore.dev/cosign/installation/)
# for the signature check in step 4.
# Refuses to run on unsupported platforms instead of installing the
# wrong binary. Never writes outside $PREFIX/bin.

set -eu

REPO="raunakiitp/CodvianRouter"
PREFIX="${PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"

die() {
    printf 'codvian-router install: %s\n' "$*" >&2
    exit 1
}

# --- pipe-to-shell guard -------------------------------------------------
# When piped from curl, stdin is the pipe, not a terminal. Refuse unless
# the user explicitly opts out of the guard.
case "${1:-}" in
    --yes|-y) ALLOW_NON_TTY=1; shift ;;
esac
if [ "${CODVIAN_ROUTER_ALLOW_NON_TTY:-0}" != "1" ] && [ "${ALLOW_NON_TTY:-0}" != "1" ]; then
    if [ ! -t 0 ]; then
        die "refusing to run with stdin piped/redirected. Download this file first, then run it:
  curl -fsSLO https://raw.githubusercontent.com/$REPO/main/scripts/install.sh
  sh install.sh
Or re-run with --yes if you really mean to run it non-interactively."
    fi
fi

# --- prerequisites --------------------------------------------------------
command -v curl >/dev/null 2>&1 || die "curl is required but not found in PATH."
command -v tar >/dev/null 2>&1 || die "tar is required but not found in PATH."
if command -v sha256sum >/dev/null 2>&1; then
    SHA256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    SHA256="shasum -a 256"
else
    die "sha256sum (Linux) or shasum (macOS) is required but neither was found."
fi

# --- platform detection ---------------------------------------------------
OS="$(uname -s)"
case "$OS" in
    Linux)  GOOS="linux" ;;
    Darwin) GOOS="darwin" ;;
    *) die "unsupported operating system: $OS (only Linux and macOS are supported). Build from source instead:
  git clone https://github.com/$REPO.git && cd CodvianRouter && go build -o codvian-router ./cmd/codvian-router" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *) die "unsupported CPU architecture: $ARCH (only amd64 and arm64 are supported). Build from source instead:
  git clone https://github.com/$REPO.git && cd CodvianRouter && go build -o codvian-router ./cmd/codvian-router" ;;
esac

# --- version resolution ---------------------------------------------------
# Argument ($1) wins, then $CODVIAN_ROUTER_VERSION, then the latest release.
VERSION="${1:-${CODVIAN_ROUTER_VERSION:-latest}}"
if [ "$VERSION" = "latest" ]; then
    printf 'Resolving the latest release...\n'
    TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$TAG" ] || die "could not resolve the latest release from the GitHub API. Pass a version explicitly: sh install.sh v0.1.0"
    VERSION="$TAG"
fi
# Normalise: accept "0.1.0" or "v0.1.0"; the tag is always v-prefixed.
case "$VERSION" in
    v*) TAG="$VERSION" ;;
    *)  TAG="v$VERSION" ;;
esac

ASSET="codvian-router_${TAG#v}_${GOOS}_${GOARCH}.tar.gz"
# CODVIAN_ROUTER_DOWNLOAD_BASE_URL is testing-only (used by the
# bin/codvian-router.test.js harness to point the installer at a local
# server serving controlled artifacts). Whatever it points at is still
# signature- + checksum-verified exactly as a GitHub download would be.
DOWNLOAD_BASE="${CODVIAN_ROUTER_DOWNLOAD_BASE_URL:-https://github.com/$REPO/releases/download}"
BASE_URL="$DOWNLOAD_BASE/$TAG"

# --- download --------------------------------------------------------------
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT INT TERM

printf 'Downloading %s ...\n' "$ASSET"
curl -fsSL -o "$TMPDIR/$ASSET" "$BASE_URL/$ASSET" \
    || die "download failed: $BASE_URL/$ASSET (check the version exists at https://github.com/$REPO/releases)"

# --- checksum verification --------------------------------------------------
# checksums.txt is published by GoReleaser alongside every release.
# The tarball MUST match it or installation aborts. The manifest is
# retried (3 attempts, backoff) and then the install FAILS CLOSED: a
# release whose checksums cannot be fetched is never installed
# unverified. A transient GitHub error is absorbed by the retries; a
# persistently missing manifest means the release cannot be
# authenticated, so refusing is the only safe answer.
# --- signature verification (Sigstore cosign, keyless) -------------------
# A checksum only proves the tarball matches checksums.txt; it says
# nothing about who wrote checksums.txt. The cosign signature over
# checksums.txt is what binds the manifest to this project's release
# workflow for this exact tag (see docs/RELEASE-SIGNING.md). It runs
# BEFORE the tarball's hash is trusted, so a failed signature aborts
# before any checksum comparison.
#
# If cosign is not installed, the install continues with checksum-only
# verification and a loud warning — except when
# CODVIAN_ROUTER_REQUIRE_SIGNATURE=1, which refuses instead. The
# recommended path is to verify the release first with
# scripts/verify-release.sh, which always requires cosign.
# The manifest download is retried (3 attempts, 2s/4s backoff) and
# then the install FAILS CLOSED — see the comment above.
MANIFEST_OK=0
ATTEMPT=1
while [ "$ATTEMPT" -le 3 ]; do
    if curl -fsSL -o "$TMPDIR/checksums.txt" "$BASE_URL/checksums.txt" 2>/dev/null; then
        MANIFEST_OK=1
        break
    fi
    if [ "$ATTEMPT" -lt 3 ]; then
        printf 'Could not download checksums.txt (attempt %s/3) — retrying in %s seconds...\n' "$ATTEMPT" "$((ATTEMPT * 2))" >&2
        sleep $((ATTEMPT * 2))
    fi
    ATTEMPT=$((ATTEMPT + 1))
done
[ "$MANIFEST_OK" = "1" ] || die "could not download checksums.txt for $TAG after 3 attempts — refusing to install a release whose manifest cannot be checked. Retry later, or report it at https://github.com/$REPO/issues."

if command -v cosign >/dev/null 2>&1; then
    curl -fsSL -o "$TMPDIR/checksums.txt.sigstore.json" "$BASE_URL/checksums.txt.sigstore.json" \
        || die "could not download checksums.txt.sigstore.json for $TAG — refusing to install a release whose signature cannot be checked."
    cosign verify-blob \
        --certificate-identity "https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$TAG" \
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
        --bundle "$TMPDIR/checksums.txt.sigstore.json" "$TMPDIR/checksums.txt" >/dev/null \
        || die "cosign signature verification FAILED for checksums.txt — the manifest is not from the Codvian Router release workflow for $TAG. Refusing to install."
    printf 'Signature verified (Sigstore cosign, keyless).\n'
elif [ "${CODVIAN_ROUTER_REQUIRE_SIGNATURE:-0}" = "1" ]; then
    die "CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is set but cosign is not installed. Install cosign (https://docs.sigstore.dev/cosign/installation/) and re-run, or unset the variable to fall back to checksum-only verification."
else
    printf 'WARNING: cosign not found — installing with checksum-only verification (the checksums.txt itself is unverified).\n'
    printf 'To verify the release signature first, install cosign and run: sh scripts/verify-release.sh %s\n' "$TAG"
fi
EXPECTED="$(grep "  $ASSET\$" "$TMPDIR/checksums.txt" | awk '{print $1}')"
[ -n "$EXPECTED" ] || die "checksums.txt does not list $ASSET — refusing to install an unverified binary."
ACTUAL="$(cd "$TMPDIR" && $SHA256 "$ASSET" | awk '{print $1}')"
if [ "$ACTUAL" != "$EXPECTED" ]; then
    die "SHA-256 MISMATCH for $ASSET (expected $EXPECTED, got $ACTUAL). The download may be corrupt or tampered with — refusing to install."
fi
printf 'Checksum verified (sha256).\n'

# --- install ---------------------------------------------------------------
tar -xzf "$TMPDIR/$ASSET" -C "$TMPDIR" \
    || die "could not extract $ASSET."
[ -f "$TMPDIR/codvian-router" ] || die "the archive did not contain a codvian-router binary."

mkdir -p "$BIN_DIR"
if [ -w "$BIN_DIR" ]; then
    INSTALL_CMD="cp"
    cp "$TMPDIR/codvian-router" "$BIN_DIR/codvian-router"
else
    if command -v sudo >/dev/null 2>&1; then
        printf '%s is not writable; using sudo to install.\n' "$BIN_DIR"
        sudo cp "$TMPDIR/codvian-router" "$BIN_DIR/codvian-router"
    else
        die "$BIN_DIR is not writable and sudo is not available. Re-run as root, or set PREFIX to a writable directory, e.g.: PREFIX=\$HOME/.local sh install.sh"
    fi
fi
chmod 755 "$BIN_DIR/codvian-router"

printf '\nInstalled codvian-router %s to %s/codvian-router\n' "$TAG" "$BIN_DIR"
printf '\nNext steps:\n'
printf '  1. Create a config and your first gateway key (the key is shown ONCE):\n'
printf '       codvian-router init\n'
printf '  2. Start the router (listens on 127.0.0.1:20128, auth ON):\n'
printf '       codvian-router serve\n'
printf '  3. Check it is up:\n'
printf '       curl http://127.0.0.1:20128/healthz\n'
printf '  4. Add providers to ~/.config/codvian-router/config.json, then send requests with\n'
printf '     Authorization: Bearer <your gateway key>.\n'
case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) printf '\nNote: %s is not in your PATH yet. Add it, or run %s/codvian-router directly.\n' "$BIN_DIR" "$BIN_DIR" ;;
esac
