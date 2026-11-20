// Package cataudit audits the Codvian Router provider catalog
// (internal/catalog) against the live models.dev catalog
// (https://models.dev/api.json) and against the network
// reachability of each catalog base URL and website.
//
// It NEVER edits the catalog: it only reports. A finding is a
// claim with a cited source (the models.dev entry, its doc URL,
// or the probe result) that a human — or a later slice — can act
// on. The catalog itself is provider-level data: it tracks no
// per-provider model list, so "new models missing" is reported
// only as providers models.dev lists that the catalog does not,
// with their model counts — per-model drift for connected
// providers is the models.dev price/context sync's job
// (CHECKLIST 179-180), not this audit's.
//
// Honesty rules for every finding:
//   - "Absent from models.dev" is NOT "retired". models.dev
//     tracks ~324 providers; local runtimes, web-cookie and
//     cloud-agent entries are expected to be absent, and an
//     API-key provider can simply be untracked there. Those
//     findings say "verify manually", never "retired".
//   - A base URL that differs from models.dev's `api` field is
//     drift, not proof ours is wrong: models.dev stores the bare
//     host where the catalog stores the /v1 base, so a host match
//     with a /v1-only path difference is reported separately
//     (base_url_path_differs) and is usually not a fix.
//   - Reachability means "some HTTP response arrived". A 401 or
//     404 still proves the host exists; only a transport failure
//     (DNS / TLS / connect / timeout) counts as unreachable, and
//     the error class is recorded, never hidden.
//   - No network call happens in tests: Compare is pure, and the
//     fetch/probe steps take injectable inputs.
package cataudit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// DefaultModelsDevURL is the public models.dev catalog. It is a
// default, not a fallback the router reaches for on its own: the
// audit only fetches it when the operator runs it.
const DefaultModelsDevURL = "https://models.dev/api.json"

// MaxCatalogBytes bounds one models.dev fetch (the catalog is
// currently ~7 MB; 64 MiB leaves ample headroom while refusing
// an unbounded read).
const MaxCatalogBytes = 64 << 20

// Finding kinds. The kind is the machine-readable verdict; Detail
// carries the human-readable evidence.
const (
	// KindRenamed: same provider ID, the display name differs
	// from what models.dev reports.
	KindRenamed = "renamed"
	// KindPossibleRename: different IDs, same normalised name —
	// a rename candidate that needs a human to confirm.
	KindPossibleRename = "possible_rename"
	// KindBaseURLDiffers: the base-URL hosts differ.
	KindBaseURLDiffers = "base_url_differs"
	// KindBaseURLPathDiffers: same host, the path differs
	// (usually our /v1 base vs models.dev's bare host).
	KindBaseURLPathDiffers = "base_url_path_differs"
	// KindBaseURLMissingInCatalog: we record no base URL but
	// models.dev lists an `api` base for the provider.
	KindBaseURLMissingInCatalog = "base_url_missing_in_catalog"
	// KindBaseURLMissingInModelsDev: we record a base URL but
	// models.dev lists no `api` for the provider (informational).
	KindBaseURLMissingInModelsDev = "base_url_missing_in_modelsdev"
	// KindMissingInModelsDev: in our catalog, absent from
	// models.dev. NOT a retirement verdict — see the package doc.
	KindMissingInModelsDev = "missing_in_modelsdev"
	// KindMissingInCatalog: in models.dev, absent from our
	// catalog — a new-provider candidate.
	KindMissingInCatalog = "missing_in_catalog"
	// KindBaseURLUnreachable: no HTTP response at all from the
	// catalog base URL (transport failure, class recorded).
	KindBaseURLUnreachable = "base_url_unreachable"
	// KindWebsiteUnreachable: no HTTP response at all from the
	// catalog website URL.
	KindWebsiteUnreachable = "website_unreachable"
	// KindVerifiedBaseUnreachable: a verified-from-docs entry
	// whose base URL is unreachable — a downgrade candidate.
	KindVerifiedBaseUnreachable = "verified_base_unreachable"
)

// RemoteModel is one model in the models.dev catalog.
type RemoteModel struct {
	ID         string
	Deprecated bool
}

// RemoteProvider is one provider in the models.dev catalog.
type RemoteProvider struct {
	ID     string
	Name   string
	Doc    string // models.dev's doc URL for the provider
	API    string // models.dev's `api` base URL ("" when absent)
	Models map[string]RemoteModel
}

// RemoteCatalog is the parsed models.dev api.json.
type RemoteCatalog struct {
	Providers map[string]RemoteProvider
	// TotalModels counts every model across providers;
	// DeprecatedModels counts the ones models.dev marks deprecated.
	TotalModels      int
	DeprecatedModels int
}

type rawRemoteModel struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type rawRemoteProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Doc    string                    `json:"doc"`
	API    string                    `json:"api"`
	Models map[string]rawRemoteModel `json:"models"`
}

// ParseRemoteCatalog parses the models.dev api.json document. The
// top level is a map of provider ID -> provider object. Malformed
// JSON fails the whole parse — a half-parsed catalog must never
// be audited against.
func ParseRemoteCatalog(data []byte) (*RemoteCatalog, error) {
	var raw map[string]rawRemoteProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("cataudit: invalid models.dev catalog JSON: %w", err)
	}
	cat := &RemoteCatalog{Providers: make(map[string]RemoteProvider, len(raw))}
	for id, rp := range raw {
		if id == "" {
			continue
		}
		p := RemoteProvider{ID: id, Name: rp.Name, Doc: rp.Doc, API: rp.API, Models: make(map[string]RemoteModel, len(rp.Models))}
		for mid, rm := range rp.Models {
			if mid == "" {
				continue
			}
			m := RemoteModel{ID: mid, Deprecated: rm.Status == "deprecated"}
			p.Models[mid] = m
			cat.TotalModels++
			if m.Deprecated {
				cat.DeprecatedModels++
			}
		}
		cat.Providers[id] = p
	}
	return cat, nil
}

// LoadTriageSummary reads a committed triage record
// (docs/catalog-triage-<date>.json) and checks its ledger adds
// up. It returns (nil, nil) when the file does not exist — an
// audit without a triage record is normal, and the audit must
// never invent triage numbers. Any other read/parse/ledger error
// is returned so a corrupt record fails loudly instead of
// embedding wrong numbers.
func LoadTriageSummary(path string) (*TriageSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cataudit: read triage record %s: %w", path, err)
	}
	var sum TriageSummary
	if err := json.Unmarshal(data, &sum); err != nil {
		return nil, fmt.Errorf("cataudit: parse triage record %s: %w", path, err)
	}
	c := sum.Candidates
	if strings.TrimSpace(sum.Date) == "" {
		return nil, fmt.Errorf("cataudit: triage record %s has no date", path)
	}
	if c.Added != c.AddedVerifiedFromDocs+c.AddedReferenceOnly {
		return nil, fmt.Errorf("cataudit: triage record %s: added %d != verified %d + reference %d",
			path, c.Added, c.AddedVerifiedFromDocs, c.AddedReferenceOnly)
	}
	if c.Triaged != c.Added+c.Rejected+c.Pending {
		return nil, fmt.Errorf("cataudit: triage record %s: triaged %d != added %d + rejected %d + pending %d",
			path, c.Triaged, c.Added, c.Rejected, c.Pending)
	}
	if c.Untouched != c.Total-c.Triaged {
		return nil, fmt.Errorf("cataudit: triage record %s: untouched %d != total %d - triaged %d",
			path, c.Untouched, c.Total, c.Triaged)
	}
	if sum.BaseURLs.Filled+sum.BaseURLs.Skipped != sum.BaseURLs.MissingBefore {
		return nil, fmt.Errorf("cataudit: triage record %s: base URLs filled %d + skipped %d != missing %d",
			path, sum.BaseURLs.Filled, sum.BaseURLs.Skipped, sum.BaseURLs.MissingBefore)
	}
	if sum.Renames.Applied+sum.Renames.Rejected != sum.Renames.Total {
		return nil, fmt.Errorf("cataudit: triage record %s: renames applied %d + rejected %d != total %d",
			path, sum.Renames.Applied, sum.Renames.Rejected, sum.Renames.Total)
	}
	return &sum, nil
}

// FetchRemoteCatalog downloads and parses the models.dev catalog.
func FetchRemoteCatalog(ctx context.Context, sourceURL string, client *http.Client) (*RemoteCatalog, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cataudit: models.dev request: %w", err)
	}
	req.Header.Set("User-Agent", "codvian-router-catalog-audit/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cataudit: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cataudit: fetch %s: HTTP %d", sourceURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cataudit: read models.dev catalog: %w", err)
	}
	if len(body) > MaxCatalogBytes {
		return nil, fmt.Errorf("cataudit: models.dev catalog exceeds %d bytes", MaxCatalogBytes)
	}
	return ParseRemoteCatalog(body)
}

// Finding is one audited drift item, with its evidence cited.
type Finding struct {
	Kind string `json:"kind"`
	// CatalogID is our catalog entry's ID ("" for
	// missing_in_catalog findings).
	CatalogID string `json:"catalog_id,omitempty"`
	// RemoteID is the models.dev provider's ID ("" when the
	// provider is absent from models.dev).
	RemoteID string `json:"remote_id,omitempty"`
	// Name is the catalog display name (or the models.dev name
	// for missing_in_catalog findings).
	Name string `json:"name,omitempty"`
	// Detail is the human-readable evidence (both values, the
	// error class, the category note).
	Detail string `json:"detail"`
	// Source is the URL evidencing the finding (models.dev doc
	// URL, or the probed URL). "" when there is no URL to cite.
	Source string `json:"source,omitempty"`
	// ModelsDevModels is the models.dev model count for the
	// provider (missing_in_catalog findings only).
	ModelsDevModels int `json:"models_dev_models,omitempty"`
}

// CatalogCounts is the honest headline for our own catalog.
type CatalogCounts struct {
	Total            int `json:"total"`
	VerifiedFromDocs int `json:"verified_from_docs"`
	ReferenceOnly    int `json:"reference_only"`
	Unverified       int `json:"unverified"`
	NoBaseURL        int `json:"no_base_url"`
	TemplatedBaseURL int `json:"templated_base_url"`
	ToSGrey          int `json:"tos_grey"`
}

// RemoteCounts is the honest headline for the models.dev side.
type RemoteCounts struct {
	Providers        int `json:"providers"`
	Models           int `json:"models"`
	DeprecatedModels int `json:"deprecated_models"`
}

// ReachabilityCounts summarises the probe step.
type ReachabilityCounts struct {
	Checked         int `json:"checked"`
	Reachable       int `json:"reachable"`
	Unreachable     int `json:"unreachable"`
	Skipped         int `json:"skipped"`
	LoopbackSkipped int `json:"loopback_skipped"`
}

// TriageSummary is the machine-readable record of one catalog
// triage slice (docs/catalog-triage-<date>.json). The audit embeds
// it verbatim when the file exists, so the dashboard can show the
// triage progress honestly (verified-added vs still-candidate)
// next to the fresh drift counts. The audit never computes it:
// the numbers come from the committed triage record.
type TriageSummary struct {
	Date            string                `json:"date"`
	Doc             string                `json:"doc"`
	AuditBefore     TriageAuditBefore     `json:"audit_before"`
	Candidates      TriageCandidates      `json:"candidates"`
	BaseURLs        TriageBaseURLs        `json:"base_urls"`
	Renames         TriageRenames         `json:"renames"`
	PossibleRenames TriagePossibleRenames `json:"possible_renames"`
}

// TriageAuditBefore is the drift the triage slice started from.
type TriageAuditBefore struct {
	MissingInCatalog        int `json:"missing_in_catalog"`
	BaseURLMissingInCatalog int `json:"base_url_missing_in_catalog"`
	Renamed                 int `json:"renamed"`
	PossibleRename          int `json:"possible_rename"`
	CatalogTotal            int `json:"catalog_total"`
}

// TriageCandidates is the new-provider candidate ledger.
type TriageCandidates struct {
	Total                 int `json:"total"`
	Triaged               int `json:"triaged"`
	Added                 int `json:"added"`
	AddedVerifiedFromDocs int `json:"added_verified_from_docs"`
	AddedReferenceOnly    int `json:"added_reference_only"`
	Rejected              int `json:"rejected"`
	Pending               int `json:"pending"`
	Untouched             int `json:"untouched"`
}

// TriageBaseURLs is the missing-base-URL ledger.
type TriageBaseURLs struct {
	MissingBefore int `json:"missing_before"`
	Filled        int `json:"filled"`
	Skipped       int `json:"skipped"`
}

// TriageRenames is the display-name rename ledger.
type TriageRenames struct {
	Total    int `json:"total"`
	Applied  int `json:"applied"`
	Rejected int `json:"rejected"`
}

// TriagePossibleRenames is the ID-level possible-rename ledger.
type TriagePossibleRenames struct {
	Total        int `json:"total"`
	Applied      int `json:"applied"`
	PendingHuman int `json:"pending_human"`
}

// Report is the full audit result: counts, every finding, and
// the limits of what the audit could establish.
type Report struct {
	GeneratedAt  string             `json:"generated_at"`
	SourceURL    string             `json:"source_url"`
	Catalog      CatalogCounts      `json:"catalog"`
	Remote       RemoteCounts       `json:"remote"`
	Counts       map[string]int     `json:"counts_by_kind"`
	Reachability ReachabilityCounts `json:"reachability"`
	Findings     []Finding          `json:"findings"`
	Limitations  []string           `json:"limitations"`
	// Triage is the committed triage record for this audit's date,
	// when one exists. Nil when no triage has been recorded: the
	// audit never invents triage numbers.
	Triage *TriageSummary `json:"triage,omitempty"`
}

// ProbeResult is the outcome of one reachability probe.
type ProbeResult struct {
	URL        string `json:"url"`
	Reachable  bool   `json:"reachable"`
	StatusCode int    `json:"status_code,omitempty"`
	// ErrorClass is "" when reachable, else one of
	// dns / tls / timeout / connect / http.
	ErrorClass string `json:"error_class,omitempty"`
	Error      string `json:"error,omitempty"`
}

// normalizeName lower-cases and strips every non-alphanumeric
// rune, so "AI-ROUTER", "AI Router" and "airouter" compare equal.
func normalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeID strips separators as well as case, so "x-ai" and
// "xai" compare equal when the exact ID misses.
func normalizeID(s string) string { return normalizeName(s) }

// normalizeBaseURL splits a base URL into a comparable host and
// path: lower-cased scheme+host, path without a trailing slash.
// It returns false when the URL does not parse or has no host.
func normalizeBaseURL(raw string) (host, path string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host = strings.ToLower(u.Scheme + "://" + u.Host)
	path = strings.TrimSuffix(u.EscapedPath(), "/")
	return host, path, true
}

// pathsDifferOnlyByV1 reports whether two paths differ only by a
// trailing "/v1" segment on one side — the catalog's convention
// (store the /v1 base) vs models.dev's (store the bare host).
func pathsDifferOnlyByV1(a, b string) bool {
	if a == b {
		return false
	}
	strip := func(p string) string { return strings.TrimSuffix(p, "/v1") }
	return strip(a) == strip(b) || strip(a) == b || a == strip(b)
}

// expectedAbsentFromModelsDev reports whether a catalog category
// is expected to be absent from models.dev, so the finding can
// say so instead of implying retirement.
func expectedAbsentFromModelsDev(c catalog.Category) bool {
	switch c {
	case catalog.CategoryLocal, catalog.CategoryNoAuth,
		catalog.CategoryWebCookie, catalog.CategoryCloudAgent,
		catalog.CategorySystem, catalog.CategorySearch,
		catalog.CategoryAudio, catalog.CategoryUpstreamProxy:
		return true
	default:
		return false
	}
}

// Compare audits local catalog entries against the remote
// models.dev catalog. It is pure: no network, no mutation.
// Findings are returned in deterministic order (catalog order,
// then remote-ID order for new providers).
func Compare(local []catalog.Provider, remote *RemoteCatalog) *Report {
	rep := &Report{
		SourceURL: DefaultModelsDevURL,
		Counts:    map[string]int{},
		Limitations: []string{
			"Absent from models.dev is not retirement: models.dev tracks a subset of providers, and local, no-auth, web-cookie and cloud-agent entries are expected to be absent.",
			"models.dev lists no per-provider model list in this catalog: 'new models missing' is reported only as providers models.dev lists that the catalog does not, with their model counts. Per-model drift for connected providers is covered by the opt-in models.dev price/context sync (CHECKLIST 179-180).",
			"A base URL that differs from models.dev's `api` field is drift, not proof the catalog is wrong: models.dev stores the bare host where the catalog stores the /v1 base, and either side can be stale. A fix needs the provider's own docs as the cited source.",
			"Reachability is a transport fact, not a correctness verdict: a host that answers 401/404 exists; a host that times out once may be a transient failure, not a dead provider.",
			"Reachability is probed from the machine running the audit, through whatever egress it has: in this VM that is a shared egress proxy, which refuses some hosts outright and is slow for others, so a connect/EOF/timeout here is a re-check signal, not proof the provider is dead. Loopback base URLs (localhost / 127.0.0.0/8 / ::1 — local runtimes such as Ollama, LocalAI, Jan, GPT4All) are never probed: they can only be checked on the operator's own machine.",
		},
	}
	if remote != nil {
		rep.Remote = RemoteCounts{
			Providers:        len(remote.Providers),
			Models:           remote.TotalModels,
			DeprecatedModels: remote.DeprecatedModels,
		}
	}

	// Catalog headline counts.
	for _, p := range local {
		rep.Catalog.Total++
		switch p.Verification {
		case catalog.VerificationDocs:
			rep.Catalog.VerifiedFromDocs++
		case catalog.VerificationReference:
			rep.Catalog.ReferenceOnly++
		default:
			rep.Catalog.Unverified++
		}
		if p.ToSGrey {
			rep.Catalog.ToSGrey++
		}
		hasBase := false
		templated := false
		for _, u := range p.BaseURLs {
			if strings.TrimSpace(u) == "" {
				continue
			}
			hasBase = true
			if strings.ContainsAny(u, "{}") {
				templated = true
			}
		}
		if !hasBase {
			rep.Catalog.NoBaseURL++
		}
		if templated {
			rep.Catalog.TemplatedBaseURL++
		}
	}

	if remote == nil {
		rep.Findings = nil
		return rep
	}

	// Remote indexes: exact ID, normalised ID, normalised name.
	byNormID := make(map[string]RemoteProvider, len(remote.Providers))
	byNormName := make(map[string]RemoteProvider, len(remote.Providers))
	for _, rp := range remote.Providers {
		byNormID[normalizeID(rp.ID)] = rp
		if n := normalizeName(rp.Name); n != "" {
			// First writer wins on a name collision: a possible
			// rename is a candidate, never a certainty.
			if _, ok := byNormName[n]; !ok {
				byNormName[n] = rp
			}
		}
	}

	add := func(f Finding) {
		rep.Findings = append(rep.Findings, f)
		rep.Counts[f.Kind]++
	}
	matchedRemote := make(map[string]bool, len(remote.Providers))

	for _, p := range local {
		rp, ok := remote.Providers[p.ID]
		if !ok {
			if rp2, ok2 := byNormID[normalizeID(p.ID)]; ok2 {
				// Same provider under a separator-variant ID.
				rp, ok = rp2, true
			}
		}
		if !ok {
			// No ID match: try a name match (possible rename),
			// else report the absence honestly.
			if n := normalizeName(p.Name); len(n) >= 3 {
				if rp2, ok2 := byNormName[n]; ok2 && !matchedRemote[rp2.ID] {
					matchedRemote[rp2.ID] = true
					add(Finding{
						Kind:      KindPossibleRename,
						CatalogID: p.ID,
						RemoteID:  rp2.ID,
						Name:      p.Name,
						Detail:    fmt.Sprintf("catalog ID %q but models.dev lists the same name under ID %q (%d models) — possible rename, needs a human to confirm", p.ID, rp2.ID, len(rp2.Models)),
						Source:    rp2.Doc,
					})
					continue
				}
			}
			detail := fmt.Sprintf("in the catalog but absent from models.dev (%s; verification %s)", p.Category, p.Verification)
			if expectedAbsentFromModelsDev(p.Category) {
				detail += " — expected: this category is not tracked by models.dev"
			} else {
				detail += " — verify manually: untracked there, or retired"
			}
			add(Finding{
				Kind:      KindMissingInModelsDev,
				CatalogID: p.ID,
				Name:      p.Name,
				Detail:    detail,
				Source:    p.Website,
			})
			continue
		}
		matchedRemote[rp.ID] = true

		// Renamed: same provider, different display name.
		if ln, rn := normalizeName(p.Name), normalizeName(rp.Name); ln != "" && rn != "" && ln != rn {
			add(Finding{
				Kind:      KindRenamed,
				CatalogID: p.ID,
				RemoteID:  rp.ID,
				Name:      p.Name,
				Detail:    fmt.Sprintf("catalog name %q, models.dev name %q", p.Name, rp.Name),
				Source:    rp.Doc,
			})
		}

		// Base URL vs models.dev `api`.
		ours := ""
		templated := false
		for _, u := range p.BaseURLs {
			if strings.TrimSpace(u) == "" {
				continue
			}
			if strings.ContainsAny(u, "{}") {
				templated = true
				continue
			}
			ours = u
			break
		}
		switch {
		case ours == "" && rp.API != "" && !templated:
			add(Finding{
				Kind:      KindBaseURLMissingInCatalog,
				CatalogID: p.ID,
				RemoteID:  rp.ID,
				Name:      p.Name,
				Detail:    fmt.Sprintf("catalog records no base URL; models.dev lists api %q", rp.API),
				Source:    rp.Doc,
			})
		case ours == "" && rp.API != "":
			// Templated base only (e.g. Azure {resource}): the
			// operator must supply the concrete host, so
			// models.dev's generic api is not a usable fix.
			// Counted in the headline, not a finding.
		case ours != "" && rp.API == "":
			add(Finding{
				Kind:      KindBaseURLMissingInModelsDev,
				CatalogID: p.ID,
				RemoteID:  rp.ID,
				Name:      p.Name,
				Detail:    fmt.Sprintf("catalog base URL %q; models.dev lists no api base for this provider", ours),
				Source:    rp.Doc,
			})
		case ours != "" && rp.API != "":
			oh, op, ok1 := normalizeBaseURL(ours)
			rh, rpath, ok2 := normalizeBaseURL(rp.API)
			if !ok1 || !ok2 {
				break
			}
			switch {
			case oh != rh:
				add(Finding{
					Kind:      KindBaseURLDiffers,
					CatalogID: p.ID,
					RemoteID:  rp.ID,
					Name:      p.Name,
					Detail:    fmt.Sprintf("catalog base %q (host %s), models.dev api %q (host %s)", ours, oh, rp.API, rh),
					Source:    rp.Doc,
				})
			case op != rpath:
				kind := KindBaseURLPathDiffers
				detail := fmt.Sprintf("same host %s; catalog path %q, models.dev path %q", oh, op, rpath)
				if pathsDifferOnlyByV1(op, rpath) {
					detail += " — differs only by a trailing /v1, the usual catalog-vs-models.dev convention"
				}
				add(Finding{
					Kind:      kind,
					CatalogID: p.ID,
					RemoteID:  rp.ID,
					Name:      p.Name,
					Detail:    detail,
					Source:    rp.Doc,
				})
			}
		}
	}

	// New providers: in models.dev, absent from our catalog.
	remoteIDs := make([]string, 0, len(remote.Providers))
	for id := range remote.Providers {
		remoteIDs = append(remoteIDs, id)
	}
	sort.Strings(remoteIDs)
	for _, id := range remoteIDs {
		if matchedRemote[id] {
			continue
		}
		rp := remote.Providers[id]
		add(Finding{
			Kind:            KindMissingInCatalog,
			RemoteID:        rp.ID,
			Name:            rp.Name,
			Detail:          fmt.Sprintf("listed by models.dev with %d model(s) but absent from the catalog", len(rp.Models)),
			Source:          rp.Doc,
			ModelsDevModels: len(rp.Models),
		})
	}
	return rep
}

// ProbeReachability checks each distinct URL with a HEAD request
// (falling back to GET when HEAD is refused or fails at the
// transport level). Any HTTP response — including 401/404 —
// counts as reachable: the host exists. Only a transport failure
// counts as unreachable, with its error class recorded.
// The probe is bounded: concurrency workers, per-attempt timeout.
func ProbeReachability(ctx context.Context, urls []string, client *http.Client, concurrency int, timeout time.Duration) map[string]ProbeResult {
	if concurrency < 1 {
		concurrency = 1
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if client == nil {
		client = &http.Client{}
	}
	seen := map[string]bool{}
	uniq := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || strings.ContainsAny(u, "{}") {
			continue
		}
		if !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	out := make(map[string]ProbeResult, len(uniq))
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, u := range uniq {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := probeOne(ctx, client, u, timeout)
			mu.Lock()
			out[u] = res
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return out
}

func probeOne(ctx context.Context, client *http.Client, rawurl string, timeout time.Duration) ProbeResult {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(attemptCtx, method, rawurl, nil)
		if err != nil {
			cancel()
			return ProbeResult{URL: rawurl, ErrorClass: "http", Error: err.Error()}
		}
		req.Header.Set("User-Agent", "codvian-router-catalog-audit/1.0")
		resp, err := client.Do(req)
		if err == nil {
			// Any HTTP response proves the host exists.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			return ProbeResult{URL: rawurl, Reachable: true, StatusCode: resp.StatusCode}
		}
		cancel()
		// A transport failure on HEAD deserves one GET attempt
		// (some hosts refuse HEAD); a transport failure on GET
		// is the verdict.
		if method == http.MethodGet {
			return ProbeResult{URL: rawurl, ErrorClass: classifyProbeError(err), Error: err.Error()}
		}
	}
	return ProbeResult{URL: rawurl, ErrorClass: "connect", Error: "unreachable"}
}

func classifyProbeError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "dns"):
		return "dns"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls"):
		return "tls"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	default:
		return "connect"
	}
}

// ApplyReachability folds probe results into the report: one
// finding per unreachable catalog base URL / website, plus a
// verified_base_unreachable finding when the entry is
// verified-from-docs. Findings are appended in catalog order for
// determinism; the counts map and reachability summary update.
func ApplyReachability(rep *Report, local []catalog.Provider, probes map[string]ProbeResult) {
	if rep == nil {
		return
	}
	checked := len(probes)
	reachable := 0
	for _, r := range probes {
		if r.Reachable {
			reachable++
		}
	}
	rep.Reachability.Checked = checked
	rep.Reachability.Reachable = reachable
	rep.Reachability.Unreachable = checked - reachable
	rep.Reachability.Skipped = rep.Catalog.NoBaseURL

	add := func(f Finding) {
		rep.Findings = append(rep.Findings, f)
		rep.Counts[f.Kind]++
	}
	for _, p := range local {
		for _, u := range p.BaseURLs {
			u = strings.TrimSpace(u)
			if u == "" || strings.ContainsAny(u, "{}") {
				continue
			}
			r, ok := probes[u]
			if !ok || r.Reachable {
				continue
			}
			add(Finding{
				Kind:      KindBaseURLUnreachable,
				CatalogID: p.ID,
				Name:      p.Name,
				Detail:    fmt.Sprintf("catalog base URL %q gave no HTTP response (%s: %s)", u, r.ErrorClass, r.Error),
				Source:    u,
			})
			if p.Verification == catalog.VerificationDocs {
				add(Finding{
					Kind:      KindVerifiedBaseUnreachable,
					CatalogID: p.ID,
					Name:      p.Name,
					Detail:    fmt.Sprintf("verified-from-docs entry whose base URL %q is unreachable (%s) — downgrade candidate, re-verify against the provider's own docs before relying on it", u, r.ErrorClass),
					Source:    u,
				})
			}
			break // one finding per entry, not per base URL
		}
		for _, u := range []string{p.Website} {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			r, ok := probes[u]
			if !ok || r.Reachable {
				continue
			}
			add(Finding{
				Kind:      KindWebsiteUnreachable,
				CatalogID: p.ID,
				Name:      p.Name,
				Detail:    fmt.Sprintf("catalog website %q gave no HTTP response (%s: %s)", u, r.ErrorClass, r.Error),
				Source:    u,
			})
		}
	}
	// Keep findings deterministic: comparison findings first (in
	// catalog order), reachability findings after, each group in
	// catalog order — ApplyReachability already appends in
	// catalog order, so nothing to sort.
}

// isLoopbackURL reports whether the URL points at the local
// machine (localhost, 127.0.0.0/8, ::1). Such base URLs belong
// to runtimes on the operator's own machine and can never be
// probed from the audit host — probing them only manufactures
// false "unreachable" findings.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	if host == "::1" || host == "[::1]" {
		return true
	}
	return false
}

// CollectProbeURLs returns every distinct probeable URL from the
// catalog: non-templated base URLs plus websites. Loopback URLs
// are excluded (see isLoopbackURL).
func CollectProbeURLs(local []catalog.Provider) []string {
	var out []string
	for _, p := range local {
		for _, u := range p.BaseURLs {
			u = strings.TrimSpace(u)
			if u == "" || strings.ContainsAny(u, "{}") || isLoopbackURL(u) {
				continue
			}
			out = append(out, u)
		}
		if w := strings.TrimSpace(p.Website); w != "" && !isLoopbackURL(w) {
			out = append(out, w)
		}
	}
	return out
}

// CountLoopbackURLs counts the catalog URLs skipped from probing
// because they point at the local machine.
func CountLoopbackURLs(local []catalog.Provider) int {
	n := 0
	for _, p := range local {
		for _, u := range p.BaseURLs {
			u = strings.TrimSpace(u)
			if u != "" && !strings.ContainsAny(u, "{}") && isLoopbackURL(u) {
				n++
			}
		}
		if w := strings.TrimSpace(p.Website); w != "" && isLoopbackURL(w) {
			n++
		}
	}
	return n
}

// MarshalJSON renders the report as indented JSON for the
// committed audit file and the dashboard embed.
func (r *Report) MarshalJSON() ([]byte, error) {
	type report Report // avoid recursion
	return json.MarshalIndent((*report)(r), "", "  ")
}
