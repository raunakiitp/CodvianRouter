package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cataudit"
)

// runCatalog manages the provider catalog:
//
//	catalog audit [--out-dir DIR] [--web-json PATH] [--models-dev-url URL]
//	              [--skip-reachability] [--concurrency N] [--probe-timeout SEC]
//	              [--triage-summary PATH]
//
// It audits the compiled-in catalog (internal/catalog) against the
// live models.dev catalog and the network reachability of each
// catalog base URL and website, and writes a dated JSON + Markdown
// report into the out dir (default docs/) plus the dashboard JSON
// (default web/catalog-audit.json, embedded by the dashboard).
// It NEVER edits the catalog: the report is the only output, and
// any fix lands separately with its cited source.
//
// The flags are registered in parseArgs (CHECKLIST 271-272) and
// arrive here on opts — the handler used to parse its own flags,
// which the global parser rejected as "unknown flag" before the
// handler ever ran, so `catalog audit --out-dir` was broken.
func runCatalog(opts options, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "codvian-router: catalog requires an action: audit")
		return 2
	}
	switch args[0] {
	case "audit":
		return runCatalogAudit(opts, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown catalog action %q (want audit)\n", args[0])
		return 2
	}
}

func runCatalogAudit(opts options, args []string) int {
	// args is only the leftover positionals — every flag was
	// consumed by parseArgs already.
	for _, a := range args {
		fmt.Fprintf(os.Stderr, "codvian-router: catalog audit: unknown argument %q\n", a)
		return 2
	}
	outDir := opts.catalogOutDir
	if outDir == "" {
		outDir = "docs"
	}
	webJSON := "web/catalog-audit.json"
	if opts.catalogWebJSONSet {
		webJSON = opts.catalogWebJSON
	}
	triageSummary := opts.catalogTriageSummary
	triageFlagSet := opts.catalogTriageSummarySet
	sourceURL := opts.catalogModelsDevURL
	if sourceURL == "" {
		sourceURL = cataudit.DefaultModelsDevURL
	}
	skipReachability := opts.catalogSkipReachability
	concurrency := opts.catalogConcurrency
	if concurrency == 0 {
		concurrency = 16
	}
	probeTimeout := 15 * time.Second
	if opts.catalogProbeTimeoutSec != 0 {
		probeTimeout = time.Duration(opts.catalogProbeTimeoutSec) * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rep, err := cataudit.Run(ctx, cataudit.RunOptions{
		SourceURL:        sourceURL,
		SkipReachability: skipReachability,
		Concurrency:      concurrency,
		ProbeTimeout:     probeTimeout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: catalog audit failed: %v\n", err)
		return 1
	}
	stamp := time.Now().Format("2006-01-02")
	// Embed the committed triage record for this audit's date when
	// one exists (default <out-dir>/catalog-triage-<date>.json).
	// --triage-summary=PATH overrides it; an empty value disables
	// embedding. A missing file is normal (no triage recorded);
	// a corrupt one fails the run so wrong numbers never ship.
	triagePath := triageSummary
	if !triageFlagSet {
		triagePath = filepath.Join(outDir, "catalog-triage-"+stamp+".json")
	}
	if triagePath != "" {
		sum, err := cataudit.LoadTriageSummary(triagePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: catalog audit: %v\n", err)
			return 1
		}
		rep.Triage = sum
	}
	jsonPath := filepath.Join(outDir, "catalog-audit-"+stamp+".json")
	mdPath, err := cataudit.WriteReport(rep, jsonPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: catalog audit: cannot write report: %v\n", err)
		return 1
	}
	webNote := "not written"
	if webJSON != "" {
		data, err := rep.MarshalJSON()
		if err == nil {
			if err := os.MkdirAll(filepath.Dir(webJSON), 0o755); err == nil {
				if err := os.WriteFile(webJSON, data, 0o644); err == nil {
					webNote = webJSON
				} else {
					fmt.Fprintf(os.Stderr, "codvian-router: catalog audit: warning: cannot write %s: %v\n", webJSON, err)
				}
			}
		}
	}
	fmt.Printf("catalog audit %s: %d catalog entries vs %d models.dev providers (%d models)\n",
		stamp, rep.Catalog.Total, rep.Remote.Providers, rep.Remote.Models)
	fmt.Printf("  verified-from-docs %d, reference-only %d, unverified %d, no base URL %d\n",
		rep.Catalog.VerifiedFromDocs, rep.Catalog.ReferenceOnly, rep.Catalog.Unverified, rep.Catalog.NoBaseURL)
	fmt.Printf("  reachability: %d probed, %d reachable, %d unreachable\n",
		rep.Reachability.Checked, rep.Reachability.Reachable, rep.Reachability.Unreachable)
	for _, k := range []string{
		cataudit.KindVerifiedBaseUnreachable, cataudit.KindBaseURLUnreachable,
		cataudit.KindWebsiteUnreachable, cataudit.KindBaseURLDiffers,
		cataudit.KindRenamed, cataudit.KindPossibleRename,
		cataudit.KindBaseURLMissingInCatalog, cataudit.KindBaseURLPathDiffers,
		cataudit.KindBaseURLMissingInModelsDev, cataudit.KindMissingInCatalog,
		cataudit.KindMissingInModelsDev,
	} {
		if n := rep.Counts[k]; n > 0 {
			fmt.Printf("  %-32s %d\n", k, n)
		}
	}
	fmt.Printf("report: %s\nmarkdown: %s\ndashboard JSON: %s\n", jsonPath, mdPath, webNote)
	fmt.Println("The audit only reports — it did not edit the catalog. Fixes land separately, each with its cited source.")
	return 0
}
