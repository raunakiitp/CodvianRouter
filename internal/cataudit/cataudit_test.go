package cataudit

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

func fixtureLocal() []catalog.Provider {
	return []catalog.Provider{
		{ID: "acme", Name: "Acme AI", Website: "https://acme.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.acme.example/v1"}, Verification: catalog.VerificationDocs},
		{ID: "oldco", Name: "OldCo", Website: "https://oldco.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.oldco.example/v1"}, Verification: catalog.VerificationReference},
		{ID: "renamed-co", Name: "Renamed Co", Website: "https://renamed.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.renamed.example/v1"}, Verification: catalog.VerificationDocs},
		{ID: "gone-local", Name: "Gone Local", Website: "https://gone.example", Category: catalog.CategoryLocal,
			Verification: catalog.VerificationReference},
		{ID: "no-base", Name: "No Base", Website: "https://nobase.example", Category: catalog.CategoryAPIKey,
			Verification: catalog.VerificationReference},
		{ID: "moved-host", Name: "Moved Host", Website: "https://moved.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.moved.example/v1"}, Verification: catalog.VerificationDocs},
		{ID: "dead-base", Name: "Dead Base", Website: "https://dead.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.dead.example/v1"}, Verification: catalog.VerificationDocs},
	}
}

func fixtureRemote() *RemoteCatalog {
	return &RemoteCatalog{
		Providers: map[string]RemoteProvider{
			"acme":       {ID: "acme", Name: "Acme AI", Doc: "https://acme.example/docs", API: "https://api.acme.example", Models: map[string]RemoteModel{"acme-1": {ID: "acme-1"}}},
			"renamed-co": {ID: "renamed-co", Name: "Renamed Corp", Doc: "https://renamed.example/docs", API: "https://api.renamed.example/v1", Models: map[string]RemoteModel{}},
			"no-base":    {ID: "no-base", Name: "No Base", Doc: "https://nobase.example/docs", API: "https://api.nobase.example/v1", Models: map[string]RemoteModel{}},
			"moved-host": {ID: "moved-host", Name: "Moved Host", Doc: "https://moved.example/docs", API: "https://api.moved-new.example/v1", Models: map[string]RemoteModel{}},
			"dead-base":  {ID: "dead-base", Name: "Dead Base", Doc: "https://dead.example/docs", API: "https://api.dead.example/v1", Models: map[string]RemoteModel{}},
			"brand-new":  {ID: "brand-new", Name: "Brand New", Doc: "https://brandnew.example/docs", API: "https://api.brandnew.example/v1", Models: map[string]RemoteModel{"bn-1": {ID: "bn-1"}, "bn-2": {ID: "bn-2"}}},
			"oldco-ltd":  {ID: "oldco-ltd", Name: "OldCo", Doc: "https://oldco.example/docs", API: "https://api.oldco.example/v1", Models: map[string]RemoteModel{}},
		},
		TotalModels: 4,
	}
}

func findingsByKind(rep *Report, kind string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestCompareRenamed(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindRenamed)
	if len(fs) != 1 {
		t.Fatalf("renamed findings = %d, want 1: %+v", len(fs), fs)
	}
	if fs[0].CatalogID != "renamed-co" || !strings.Contains(fs[0].Detail, "Renamed Corp") {
		t.Errorf("renamed finding wrong: %+v", fs[0])
	}
}

func TestComparePossibleRename(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindPossibleRename)
	if len(fs) != 1 {
		t.Fatalf("possible_rename findings = %d, want 1: %+v", len(fs), fs)
	}
	if fs[0].CatalogID != "oldco" || fs[0].RemoteID != "oldco-ltd" {
		t.Errorf("possible rename wrong: %+v", fs[0])
	}
}

func TestCompareMissingInModelsDevHonest(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindMissingInModelsDev)
	if len(fs) != 1 {
		t.Fatalf("missing_in_modelsdev = %d, want 1 (gone-local only): %+v", len(fs), fs)
	}
	if fs[0].CatalogID != "gone-local" {
		t.Errorf("wrong entry: %+v", fs[0])
	}
	// A local-runtime entry must be labelled expected, never retired.
	if !strings.Contains(fs[0].Detail, "expected") {
		t.Errorf("local entry must say expected absence, got: %s", fs[0].Detail)
	}
	if strings.Contains(strings.ToLower(fs[0].Detail), "retired") {
		t.Errorf("finding must never claim retirement: %s", fs[0].Detail)
	}
}

func TestCompareMissingInCatalog(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindMissingInCatalog)
	if len(fs) != 1 {
		t.Fatalf("missing_in_catalog = %d, want 1: %+v", len(fs), fs)
	}
	if fs[0].RemoteID != "brand-new" || fs[0].ModelsDevModels != 2 {
		t.Errorf("new provider finding wrong: %+v", fs[0])
	}
}

func TestCompareBaseURLPathDiffersOnlyV1(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindBaseURLPathDiffers)
	if len(fs) != 1 {
		t.Fatalf("base_url_path_differs = %d, want 1 (acme /v1 convention): %+v", len(fs), fs)
	}
	if fs[0].CatalogID != "acme" || !strings.Contains(fs[0].Detail, "/v1") {
		t.Errorf("path-differs finding wrong: %+v", fs[0])
	}
	// Same host + /v1-only difference must never be a host-differ finding.
	if len(findingsByKind(rep, KindBaseURLDiffers)) != 1 {
		t.Fatalf("base_url_differs = %d, want exactly 1 (moved-host)", len(findingsByKind(rep, KindBaseURLDiffers)))
	}
}

func TestCompareBaseURLDiffers(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindBaseURLDiffers)
	if len(fs) != 1 || fs[0].CatalogID != "moved-host" {
		t.Fatalf("base_url_differs = %+v, want moved-host only", fs)
	}
	if !strings.Contains(fs[0].Detail, "api.moved-new.example") {
		t.Errorf("detail must cite the models.dev host: %s", fs[0].Detail)
	}
}

func TestCompareBaseURLMissingInCatalog(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	fs := findingsByKind(rep, KindBaseURLMissingInCatalog)
	if len(fs) != 1 || fs[0].CatalogID != "no-base" {
		t.Fatalf("base_url_missing_in_catalog = %+v, want no-base only", fs)
	}
}

func TestCompareCounts(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	if rep.Catalog.Total != 7 {
		t.Errorf("catalog total = %d, want 7", rep.Catalog.Total)
	}
	if rep.Catalog.VerifiedFromDocs != 4 {
		t.Errorf("verified = %d, want 4", rep.Catalog.VerifiedFromDocs)
	}
	if rep.Catalog.NoBaseURL != 2 {
		t.Errorf("no_base_url = %d, want 2 (gone-local, no-base)", rep.Catalog.NoBaseURL)
	}
	if rep.Remote.Providers != 7 || rep.Remote.Models != 4 {
		t.Errorf("remote = %d providers / %d models, want 7/4", rep.Remote.Providers, rep.Remote.Models)
	}
	for kind, want := range map[string]int{
		KindRenamed: 1, KindPossibleRename: 1, KindMissingInModelsDev: 1,
		KindMissingInCatalog: 1, KindBaseURLPathDiffers: 1, KindBaseURLDiffers: 1,
		KindBaseURLMissingInCatalog: 1,
	} {
		if rep.Counts[kind] != want {
			t.Errorf("counts[%s] = %d, want %d", kind, rep.Counts[kind], want)
		}
	}
}

func TestCompareNilRemoteIsCountsOnly(t *testing.T) {
	rep := Compare(fixtureLocal(), nil)
	if rep.Catalog.Total != 7 {
		t.Errorf("total = %d, want 7", rep.Catalog.Total)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("nil remote must yield no findings, got %d", len(rep.Findings))
	}
}

func TestParseRemoteCatalog(t *testing.T) {
	data := []byte(`{"acme":{"id":"acme","name":"Acme AI","doc":"https://acme.example/docs","api":"https://api.acme.example","models":{"acme-1":{"id":"acme-1","status":"active"},"acme-old":{"id":"acme-old","status":"deprecated"}}},"noapi":{"id":"noapi","name":"No API","models":{}}}`)
	cat, err := ParseRemoteCatalog(data)
	if err != nil {
		t.Fatalf("ParseRemoteCatalog: %v", err)
	}
	if len(cat.Providers) != 2 || cat.TotalModels != 2 || cat.DeprecatedModels != 1 {
		t.Errorf("parsed = %d providers / %d models / %d deprecated, want 2/2/1", len(cat.Providers), cat.TotalModels, cat.DeprecatedModels)
	}
	if cat.Providers["acme"].API != "https://api.acme.example" {
		t.Errorf("api field not parsed: %q", cat.Providers["acme"].API)
	}
	if _, err := ParseRemoteCatalog([]byte(`{broken`)); err == nil {
		t.Error("malformed JSON must fail the whole parse")
	}
}

func TestProbeReachability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// A URL that refuses connections: closed listener port.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := closed.URL
	closed.Close()

	// AUD-TEST determinism: the third probe is a HOSTNAME that must
	// fail DNS resolution. The old test relied on the environment's
	// resolver for `nonexistent.invalid` — offline/sandboxed hosts
	// time the probe out instead, making the test flaky-by-rig.
	// The injected client dials deterministically: IP literals (the
	// two httptest servers, which never touch DNS anyway) dial
	// normally; any HOSTNAME resolves to the synthetic "no such
	// host" error, exactly the answer a conforming resolver gives
	// for the reserved .invalid TLD (RFC 6761). Same behavior under
	// test, no external DNS dependency.
	deterministicClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err == nil && net.ParseIP(host) != nil {
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				}
				return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
			},
		},
	}

	probes := ProbeReachability(context.Background(), []string{srv.URL, deadURL, "http://nonexistent.invalid"}, deterministicClient, 4, 5*time.Second)
	// The test server refuses HEAD with 405: any HTTP response
	// proves the host exists, so the probe is reachable on HEAD.
	if !probes[srv.URL].Reachable || probes[srv.URL].StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("live server must be reachable on HEAD 405: %+v", probes[srv.URL])
	}
	if probes[deadURL].Reachable || probes[deadURL].ErrorClass == "" {
		t.Errorf("closed port must be unreachable with a class: %+v", probes[deadURL])
	}
	// .invalid must be unreachable with a recorded class. The
	// class is dns on a direct resolver and connect behind this
	// VM's egress proxy (which answers EOF) — either is honest.
	if p := probes["http://nonexistent.invalid"]; p.Reachable || (p.ErrorClass != "dns" && p.ErrorClass != "connect") {
		t.Errorf(".invalid must be unreachable with dns/connect class: %+v", p)
	}
}

func TestApplyReachability(t *testing.T) {
	local := fixtureLocal()
	rep := Compare(local, fixtureRemote())
	probes := map[string]ProbeResult{
		"https://api.dead.example/v1": {URL: "https://api.dead.example/v1", ErrorClass: "dns", Error: "no such host"},
		"https://dead.example":        {URL: "https://dead.example", Reachable: true, StatusCode: 200},
		"https://api.acme.example/v1": {URL: "https://api.acme.example/v1", Reachable: true, StatusCode: 401},
	}
	ApplyReachability(rep, local, probes)
	unreach := findingsByKind(rep, KindBaseURLUnreachable)
	if len(unreach) != 1 || unreach[0].CatalogID != "dead-base" {
		t.Fatalf("base_url_unreachable = %+v, want dead-base only", unreach)
	}
	// dead-base is verified-from-docs, so it also earns the downgrade candidate.
	ver := findingsByKind(rep, KindVerifiedBaseUnreachable)
	if len(ver) != 1 || ver[0].CatalogID != "dead-base" {
		t.Fatalf("verified_base_unreachable = %+v, want dead-base only", ver)
	}
	if rep.Reachability.Checked != 3 || rep.Reachability.Reachable != 2 || rep.Reachability.Unreachable != 1 {
		t.Errorf("reachability = %+v, want 3/2/1", rep.Reachability)
	}
}

func TestWriteMarkdownHonest(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	var sb strings.Builder
	WriteMarkdown(rep, &sb)
	out := sb.String()
	for _, want := range []string{
		"never edits the catalog", "NOT a retirement verdict",
		"Renamed (same ID, different name)", "Brand New",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestLoopbackURLsNeverProbed(t *testing.T) {
	local := []catalog.Provider{
		{ID: "ollama-local", Name: "Ollama", Website: "https://ollama.com", Category: catalog.CategoryLocal,
			BaseURLs: []string{"http://localhost:11434"}, Verification: catalog.VerificationDocs},
		{ID: "jan", Name: "Jan", Website: "https://jan.ai", Category: catalog.CategoryLocal,
			BaseURLs: []string{"http://127.0.0.1:1337/v1"}, Verification: catalog.VerificationDocs},
		{ID: "real", Name: "Real", Website: "https://real.example", Category: catalog.CategoryAPIKey,
			BaseURLs: []string{"https://api.real.example/v1"}, Verification: catalog.VerificationDocs},
	}
	urls := CollectProbeURLs(local)
	for _, u := range urls {
		if strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") {
			t.Errorf("loopback URL must never be probed: %s", u)
		}
	}
	if got := CountLoopbackURLs(local); got != 2 {
		t.Errorf("loopback skipped = %d, want 2", got)
	}
	// Even with an empty probe map (nothing probed), a loopback
	// entry must never earn an unreachable finding.
	rep := Compare(local, nil)
	ApplyReachability(rep, local, map[string]ProbeResult{})
	if len(findingsByKind(rep, KindBaseURLUnreachable)) != 0 {
		t.Errorf("loopback entries must never be flagged unreachable: %+v", rep.Findings)
	}
	if len(findingsByKind(rep, KindVerifiedBaseUnreachable)) != 0 {
		t.Errorf("loopback entries must never be downgrade candidates: %+v", rep.Findings)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	for u, want := range map[string]bool{
		"http://localhost:11434":        true,
		"http://localhost:11434/v1":     true,
		"http://127.0.0.1:1337/v1":      true,
		"http://127.0.0.2:8080":         true,
		"http://[::1]:8080/v1":          true,
		"https://api.example.com/v1":    false,
		"https://localhost.example.com": false,
		"":                              false,
		"not a url":                     false,
	} {
		if got := isLoopbackURL(u); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestLoadTriageSummary(t *testing.T) {
	dir := t.TempDir()
	good := `{"date":"2026-11-18","doc":"internal catalog triage record",` +
		`"audit_before":{"missing_in_catalog":228,"base_url_missing_in_catalog":44,"renamed":21,"possible_rename":12,"catalog_total":423},` +
		`"candidates":{"total":228,"triaged":46,"added":32,"added_verified_from_docs":4,"added_reference_only":28,"rejected":12,"pending":2,"untouched":182},` +
		`"base_urls":{"missing_before":44,"filled":43,"skipped":1},` +
		`"renames":{"total":21,"applied":4,"rejected":17},` +
		`"possible_renames":{"total":12,"applied":0,"pending_human":12}}`
	path := dir + "/catalog-triage-2026-11-18.json"
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := LoadTriageSummary(path)
	if err != nil {
		t.Fatalf("LoadTriageSummary(good): %v", err)
	}
	if sum == nil || sum.Candidates.Added != 32 || sum.BaseURLs.Filled != 43 {
		t.Fatalf("LoadTriageSummary(good) = %+v, want 32 added / 43 filled", sum)
	}
	// Missing file is normal: (nil, nil), never invented numbers.
	sum, err = LoadTriageSummary(dir + "/does-not-exist.json")
	if err != nil || sum != nil {
		t.Fatalf("LoadTriageSummary(missing) = (%v, %v), want (nil, nil)", sum, err)
	}
	// A ledger that does not add up must fail loudly.
	bad := strings.Replace(good, `"added":32`, `"added":31`, 1)
	badPath := dir + "/bad.json"
	if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTriageSummary(badPath); err == nil {
		t.Fatal("LoadTriageSummary(bad ledger) must fail")
	}
	// A report with triage must round-trip through MarshalJSON.
	rep2 := &Report{GeneratedAt: "2026-11-18T00:00:00Z"}
	rep2.Triage, err = LoadTriageSummary(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := rep2.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"added": 32`) || !strings.Contains(string(data), `"triage"`) {
		t.Errorf("MarshalJSON dropped the triage record: %s", data)
	}
}

func TestWriteMarkdownTriageSection(t *testing.T) {
	rep := Compare(fixtureLocal(), fixtureRemote())
	rep.Triage = &TriageSummary{Date: "2026-11-18", Doc: "internal catalog triage record",
		Candidates: TriageCandidates{Total: 228, Triaged: 46, Added: 32, AddedVerifiedFromDocs: 4, AddedReferenceOnly: 28, Rejected: 12, Pending: 2, Untouched: 182},
		BaseURLs:   TriageBaseURLs{MissingBefore: 44, Filled: 43, Skipped: 1},
		Renames:    TriageRenames{Total: 21, Applied: 4, Rejected: 17}}
	var sb strings.Builder
	WriteMarkdown(rep, &sb)
	out := sb.String()
	for _, want := range []string{"## Triage 2026-11-18", "32 (4 verified from docs, 28 reference-only)", "43 of 44", "internal catalog triage record"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown with triage missing %q", want)
		}
	}
	// Without a triage record there is no section — never invented.
	rep.Triage = nil
	sb.Reset()
	WriteMarkdown(rep, &sb)
	if strings.Contains(sb.String(), "## Triage") {
		t.Error("markdown without triage must not render a triage section")
	}
}
