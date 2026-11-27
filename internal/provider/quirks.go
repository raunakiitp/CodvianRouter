package provider

import (
	"net/url"
	"strings"
)

// Per-provider quirk table (A1 gap 3): documented deviations from the
// generic OpenAI-compatible surface that NewClient must accommodate after
// the wire selection. Everything that is not anthropic / gemini / cohere /
// bedrock / vertex speaks the generic OpenAI client (client.go), whose URL
// builders insert "/v1" via joinAPIPath; a few catalog providers document
// surfaces where that default is wrong (chat paths with no /v1) or where
// the wire itself is shaped differently (legacy Azure deployment-scoped
// paths). NewClient consults this table by the client's base URL and
// carries the resolved quirks on the client, so every URL the client
// builds and every chat payload it sends honors them.
//
// Every entry below cites its source: the catalog's own verified.go entry
// (internal/catalog, read-only here) plus the provider's public docs the
// entry was verified against. NO invented quirks: a behavior with no
// documented source does not enter this table, and a quirk that only
// matters on a surface this client does not build (embeddings on legacy
// Azure, native-wire paths) is not claimed here.
//
// Two quirk classes are deliberately out of scope:
//   - Auth headers (x-api-key, api-key, x-fireworks-api-key, ...): the
//     catalog's per-endpoint auth resolution owns them (internal/catalog
//     auth.go, resolved into the client's authHeaders by the registry) —
//     duplicating them here would fight that resolution.
//   - Native-wire paths: the anthropic/gemini/cohere/bedrock/vertex
//     wrappers build their own URLs and never consult this table —
//     NewClient resolves quirks only on the generic OpenAI path, after the
//     wire selection has chosen it.

// quirkSet is the documented deviation set one provider may need from the
// generic OpenAI client.
type quirkSet struct {
	// NoV1Insert: the upstream's API paths sit directly at the base path.
	// The client must append the API segment verbatim (joinRawPath) and
	// must NOT insert "/v1/" (joinAPIPath's default would build a path the
	// upstream does not serve — e.g. /api/v3/v1/chat/completions on Ark).
	NoV1Insert bool

	// AzureLegacyDeploymentPath: the legacy Azure OpenAI data plane serves
	// chat completions only at the deployment-scoped path
	// /openai/deployments/{deployment}/chat/completions?api-version=<GA>,
	// where the deployment IS the request's model — so the model moves to
	// the URL path (chatURLForModel) and out of the JSON body. The Foundry
	// /openai/v1 surface needs none of this; the entry's notPathSuffix
	// guard never matches a base ending in /openai/v1, and the quirk is
	// chat-only (the generic embeddings/images builders stay untouched).
	AzureLegacyDeploymentPath bool
}

// quirkEntry is one table row. Matching is by the documented host of the
// upstream surface: the quirk is a property of the upstream, so a catalog
// add whose BaseURL was overridden to a proxy host inherits no quirks (a
// proxy mirrors its own path layout, not the upstream's). hostSuffix
// matches the base URL's host exactly or as a parent domain (region hosts:
// the Ark docs document region variants). notPathSuffix excludes bases
// whose path ends in that suffix (the Foundry /openai/v1 guard).
type quirkEntry struct {
	// catalogID is the catalog provider ID this row documents (for
	// traceability; matching itself is host-based).
	catalogID     string
	hostSuffix    string
	notPathSuffix string
	quirks        quirkSet
}

// providerQuirkTable is the registry consulted by NewClient. Rows are
// ordered host-specific first; a base matches at most one row in practice
// because each row names one vendor's documented host.
var providerQuirkTable = []quirkEntry{
	{
		// Catalog "deepseek" (internal/catalog/verified.go): "OpenAI base
		// has NO /v1 in the chat path (/chat/completions)".
		// https://api-docs.deepseek.com/ (base_url https://api.deepseek.com,
		// POST https://api.deepseek.com/chat/completions).
		catalogID:  "deepseek",
		hostSuffix: "api.deepseek.com",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "perplexity": "Sonar /chat/completions + /v1/sonar" on
		// the bare https://api.perplexity.ai base — the chat path carries
		// no /v1.
		// https://docs.perplexity.ai/docs/orchestrator/quickstart.
		catalogID:  "perplexity",
		hostSuffix: "api.perplexity.ai",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "qianfan": OpenAI-compatible v2 surface, base
		// https://qianfan.baidubce.com/v2 — chat is /v2/chat/completions.
		// https://intl.cloud.baidu.com/doc/qianfan/index.html.
		catalogID:  "qianfan",
		hostSuffix: "qianfan.baidubce.com",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "bigmodel": "General API base
		// https://open.bigmodel.cn/api/paas/v4 hosting the GLM family" —
		// chat is /api/paas/v4/chat/completions.
		// https://docs.bigmodel.cn/cn/guide/develop/http/introduction.
		catalogID:  "bigmodel",
		hostSuffix: "open.bigmodel.cn",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "glm": GLM Coding Plan OpenAI surface, base
		// https://api.z.ai/api/coding/paas/v4 — chat is
		// /api/coding/paas/v4/chat/completions (the same host also serves
		// the Anthropic surface /api/anthropic, which never reaches this
		// table: the wire selection routes it to the Messages client).
		// https://docs.z.ai/devpack/tool/others.
		catalogID:  "glm",
		hostSuffix: "api.z.ai",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "doubao": Ark data plane at
		// ark.cn-beijing.volces.com/api/v3 — chat is
		// /api/v3/chat/completions; "region hosts vary per deployment per
		// the docs", so the suffix matches every *.volces.com Ark host.
		// https://docs.volcengine.com/docs/ark/base-url-and-authentication.
		catalogID:  "doubao",
		hostSuffix: "volces.com",
		quirks:     quirkSet{NoV1Insert: true},
	},
	{
		// Catalog "azure-openai": "model = deployment name, not model ID.
		// /openai/v1 needs no api-version; legacy path needs ?api-version="
		// — the legacy data plane serves chat at the deployment-scoped
		// path with the GA api-version query.
		// https://learn.microsoft.com/en-us/azure/ai-services/openai/reference
		// (POST {endpoint}/openai/deployments/{deployment-id}/chat/completions?api-version=2024-06-01).
		hostSuffix:    "openai.azure.com",
		notPathSuffix: "/openai/v1",
		quirks:        quirkSet{AzureLegacyDeploymentPath: true},
	},
}

// quirksForBaseURL resolves the quirk set for a client base URL: the first
// table row whose documented host matches the base wins. ok is false for
// hosts with no documented quirks — those keep the plain generic behavior.
func quirksForBaseURL(baseURL string) (quirkSet, bool) {
	host, path := baseURLHostPath(baseURL)
	if host == "" {
		return quirkSet{}, false
	}
	for _, e := range providerQuirkTable {
		if e.matches(host, path) {
			return e.quirks, true
		}
	}
	return quirkSet{}, false
}

// matches reports whether this entry governs a base with the given host
// and path. Host match is exact or a subdomain of the documented host
// (strings.ToLower both sides; hosts are case-insensitive, and
// NormalizeBaseURL already lowercases the stored form).
func (e quirkEntry) matches(host, path string) bool {
	host = strings.ToLower(host)
	path = strings.ToLower(path)
	hostOK := host == e.hostSuffix || strings.HasSuffix(host, "."+e.hostSuffix)
	if !hostOK {
		return false
	}
	if e.notPathSuffix != "" && strings.HasSuffix(path, e.notPathSuffix) {
		return false
	}
	return true
}

// baseURLHostPath extracts the lowercased host and the raw path of a base
// URL. An unparseable URL (ValidateBaseURL rejects those before quirks are
// consulted, but stay safe) yields an empty host — no quirks.
func baseURLHostPath(baseURL string) (host, path string) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "", ""
	}
	return u.Host, u.Path
}

// azureLegacyAPIVersion is the GA api-version the legacy deployment path
// carries (Azure OpenAI REST API reference, learn.microsoft.com —
// api-version=2024-06-01 is the GA release the reference documents for
// chat completions).
const azureLegacyAPIVersion = "2024-06-01"

// azureLegacyChatURL builds the legacy deployment-scoped chat URL:
// {base}/openai/deployments/{deployment}/chat/completions?api-version=...
// The base is the resource root (https://{resource}.openai.azure.com);
// the deployment is the request's model, escaped as one path segment.
// Source: https://learn.microsoft.com/en-us/azure/ai-services/openai/reference.
func azureLegacyChatURL(base, deployment string) string {
	return joinRawPath(base, "openai/deployments/"+url.PathEscape(deployment)+"/chat/completions") +
		"?api-version=" + azureLegacyAPIVersion
}
