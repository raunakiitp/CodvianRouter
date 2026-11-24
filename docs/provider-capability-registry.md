# Provider Capability Metadata & Discovery (P1.5)

## What this is

A single, evidence-backed capability registry: for every (provider,
model) pair the router knows, each capability's **support state** and
**provenance**. It is the source of truth for both the API surface and
capability-aware routing.

What it is **not**: an inference engine. Nothing is guessed from a model
ID, a provider name, or a family. `unknown` means unknown.

## Capabilities recorded

| Group | Names |
| --- | --- |
| Interaction | `tool_calling`, `vision`, `image_input`, `image_output`, `audio_input`, `audio_output`, `video`, `reasoning` |
| Output shape | `structured_output`, `json_mode`, `streaming` |
| Limits | `context_window`, `max_output_tokens` |
| API surfaces | `api_chat`, `api_responses`, `api_embeddings`, `api_rerank`, `api_images`, `api_audio` |

## Support states

| State | Meaning |
| --- | --- |
| `supported` | affirmative evidence exists |
| `unsupported` | affirmative evidence of absence (e.g. emulated tool calling is *not* native support) |
| `unknown` | no affirmative evidence — **never** treated as support |

## Provenance

Every entry carries:

- `source` — `catalog_docs`, `catalog_reference`, `models_dev_sync`,
  `live_probe`, `operator_config`, `catalog_wire_format`
- `verification` — `verified` (affirmative external report) or
  `unverified` (declared, unconfirmed)
- `confidence` — `high` / `medium` / `low`
- `observed_at` — RFC3339 timestamp
- `note` — human-readable detail

An entry is **verified capable** only when `support == supported` **and**
`verification == verified`. Unknown and merely-declared capabilities never
verify.

### Source rules

| Evidence | Support | Verification |
| --- | --- | --- |
| Catalog `tool_calling=supported` + docs verification | supported | verified |
| Catalog `tool_calling=emulated` | **unsupported** (not native) | verified |
| Catalog tool-calling marker absent | unknown | unverified |
| `Vision/Audio/Video` + `*Known` (models.dev sync) | supported | verified |
| `Vision/Audio/Video` set by operator, no sync report | supported | unverified |
| `Tools` pointer set (operator or live probe) | supported/unsupported | verified |
| `Reasoning` flag | supported | unverified |
| `WireFormats` present | streaming supported | verified |
| Provider added by the operator, no catalog entry | tool calling unknown | unverified |

## API

| Route | Purpose |
| --- | --- |
| `GET /api/capabilities` | full matrix (`object: capabilities.matrix`) with per-model capability arrays |
| `GET /api/providers/{id}/capabilities` | the same for one provider (`object: provider.capabilities`); 404 when the provider is unknown |
| `POST /api/capabilities/refresh` | rebuilds the in-process snapshot from existing sources; no network I/O, no cost, cannot invent capabilities |

All three are Tier MANAGEMENT (`AuthExempt: true`): read-only metadata, or
a bounded local rebuild. No keys and no base URLs are ever included.

## Capability-aware routing

`router.Config.RequireCapabilities` plus `router.Config.CapabilityLookup`
gate the direct path: an attempt is skipped
(`skipped_capability_unverified:<cap>`) unless it holds **every**
required capability with verified support. Unknown never passes, unlike
the CT-1 tool pre-check which fails open for plain chat traffic by
design.

Operator control — `routing.require_capabilities` in the config file:

```json
{
  "routing": {
    "require_capabilities": ["vision", "tool_calling"]
  }
}
```

Unknown names fail at `Validate` with the full vocabulary in the error,
never silently. Empty (the default) applies no filter — default routing
behaviour is unchanged.

The engine's lookup is computed on demand from the registry
(`router.CapabilityLookupFromRegistry`), so a model added or synced
seconds ago is already visible; there is no snapshot to go stale on the
request path. The API endpoints serve a snapshot that
`POST /api/capabilities/refresh` rebuilds.

## Discovery refresh

Refresh is bounded and operator-controlled:

- No network I/O — it re-reads the registry and whatever the opt-in
  models.dev sync has already stored.
- `models.dev` syncing itself remains opt-in and is triggered elsewhere;
  the capability refresh never fetches anything.
- No paid API credits are consumed.

## Tests

| Test | Pins |
| --- | --- |
| `internal/capability` `TestUnknownToolCallingIsNotVerified` | unknown ≠ verified; operator-declared ≠ verified |
| `TestEmulatedToolCallingIsNotNativeSupport` | emulated is not native support |
| `TestVerifiedCapabilityRequiresBothSupportAndVerification` | verification needs both axes |
| `TestIndexLookupMissIsNotAnEmptySet` | absent pair → `ok=false`, never an empty pass |
| `TestBuildIsDeterministicAndUsesCatalogFacts` | stable output; every model carries tool-calling state |
| `internal/capability` `TestModelLookupOnDemand` | single-model lookup matches the matrix |
| `internal/server` `TestCapabilitiesEndpointRequiresWiring` | 503 without the option |
| `TestCapabilitiesMatrixReportsProvenance` | provenance + unknown-not-verified across the wire |
| `TestProviderCapabilitiesEndpoint` | per-provider shape; 404 for unknown provider |
| `internal/router` `TestP15CapabilityGateSkipsUnverified` | unverified and unknown attempts never dialled |
| `TestP15CapabilityGateNoVerifiedAttempt` | no verified target → `ErrNoAvailableAttempt` |
| `TestP15NoRequirementKeepsDefaultRouting` | empty requirement = unchanged behaviour |
| `internal/config` `TestRequireCapabilitiesValidation` | unknown names rejected at load |
| `TestRequireCapabilitiesRoundTripAndClone` | JSON + Clone/Snapshot isolation |
