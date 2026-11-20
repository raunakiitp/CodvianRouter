#!/usr/bin/env node
// codvian-router — npm launcher.
//
// Installing this package via npm does NOT run any downloaded code: the
// wrapper only downloads a prebuilt binary from the project's GitHub
// releases (https://github.com/raunakiitp/CodvianRouter/releases) on
// first run, verifies it, then execs it, forwarding arguments and
// signals. Nothing is downloaded at install time. The downloaded binary
// is cached under bin/.cache/ next to this file; delete that directory
// to force a fresh download (and a fresh verification).
//
// Verification-first install (same rule as scripts/install.sh): before
// the archive is extracted or trusted, the wrapper
//   1. downloads checksums.txt for the release,
//   2. if `cosign` is installed, verifies the Sigstore keyless
//      signature over checksums.txt (identity pinned to the release
//      workflow + this exact tag) and REFUSES on any failure — a
//      checksums file without a valid signature proves nothing, since
//      anyone who can swap the archive can swap the checksums too;
//   3. verifies the archive's SHA-256 against the (now trusted, or
//      checksum-only — see below) manifest and REFUSES on mismatch or
//      on a missing entry.
// A failed verification deletes the downloaded archive and aborts
// before anything is extracted or executed.
//
// No-cosign behaviour (honest, mirrors install.sh): without cosign the
// wrapper warns loudly and continues with checksum-only verification,
// unless CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is set, which refuses
// instead. Checksum-only proves the download matches the manifest —
// it does NOT prove the manifest came from the project. The
// recommended path is to verify the release first with
// scripts/verify-release.sh (which always requires cosign), or install
// cosign and let this wrapper check the signature itself.
//
// Fail-closed on an unreachable manifest: if checksums.txt itself
// cannot be downloaded, the wrapper retries the download (3
// attempts, backoff) and then REFUSES to install — a release whose
// manifest cannot be checked is never installed unverified. A
// transient GitHub error is absorbed by the retries; the signature
// is checked whenever the manifest downloads.
//   - A cached binary IS re-verified on every run: the SHA-256 of
//     the verified binary is stored alongside it
//     (<binary>.sha256) when it is first cached, and the cached
//     binary's hash is checked against that sidecar before it is
//     used. A tampered binary, or a cache without a sidecar (caches
//     written before this fix), is discarded and re-downloaded —
//     which re-runs the full signature + checksum verification.
//     The re-check is local only; a good cache is never
//     re-downloaded.
// The SBOM signature is not checked here: the wrapper installs the
// binary, and the binary is authenticated by the signed checksums.txt.
// Auditors verify the SBOM with scripts/verify-release.sh.
// CODVIAN_ROUTER_BINARY bypasses download AND verification entirely:
// it is the operator's own binary, their responsibility.
//
// The router itself is local-only (binds 127.0.0.1:20128) and auth is
// always ON — run `codvian-router init` once to create a config and a
// gateway key, then `codvian-router serve`.
//
// Overrides (for testing / local builds; not part of normal use):
//   CODVIAN_ROUTER_BINARY=<path>   use this binary instead of downloading
//   CODVIAN_ROUTER_VERSION=<x.y.z> download that release instead of this
//                                  package's version
//   CODVIAN_ROUTER_PLATFORM=<os>-<arch>  pretend to be that platform
//                                  (testing the platform error path)
//   CODVIAN_ROUTER_REQUIRE_SIGNATURE=1  refuse instead of falling back
//                                  to checksum-only when cosign is absent
//   CODVIAN_ROUTER_DOWNLOAD_BASE_URL=<url>  testing only: download
//                                  release files from here instead of
//                                  https://github.com/<repo>/releases/download
//   CODVIAN_ROUTER_CACHE_DIR=<dir>  testing only: cache the binary here
//                                  instead of bin/.cache/

"use strict";

const { spawn, spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const https = require("node:https");
const os = require("node:os");
const path = require("node:path");

const REPO = "raunakiitp/CodvianRouter";
const PACKAGE_VERSION = require("../package.json").version;

function fail(message) {
  console.error(`codvian-router: ${message}`);
  process.exit(1);
}

// Map Node's platform/arch to the GoReleaser asset names produced by
// .goreleaser.yml: codvian-router_<version>_<os>_<arch>.tar.gz
// (Windows: .zip, binary codvian-router.exe).
function resolvePlatform() {
  const override = process.env.CODVIAN_ROUTER_PLATFORM;
  const platform = override ? override.split("-")[0] : os.platform();
  const arch = override ? override.split("-").slice(1).join("-") : os.arch();
  const table = {
    "darwin-arm64": { goos: "darwin", goarch: "arm64" },
    "darwin-x64": { goos: "darwin", goarch: "amd64" },
    "linux-arm64": { goos: "linux", goarch: "arm64" },
    "linux-x64": { goos: "linux", goarch: "amd64" },
    "win32-x64": { goos: "windows", goarch: "amd64" },
    "win32-arm64": { goos: "windows", goarch: "arm64" },
  };
  const entry = table[`${platform}-${arch}`];
  if (!entry) {
    fail(
      `unsupported platform: ${platform}-${arch}.\n` +
        "Prebuilt binaries are published for macOS (arm64, x64), Linux (amd64, arm64) " +
        "and Windows (amd64, arm64) only. To run elsewhere, build from source:\n" +
        "  git clone https://github.com/raunakiitp/CodvianRouter.git\n" +
        "  cd CodvianRouter && go build -o codvian-router ./cmd/codvian-router\n" +
        "Or set CODVIAN_ROUTER_BINARY to a locally built binary."
    );
  }
  return entry;
}

// Base URL for release downloads. Production default is the GitHub
// release download path; the override exists so the test suite
// (bin/codvian-router.test.js) can point the wrapper at a local server
// serving controlled artifacts. It is testing-only, not a mirror
// mechanism: whatever it points at is still signature- + checksum-
// verified exactly as a GitHub download would be.
function downloadBaseUrl() {
  const override = process.env.CODVIAN_ROUTER_DOWNLOAD_BASE_URL;
  if (override) return override.replace(/\/$/, "");
  return `https://github.com/${REPO}/releases/download`;
}

function download(url, dest) {
  return new Promise((resolve, reject) => {
    let transport;
    try {
      const parsed = new URL(url);
      if (parsed.protocol === "https:") transport = https;
      else if (parsed.protocol === "http:") transport = http;
      else throw new Error(`unsupported download URL scheme: ${parsed.protocol}`);
    } catch (err) {
      reject(err);
      return;
    }
    const file = fs.createWriteStream(dest);
    const req = transport.get(
      url,
      { headers: { "User-Agent": "codvian-router-npm-wrapper" } },
      (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          // Follow GitHub's redirect to the release asset.
          file.close();
          fs.unlink(dest, () => {});
          download(res.headers.location, dest).then(resolve, reject);
          return;
        }
        if (res.statusCode !== 200) {
          file.close();
          fs.unlink(dest, () => {});
          reject(new Error(`download failed: HTTP ${res.statusCode} for ${url}`));
          return;
        }
        res.pipe(file);
        file.on("finish", () => file.close(resolve));
      }
    );
    req.on("error", (err) => {
      file.close();
      fs.unlink(dest, () => {});
      reject(err);
    });
    req.setTimeout(120000, () => req.destroy(new Error("download timed out after 120s")));
  });
}

function sha256File(filePath) {
  const hash = crypto.createHash("sha256");
  hash.update(fs.readFileSync(filePath));
  return hash.digest("hex");
}

// Parse a sha256sum-format manifest ("<hex>  <filename>" per line) and
// return the expected hex digest for `asset`, or null when the manifest
// does not list it. A manifest that does not list the asset is a
// refusal, not a pass — see verifyDownloadedArchive.
function expectedChecksum(checksumsText, asset) {
  for (const line of checksumsText.split("\n")) {
    const m = /^([0-9a-f]{64})[ \t]+\*?(.+)$/.exec(line.trim());
    if (m && m[2] === asset) return m[1];
  }
  return null;
}

// Cache integrity: the SHA-256 of a verified binary is stored in a
// sidecar file (<binary>.sha256) when the binary is first cached.
// verifyCachedBinary re-checks the cached binary against that
// sidecar on every run. It returns false (never throws) when the
// sidecar is missing — e.g. a cache written before this fix —
// malformed, or the hash does not match; the caller then discards
// the cache and re-downloads, which re-runs the full verification.
function binaryChecksumPath(binaryPath) {
  return `${binaryPath}.sha256`;
}

function writeBinaryChecksum(binaryPath) {
  const sidecar = binaryChecksumPath(binaryPath);
  const tmp = `${sidecar}.tmp`;
  fs.writeFileSync(tmp, `${sha256File(binaryPath)}  ${path.basename(binaryPath)}\n`, "utf8");
  fs.renameSync(tmp, sidecar);
}

function verifyCachedBinary(binaryPath) {
  let expected;
  try {
    expected = fs.readFileSync(binaryChecksumPath(binaryPath), "utf8").trim().split(/\s+/)[0];
  } catch {
    return false;
  }
  if (!/^[0-9a-f]{64}$/.test(expected || "")) return false;
  let actual;
  try {
    actual = sha256File(binaryPath);
  } catch {
    return false;
  }
  return actual === expected;
}

let cosignChecked = null;
function cosignAvailable() {
  if (cosignChecked === null) {
    const result = spawnSync("cosign", ["version"], { stdio: "ignore" });
    cosignChecked = !result.error;
  }
  return cosignChecked;
}

// Verification-first: signature over checksums.txt (when cosign is
// present), then SHA-256 of the archive against the manifest. Throws
// on any refusal — including a checksums.txt that cannot be
// downloaded at all (retried 3 times with backoff first, then fail
// closed, mirroring scripts/install.sh).
async function verifyDownloadedArchive({ cacheDir, asset, archivePath, version }) {
  const tag = `v${version}`;
  const base = downloadBaseUrl();
  const checksumsPath = path.join(cacheDir, "checksums.txt");
  const bundlePath = path.join(cacheDir, "checksums.txt.sigstore.json");

  // The manifest is retried (3 attempts, 1s/2s backoff) and then the
  // install FAILS CLOSED: a release whose checksums cannot be fetched
  // is never installed unverified.
  let checksumsText;
  let lastErr = null;
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      await download(`${base}/${tag}/checksums.txt`, checksumsPath);
      checksumsText = fs.readFileSync(checksumsPath, "utf8");
      lastErr = null;
      break;
    } catch (err) {
      lastErr = err;
      if (attempt < 3) {
        console.error(
          `codvian-router: WARNING: could not download checksums.txt for ${tag} (attempt ${attempt}/3: ${err.message}) — retrying...`
        );
        await new Promise((r) => setTimeout(r, 1000 * attempt));
      }
    }
  }
  if (lastErr) {
    throw new Error(
      `could not download checksums.txt for ${tag} after 3 attempts (${lastErr.message}) — refusing to install a release whose manifest cannot be checked. ` +
        `Retry later, or report it at https://github.com/${REPO}/issues.`
    );
  }

  // The signature is checked BEFORE the archive's hash is trusted, so
  // a failed signature aborts before any checksum comparison.
  const identity = `https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${tag}`;
  if (cosignAvailable()) {
    try {
      await download(`${base}/${tag}/checksums.txt.sigstore.json`, bundlePath);
    } catch (err) {
      throw new Error(
        `could not download checksums.txt.sigstore.json for ${tag} — refusing to install a release whose signature cannot be checked. (${err.message})`
      );
    }
    const result = spawnSync(
      "cosign",
      [
        "verify-blob",
        "--certificate-identity",
        identity,
        "--certificate-oidc-issuer",
        "https://token.actions.githubusercontent.com",
        "--bundle",
        bundlePath,
        checksumsPath,
      ],
      { stdio: "pipe", encoding: "utf8" }
    );
    if (result.error || result.status !== 0) {
      const detail = ((result.stderr || "") + (result.stdout || "")).trim() || (result.error && result.error.message) || "";
      throw new Error(
        `cosign signature verification FAILED for checksums.txt — the manifest is not from the Codvian Router release workflow for ${tag}. Refusing to install.` +
          (detail ? `\ncosign: ${detail}` : "")
      );
    }
    console.error("codvian-router: Signature verified (Sigstore cosign, keyless).");
  } else if (process.env.CODVIAN_ROUTER_REQUIRE_SIGNATURE === "1") {
    throw new Error(
      "CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is set but cosign is not installed. Install cosign (https://docs.sigstore.dev/cosign/installation/) and re-run, or unset the variable to fall back to checksum-only verification."
    );
  } else {
    console.error(
      "codvian-router: WARNING: cosign not found — installing with checksum-only verification (the checksums.txt itself is unverified).\n" +
        `codvian-router: To verify the release signature first, install cosign and run: sh scripts/verify-release.sh ${tag}`
    );
  }

  const expected = expectedChecksum(checksumsText, asset);
  if (!expected) {
    throw new Error(`checksums.txt does not list ${asset} — refusing to install an unverified binary.`);
  }
  const actual = sha256File(archivePath);
  if (actual !== expected) {
    throw new Error(
      `SHA-256 MISMATCH for ${asset} (expected ${expected}, got ${actual}). The download may be corrupt or tampered with — refusing to install.`
    );
  }
  console.error("codvian-router: Checksum verified (sha256).");
}

function extract(archive, destDir, isWindows) {
  // Extract with the system tar (ships with macOS, Linux, and Windows 10+).
  // It handles both .tar.gz and .zip. No npm dependencies are used.
  const tar = isWindows ? "tar.exe" : "tar";
  const result = spawnSync(tar, ["-xzf", archive, "-C", destDir], { stdio: "inherit" });
  if (result.error) {
    fail(
      `could not extract the downloaded archive: ${result.error.message}.\n` +
        `Install 'tar' and re-run, or download the release manually from ` +
        `https://github.com/${REPO}/releases`
    );
  }
  if (result.status !== 0) {
    fail(
      `extraction failed (tar exited ${result.status}). The release asset may be ` +
        `corrupt — delete ${path.dirname(destDir)} and re-run, or download manually from ` +
        `https://github.com/${REPO}/releases`
    );
  }
}

async function ensureBinary() {
  const override = process.env.CODVIAN_ROUTER_BINARY;
  if (override) {
    if (!fs.existsSync(override)) fail(`CODVIAN_ROUTER_BINARY not found: ${override}`);
    return override;
  }

  const { goos, goarch } = resolvePlatform();
  const isWindows = goos === "windows";
  const version = process.env.CODVIAN_ROUTER_VERSION || PACKAGE_VERSION;
  const cacheDir = process.env.CODVIAN_ROUTER_CACHE_DIR || path.join(__dirname, ".cache");
  const binaryName = isWindows ? "codvian-router.exe" : "codvian-router";
  const binaryPath = path.join(cacheDir, `codvian-router-${goos}-${goarch}${isWindows ? ".exe" : ""}`);

  if (fs.existsSync(binaryPath)) {
    // Re-verify the cached binary against the sidecar written when
    // it was first cached. A tampered binary — or a cache with no
    // sidecar — is discarded and re-downloaded below, which re-runs
    // the full signature + checksum verification. A good cache is
    // never re-downloaded: this check is local only.
    if (verifyCachedBinary(binaryPath)) {
      verifyExecutable(binaryPath);
      return binaryPath;
    }
    console.error(
      "codvian-router: cached binary failed checksum re-verification — discarding it and re-downloading."
    );
    try {
      fs.unlinkSync(binaryPath);
    } catch {}
    try {
      fs.unlinkSync(binaryChecksumPath(binaryPath));
    } catch {}
  }

  const ext = isWindows ? "zip" : "tar.gz";
  const asset = `codvian-router_${version}_${goos}_${goarch}.${ext}`;
  const url = `${downloadBaseUrl()}/v${version}/${asset}`;

  fs.mkdirSync(cacheDir, { recursive: true });
  const archivePath = path.join(cacheDir, asset);
  console.error(`codvian-router: first run — downloading ${asset} from github.com/${REPO}/releases ...`);
  try {
    await download(url, archivePath);
  } catch (err) {
    fail(
      `${err.message}.\n` +
        `No usable release was found for codvian-router v${version} (${goos}/${goarch}). ` +
        `Check https://github.com/${REPO}/releases, or build from source and set CODVIAN_ROUTER_BINARY.`
    );
  }
  // Verification-first: the archive is not extracted or trusted until
  // the signature (when cosign is present) and the checksum pass. A
  // refusal deletes the archive and aborts here.
  try {
    await verifyDownloadedArchive({ cacheDir, asset, archivePath, version });
  } catch (err) {
    fs.unlink(archivePath, () => {});
    fail(err.message);
  }
  try {
    extract(archivePath, cacheDir, isWindows);
  } finally {
    fs.unlink(archivePath, () => {});
  }
  // The archive holds the binary as plain `codvian-router[.exe]`; rename it
  // to the platform-specific cached name so concurrent platforms cannot
  // collide in a shared package directory.
  const extracted = path.join(cacheDir, binaryName);
  if (!fs.existsSync(extracted)) {
    fail(
      `the release archive did not contain ${binaryName}. ` +
        `Delete ${cacheDir} and re-run, or report this at https://github.com/${REPO}/issues.`
    );
  }
  fs.renameSync(extracted, binaryPath);
  // Pin the verified binary's hash next to it so later runs can
  // re-verify the cache instead of trusting it blindly.
  writeBinaryChecksum(binaryPath);
  verifyExecutable(binaryPath);
  return binaryPath;
}

function verifyExecutable(binaryPath) {
  let stat;
  try {
    stat = fs.statSync(binaryPath);
  } catch {
    fail(`downloaded binary is missing: ${binaryPath}`);
  }
  if (!stat.isFile()) fail(`downloaded binary is not a file: ${binaryPath}`);
  if (os.platform() !== "win32") {
    try {
      fs.accessSync(binaryPath, fs.constants.X_OK);
    } catch {
      // Best effort: mark it executable; if that fails, the exec below errors clearly.
      try {
        fs.chmodSync(binaryPath, 0o755);
      } catch {
        fail(`downloaded binary is not executable: ${binaryPath}`);
      }
    }
  }
}

async function main() {
  const binary = await ensureBinary();
  const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });

  // Forward signals so Ctrl-C / termination reaches the router, not just us.
  for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(sig, () => child.kill(sig));
  }
  child.on("error", (err) => fail(`could not run ${binary}: ${err.message}`));
  child.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code === null ? 1 : code);
  });
}

main().catch((err) => fail(err.message));
