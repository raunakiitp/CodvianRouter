#!/bin/sh
# verify-image.sh — verify a Codvian Router Docker image BEFORE running it.
#
# What it checks, in order (each step must pass or the script stops):
#   1. Sigstore cosign keyless signature over the image — proves the
#      image was published by this project's release workflow for this
#      exact tag (identity pinned below; a signature from any other
#      workflow, repo, or tag is rejected). cosign resolves the tag to
#      its digest and checks the signature for that digest, so a
#      tampered image re-pushed under the same tag does NOT verify.
#      This is the step that matters: without it, the tag is just a
#      label anyone with push access could have pointed at any bytes.
#   2. SLSA build-provenance attestation (`gh attestation verify
#      oci://...`) when the `gh` CLI is present — proves GitHub
#      Actions built the image from this repo. Skipped with a note
#      when `gh` is absent.
#
# Usage:
#   sh scripts/verify-image.sh v0.1.0 [--image REF] [--skip-attestation]
#       Verify ghcr.io/raunakiitp/codvian-router:<tag> (or the image
#       named by --image — a different tag, or a digest pin
#       ghcr.io/raunakiitp/codvian-router@sha256:... for the exact
#       bytes). Only after this script prints VERIFIED should you run:
#         docker run ... ghcr.io/raunakiitp/codvian-router:<tag>
#   sh scripts/verify-image.sh --image REF --key PUBKEY
#       Verify an arbitrary image against a local cosign public key
#       instead of the keyless workflow identity — this is how the
#       pipeline's own tamper test exercises this script (local
#       registry images signed with a throwaway keypair; see
#       docs/slice-221-222-docker-image-signing-report.md). Real
#       releases always use the keyless signature and need no --key.
#
# Exit status is 0 only when every applicable check passed. On ANY
# failure: do not run the image. Report it at
# https://github.com/raunakiitp/CodvianRouter/issues.
#
# Requires: cosign, docker (only to pull/run the image afterwards —
# not for verification itself). Optional: gh (attestation).
# cosign: https://docs.sigstore.dev/cosign/installation/
#
# No release has been published yet — until the first release exists
# this script can only be exercised in --key mode against local test
# images.

set -eu

REPO="raunakiitp/CodvianRouter"
IMAGE_REPO="ghcr.io/raunakiitp/codvian-router"
WORKFLOW="https://github.com/$REPO/.github/workflows/release.yml"
ISSUER="https://token.actions.githubusercontent.com"

die() {
    printf 'verify-image: %s\n' "$*" >&2
    exit 1
}

note() {
    printf 'verify-image: %s\n' "$*"
}

# --- arguments ------------------------------------------------------------
VERSION=""
IMAGE=""
KEY=""
SKIP_ATTESTATION=0

while [ $# -gt 0 ]; do
    case "$1" in
        --image) IMAGE="${2:-}"; [ -n "$IMAGE" ] || die "--image needs an image reference"; shift 2 ;;
        --key) KEY="${2:-}"; [ -n "$KEY" ] || die "--key needs a public-key file"; shift 2 ;;
        --skip-attestation) SKIP_ATTESTATION=1; shift ;;
        -h|--help)
            sed -n '2,30p' "$0"
            exit 0 ;;
        v*|0*|latest) VERSION="$1"; shift ;;
        *) die "unknown argument: $1 (usage: verify-image.sh <version> [--image REF] | --image REF --key PUBKEY)" ;;
    esac
done

# Keyless verification needs the exact tag the signature was issued
# for; keypair (--key) mode does not — it verifies a raw Ed25519
# signature against the given public key.
if [ -z "$KEY" ]; then
    [ -n "$VERSION" ] || die "usage: verify-image.sh <version> [--image REF] (or pass --key for keypair mode)"
    case "$VERSION" in
        v*) TAG="$VERSION" ;;
        *)  TAG="v$VERSION" ;;
    esac
fi

if [ -z "$IMAGE" ]; then
    [ -n "${TAG:-}" ] || die "--image is required when no version is given"
    IMAGE="$IMAGE_REPO:$TAG"
fi

command -v cosign >/dev/null 2>&1 || die "cosign is required but not found in PATH. Install it first: https://docs.sigstore.dev/cosign/installation/"

# Local test registries (localhost / 127.0.0.1) serve plain HTTP; cosign
# refuses them unless told they are insecure. This branch only ever
# triggers for a loopback reference, never for ghcr.io.
INSECURE=""
case "$IMAGE" in
    localhost:*|localhost/*|127.0.0.1:*|127.0.0.1/*) INSECURE="--allow-insecure-registry" ;;
esac

# --- 1. cosign signature over the image ------------------------------------
# Keyless (real releases): the signature pins to the release workflow +
# tag, and the Rekor transparency-log entry inside it is checked.
# Keypair (--key): the signature was made with a local cosign keypair
# (pipeline self-test only). A local key has no Rekor entry by design,
# so keypair mode passes --insecure-ignore-tlog — it verifies the raw
# Ed25519 signature only. The keyless path NEVER passes that flag:
# skipping the log check on a real release would defeat the point of
# keyless signing.
note "verifying $IMAGE ..."
if [ -n "$KEY" ]; then
    # shellcheck disable=SC2086
    cosign verify --key "$KEY" --insecure-ignore-tlog $INSECURE "$IMAGE" >/dev/null \
        || die "cosign signature verification FAILED for $IMAGE (key $KEY). Do not run this image."
else
    # shellcheck disable=SC2086
    cosign verify \
        --certificate-identity "$WORKFLOW@refs/tags/$TAG" \
        --certificate-oidc-issuer "$ISSUER" \
        $INSECURE "$IMAGE" >/dev/null \
        || die "cosign signature verification FAILED for $IMAGE. The signature is not from $WORKFLOW@$TAG — do not run this image."
fi
note "signature OK: $IMAGE"

# --- 2. build-provenance attestation ---------------------------------------
# Keyless mode only: attestations live in GitHub's attestation store,
# not in the registry, so they can only be checked for a published
# image. `gh attestation verify oci://...` fetches the attestation
# for the image and checks the SLSA provenance (built by this repo's
# release workflow from the tagged commit).
if [ -z "$KEY" ] && [ "$SKIP_ATTESTATION" = "0" ]; then
    if command -v gh >/dev/null 2>&1; then
        gh attestation verify "oci://$IMAGE" --repo "$REPO" >/dev/null \
            || die "build-provenance attestation verification FAILED for $IMAGE. Do not run this image."
        note "provenance attestation OK: $IMAGE"
    else
        note "NOTE: gh CLI not found — skipping the build-provenance attestation check (the signature above already passed). Install gh (https://cli.github.com) and re-run to check it, or pass --skip-attestation to silence this note."
    fi
elif [ -n "$KEY" ]; then
    note "NOTE: keypair mode cannot check build-provenance attestations (they live in GitHub's attestation store, not in the registry). Verify the published image with this script in keyless mode to check them."
fi

printf '\nVERIFIED: %s\n' "$IMAGE"
printf '  - cosign signature: OK\n'
if [ -z "$KEY" ] && [ "$SKIP_ATTESTATION" = "0" ] && command -v gh >/dev/null 2>&1; then
    printf '  - build-provenance attestation: OK\n'
fi
printf 'Safe to run. (See docs/INSTALL.md §3 for the run commands.)\n'
