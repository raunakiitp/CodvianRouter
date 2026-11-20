# API fixtures — contract-test harness (M8.0)

Contract tests for M8 (API breadth) run against **recorded fixtures**,
not live providers. The harness is `internal/apitest/` (stdlib only);
this file is the schema every M8 slice must follow. Changing the schema
means updating this file, the harness, and every fixture in the tree.

## The one rule

**A fixture is recorded mock data, never live provider data.**

- `recorded_response` is what a mock upstream returned when the
  fixture was recorded (or a hand-written canned response in the same
  shape). No real provider is ever called to produce a fixture.
- `provider` names the mock upstream (e.g. `"mock-upstream"`), never a
  real provider account. `model` names the mock model.
- Unknown prices stay `$0`/unknown. A fixture must never invent a
  price, a capability, or a provider behaviour.
- Every fixture file must carry `"fixture": true`. The loader rejects
  any file without it — that flag is the label that keeps mock data
  from ever being presented as live data.

## Layout and naming

```
tests/fixtures/v1/<slice>/<name>.json
```

- `<slice>` is the M8 slice id that owns the fixture, e.g. `m8.0`,
  `m8.2`. One directory per slice.
- `<name>` is kebab-case, `<endpoint>-<expected-outcome>`, e.g.
  `chat-completions-200`, `chat-completions-402-unpriced-model`.
  The file name is `<name>.json`; the `name` field inside the file
  must match it, and `slice` must match the directory.
- Path components are plain file names only — the loader rejects
  separators and `..`.

## Schema (version 1)

| Field | Required | Meaning |
|---|---|---|
| `schema_version` | yes | Must be `1`. |
| `slice` | yes | Owning M8 slice id; must match the directory. |
| `name` | yes | Fixture name; must match the file name. |
| `fixture` | yes | Must be `true` (recorded mock data label). |
| `method` | yes | HTTP method the test sends, e.g. `"POST"`. |
| `path` | yes | Request path, starting with `/`, e.g. `"/v1/chat/completions"`. |
| `provider` | yes | Mock upstream id, e.g. `"mock-upstream"`. |
| `model` | yes | Mock model id, e.g. `"mock-model"`. |
| `recorded_response.status` | yes | HTTP status the mock upstream returned. |
| `assert.status` | yes | HTTP status the handler under test must return. |
| `recorded_from` | no | How the recorded response was produced, e.g. `"mock-upstream: canned OpenAI-shaped response, 2026-11-19"`. |
| `request.headers` | no | Headers the test sends. |
| `request.body` | no | JSON request body the test sends. |
| `recorded_response.headers` | no | Headers the mock upstream returned. |
| `recorded_response.body` | no | JSON body the mock upstream returned. |
| `assert.required_fields` | no | Dotted JSON paths that must exist in the response. |
| `assert.json_paths` | no | Dotted JSON path → expected value. `null` means "must exist, any value". |
| `notes` | no | Free text: what this fixture pins and what it does not. |

Dotted paths use `.` segments; numeric segments index arrays
(`"choices.0.message.role"`). Numbers compare by JSON value, so `10`
and `10.0` are equal.

## Using the harness

```go
f := apitest.LoadFixture(t, "m8.2", "chat-completions-200")
apitest.Run(t, handlerUnderTest, f)
```

`LoadFixture` fails the test on a missing file, bad JSON, an unknown
field, or any schema violation (wrong `schema_version`, `fixture`
not `true`, missing required field, `slice`/`name` mismatch).
`Run` serves the handler on an `httptest` server, sends the fixture's
request, and fails the test on the first mismatch: status, a missing
required field, or a JSON-path value.

The test wires the mock upstream itself — typically an `httptest`
server whose handler replays `f.RecordedResponse` — and passes the
handler under test (not the mock) to `Run`. See
`internal/apitest/fixture_test.go` (`TestRunChatCompletions200` with
its test-only forwarding proxy) for the pattern, and
`tests/fixtures/v1/m8.0/chat-completions-200.json` for a complete
example fixture.

## Harness behaviours to know

- `path` may carry a query string: the loader only requires a leading
  `/`, and `Run` appends the path verbatim to the test-server URL.
- A status-only fixture (no `required_fields`, no `json_paths`) passes
  a non-JSON body: `CheckResponse` returns after the status check
  without parsing the body.
- `Run` uses the test server's default client: it follows redirects
  (up to 10) and has no timeout — a hanging handler hangs the test.
- `recorded_from` has no required format beyond saying the response
  is mock data; the loader never validates its content.

## Toggle rule

Every new contract test must fail when the feature is removed. For
the harness itself, a fixture with a wrong expected status must fail
the test: `internal/apitest/fixture_test.go` proves this with
`TestCheckResponseWrongStatusFails` (assert 201 against a 200
response), `TestCheckResponseWrongJSONPathFails`, and
`TestCheckResponseMissingRequiredFieldFails`. Slice tests that add a
fixture must include the matching negative case (wrong status / wrong
contract field) or a note in the slice's gate section explaining why
the positive test alone is toggle-proven (e.g. it fails against the
pre-slice handler).

## Adding a fixture (checklist for M8 slices)

1. Write `tests/fixtures/v1/<slice>/<name>.json` per the schema above.
2. Label it: `"fixture": true` and a `recorded_from` that says it is
   mock data. No real prices, no real provider behaviour.
3. Write the test: load it with `LoadFixture`, wire a mock upstream
   replaying `recorded_response`, run it with `Run` against the real
   handler.
4. Add the negative case (wrong expected status must fail).
5. Run `go test -race -count=1 ./internal/apitest/` plus the slice's
   own package tests; record the gate in CHECKLIST.md.
