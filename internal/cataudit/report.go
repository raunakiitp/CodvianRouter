package cataudit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// RunOptions controls one audit run.
type RunOptions struct {
	// SourceURL is the models.dev catalog URL.
	SourceURL string
	// SkipReachability skips the base-URL / website probe step
	// (offline runs).
	SkipReachability bool
	// Concurrency is the probe worker count.
	Concurrency int
	// ProbeTimeout is the per-attempt probe timeout.
	ProbeTimeout time.Duration
	// HTTPClient is the client for the models.dev fetch and the
	// probes (nil = default).
	HTTPClient *http.Client
}

// Run executes a full audit: fetch models.dev, compare, probe
// reachability (unless skipped), and stamp the report. It never
// edits the catalog — the returned report is the only output.
func Run(ctx context.Context, opts RunOptions) (*Report, error) {
	if opts.SourceURL == "" {
		opts.SourceURL = DefaultModelsDevURL
	}
	remote, err := FetchRemoteCatalog(ctx, opts.SourceURL, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	local := catalog.All()
	rep := Compare(local, remote)
	rep.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	rep.SourceURL = opts.SourceURL
	if !opts.SkipReachability {
		urls := CollectProbeURLs(local)
		probes := ProbeReachability(ctx, urls, opts.HTTPClient, opts.Concurrency, opts.ProbeTimeout)
		rep.Reachability.LoopbackSkipped = CountLoopbackURLs(local)
		ApplyReachability(rep, local, probes)
	} else {
		rep.Limitations = append(rep.Limitations, "Reachability was skipped for this run (--skip-reachability): no base URL or website was probed, so no unreachable findings exist.")
	}
	return rep, nil
}

// WriteReport writes the report as JSON (path) and Markdown
// (path with the .json suffix replaced by .md). Both files are
// written atomically (temp file + rename) with 0644 permissions.
func WriteReport(rep *Report, jsonPath string) (mdPath string, err error) {
	data, err := rep.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("cataudit: encode report: %w", err)
	}
	if err := writeFileAtomic(jsonPath, data); err != nil {
		return "", err
	}
	mdPath = strings.TrimSuffix(jsonPath, ".json") + ".md"
	var sb strings.Builder
	WriteMarkdown(rep, &sb)
	if err := writeFileAtomic(mdPath, []byte(sb.String())); err != nil {
		return "", err
	}
	return mdPath, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cataudit: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".cataudit-*")
	if err != nil {
		return fmt.Errorf("cataudit: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("cataudit: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cataudit: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cataudit: close %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("cataudit: chmod %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cataudit: rename %s: %w", path, err)
	}
	return nil
}

// kindOrder is the order findings are grouped in the Markdown
// report: actionable drift first, informational last.
var kindOrder = []string{
	KindVerifiedBaseUnreachable,
	KindBaseURLUnreachable,
	KindWebsiteUnreachable,
	KindBaseURLDiffers,
	KindRenamed,
	KindPossibleRename,
	KindBaseURLMissingInCatalog,
	KindBaseURLPathDiffers,
	KindBaseURLMissingInModelsDev,
	KindMissingInCatalog,
	KindMissingInModelsDev,
}

var kindTitles = map[string]string{
	KindVerifiedBaseUnreachable:   "Verified entries with an unreachable base URL (downgrade candidates)",
	KindBaseURLUnreachable:        "Unreachable base URLs (no HTTP response at all)",
	KindWebsiteUnreachable:        "Unreachable websites (no HTTP response at all)",
	KindBaseURLDiffers:            "Base URL host differs from models.dev `api`",
	KindRenamed:                   "Renamed (same ID, different name)",
	KindPossibleRename:            "Possible renames (same name, different ID — needs a human)",
	KindBaseURLMissingInCatalog:   "No base URL in the catalog, models.dev lists one",
	KindBaseURLPathDiffers:        "Same host, path differs (usually the /v1 convention)",
	KindBaseURLMissingInModelsDev: "Base URL in the catalog, models.dev lists none (informational)",
	KindMissingInCatalog:          "In models.dev, absent from the catalog (new-provider candidates)",
	KindMissingInModelsDev:        "In the catalog, absent from models.dev (NOT a retirement verdict)",
}

// WriteMarkdown renders the report as a human-readable audit
// document: counts first, then one section per finding kind with
// the concrete drift list. Nothing here is a fix — the file is
// the record the next slice acts on, with sources cited per row.
func WriteMarkdown(rep *Report, w io.Writer) {
	fmt.Fprintf(w, "# Provider catalog freshness audit — %s\n\n", shortDate(rep.GeneratedAt))
	fmt.Fprintf(w, "Source: [%s](%s), fetched live for this run. Local catalog: `internal/catalog` (Go data, %d entries). This audit only reports — it never edits the catalog. Fixes land separately, each with its cited source.\n\n", rep.SourceURL, rep.SourceURL, rep.Catalog.Total)
	fmt.Fprintf(w, "## Counts\n\n")
	fmt.Fprintf(w, "| Side | Count |\n|---|---|\n")
	fmt.Fprintf(w, "| Catalog entries | %d |\n", rep.Catalog.Total)
	fmt.Fprintf(w, "| — verified from docs | %d |\n", rep.Catalog.VerifiedFromDocs)
	fmt.Fprintf(w, "| — reference metadata only | %d |\n", rep.Catalog.ReferenceOnly)
	fmt.Fprintf(w, "| — unverified | %d |\n", rep.Catalog.Unverified)
	fmt.Fprintf(w, "| — no base URL | %d |\n", rep.Catalog.NoBaseURL)
	fmt.Fprintf(w, "| — templated base URL | %d |\n", rep.Catalog.TemplatedBaseURL)
	fmt.Fprintf(w, "| — ToS-grey | %d |\n", rep.Catalog.ToSGrey)
	fmt.Fprintf(w, "| models.dev providers | %d |\n", rep.Remote.Providers)
	fmt.Fprintf(w, "| models.dev models | %d (%d deprecated) |\n", rep.Remote.Models, rep.Remote.DeprecatedModels)
	fmt.Fprintf(w, "| Reachability probed | %d (%d reachable, %d unreachable, %d skipped — no base URL, %d loopback skipped — local runtimes, unprobable from here) |\n", rep.Reachability.Checked, rep.Reachability.Reachable, rep.Reachability.Unreachable, rep.Reachability.Skipped, rep.Reachability.LoopbackSkipped)
	if tr := rep.Triage; tr != nil {
		fmt.Fprintf(w, "\n## Triage %s (committed record: `%s`)\n\n", tr.Date, tr.Doc)
		fmt.Fprintf(w, "| | Count |\n|---|---|\n")
		fmt.Fprintf(w, "| Candidates triaged | %d of %d |\n", tr.Candidates.Triaged, tr.Candidates.Total)
		fmt.Fprintf(w, "| Providers added | %d (%d verified from docs, %d reference-only) |\n", tr.Candidates.Added, tr.Candidates.AddedVerifiedFromDocs, tr.Candidates.AddedReferenceOnly)
		fmt.Fprintf(w, "| Candidates rejected | %d |\n", tr.Candidates.Rejected)
		fmt.Fprintf(w, "| Candidates pending | %d |\n", tr.Candidates.Pending)
		fmt.Fprintf(w, "| Candidates untouched | %d |\n", tr.Candidates.Untouched)
		fmt.Fprintf(w, "| Missing base URLs filled | %d of %d |\n", tr.BaseURLs.Filled, tr.BaseURLs.MissingBefore)
		fmt.Fprintf(w, "| Renames applied | %d of %d |\n", tr.Renames.Applied, tr.Renames.Total)
		fmt.Fprintf(w, "| Possible renames applied | %d of %d |\n", tr.PossibleRenames.Applied, tr.PossibleRenames.Total)
	}
	fmt.Fprintf(w, "\n## Findings by kind\n\n")
	fmt.Fprintf(w, "| Kind | Count |\n|---|---|\n")
	for _, k := range kindOrder {
		if n := rep.Counts[k]; n > 0 {
			fmt.Fprintf(w, "| %s | %d |\n", k, n)
		}
	}
	// Kinds outside kindOrder (future kinds) still get listed.
	var extra []string
	for k, n := range rep.Counts {
		if n == 0 {
			continue
		}
		known := false
		for _, kk := range kindOrder {
			if kk == k {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		fmt.Fprintf(w, "| %s | %d |\n", k, rep.Counts[k])
	}
	fmt.Fprintf(w, "\n")

	byKind := map[string][]Finding{}
	for _, f := range rep.Findings {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}
	orderedKinds := append(append([]string{}, kindOrder...), extra...)
	for _, k := range orderedKinds {
		fs := byKind[k]
		if len(fs) == 0 {
			continue
		}
		title, ok := kindTitles[k]
		if !ok {
			title = k
		}
		fmt.Fprintf(w, "## %s (%d)\n\n", title, len(fs))
		fmt.Fprintf(w, "| Catalog ID | models.dev ID | Name | Detail |\n|---|---|---|---|\n")
		for _, f := range fs {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
				mdCell(f.CatalogID), mdCell(f.RemoteID), mdCell(f.Name), mdCell(f.Detail))
		}
		fmt.Fprintf(w, "\n")
	}
	fmt.Fprintf(w, "## Limitations\n\n")
	for _, l := range rep.Limitations {
		fmt.Fprintf(w, "- %s\n", l)
	}
	fmt.Fprintf(w, "\nGenerated %s by `codvian-router catalog audit`. JSON: `%s`.\n",
		rep.GeneratedAt, "catalog-audit-"+shortDate(rep.GeneratedAt)+".json")
}

func shortDate(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Format("2006-01-02")
	}
	if len(rfc3339) >= 10 {
		return rfc3339[:10]
	}
	return rfc3339
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
