#!/bin/sh
# verify-release.sh — verify a Codvian Router release BEFORE installing it.
#
# What it checks, in order (each step must pass or the script stops):
#   1. Sigstore cosign keyless signature over checksums.txt — proves the
#      manifest was produced by this project's release workflow for this
#      exact tag (identity pinned below; a signature from any other
#      workflow, repo, or tag is rejected). Without this step a
#      checksums.txt is just a text file anyone could have written.
#   2. SHA-256 of the release archive against checksums.txt — proves the
#      bytes you downloaded are the bytes the release built.
#   3. Sigstore keyless signature over the archive's SBOM, and that the
#      SBOM is valid JSON — proves the SBOM came from the same release.
#   4. SLSA build-provenance attestation (`gh attestation verify`) when
#      the `gh` CLI is present — proves GitHub Actions built the artifact
#      from this repo. Skipped with a note when `gh` is absent.
#
# Usage:
#   sh scripts/verify-release.sh v0.1.0 [--dir DIR] [--skip-attestation]
#       Download the release for this machine from GitHub and verify it.
#       Files go to a temporary directory unless --dir is given (then
#       they are kept there for inspection; install from that directory
#       only after this script prints VERIFIED).
#   sh scripts/verify-release.sh --local DIR [--key PUBKEY] [--skip-attestation]
#       Verify files already on disk in DIR (no download). DIR must hold
#       checksums.txt, its Sigstore bundle, the archive(s), and the SBOM(s).
#       With --key, verify the bundles against a local cosign public key
#       instead of the keyless workflow identity — this is how the
#       pipeline's own tamper test exercises this script (see
#       docs/RELEASE-SIGNING.md); real releases always use the keyless
#       bundles and need no --key.
#
# Exit status is 0 only when every applicable check passed. On ANY
# failure: do not install the files. Report it at
# https://github.com/raunakiitp/CodvianRouter/issues.
#
# Requires: curl (remote mode), cosign, sha256sum (Linux) or shasum
# (macOS). Optional: python3 or jq (SBOM JSON check), gh (attestation).
# cosign: https://docs.sigstore.dev/cosign/installation/
#
# No release has been published yet — until the first release exists
# this script can only be exercised in --local mode against test files.

set -eu

REPO="raunakiitp/CodvianRouter"
WORKFLOW="https://github.com/$REPO/.github/workflows/release.yml"
ISSUER="https://token.actions.githubusercontent.com"

die() {
    printf 'verify-release: %s\n' "$*" >&2
    exit 1
}

note() {
    printf 'verify-release: %s\n' "$*"
}

# --- arguments ------------------------------------------------------------
MODE="remote"
VERSION=""
LOCAL_DIR=""
KEY=""
SKIP_ATTESTATION=0
KEEP_DIR=""

while [ $# -gt 0 ]; do
    case "$1" in
        --local) MODE="local"; LOCAL_DIR="${2:-}"; [ -n "$LOCAL_DIR" ] || die "--local needs a directory"; shift 2 ;;
        --key) KEY="${2:-}"; [ -n "$KEY" ] || die "--key needs a public-key file"; shift 2 ;;
        --skip-attestation) SKIP_ATTESTATION=1; shift ;;
        --dir) KEEP_DIR="${2:-}"; [ -n "$KEEP_DIR" ] || die "--dir needs a directory"; shift 2 ;;
        -h|--help)
            sed -n '2,30p' "$0"
            exit 0 ;;
        v*|0*|latest) VERSION="$1"; shift ;;
        *) die "unknown argument: $1 (usage: verify-release.sh <version> | --local DIR [--key PUBKEY])" ;;
    esac
done

[ "$MODE" = "local" ] || [ -n "$VERSION" ] || die "usage: verify-release.sh <version> | --local DIR"
[ "$MODE" = "local" ] || [ -z "$LOCAL_DIR" ] || die "--local and a version do not mix"

command -v cosign >/dev/null 2>&1 || die "cosign is required but not found in PATH. Install it first: https://docs.sigstore.dev/cosign/installation/"
if command -v sha256sum >/dev/null 2>&1; then
    SHA256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    SHA256="shasum -a 256"
else
    die "sha256sum (Linux) or shasum (macOS) is required but neither was found."
fi

# --- platform detection (remote mode only) --------------------------------
detect_platform() {
    OS="$(uname -s)"
    case "$OS" in
        Linux) GOOS="linux" ;;
        Darwin) GOOS="darwin" ;;
        *) die "unsupported operating system: $OS (only Linux and macOS are supported)" ;;
    esac
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64) GOARCH="amd64" ;;
        arm64|aarch64) GOARCH="arm64" ;;
        *) die "unsupported CPU architecture: $ARCH (only amd64 and arm64 are supported)" ;;
    esac
}

# --- cosign verification ---------------------------------------------------
# Both modes verify a Sigstore bundle (<file>.sigstore.json) — cosign v3
# sign-blob only emits bundles. Keyless (real releases): the bundle pins
# the signature to the release workflow + tag, and the Rekor
# transparency-log entry inside it is checked. Keypair (--key): the
# bundle was made with a local cosign keypair (pipeline self-test only).
# A local key has no Rekor entry by design, so keypair mode passes
# --insecure-ignore-tlog — it verifies the raw Ed25519 signature only.
# The keyless path NEVER passes that flag: skipping the log check on a
# real release would defeat the point of keyless signing.
verify_signature() {
    file="$1"   # file whose signature is checked, relative to $DIR
    bundle="$DIR/$file.sigstore.json"
    [ -f "$bundle" ] || die "missing Sigstore bundle: $bundle (releases ship <file>.sigstore.json next to <file>)"
    if [ -n "$KEY" ]; then
        cosign verify-blob --key "$KEY" --bundle "$bundle" --insecure-ignore-tlog "$DIR/$file" >/dev/null \
            || die "cosign signature verification FAILED for $file (key $KEY). Do not install these files."
    else
        cosign verify-blob \
            --certificate-identity "$WORKFLOW@refs/tags/$TAG" \
            --certificate-oidc-issuer "$ISSUER" \
            --bundle "$bundle" "$DIR/$file" >/dev/null \
            || die "cosign signature verification FAILED for $file. The signature is not from $WORKFLOW@$TAG — do not install these files."
    fi
    note "signature OK: $file"
}

# --- SBOM JSON check --------------------------------------------------------
check_sbom_json() {
    file="$1"
    if command -v python3 >/dev/null 2>&1; then
        python3 -m json.tool "$DIR/$file" >/dev/null \
            || die "SBOM $file is not valid JSON — the release is malformed; do not install it."
    elif command -v jq >/dev/null 2>&1; then
        jq -e . "$DIR/$file" >/dev/null \
            || die "SBOM $file is not valid JSON — the release is malformed; do not install it."
    else
        note "WARNING: neither python3 nor jq found — skipping the SBOM JSON-validity check for $file. Its signature (above) still proves it came from the release."
        return 0
    fi
    note "SBOM valid JSON: $file"
}

# --- download (remote mode) -------------------------------------------------
if [ "$MODE" = "remote" ]; then
    case "$VERSION" in
        v*) TAG="$VERSION" ;;
        *)  TAG="v$VERSION" ;;
    esac
    detect_platform
    ASSET="codvian-router_${TAG#v}_${GOOS}_${GOARCH}.tar.gz"
    BASE_URL="https://github.com/$REPO/releases/download/$TAG"
    if [ -n "$KEEP_DIR" ]; then
        DIR="$KEEP_DIR"
        mkdir -p "$DIR"
    else
        DIR="$(mktemp -d)"
        trap 'rm -rf "$DIR"' EXIT INT TERM
    fi
    note "verifying $TAG for $GOOS/$GOARCH ..."
    for f in "$ASSET" checksums.txt checksums.txt.sigstore.json "$ASSET.sbom.json" "$ASSET.sbom.json.sigstore.json"; do
        curl -fsSL -o "$DIR/$f" "$BASE_URL/$f" \
            || die "download failed: $BASE_URL/$f (check the version exists at https://github.com/$REPO/releases)"
    done
else
    DIR="$LOCAL_DIR"
    [ -d "$DIR" ] || die "--local directory not found: $DIR"
    [ -f "$DIR/checksums.txt" ] || die "no checksums.txt in $DIR"
    # In local mode the tag is unknown; keyless verification needs the
    # exact tag the bundle was issued for, so local keyless verification
    # reads it from the bundle is not possible — require --key, or a
    # TAG env var naming the tag the files came from.
    if [ -z "$KEY" ]; then
        TAG="${TAG:-}"
        [ -n "$TAG" ] || die "local keyless verification needs the release tag: set TAG=vX.Y.Z (or pass --key for keypair mode)"
        case "$TAG" in v*) ;; *) TAG="v$TAG" ;; esac
    fi
    note "verifying local release directory $DIR ..."
fi

# --- 1. signature over checksums.txt ---------------------------------------
verify_signature checksums.txt

# --- 2. SHA-256 of the archive(s) against checksums.txt ---------------------
# Remote mode downloads one platform's archive while checksums.txt lists
# all six, so missing entries are ignored there. Local mode checks the
# directory exactly as it stands: every listed file must be present.
if [ "$MODE" = "remote" ]; then
    (cd "$DIR" && $SHA256 -c --ignore-missing checksums.txt) \
        || die "SHA-256 MISMATCH: a downloaded file does not match checksums.txt. Do not install these files."
else
    (cd "$DIR" && $SHA256 -c checksums.txt) \
        || die "SHA-256 MISMATCH: a file in $DIR does not match checksums.txt. Do not install these files."
fi
note "checksums OK: files match checksums.txt"

# --- 3. SBOM signature + JSON ----------------------------------------------
# Every *.sbom.json present in DIR is verified (remote mode: exactly the
# one for this platform; local mode: every SBOM in the directory).
found_sbom=0
for sbom in "$DIR"/*.sbom.json; do
    [ -e "$sbom" ] || continue
    found_sbom=1
    name="$(basename "$sbom")"
    verify_signature "$name"
    check_sbom_json "$name"
done
[ "$found_sbom" = "1" ] || die "no SBOM found in $DIR — a complete release ships <archive>.sbom.json; refusing to call this verified."

# --- 4. build-provenance attestation ---------------------------------------
# Remote mode only: attestations live in GitHub's attestation store, not
# in the release directory, so they can only be checked against a
# published release. `gh attestation verify` fetches the attestation
# for the artifact and checks the SLSA provenance (built by this repo's
# release workflow from the tagged commit).
if [ "$MODE" = "remote" ] && [ "$SKIP_ATTESTATION" = "0" ]; then
    if command -v gh >/dev/null 2>&1; then
        gh attestation verify "$DIR/$ASSET" --repo "$REPO" >/dev/null \
            || die "build-provenance attestation verification FAILED for $ASSET. Do not install it."
        note "provenance attestation OK: $ASSET"
    else
        note "NOTE: gh CLI not found — skipping the build-provenance attestation check (steps 1–3 above already passed). Install gh (https://cli.github.com) and re-run to check it, or pass --skip-attestation to silence this note."
    fi
elif [ "$MODE" = "local" ]; then
    note "NOTE: local mode cannot check build-provenance attestations (they live in GitHub's attestation store, not in the directory). Verify the published release with this script in remote mode to check them."
fi

printf '\nVERIFIED: %s\n' "$([ "$MODE" = "remote" ] && echo "$TAG ($GOOS/$GOARCH)" || echo "local directory $DIR")"
printf '  - cosign signature over checksums.txt: OK\n'
printf '  - SHA-256 checksums: OK\n'
printf '  - SBOM signature + JSON: OK\n'
if [ "$MODE" = "remote" ] && [ "$SKIP_ATTESTATION" = "0" ] && command -v gh >/dev/null 2>&1; then
    printf '  - build-provenance attestation: OK\n'
fi
printf 'Safe to install. (Install with scripts/install.sh, which re-checks the signature itself.)\n'
