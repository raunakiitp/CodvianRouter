"use strict";
// Tests for the npm wrapper's verification-first install path
// (bin/codvian-router.js) and the install script (scripts/install.sh).
//
// The wrapper and the installer download release artifacts from GitHub
// in production. Here they are pointed at a local HTTP server
// (CODVIAN_ROUTER_DOWNLOAD_BASE_URL) serving controlled artifacts, so
// the tests can prove, without any network or any published release,
// that a tampered download is refused and an untampered one installs.
//
// The "binary" inside the test archive is a tiny shell script that
// prints a marker — it stands in for the real router binary. A test
// that sees the marker knows the binary was actually executed; a test
// that must refuse asserts the marker never appears and no binary was
// cached or installed.
//
// The cosign used here is a stub shell script placed first on PATH
// (the VM has no real cosign). It records its argv so the tests can
// assert the wrapper passes the pinned release-workflow identity, and
// exits 1 when COSIGN_FAIL=1 to simulate a failed signature check.
// It is a test double for an external tool, not a fake verification:
// the wrapper's own code paths (download -> signature -> checksum ->
// refuse/install) are what is under test.
//
// The happy-path tests at the bottom of this file go further: they
// build the REAL binary (`go build ./cmd/codvian-router`), serve it
// as a release fixture, and run the full install -> --version ->
// init -> provider add -> serve -> chat-completion flow through both
// install.sh and the npm wrapper, in an isolated HOME, against a
// local mock upstream. That is CHECKLIST items 321–322: the
// verification-path tests above prove a tampered download is refused;
// the happy-path tests prove an untampered install actually works
// end to end. They need `go` (CODVIAN_ROUTER_GO, `go` on PATH, or
// /home/hatch/go/bin/go) and `tar`.
//
// Run: npm test   (or: node --test bin/codvian-router.test.js)
// TMPDIR must point at writable scratch (this VM's /tmp is nearly
// full; the gate runs with TMPDIR=$HOME/.tmp).

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { spawn, spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const http = require("node:http");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");

const WRAPPER = path.join(__dirname, "codvian-router.js");
const INSTALL_SH = path.join(__dirname, "..", "scripts", "install.sh");
const VERSION = "0.0.0-test";
const TAG = `v${VERSION}`;
const ASSET = `codvian-router_${VERSION}_linux_amd64.tar.gz`;
const STUB_MARKER = "CODVIAN-ROUTER-STUB";
const PINNED_IDENTITY = `https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml@refs/tags/${TAG}`;

// Build a fake release: a stub "binary" tarred as the GoReleaser
// asset, a checksums.txt with its real SHA-256, and a dummy Sigstore
// bundle. Returns { root, served, cacheDir } where `served` is a
// mutable Map of filename -> Buffer that the local server answers
// with (tests tamper by replacing entries).
function buildFixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cr-npm-test-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const stubPath = path.join(root, "codvian-router");
  fs.writeFileSync(stubPath, `#!/bin/sh\necho ${STUB_MARKER} "$@"\n`, { mode: 0o755 });
  const assetPath = path.join(root, ASSET);
  const tar = spawnSync("tar", ["--owner=0", "--group=0", "-czf", assetPath, "codvian-router"], { cwd: root, encoding: "utf8" });
  assert.equal(tar.status, 0, `tar failed: ${tar.stderr}`);
  // NOTE: --owner=0 --group=0 is load-bearing on this VM, not cosmetic.
  // The fixture lives under $HOME/.tmp, a setgid directory whose group
  // (nogroup, 65534) a plain `tar -czf` would record in the archive —
  // and this VM's user namespace refuses the extraction-side chown to
  // that gid ("Cannot change ownership ... Operation not permitted",
  // the same userns limitation recorded for Docker in CHECKLIST item
  // 75). Recording root:root makes the archive extract cleanly here
  // and everywhere else; it changes nothing about the artifact bytes
  // the wrapper verifies (the checksum is computed after tarring).
  const archiveBytes = fs.readFileSync(assetPath);
  const sha = crypto.createHash("sha256").update(archiveBytes).digest("hex");
  const served = new Map();
  served.set(ASSET, archiveBytes);
  served.set("checksums.txt", Buffer.from(`${sha}  ${ASSET}\n`, "utf8"));
  served.set("checksums.txt.sigstore.json", Buffer.from('{"bundle":"test-fixture"}', "utf8"));
  const cacheDir = path.join(root, "cache");
  return { root, served, cacheDir, archiveBytes, sha };
}

// Serve the fixture as a GitHub release download path:
//   /releases/download/<tag>/<filename>
// `tag` defaults to the stub-fixture tag; the happy-path tests pass
// the real release tag (v<package.json version>).
async function startServer(t, served, requests, tag = TAG) {
  const server = http.createServer((req, res) => {
    requests.push(req.url);
    const prefix = `/releases/download/${tag}/`;
    if (!req.url.startsWith(prefix)) {
      res.writeHead(404);
      res.end("not found");
      return;
    }
    const body = served.get(req.url.slice(prefix.length));
    if (body === undefined) {
      res.writeHead(404);
      res.end("not found");
      return;
    }
    res.writeHead(200, { "Content-Type": "application/octet-stream" });
    res.end(body);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  return server.address().port;
}

function wrapperEnv(port, cacheDir, extra = {}) {
  return {
    ...process.env,
    CODVIAN_ROUTER_VERSION: VERSION,
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_CACHE_DIR: cacheDir,
    ...extra,
  };
}

// Run a child process WITHOUT blocking this process's event loop.
// This matters: the local HTTP server above lives in this same
// process, so a blocking spawnSync would deadlock — the child waits
// for the server to answer its download while the server waits for
// spawnSync to return. (That exact deadlock hung the first version
// of this suite; async spawn is load-bearing here, not style.)
function runAsync(argv, env, timeoutMs = 60000) {
  return new Promise((resolve, reject) => {
    const child = spawn(argv[0], argv.slice(1), { env });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (d) => {
      stdout += d;
    });
    child.stderr.on("data", (d) => {
      stderr += d;
    });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`${argv[0]} timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    child.on("error", (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.on("close", (status) => {
      clearTimeout(timer);
      resolve({ status, stdout, stderr });
    });
  });
}

function runWrapper(env, args = ["__probe__"]) {
  return runAsync(["node", WRAPPER, ...args], env);
}

function cachedBinary(cacheDir) {
  return path.join(cacheDir, "codvian-router-linux-amd64");
}

// A stub `cosign` on PATH: logs argv, exits 0 (valid signature) or 1
// (COSIGN_FAIL=1, invalid signature). Returns { binDir, logPath }.
function installCosignStub(t, fail) {
  const binDir = fs.mkdtempSync(path.join(os.tmpdir(), "cr-cosign-stub-"));
  const logPath = path.join(binDir, "cosign-argv.log");
  const stub = path.join(binDir, "cosign");
  fs.writeFileSync(
    stub,
    `#!/bin/sh\nprintf '%s\\n' "$*" >> "${logPath}"\n` +
      `if [ "$COSIGN_FAIL" = "1" ]; then echo "fake cosign: signature verification failed" >&2; exit 1; fi\n` +
      `exit 0\n`,
    { mode: 0o755 }
  );
  t.after(() => fs.rmSync(binDir, { recursive: true, force: true }));
  return {
    binDir,
    logPath,
    pathEnv: `${binDir}:${process.env.PATH}`,
    failEnv: fail ? "1" : "0",
  };
}

test("untampered download verifies (checksum-only, no cosign) and runs", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const r = await runWrapper(wrapperEnv(port, cacheDir));
  assert.equal(r.status, 0, `wrapper failed: ${r.stderr}`);
  assert.match(r.stdout, new RegExp(`${STUB_MARKER} __probe__`), "stub binary was not executed");
  assert.match(r.stderr, /Checksum verified \(sha256\)/, "checksum confirmation missing");
  assert.match(r.stderr, /WARNING: cosign not found/, "no-cosign warning missing");
  assert.ok(fs.existsSync(cachedBinary(cacheDir)), "binary was not cached");
  assert.ok(requests.some((u) => u.endsWith(`/${ASSET}`)), "archive was not downloaded");
  assert.ok(requests.some((u) => u.endsWith("/checksums.txt")), "checksums.txt was not downloaded");
});

test("tampered archive is refused (checksum mismatch, binary never runs)", { timeout: 60000 }, async (t) => {
  const { served, cacheDir, archiveBytes } = buildFixture(t);
  served.set(ASSET, Buffer.concat([archiveBytes, Buffer.from("tampered-bytes")]));
  const requests = [];
  const port = await startServer(t, served, requests);
  const r = await runWrapper(wrapperEnv(port, cacheDir));
  assert.notEqual(r.status, 0, "tampered archive was accepted");
  assert.match(r.stderr, /SHA-256 MISMATCH/, "refusal did not name the checksum mismatch");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "tampered binary was executed");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "tampered binary was cached");
  assert.ok(!fs.existsSync(path.join(cacheDir, ASSET)), "tampered archive was left in the cache");
});

test("tampered checksums entry is refused", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  served.set("checksums.txt", Buffer.from(`${"0".repeat(64)}  ${ASSET}\n`, "utf8"));
  const requests = [];
  const port = await startServer(t, served, requests);
  const r = await runWrapper(wrapperEnv(port, cacheDir));
  assert.notEqual(r.status, 0, "tampered checksums entry was accepted");
  assert.match(r.stderr, /SHA-256 MISMATCH/, "refusal did not name the checksum mismatch");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "binary was executed after a bad manifest");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "binary was cached after a bad manifest");
});

test("CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 refuses without cosign", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const r = await runWrapper(wrapperEnv(port, cacheDir, { CODVIAN_ROUTER_REQUIRE_SIGNATURE: "1" }));
  assert.notEqual(r.status, 0, "install proceeded without a signature check despite REQUIRE_SIGNATURE=1");
  assert.match(r.stderr, /CODVIAN_ROUTER_REQUIRE_SIGNATURE=1/, "refusal did not name the override");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "binary was executed");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "binary was cached");
});

test("cosign present + valid signature verifies with the pinned identity", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const stub = installCosignStub(t, false);
  const r = await runWrapper(
    wrapperEnv(port, cacheDir, { PATH: stub.pathEnv, COSIGN_FAIL: stub.failEnv, COSIGN_LOG: stub.logPath })
  );
  assert.equal(r.status, 0, `wrapper failed: ${r.stderr}`);
  assert.match(r.stdout, new RegExp(`${STUB_MARKER} __probe__`), "stub binary was not executed");
  assert.match(r.stderr, /Signature verified \(Sigstore cosign, keyless\)/, "signature confirmation missing");
  assert.match(r.stderr, /Checksum verified \(sha256\)/, "checksum confirmation missing");
  assert.doesNotMatch(r.stderr, /checksum-only/, "checksum-only warning printed despite cosign");
  const log = fs.readFileSync(stub.logPath, "utf8");
  assert.ok(log.includes("verify-blob"), "cosign was not asked to verify a blob");
  assert.ok(log.includes(PINNED_IDENTITY), `cosign was not given the pinned identity ${PINNED_IDENTITY}`);
  assert.ok(requests.some((u) => u.endsWith("/checksums.txt.sigstore.json")), "signature bundle was not downloaded");
});

test("cosign present + invalid signature refuses", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const stub = installCosignStub(t, true);
  const r = await runWrapper(
    wrapperEnv(port, cacheDir, { PATH: stub.pathEnv, COSIGN_FAIL: stub.failEnv, COSIGN_LOG: stub.logPath })
  );
  assert.notEqual(r.status, 0, "invalid signature was accepted");
  assert.match(r.stderr, /cosign signature verification FAILED/, "refusal did not name the signature failure");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "binary was executed after a failed signature");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "binary was cached after a failed signature");
});

test("cosign present + missing signature bundle refuses", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  served.delete("checksums.txt.sigstore.json");
  const requests = [];
  const port = await startServer(t, served, requests);
  const stub = installCosignStub(t, false);
  const r = await runWrapper(
    wrapperEnv(port, cacheDir, { PATH: stub.pathEnv, COSIGN_FAIL: stub.failEnv, COSIGN_LOG: stub.logPath })
  );
  assert.notEqual(r.status, 0, "install proceeded with an uncheckable signature");
  assert.match(r.stderr, /could not download checksums\.txt\.sigstore\.json/, "refusal did not name the missing bundle");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "binary was executed");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "binary was cached");
});

test("unreachable checksums.txt is refused after retries (fail closed)", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  served.delete("checksums.txt");
  const requests = [];
  const port = await startServer(t, served, requests);
  const r = await runWrapper(wrapperEnv(port, cacheDir));
  assert.notEqual(r.status, 0, "install proceeded with an unreachable manifest");
  assert.match(r.stderr, /could not download checksums\.txt.*after 3 attempts/, "refusal did not name the missing manifest");
  assert.doesNotMatch(r.stdout, new RegExp(STUB_MARKER), "binary was executed without a manifest");
  assert.ok(!fs.existsSync(cachedBinary(cacheDir)), "binary was cached without a manifest");
  const manifestRequests = requests.filter((u) => u.endsWith("/checksums.txt"));
  assert.equal(manifestRequests.length, 3, `manifest was requested ${manifestRequests.length} times, expected 3 retries`);
});

test("tampered cached binary is caught and re-downloaded, never executed", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const first = await runWrapper(wrapperEnv(port, cacheDir));
  assert.equal(first.status, 0, `first run failed: ${first.stderr}`);
  assert.match(first.stdout, new RegExp(`${STUB_MARKER} __probe__`), "stub binary was not executed on first run");
  const goodHash = crypto.createHash("sha256").update(fs.readFileSync(cachedBinary(cacheDir))).digest("hex");
  // Tamper with the cached binary in place.
  fs.writeFileSync(cachedBinary(cacheDir), "tampered-binary-bytes", "utf8");
  const second = await runWrapper(wrapperEnv(port, cacheDir));
  assert.equal(second.status, 0, `re-download after tamper failed: ${second.stderr}`);
  assert.match(second.stdout, new RegExp(`${STUB_MARKER} __probe__`), "fresh binary was not executed after tamper");
  assert.match(second.stderr, /failed checksum re-verification/, "tamper was not reported as a re-verification failure");
  const restoredHash = crypto.createHash("sha256").update(fs.readFileSync(cachedBinary(cacheDir))).digest("hex");
  assert.equal(restoredHash, goodHash, "cached binary was not restored to the verified bytes");
  const assetRequests = requests.filter((u) => u.endsWith(`/${ASSET}`));
  assert.equal(assetRequests.length, 2, `archive was downloaded ${assetRequests.length} times, expected 2 (first + re-download)`);
});

test("cached binary without a checksum sidecar is re-downloaded", { timeout: 60000 }, async (t) => {
  const { served, cacheDir } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const first = await runWrapper(wrapperEnv(port, cacheDir));
  assert.equal(first.status, 0, `first run failed: ${first.stderr}`);
  assert.ok(fs.existsSync(`${cachedBinary(cacheDir)}.sha256`), "checksum sidecar was not written on first run");
  // Simulate a cache written before sidecars existed.
  fs.unlinkSync(`${cachedBinary(cacheDir)}.sha256`);
  const second = await runWrapper(wrapperEnv(port, cacheDir));
  assert.equal(second.status, 0, `re-download without sidecar failed: ${second.stderr}`);
  assert.match(second.stdout, new RegExp(`${STUB_MARKER} __probe__`), "binary was not executed after sidecar-less re-download");
  assert.ok(fs.existsSync(`${cachedBinary(cacheDir)}.sha256`), "checksum sidecar was not rewritten");
  const assetRequests = requests.filter((u) => u.endsWith(`/${ASSET}`));
  assert.equal(assetRequests.length, 2, `archive was downloaded ${assetRequests.length} times, expected 2`);
});

test("install.sh installs the untampered artifact", { timeout: 60000 }, async (t) => {
  const { served, root } = buildFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests);
  const prefix = path.join(root, "prefix");
  const r = await runAsync(["sh", INSTALL_SH, TAG], {
    ...process.env,
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_ALLOW_NON_TTY: "1",
    PREFIX: prefix,
  });
  assert.equal(r.status, 0, `install.sh failed: ${r.stderr}`);
  const installed = path.join(prefix, "bin", "codvian-router");
  assert.ok(fs.existsSync(installed), "binary was not installed");
  const run = await runAsync([installed, "__probe__"], process.env, 30000);
  assert.match(run.stdout, new RegExp(`${STUB_MARKER} __probe__`), "installed binary does not run");
});

test("install.sh refuses a tampered download", { timeout: 60000 }, async (t) => {
  const { served, root, archiveBytes } = buildFixture(t);
  served.set(ASSET, Buffer.concat([archiveBytes, Buffer.from("tampered-bytes")]));
  const requests = [];
  const port = await startServer(t, served, requests);
  const prefix = path.join(root, "prefix");
  const r = await runAsync(["sh", INSTALL_SH, TAG], {
    ...process.env,
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_ALLOW_NON_TTY: "1",
    PREFIX: prefix,
  });
  assert.notEqual(r.status, 0, "install.sh accepted a tampered archive");
  assert.match(r.stderr, /SHA-256 MISMATCH/, "refusal did not name the checksum mismatch");
  assert.ok(!fs.existsSync(path.join(prefix, "bin", "codvian-router")), "tampered binary was installed");
});

test("install.sh refuses when checksums.txt is unreachable (fail closed)", { timeout: 60000 }, async (t) => {
  const { served, root } = buildFixture(t);
  served.delete("checksums.txt");
  const requests = [];
  const port = await startServer(t, served, requests);
  const prefix = path.join(root, "prefix");
  const r = await runAsync(["sh", INSTALL_SH, TAG], {
    ...process.env,
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_ALLOW_NON_TTY: "1",
    PREFIX: prefix,
  });
  assert.notEqual(r.status, 0, "install.sh proceeded with an unreachable manifest");
  assert.match(r.stderr, /could not download checksums\.txt.*after 3 attempts/, "refusal did not name the missing manifest");
  assert.ok(!fs.existsSync(path.join(prefix, "bin", "codvian-router")), "binary was installed without a manifest");
  const manifestRequests = requests.filter((u) => u.endsWith("/checksums.txt"));
  assert.equal(manifestRequests.length, 3, `manifest was requested ${manifestRequests.length} times, expected 3 retries`);
});

// ---------------------------------------------------------------------------
// Happy path with the REAL binary (CHECKLIST 321–322)
// ---------------------------------------------------------------------------
// The tests above prove a tampered download is refused. These prove
// the untampered happy path actually works end to end: build the real
// binary, serve it as a release fixture, and run install -> --version
// -> init -> provider add -> serve -> chat completion through both
// install.sh and the npm wrapper, in an isolated HOME, against a
// local mock upstream (no network, no published release, no real
// provider). The mock answers /v1/models and /v1/chat/completions
// with fixed OpenAI-compatible JSON; the assertions check the router
// relays it, not the mock's exact bytes.

const REAL_VERSION = require("../package.json").version; // e.g. 0.1.0
const REAL_TAG = `v${REAL_VERSION}`;
const REAL_ASSET = `codvian-router_${REAL_VERSION}_linux_amd64.tar.gz`;
const REPO_ROOT = path.join(__dirname, "..");

function goBinary() {
  if (process.env.CODVIAN_ROUTER_GO) return process.env.CODVIAN_ROUTER_GO;
  if (!spawnSync("go", ["version"], { encoding: "utf8" }).error) return "go";
  const fallback = "/home/hatch/go/bin/go";
  if (fs.existsSync(fallback)) return fallback;
  return "go"; // let the build below fail with a clear error
}

// Build the real binary once per file run, into a fixed scratch dir
// (rebuilt every run so a stale binary can never mask a source
// change; `go build` is incremental, ~3 s warm). No cleanup: the dir
// is scratch under os.tmpdir(), and the next run rebuilds over it.
let realBinaryPromise = null;
function buildRealBinary() {
  if (!realBinaryPromise) {
    realBinaryPromise = (async () => {
      const dir = path.join(os.tmpdir(), "cr-real-binary");
      fs.mkdirSync(dir, { recursive: true });
      const binPath = path.join(dir, "codvian-router");
      const go = goBinary();
      const r = spawnSync(go, ["build", "-o", binPath, "./cmd/codvian-router"], {
        cwd: REPO_ROOT,
        encoding: "utf8",
        timeout: 180000,
        env: { ...process.env },
      });
      if (r.error || r.status !== 0) {
        throw new Error(`go build failed (${go}): ${r.error ? r.error.message : r.stderr}`);
      }
      return { dir, binPath };
    })();
  }
  return realBinaryPromise;
}

// Tar the real binary as the GoReleaser asset for REAL_VERSION and
// serve it with its real SHA-256 manifest. No Sigstore bundle: this
// VM has no cosign, so both install paths take the documented
// checksum-only branch (loud warning, install proceeds).
async function buildRealFixture(t) {
  const { dir, binPath } = await buildRealBinary();
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cr-real-fixture-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const assetPath = path.join(root, REAL_ASSET);
  const tar = spawnSync("tar", ["--owner=0", "--group=0", "-czf", assetPath, "codvian-router"], {
    cwd: dir,
    encoding: "utf8",
  });
  assert.equal(tar.status, 0, `tar failed: ${tar.stderr}`);
  const archiveBytes = fs.readFileSync(assetPath);
  const sha = crypto.createHash("sha256").update(archiveBytes).digest("hex");
  const served = new Map();
  served.set(REAL_ASSET, archiveBytes);
  served.set("checksums.txt", Buffer.from(`${sha}  ${REAL_ASSET}\n`, "utf8"));
  return { served, sha };
}

// A local OpenAI-compatible mock upstream: one model, one fixed
// completion. Returns its port.
function startMockUpstream(t) {
  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c) => {
      body += c;
    });
    req.on("end", () => {
      if (req.method === "GET" && req.url === "/v1/models") {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ object: "list", data: [{ id: "mock-model", object: "model", owned_by: "mock" }] }));
      } else if (req.method === "POST" && req.url === "/v1/chat/completions") {
        let model = "mock-model";
        try {
          model = JSON.parse(body).model || model;
        } catch {
          // keep the default
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(
          JSON.stringify({
            id: "chatcmpl-mock",
            object: "chat.completion",
            created: 0,
            model,
            choices: [{ index: 0, message: { role: "assistant", content: "mock hello" }, finish_reason: "stop" }],
            usage: { prompt_tokens: 5, completion_tokens: 2, total_tokens: 7 },
          })
        );
      } else {
        res.writeHead(404);
        res.end("not found");
      }
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      t.after(() => server.close());
      resolve(server.address().port);
    });
  });
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.on("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const port = s.address().port;
      s.close(() => resolve(port));
    });
  });
}

function httpJson(port, reqPath, { method = "GET", body = null, key = null } = {}) {
  return new Promise((resolve, reject) => {
    const data = body ? JSON.stringify(body) : null;
    const req = http.request(
      {
        host: "127.0.0.1",
        port,
        path: reqPath,
        method,
        headers: {
          ...(data ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) } : {}),
          ...(key ? { Authorization: `Bearer ${key}` } : {}),
        },
      },
      (res) => {
        let raw = "";
        res.setEncoding("utf8");
        res.on("data", (c) => {
          raw += c;
        });
        res.on("end", () => {
          let json = null;
          try {
            json = JSON.parse(raw);
          } catch {
            // raw stays for the assertion message
          }
          resolve({ status: res.statusCode, raw, json });
        });
      }
    );
    req.on("error", reject);
    if (data) req.write(data);
    req.end();
  });
}

// Isolated HOME for a router child process: config + data dir both
// land under it, never the operator's real HOME. XDG_CONFIG_HOME /
// XDG_DATA_HOME are pinned into the same home because Go's
// os.UserConfigDir prefers them over $HOME when set.
function isolatedHome(t, extra = {}) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "cr-home-"));
  t.after(() => fs.rmSync(home, { recursive: true, force: true }));
  const env = {
    ...process.env,
    HOME: home,
    XDG_CONFIG_HOME: path.join(home, ".config"),
    XDG_DATA_HOME: path.join(home, ".local", "share"),
    ...extra,
  };
  return { home, env };
}

async function startServe(argv, env, port) {
  const child = spawn(argv[0], [...argv.slice(1), "--port", String(port), "serve"], { env });
  let stderr = "";
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (d) => {
    stderr += d;
  });
  const deadline = Date.now() + 20000;
  for (;;) {
    try {
      const r = await httpJson(port, "/healthz");
      if (r.status === 200 && r.json && r.json.status === "ok") return child;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) {
      child.kill("SIGKILL");
      throw new Error(`serve did not become healthy on :${port} within 20s. stderr so far:\n${stderr}`);
    }
    await new Promise((r) => setTimeout(r, 250));
  }
}

async function stopServe(child) {
  if (child.exitCode !== null) return;
  child.kill("SIGTERM");
  await Promise.race([
    new Promise((r) => child.on("close", r)),
    new Promise((r) =>
      setTimeout(() => {
        child.kill("SIGKILL");
        r();
      }, 5000)
    ),
  ]);
}

// The full installed-binary happy path: --version -> init ->
// provider add (mock) -> serve -> healthz / 401 / models / chat.
// `argv` is the installed binary, or ["node", wrapper] for npm.
async function happyPathFlow(t, argv, home, env, mockPort) {
  const version = await runAsync([...argv, "--version"], env);
  assert.equal(version.status, 0, `--version failed: ${version.stderr}`);
  assert.equal(version.stdout.trim(), `codvian-router v${REAL_VERSION}`, "--version printed the wrong version");

  const keyFile = path.join(home, "key.txt");
  const init = await runAsync([...argv, "init", "--key-file", keyFile], env);
  assert.equal(init.status, 0, `init failed: ${init.stderr}`);
  const key = fs.readFileSync(keyFile, "utf8").trim();
  assert.match(key, /^crk_[A-Za-z0-9]{32}$/, "gateway key has the wrong shape");
  assert.ok(
    fs.existsSync(path.join(home, ".config", "codvian-router", "config.json")),
    "init did not write config.json under the isolated HOME"
  );

  const add = await runAsync(
    [...argv, "provider", "add", `http://127.0.0.1:${mockPort}/v1`, "test-mock-key", "--name", "mock"],
    env
  );
  assert.equal(add.status, 0, `provider add failed: ${add.stderr}`);
  assert.match(add.stdout, /models: 1/, "mock provider did not report 1 model");

  const port = await freePort();
  const child = await startServe(argv, env, port);
  t.after(() => stopServe(child));

  const noKey = await httpJson(port, "/v1/models");
  assert.equal(noKey.status, 401, "GET /v1/models without a key must be 401");
  const health = await httpJson(port, "/healthz");
  assert.equal(health.status, 200, `healthz: ${health.status} ${health.raw}`);
  assert.deepEqual(health.json, { status: "ok" });
  const models = await httpJson(port, "/v1/models", { key });
  assert.equal(models.status, 200, `GET /v1/models with key: ${models.status} ${models.raw}`);
  assert.ok(
    models.json.data.some((m) => m.id === "mock-model"),
    `mock-model missing from /v1/models: ${models.raw}`
  );
  const chat = await httpJson(port, "/v1/chat/completions", {
    method: "POST",
    key,
    body: { model: "mock-model", messages: [{ role: "user", content: "Hello" }] },
  });
  assert.equal(chat.status, 200, `POST /v1/chat/completions: ${chat.status} ${chat.raw}`);
  assert.equal(chat.json.choices[0].message.content, "mock hello", "completion content is not the mock's");
  assert.equal(chat.json.usage.total_tokens, 7, "completion usage is not the mock's");
}

test("install.sh + real binary: install, --version, init, serve smoke", { timeout: 180000 }, async (t) => {
  const { served } = await buildRealFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests, REAL_TAG);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cr-happy-install-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const prefix = path.join(root, "prefix");
  const r = await runAsync(["sh", INSTALL_SH, REAL_TAG], {
    ...process.env,
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_ALLOW_NON_TTY: "1",
    PREFIX: prefix,
  });
  assert.equal(r.status, 0, `install.sh failed: ${r.stderr}`);
  // install.sh prints its progress to stdout (the npm wrapper uses
  // stderr); check the combined output.
  const out = `${r.stdout}\n${r.stderr}`;
  assert.match(out, /Checksum verified \(sha256\)/, "checksum confirmation missing");
  assert.match(out, /WARNING: cosign not found/, "no-cosign warning missing (this VM has no cosign)");
  const installed = path.join(prefix, "bin", "codvian-router");
  assert.ok(fs.existsSync(installed), "binary was not installed");

  const mockPort = await startMockUpstream(t);
  const { home, env } = isolatedHome(t);
  await happyPathFlow(t, [installed], home, env, mockPort);
});

test("npm wrapper + real binary: first-run download, --version, init, serve smoke", { timeout: 180000 }, async (t) => {
  const { served } = await buildRealFixture(t);
  const requests = [];
  const port = await startServer(t, served, requests, REAL_TAG);
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cr-happy-npm-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const cacheDir = path.join(root, "cache");
  const mockPort = await startMockUpstream(t);
  const { home, env } = isolatedHome(t, {
    CODVIAN_ROUTER_DOWNLOAD_BASE_URL: `http://127.0.0.1:${port}/releases/download`,
    CODVIAN_ROUTER_CACHE_DIR: cacheDir,
  });
  await happyPathFlow(t, ["node", WRAPPER], home, env, mockPort);
  assert.ok(requests.some((u) => u.endsWith(`/${REAL_ASSET}`)), "wrapper did not download the release asset");
  assert.ok(
    fs.existsSync(path.join(cacheDir, "codvian-router-linux-amd64")),
    "wrapper did not cache the downloaded binary"
  );

  // Second run must use the cache, not download again.
  const again = await runAsync(["node", WRAPPER, "--version"], env);
  assert.equal(again.status, 0, `cached --version failed: ${again.stderr}`);
  assert.equal(again.stdout.trim(), `codvian-router v${REAL_VERSION}`);
  assert.doesNotMatch(again.stderr, /first run — downloading/, "second run re-downloaded instead of using the cache");
});

test("install.sh refuses piped stdin", { timeout: 30000 }, async (t) => {
  const r = await runAsync(["sh", "-c", `cat "${INSTALL_SH}" | sh`], process.env);
  assert.notEqual(r.status, 0, "piped install.sh was accepted");
  assert.match(r.stderr, /refusing to run with stdin piped/, "refusal did not name the pipe guard");
});
