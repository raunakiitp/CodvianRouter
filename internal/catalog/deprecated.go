// Deprecated models (research-log entry 179, Inference Gateway rule).
//
// /v1/models hides models the catalog marks deprecated; ?include=deprecated
// lists them again; requests naming a deprecated model are still forwarded
// (never a hard block on a stale catalog entry).
//
// The table below is the seed, not the whole mechanism: a model is
// deprecated when EITHER its operator-stored provider.Model.Deprecated
// flag is set OR its ID appears here. The seed holds only model IDs
// whose retirement the provider itself published and that has already
// passed — checked 2026-11-18 against OpenAI's deprecations page
// (developers.openai.com/api/docs/deprecations) via independent
// trackers that cite it. No "probably retired" guesses, no bare
// aliases (gpt-4, gpt-3.5-turbo) whose meaning varies by provider.
//
//   - gpt-3.5-turbo-0301 — shut down 2024-09-13
//   - gpt-4-0314, gpt-4-0125-preview, gpt-4-turbo-preview — shut down 2026-03-26
//   - gpt-4-32k-0314 — shut down 2025-06-06
//   - gpt-3.5-turbo-instruct, babbage-002, davinci-002 — shut down 2026-09-28
//   - gpt-3.5-turbo, gpt-4, gpt-4-turbo, gpt-4o-2024-05-13, o1, o1-pro,
//     o3-mini, o4-mini — shut down 2026-10-23
//
// The 2026-10-23 batch is included because OpenAI's notice lists the
// bare IDs themselves as shut down on the API, not just their snapshots.
// A provider that still serves one of these IDs under its own aliasing
// is unaffected in practice: deprecation only hides the row from the
// default list — the request still forwards, and the upstream's own
// answer (200 or 404) is what the client sees.
package catalog

// DeprecatedModel is one catalog-known retired model.
type DeprecatedModel struct {
	// ID is the exact model ID, as the provider published it.
	ID string
	// Retired is the provider-published shutdown date (YYYY-MM-DD).
	Retired string
	// Source is where the date was published.
	Source string
}

// DeprecatedModels is the catalog-known retired-model table, keyed by
// exact model ID. It is deliberately small: only retirements the
// provider published and that have already passed. Unknown models are
// never deprecated by guesswork — absence from this table means
// "not known deprecated", never "known current".
var DeprecatedModels = map[string]DeprecatedModel{
	"gpt-3.5-turbo-0301":     {ID: "gpt-3.5-turbo-0301", Retired: "2024-09-13", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4-0314":             {ID: "gpt-4-0314", Retired: "2026-03-26", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4-0125-preview":     {ID: "gpt-4-0125-preview", Retired: "2026-03-26", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4-turbo-preview":    {ID: "gpt-4-turbo-preview", Retired: "2026-03-26", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4-32k-0314":         {ID: "gpt-4-32k-0314", Retired: "2025-06-06", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-3.5-turbo-instruct": {ID: "gpt-3.5-turbo-instruct", Retired: "2026-09-28", Source: "https://developers.openai.com/api/docs/deprecations"},
	"babbage-002":            {ID: "babbage-002", Retired: "2026-09-28", Source: "https://developers.openai.com/api/docs/deprecations"},
	"davinci-002":            {ID: "davinci-002", Retired: "2026-09-28", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-3.5-turbo":          {ID: "gpt-3.5-turbo", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4":                  {ID: "gpt-4", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4-turbo":            {ID: "gpt-4-turbo", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"gpt-4o-2024-05-13":      {ID: "gpt-4o-2024-05-13", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"o1":                     {ID: "o1", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"o1-pro":                 {ID: "o1-pro", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"o3-mini":                {ID: "o3-mini", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
	"o4-mini":                {ID: "o4-mini", Retired: "2026-10-23", Source: "https://developers.openai.com/api/docs/deprecations"},
}

// IsDeprecated reports whether the catalog marks id deprecated, with
// the table entry. It is an exact-ID match: no prefix or substring
// guessing (a fine-tune "ft-gpt-4-..." is a different model ID).
func IsDeprecated(id string) (DeprecatedModel, bool) {
	d, ok := DeprecatedModels[id]
	return d, ok
}
