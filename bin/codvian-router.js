#!/usr/bin/env node
// codvian-router — npm launcher.
//
// Installing this package via npm does NOT run any downloaded code: the
// wrapper only downloads a prebuilt binary from the project's GitHub
// releases (https://github.com/raunakiitp/CodvianRouter/releases) on
// first run, verifies the file exists and is executable, then execs it,
// forwarding arguments and signals. Nothing is downloaded at install
// time. The downloaded binary is cached under bin/.cache/ next to this
// file; delete that directory to force a fresh download.
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

"use strict";

const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
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

function download(url, dest) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(dest);
    const req = https.get(
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
  const cacheDir = path.join(__dirname, ".cache");
  const binaryName = isWindows ? "codvian-router.exe" : "codvian-router";
  const binaryPath = path.join(cacheDir, `codvian-router-${goos}-${goarch}${isWindows ? ".exe" : ""}`);

  if (fs.existsSync(binaryPath)) {
    verifyExecutable(binaryPath);
    return binaryPath;
  }

  const ext = isWindows ? "zip" : "tar.gz";
  const asset = `codvian-router_${version}_${goos}_${goarch}.${ext}`;
  const url = `https://github.com/${REPO}/releases/download/v${version}/${asset}`;

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
