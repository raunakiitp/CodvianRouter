# Developer tooling (scripts/)

Each script is a one-purpose entry point. Local workflows mirror CI; exit
codes are 0 = success, non-zero = failure. Supported platform notes are
given per script. Run anything from the repo root.

## CI gate (the authoritative test gate)
- `scripts/ci.sh` - gofmt, `go build ./...`, `go vet ./...`,
  `go test ./... -race -count=1`, Docker go-version match. Runs in
  `.github/workflows/ci.yml`. Optional bench smoke: `CI_BENCH_SMOKE=1`.
- `scripts/check-route-guard.sh` - CI bonus: spawn-capable routes must be
  LOCAL_ONLY (mirrors `TestRouteTableSpawnCapableAreLocalOnly`).
- `scripts/audit-fake.sh` - static "UI without backend" audit of the
  dashboard (slow; run on demand, not in the default CI gate).

## Benchmarks (Linux authoritative; see docs/BENCHMARK.md)
- `scripts/bench.sh` - the reproducible latency/RPS/memory rig
  (Geekflare-derived). Requires Linux (`flock`, `/proc/loadavg`) and a
  quiet machine; refuses to publish while the host is busy. Do not run on
  Windows (no `flock`). `.github/workflows/bench-infra-smoke.yml` runs a
  one-iteration compile/execute smoke on Linux CI.
- `scripts/bench-geekflare.sh`, `scripts/bench-loadgen.py`,
  `scripts/bench-mock-upstream.py` - underlying rig pieces used by bench.sh.

## Supply chain (G-06)
- `scripts/sbom.sh` / `scripts/sbom.py` - CycloneDX-1.4 SBOM of the Go
  module graph. `--self-check` validates generation for CI. Vulnerabilities
  are scanned by `govulncheck` in `.github/workflows/supply-chain.yml`.

## Smoke scripts (smoke-*.py) and status
- `scripts/status.sh`, `scripts/gate.sh`, `scripts/wip.sh`,
  `scripts/pkgtest.sh` - developer conveniences.
- `scripts/smoke-*.py` - end-to-end smoke scripts (client, failover,
  budgets, responses, security cluster, ...). They exercise the real
  binary; see each file's header for its provider/credential needs.
- `scripts/run-client-smokes.sh` - runs the five M8.1 client-protocol
  drivers (`smoke-client-{claude-code,codex,cursor,cline,aider}.py`)
  sequentially against a router each driver boots itself; every upstream
  is a localhost mock (no network, no credential). Runs in
  `.github/workflows/client-smoke.yml`.

## Notes / maintenance
- Never `rm` a script another workflow or doc references without grepping
  for it first. `scripts/bench-*.sh` share the `.bench-lock`.
- When adding a route, run `python scripts/gen-api-docs.py` to regenerate
  `docs/API_REFERENCE.md`, `docs/llm.txt`, `web/tryit.html` and
  `tests/fixtures/v1/m8.21/routes.json` together.
