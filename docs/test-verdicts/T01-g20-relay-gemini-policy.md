# T01 (fallback) — G20 relay/Gemini policy paths — VERDICT: PASS

- Slice: G20 relay/Gemini policy paths — verify `checkKeyPolicy` call sites
  exist in chat, legacy completions, responses, embeddings, audio, images,
  and media routes (grep, file:line each).
- Verdict: **PASS** (call-site verification by code read; no tests run for
  this slice beyond the G20 tests already green in the T01 combined run)
- Date: 2026-11-19 19:42 IST
- Tester: verify-only (T01 fallback); no code/doc edits made except this
  verdict file; no commits, no pushes.

## Method

`grep -rn "checkKeyPolicy|checkKeyTrafficModel|checkKeyTrafficForChat|..." internal/server/*.go`
(non-test files), then mapped each hit to its enclosing handler and its
route registration in `internal/server/server.go`, and confirmed each site
is `if refused := ...; refused { return }` — i.e. a pre-call gate that
returns before any provider/engine call.

Funnel: `checkKeyTrafficForChat` (`keytraffic.go:402-407`) →
`checkKeyTrafficModel` (`keytraffic.go:351-360`) → `checkKeyPolicy`
(`admin_keys_api.go:269`). So every `checkKeyTrafficForChat` /
`checkKeyTrafficModel` site below enforces the same ModelAllowed /
ForcedModel policy as the chat path. `checkKeyPolicy` is called directly
only in `keytraffic.go:360` and in the G21 batch gate
(`batches_api.go:358`); all other routes reach it through the funnel.

## Evidence — call site per route (all `refused → return`, pre-call)

| Route | Handler / registration | Policy gate |
|---|---|---|
| POST /v1/chat/completions | `handleChatCompletions` (`server.go:4247`); registered `server.go:1523` | `server.go:4500` `checkKeyTrafficForChat(w, r, &req)` |
| POST /v1/completions (legacy) | `handleLegacyCompletions` (`legacy_completions.go:284`); registered `server.go:1562` | `legacy_completions.go:406` `checkKeyTrafficForChat(w, r, &req)` |
| POST /v1/responses | `handleResponses` (`responses.go:984`); registered `server.go:1586` | `responses.go:1181` `checkKeyTrafficForChat(w, r, &chat)` (translated request) |
| POST /v1/embeddings + POST /v1/rerank | shared `handleEmbedRerank` (`embeddings.go:81`); `handleEmbeddings` `:69`, `handleRerank` `:74`; registered `server.go:1559-1560` | `embeddings.go:296` `checkKeyTrafficForChat(w, r, &bookReq)` |
| Audio (speech, TTS, transcriptions, translations) | `handleAudio` (`audio.go:148`); registered `server.go:1570-1574` | `audio.go:371` `checkKeyTrafficForChat(w, r, &bookReq)` |
| Images (generations, edits) | `handleImages` (`images.go:96`); registered `server.go:1567-1568` | `images.go:332` `checkKeyTrafficForChat(w, r, &bookReq)` |
| Media (search, OCR, video create, video task, music) | shared `serveMediaVerify` (`media_verify.go:598`); five callers at `media_verify.go:304,309,314,319,324` | `media_verify.go:781` `checkKeyTrafficForChat(w, r, &bookReq)` |
| Relay POST /v1/messages | `handleMessages` (`relay.go:251`); registered `server.go:1583` | `relay.go:398` `checkKeyTrafficModel(w, r, 0, head.Model)` — model taken from the request head; comment at `:391-397` states same 403 `key_forced_model` / `key_model_not_allowed` semantics as chat |
| Gemini native :generateContent / :streamGenerateContent | shared `geminiGenerate` (`gemini_native.go:1077`); called from `:1044` (non-stream) and `:1046` (stream) | `gemini_native.go:1122` `checkKeyTrafficModel(w, r, 0, model)` — comment at `:1114-1121` states policy runs BEFORE the limiter admits the request |
| A2A (extra, not requested) | `a2aAdmit` (`a2a.go:591`) | `a2a.go:762` `checkKeyTrafficForChat(rec, r, chatReq)` |

Every site above was confirmed in source as:

```go
if refused := s.checkKeyTrafficForChat(...); refused {
    return
}
```

(or the `checkKeyTrafficModel` equivalent for relay/Gemini), placed
before the throttle delay and before any engine/provider call.

## Test evidence (from the T01 combined run, same tree)

`go test -count=1 -v -run 'TestG20|TestG21|TestBatches' ./internal/server/`
→ **22 PASS, 0 FAIL**, including the 5 G20 model-policy tests in
`internal/server/g20_model_policy_test.go`. No dedicated per-route
policy test was run for this fallback slice — the task asked for
grep/file:line verification, which is what was done.

## Failures / nits

- None in the requested routes. All listed routes have a pre-call
  `checkKeyPolicy` funnel gate that returns on refusal.
- Observation (OUT OF SCOPE, for the coordinator, not a verdict item):
  two neighbouring POST surfaces have NO `checkKey*` call at all —
  `handleMessagesCountTokens` (`count_tokens.go:350`, POST
  `/v1/messages/count_tokens`, registered `server.go:1585`) and
  `handleModerations` (`moderations.go:214`). Whether those need a
  policy gate is a product decision, not something this slice was
  asked to verify; flagged only so it is not silently assumed covered.
