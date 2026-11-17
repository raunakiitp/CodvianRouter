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
#   4. verifies the tarball's SHA-256 against checksums.txt when that
#      file is available (a missing checksum file prints a loud warning
#      and continues — see below)
#   5. installs the binary, then prints the next steps.
#
# Requires: curl, tar, and sha256sum (Linux) or shasum (macOS).
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
BASE_URL="https://github.com/$REPO/releases/download/$TAG"

# --- download --------------------------------------------------------------
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT INT TERM

printf 'Downloading %s ...\n' "$ASSET"
curl -fsSL -o "$TMPDIR/$ASSET" "$BASE_URL/$ASSET" \
    || die "download failed: $BASE_URL/$ASSET (check the version exists at https://github.com/$REPO/releases)"

# --- checksum verification --------------------------------------------------
# checksums.txt is published by GoReleaser alongside every release.
# When it is present, the tarball MUST match or installation aborts.
# When it cannot be fetched, installation continues only after a loud
# warning — a release without checksums is unexpected and should be
# reported, but blocking install entirely would strand users if GitHub
# serves a transient error for that one file.
if curl -fsSL -o "$TMPDIR/checksums.txt" "$BASE_URL/checksums.txt" 2>/dev/null; then
    EXPECTED="$(grep "  $ASSET\$" "$TMPDIR/checksums.txt" | awk '{print $1}')"
    [ -n "$EXPECTED" ] || die "checksums.txt does not list $ASSET — refusing to install an unverified binary."
    ACTUAL="$(cd "$TMPDIR" && $SHA256 "$ASSET" | awk '{print $1}')"
    if [ "$ACTUAL" != "$EXPECTED" ]; then
        die "SHA-256 MISMATCH for $ASSET (expected $EXPECTED, got $ACTUAL). The download may be corrupt or tampered with — refusing to install."
    fi
    printf 'Checksum verified (sha256).\n'
else
    printf 'WARNING: could not download checksums.txt for %s — installing WITHOUT checksum verification.\n' "$TAG"
    printf 'This is unexpected; please report it at https://github.com/%s/issues.\n' "$REPO"
fi

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
