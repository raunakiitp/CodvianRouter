# Demo — failover and hedging against local mock upstreams

> **No hosted demo exists.** This demo runs entirely on your machine: two
> local Python mock servers stand in for providers, and the real
> `codvian-router` binary routes between them. Everything below was run
> end to end on 2026-11-17 (Go 1.27.4, this VM) and reproduced the
> coordinator's gate numbers; the gate's own numbers are quoted alongside
> and labelled as such.

## What it proves

| Step | Proves |
|---|---|
| 1 — health + auth | The binary starts, `/healthz` answers, requests without a key get 401 |
| 2 — 429 failover | Provider A returns 429 instantly; the router fails over to B and the request still succeeds in ~52–56 ms |
| 3 — hedge | Provider A is slow (2,000 ms); the router starts a backup attempt on B after 150 ms and B wins in ~204 ms |
| 4 — ledger | The SQLite ledger records the failed A attempt (0/0/0 tokens, `split_known=0`) and the successful B attempt (10/5/15, `split_known=1`, cost $0) — honest accounting, nothing invented |

## Prerequisites

- The binary built from source ([INSTALL.md](INSTALL.md)): `go build -o
  codvian-router ./cmd/codvian-router`
- Python 3 (for the mocks) and `curl`
- Go 1.22+ (only for the one-off config builder in step 0 — there is no
  `add-provider` CLI yet, so the demo config is built with the router's
  own packages)

## Step 0 — build the demo config

There is no CLI to add a provider yet, so build the config with a
throwaway program that uses the router's own `config`, `crypto`, and
`provider` packages. **Put it in a scratch copy of the repo, not in the
real tree** (`cp -r CodvianRouter /tmp/router-demo`, then create
`/tmp/router-demo/cmd/mkdemo/main.go` with the code below).

```go
// Command mkdemo builds a demo config (throwaway tool).
package main

import (
	"fmt"
	"os"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

type cfgStore struct {
	cfg  *config.Config
	path string
}

func (s *cfgStore) Load() ([]provider.ProviderConfig, error) {
	out := make([]provider.ProviderConfig, len(s.cfg.Providers))
	copy(out, s.cfg.Providers)
	return out, nil
}

func (s *cfgStore) Save(cfgs []provider.ProviderConfig) error {
	s.cfg.Providers = append([]provider.ProviderConfig(nil), cfgs...)
	return s.cfg.Save(s.path)
}

func main() {
	cfgPath, dataDir := os.Args[1], os.Args[2]
	cfg := config.DefaultConfig(dataDir)
	if err := cfg.EnsureDataDir(); err != nil {
		panic(err)
	}
	key, raw, err := config.IssueKey("demo", []string{"chat", "models"}, 0)
	if err != nil {
		panic(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		panic(err)
	}
	vault, err := crypto.New(dataDir)
	if err != nil {
		panic(err)
	}
	reg := provider.NewRegistry(&cfgStore{cfg: cfg, path: cfgPath},
		provider.WithCipher(provider.NewVaultCipher(vault)))
	if err := reg.Load(); err != nil {
		panic(err)
	}
	// The mocks must already be running: Add() tests the key inline
	// (GET /v1/models) and auto-fetches the model list.
	for _, p := range [][2]string{
		{"mock-a", "http://127.0.0.1:21001"},
		{"mock-b", "http://127.0.0.1:21002"},
	} {
		prov, err := reg.AddNamed(p[0], p[1], "mock-key")
		if err != nil {
			panic(fmt.Errorf("add %s: %w", p[0], err))
		}
		fmt.Printf("added %s id=%s models=%v\n", p[0], prov.ID, prov.Models)
	}
	if err := cfg.Save(cfgPath); err != nil {
		panic(err)
	}
	fmt.Printf("gateway key: %s\n", raw)
}
```

## Step 1 — start the mocks

Save this as `mock.py`. It serves `GET /v1/models` (always lists
`bench-model`) and `POST /v1/chat/completions` in three modes:
`429` (instant 429 with `Retry-After: 1`), `slow` (2,000 ms delay, then
success), `ok` (50 ms delay, then success). The success body reports
usage `prompt_tokens 10 / completion_tokens 5 / total_tokens 15` — the
same numbers the gate's ledger check used.

```python
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODE = sys.argv[2]  # "429", "slow", or "ok"
PORT = int(sys.argv[1])

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/v1/models":
            body = json.dumps({"object": "list",
                "data": [{"id": "bench-model", "object": "model"}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body)
        else:
            self.send_response(404); self.end_headers()
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        self.rfile.read(n)
        if self.path == "/v1/chat/completions":
            if MODE == "429":
                body = json.dumps({"error": {"message": "rate limited",
                    "type": "rate_limit_error"}}).encode()
                self.send_response(429)
                self.send_header("Retry-After", "1")
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers(); self.wfile.write(body)
                return
            time.sleep(2.0 if MODE == "slow" else 0.05)
            body = json.dumps({
                "id": "chatcmpl-mock", "object": "chat.completion",
                "model": "bench-model",
                "choices": [{"index": 0, "message": {"role": "assistant",
                    "content": "hello from mock"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 10, "completion_tokens": 5,
                          "total_tokens": 15},
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body)
            return
        self.send_response(404); self.end_headers()

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
```

Start mock A in `429` mode and mock B in `ok` mode, then build the config
(the builder calls the mocks, so they must be up first):

```sh
python3 mock.py 21001 429 &
python3 mock.py 21002 ok &
cd /tmp/router-demo
go run ./cmd/mkdemo /tmp/router-demo-config/config.json /tmp/router-demo-config/data
```

Expected output (provider IDs are random per run; the gateway key is
yours — keep it):

```text
added mock-a id=prov_<random> models=[{bench-model bench-model 0 0 0}]
added mock-b id=prov_<random> models=[{bench-model bench-model 0 0 0}]
gateway key: crk_<32 random characters>
```

## Step 2 — start the router, check health and auth

```sh
/tmp/codvian-router --config /tmp/router-demo-config/config.json --port 20199 serve
```

Expected startup lines:

```text
codvian-router v0.1.0 listening on 127.0.0.1:20199 (auth ON)
config: /tmp/router-demo-config/config.json
providers: 2 configured, 2 clients ready
```

Then, with `KEY` set to the gateway key from step 1:

```sh
curl -s http://127.0.0.1:20199/healthz
# {"status":"ok"}

curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:20199/v1/models
# 401
```

## Step 3 — 429 failover (mock A returns 429, mock B serves)

```sh
curl -s -w "\nHTTP %{http_code}  total %{time_total}s\n" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"bench-model","messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:20199/v1/chat/completions
```

Expected (verified in this docs run, 2026-11-17, 3/3 requests):

```json
{"id":"chatcmpl-<random>","object":"chat.completion","created":<unix time>,"model":"bench-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello from mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
HTTP 200  total 0.0524s
```

Timings in this docs run: **0.0524–0.0543 s** for 3 requests. The gate's
own measurement was **52.4–55.8 ms, 5/5 OK**, i.e. ~1–2 ms of router
overhead vs 51.2 ms direct to mock B. The 429 on A never reaches you —
the request succeeds via B.

## Step 4 — hedge (mock A slow, mock B wins)

Stop mock A and the router, restart mock A in `slow` mode, restart the
router (a fresh router avoids A's 429 cooldown from step 3):

```sh
kill <mock-A pid> <router pid>
python3 mock.py 21001 slow &
/tmp/codvian-router --config /tmp/router-demo-config/config.json --port 20199 serve &
```

Run the same curl as step 3, three times. Expected (verified in this
docs run, 2026-11-17, 3/3 requests):

```text
HTTP 200  total 0.2051s
HTTP 200  total 0.2040s
HTTP 200  total 0.2038s
```

Timings in this docs run: **203.8–205.1 ms**. The gate's own measurement
was **203.9–204.4 ms, 5/5 OK** — 150 ms hedge delay + ~51 ms for B. The
router did not wait for A's 2,000 ms: it started the backup attempt on B
after the default 150 ms hedge delay and returned B's answer. The
response body is identical to step 3.

## Step 5 — quota hint 0 skips a provider entirely

Edit `/tmp/router-demo-config/config.json`: set mock A's
`"quota_hint": 0`, restart the router, and run the step 3 curl again.
Expected: the request still succeeds in ~52 ms — A is never attempted
and leaves no ledger row. (Gate measurement: **52.2–53.5 ms, 5/5 OK**.
This step was verified by the coordinator's gate, not re-run in this
docs run.)

## Step 6 — check the ledger

The ledger is SQLite at `/tmp/router-demo-config/data/ledger.db`, table
`usage_events` (columns: `id, ts, gateway_key_id, provider, model,
prompt_tokens, completion_tokens, total_tokens, tokens_split_known,
cost_usd, ok, latency_ms`).

```sh
python3 -c "
import sqlite3
c = sqlite3.connect('/tmp/router-demo-config/data/ledger.db')
for row in c.execute('SELECT provider, model, prompt_tokens, completion_tokens, total_tokens, tokens_split_known, cost_usd, ok, latency_ms FROM usage_events ORDER BY id'):
    print(row)
"
```

Expected rows from the verified run (excerpt — one failed-A row and one
successful-B row per step are shown; the full run had 3 B rows in step 3
and 3 B-win + 3 A-loser rows in step 4. Provider IDs differ per run;
`prov_dac0…` is mock A, `prov_f2eb…` is mock B):

```text
# Step 3 (429 failover): A failed, B succeeded
('prov_dac0bd1785bfb90c', 'bench-model', 0, 0, 0, 0, 0.0, 0, 1)
('prov_f2eb835e643c2046', 'bench-model', 10, 5, 15, 1, 0.0, 1, 51)
# Step 4 (hedge): B won, the hedged A loser is recorded as a failed
# attempt with zero tokens — never as a success, never with invented usage
('prov_f2eb835e643c2046', 'bench-model', 10, 5, 15, 1, 0.0, 1, 51)
('prov_dac0bd1785bfb90c', 'bench-model', 0, 0, 0, 0, 0.0, 0, 202)
```

What this proves: a failed attempt records `prompt 0 / completion 0 /
total 0` with `tokens_split_known=0`; a successful attempt records the
full split with `tokens_split_known=1` and `cost_usd 0.0` (no per-model
prices configured — cost is never invented). A quota-skipped provider
leaves no row at all.

## Step 7 — streaming failover (gate-verified, not re-run here)

The gate verified streaming with a 429 failover: SSE chunks `bench ` +
`ok` + `stop` + `[DONE]` arrive correctly. To repeat it, send the step 3
request with `"stream": true` and inspect the SSE stream. This step is
listed from the gate report; the docs run did not re-exercise streaming
(the mock above answers non-streaming JSON).

## Cleanup

```sh
kill <mock-A pid> <mock-B pid> <router pid>
rm -rf /tmp/router-demo /tmp/router-demo-config
```

## What this demo does NOT show

- Real providers — the mocks answer in 50 ms on localhost. Real-world
  failover and hedge numbers depend on upstream latency; the numbers
  above are mock measurements, not a public benchmark.
- The `auto` model / smart routing modes — planned (Phase 2), not built.
- The semantic cache in the request path — built as a package, not yet
  wired into the server/engine path (Phase 2 wiring).
- Costed usage — cost is $0 in this demo because no per-model prices are
  configured; that is honest, not a bug.
