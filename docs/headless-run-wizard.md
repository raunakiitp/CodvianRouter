# Headless mode, `run <cli> --model`, and the first-run wizard

CHECKLIST 197–198 (capability gaps closed — headless mode
+ `run <cli> --model` onboarding).

## `serve --headless`

```sh
codvian-router serve --headless
```

Starts the gateway as a pure API process for servers and containers:
no dashboard server, no browser/tray behaviour. Every `/dashboard*`
route answers **404** (`dashboard disabled in headless mode — this
server serves the API only`), while `/v1/*`, `/metrics` and
`/healthz` keep working exactly as in normal mode.

**It refuses to start without a gateway key configured**
(auth-on-from-first-boot — never a headless unauthenticated gateway).
The check runs in `runServe` before the stack is built, so a missing
config file or a config with zero gateway keys exits 1 with:

```text
codvian-router: serve --headless refused: no gateway key configured in … — run `codvian-router init` to create one first …
```

Create the key first with `codvian-router init` (or start a normal
server once and `POST /admin/bootstrap`), then start headless. The
startup lines print `headless: ON (API only …)` and
`dashboard: disabled in headless mode` instead of the dashboard URL.

## `run <cli> --model <model>`

```sh
codvian-router run codex --model gpt-4o
codvian-router run codex --model gpt-4o -- --sandbox workspace-write
```

Launches a named local CLI with the router as its base URL. The CLI
runs with its stdio inherited and its exit code propagated, and it
sees these environment variables:

| Variable | Value |
|---|---|
| `OPENAI_BASE_URL` | `http://127.0.0.1:<port>/v1` |
| `OPENAI_API_KEY` | the gateway key for this run |
| `CODVIAN_ROUTER_BASE_URL` | same base URL |
| `CODVIAN_ROUTER_API_KEY` | same gateway key |
| `CODVIAN_ROUTER_MODEL` | the `--model` value |

Only OpenAI-wire CLIs pick `OPENAI_*` up automatically; a CLI that
speaks another wire needs its own configuration (the
`CODVIAN_ROUTER_*` vars name the router explicitly so a wrapper can
use them). Extra arguments after `--` are passed to the CLI verbatim;
`--model` is consumed by the router and never forwarded.

How the gateway is provided:

- **No gateway running** (the just-works path): `run` issues a
  **fresh ephemeral gateway key** (`run-<cli>`, chat+models scope),
  starts its own `serve --headless` gateway, waits up to 15 s for
  `/healthz`, launches the CLI, and when the CLI exits stops the
  gateway and **revokes the ephemeral key**. If revocation fails, the
  key ID is printed so it can be removed by hand (the raw key is
  never printed).
- **A gateway is already running** on the configured port: `run`
  reuses it — but only with a key from `CODVIAN_ROUTER_API_KEY`.
  A stored gateway key's raw value cannot be recovered from the
  config (only its hash is stored), and a key issued now would be
  unknown to the already-running process (each process verifies
  against its own in-memory config), so without that variable `run`
  refuses honestly instead of launching a CLI that cannot
  authenticate: stop the running gateway first, or set
  `CODVIAN_ROUTER_API_KEY` to an existing key.

Honest failures (exit 1 or 2, nothing launched):

- missing CLI binary: `CLI "foo" not found on PATH — install it first (nothing was launched)`
- missing `--model`: `run needs --model <model> …`
- no providers configured: `no providers configured — … run init --wizard first`
- headless gateway never becomes healthy within 15 s: the gateway is
  killed, the ephemeral key revoked, the CLI never launched.

## `init --wizard` (first-run wizard)

```sh
codvian-router init --wizard   # `codvian-router wizard` is the same
```

An interactive walkthrough: port → provider choice → API key →
model → **real test call** → done. Every step is skippable with an
empty answer (or EOF on piped input), and every failure is reported
honestly without aborting the rest of the wizard.

1. **Port** — default is the configured listen port (20128 on a fresh
   install); an invalid port keeps the current one.
2. **Provider** — a catalog id (e.g. `groq`, `openai`, `anthropic`,
   `deepseek`). An unknown id is reported and re-prompted once, then
   the step is skipped.
3. **API key** — typed, never printed. The provider is added with the
   same live key test as `provider add-catalog`: a bad key, a dead
   upstream, or a ToS-grey entry is reported as `provider NOT added:
   …` and the wizard continues.
4. **Model + test call** — defaults to the added provider's first
   model. The test is one real chat call through the live engine
   (`Say hello in one short sentence.`, 60 s timeout); success prints
   the reply (first 200 runes), failure prints `test call FAILED: …`
   and the config is still saved.

On a fresh install the wizard creates the config and the first
gateway key (shown once). On an **existing** config it only adds a
provider and tests — it never overwrites anything already configured,
so it is safe to re-run any time.

## Dashboard

Settings has a "Headless mode & first-run wizard" card fed live by
`GET /dashboard/headless` (Tier 3, auth-exempt, no secrets):

```json
{"headless": false, "dashboard_enabled": true, "wizard_available": true,
 "headless_command": "codvian-router serve --headless",
 "wizard_command": "codvian-router init --wizard",
 "run_command": "codvian-router run <cli> --model <model>",
 "note": "Headless mode serves the API only — …"}
```

A dashboard server always reports `headless: false` — a headless
server has no dashboard to ask (the feed itself 404s there, like
every `/dashboard*` route). The dashboard cannot spawn processes,
so the wizard is **not** run from the browser: the card shows the
exact terminal command and a copy button, and says so plainly.

## Limits (honest)

- `run` only sets OpenAI-wire environment variables; non-OpenAI-wire
  CLIs need their own configuration.
- `run` reusing an already-running gateway needs
  `CODVIAN_ROUTER_API_KEY` — a stored key cannot be recovered.
- The wizard's test call goes through the engine, so it spends one
  real (tiny) upstream call; with no provider there is nothing to test.
- No rendered-browser visual check of the new Settings card (headless
  Chromium will not start in this VM); the card is HTTP-verified via
  the feed and `node --check` on the dashboard JS.
