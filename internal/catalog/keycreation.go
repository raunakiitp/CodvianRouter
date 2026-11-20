// Key-creation links (parity tracker Area 3, "Key-creation links").
//
// the reference router records a key-creation hint for 129 providers; the
// catalog recorded none. This file seeds KeyCreationURL for the
// providers whose hint names a key page that differs from the
// homepage (the homepage itself is already the Website field, so
// re-recording it adds nothing).
//
// Source: the reference router reference registry authHint fields
// (src/shared/constants/providers/**/*.ts, read 2026-11-19). Every
// entry below was hand-checked against that rule: the URL must be
// a key-management / signup / console page, never an API base
// endpoint (an apiHint base URL is not a key page) and never a
// bare homepage. Hints that name only a homepage, or no URL at
// all, are NOT seeded here — they need per-provider research
// against the provider's own docs, not a guess. The remaining
// ~110 key pages are that follow-up data task, not this slice.
//
// The seed is reference metadata, not docs-verified: a URL here
// says "the reference router's hint points at this key page", not "we
// verified this key page against the provider's docs". The
// Provider.Verification label is unchanged by the fill.
package catalog

// keyCreationURLs maps catalog IDs to key-creation URLs. Applied in
// All() only where the entry records none; a verified entry's own
// KeyCreationURL (none set today) would win over the seed.
var keyCreationURLs = map[string]string{
	// Key page named by the provider's own hint, checked 2026-11-19.
	"baichuan":              "https://platform.baichuan-ai.com",
	"baidu":                 "https://console.bce.baidu.com",
	"beatapi":               "https://beatapi.io/dashboard/apikeys",
	"cloudflare-workers-ai": "https://dash.cloudflare.com",
	"doubao":                "https://console.volcengine.com",
	"empiriolabs":           "https://platform.empiriolabs.ai/dashboard/api-keys",
	"freetheai":             "https://freetheai.org/signup",
	"iflytek":               "https://console.xfyun.cn",
	"leonardo":              "https://leonardo.ai/developer",
	"logfare":               "https://logfare.ai/register",
	"magnific":              "https://magnific.com/user/api-keys",
	"oneminai":              "https://docs.1min.ai/docs/api/create-api-key",
	"sparkdesk":             "https://console.xfyun.cn",
	"stepfun":               "https://platform.stepfun.com",
	"tencent":               "https://console.cloud.tencent.com",
	"tinyfish":              "https://agent.tinyfish.ai/api-keys",
	"tokenharbor":           "https://tokenharbor.ai/dashboard",
	"yi":                    "https://platform.lingyiwanwu.com",
}
