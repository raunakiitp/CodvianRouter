# Codvian Router — Config Schema (v1)

The router reads its configuration from a single JSON file (default
`~/.config/codvian-router/config.json`). This document is the authoritative
description of version 1 of that file. The code that implements it lives
in `internal/config/config.go`; if this document and the code ever
disagree, the code wins and this document must be fixed.

## Top-level fields

| Field | JSON name | Type | Required | Default | Constraints |
|---|---|---|---|---|---|
| Version | `version` | integer | yes | `1` | Must be exactly `1`. Any other value is rejected on load. |
| Listen | `listen` | string | yes | `"127.0.0.1:20128"` | `host:port`. The host must be a loopback IP (`127.0.0.1` or `::1`); the router is local-only and never binds `0.0.0.0` or a LAN address. The port must be present and in range 1–65535. |
| DataDir | `data_dir` | string | yes | `~/.local/share/codvian-router` | Must not be empty. Created with mode `0700` if missing. |
| GatewayKeys | `gateway_keys` | array of gateway-key objects | no | `[]` | Each entry: `id` (string, unique), `name` (string), `key_hash` (hex SHA-256 of the raw key — the raw key is never stored), `scopes` (array of strings), `budget_usd` (number, ≥ 0), `created_at` (RFC 3339 timestamp). Duplicate `id` values are rejected by `ValidateFile`. See `gatewaykey.go`. |
| Providers | `providers` | array of provider objects | no | `[]` | Each entry is a `provider.ProviderConfig`: `id` (string, unique, non-empty), `name` (string), `base_url` (string, non-empty, unique after normalization), `api_key_encrypted` (string, sealed value — never plaintext), `wire_format` (string), `models` (array of `{id, name, context_window?, input_price_usd?, output_price_usd?}`, optional), `enabled` (boolean), `quota_hint` (number), `created_at` (RFC 3339 timestamp). Duplicate `id` values and duplicate `base_url` values (after normalization) are rejected by `ValidateFile`. |
| BudgetUSD | `budget_usd` | number (float) | no | `0` | Global spend cap in USD. `0` means no cap. Must not be negative. |
| FailClosed | `fail_closed` | boolean | no | `true` | When `true`, the router refuses work instead of bypassing limits when a check cannot be performed. |
| TelemetryEnabled | `telemetry_enabled` | boolean | no | `false` | No telemetry is ever sent unless this is explicitly `true`. |
| AutoRouter | `auto_router` | object | no | `{threshold: 0.5, default_tier: "reasoning"}` | Local auto-router (`auto/*` virtual models). `threshold` (number, 0 = default 0.5, otherwise in (0, 1]): a tier match below it falls back to the default tier instead of guessing. `thresholds` (object, virtual-model name → number in (0, 1]) overrides the threshold per virtual model (`auto`, `auto/fast`, `auto/cheap`, `auto/coding`, `auto/reasoning`). `default_tier` (string, one of `coding`, `chat`, `reasoning`, `quick-qa`) is the fallback tier. `default_model` (string) is the concrete model used when the picked tier has no `tier_models` entry; empty means auto-routing fails honestly instead of inventing a model. `tier_models` (object, tier name → array of concrete model IDs in preference order) maps tiers to models. No keys are stored here — only model names. |

Notes on "required": a missing field decodes to its Go zero value, and
`Validate` then rejects the file if that zero value is unsafe (for
example a missing `version` decodes to `0` and is rejected, and a missing
`data_dir` is rejected as empty). A missing `listen` is rejected because
it has no host or port. `gateway_keys` and `providers`, when absent, are
treated as empty lists. `fail_closed` absent decodes to `false` — callers
that create a config should use `DefaultConfig`, which sets the safe
defaults above, rather than hand-writing a partial file.

The file itself is written atomically (temp file + rename) with mode
`0600`.

## Unknown-field preservation

A v1 reader **preserves** any top-level field it does not recognise:

- On load, every top-level key outside the table above is stored, raw
  JSON bytes included, in `Config.Extensions`
  (`map[string]json.RawMessage`), keyed by its JSON name.
- On save, each entry is re-emitted as a top-level key with its raw
  value verbatim, so a Load → Save round-trip does not lose, rename, or
  reinterpret fields written by a newer (or older, or foreign) writer.
- `Extensions` itself is tagged `json:"-"`: it never appears as a field
  of its own in the file. If an extension key collides with a known
  field name, the known field wins and the extension entry is dropped
  on marshal.
- Unknown fields are **never validated or interpreted** by v1 code.
  They are opaque payload carried through, nothing more.

Example: a file containing `"future_feature": {"enabled": true}` loads
fine under v1, and after the router saves the config the key is still
there with the same value.

## Forward-compatibility promise

- **v1 readers ignore nothing they do not understand** — unknown fields
  survive round-trips, as above. A newer writer's extra fields are
  safe to carry through a v1 binary.
- A future **v2 will be a separate migration, not a silent
  reinterpretation**: it will use `version: 2`, v1 readers will reject
  it outright (unsupported version), and any upgrade path will be an
  explicit, documented migration step. The meaning of an existing v1
  field will never change under the v1 version number.
- The one thing v1 does *not* promise: unknown fields inside a
  *nested* object (for example an extra key inside one `providers`
  entry) are handled by that nested type's own decoding, not by
  `Extensions`, and may be dropped. Only top-level unknown fields are
  preserved.
