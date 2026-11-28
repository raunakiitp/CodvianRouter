// M10-16 dashboard Batch (read-only) + Responses sections.
//
// Batch: read-only job list from GET /v1/batches (server.go batches
// block :1571-1577, guard.go:329 — gateway key, chat scope, list is
// per-key). Create/cancel are NOT wired in M10 — batch creation
// spends quota through a gateway-key-authenticated API and the
// operator-key flow for it is undecided — so the section has NO
// create/cancel buttons at all (not disabled buttons — none).
// Responses: identifiers, model, status, continuation chain and
// counts only from GET /dashboard/responses + /dashboard/responses/tree
// (server.go:1591-1592, auth-exempt). No prompt or response text is
// ever shown: the tests below seed real prompt/answer strings and
// assert they appear nowhere in either feed, in the tree, or in the
// M10-16 JS. Conversations are CUT: the sector-typical Conversations is
// chat history we do not store; Responses is our equivalent and is
// labelled as such in the section heading.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m10_16Section returns the served HTML of one <section id="view-...">.
func m10_16Section(t *testing.T, body, viewID string) string {
	t.Helper()
	start := strings.Index(body, `id="`+viewID+`"`)
	if start < 0 {
		t.Fatalf("dashboard has no %s section", viewID)
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("%s section is not closed", viewID)
	}
	return rest[:end]
}

// m10_16InlineScript returns the served dashboard's single inline
// <script> block.
func m10_16InlineScript(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<script>")
	if start < 0 {
		t.Fatalf("dashboard has no inline script")
	}
	rest := body[start+len("<script>"):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatalf("inline script is not closed")
	}
	return rest[:end]
}

// m10_16JS returns the M10-16 JS region only (M10-16-START..END
// markers), so absence assertions about prompt/response text apply
// to this slice's code and not to unrelated sections.
func m10_16JS(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "/* M10-16-START */")
	end := strings.Index(script, "/* M10-16-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("M10-16 JS markers (M10-16-START/END) not found in the inline script")
	}
	return script[start:end]
}

func TestDashboardM10_16SectionsRender(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	batch := m10_16Section(t, body, "view-batch")
	responses := m10_16Section(t, body, "view-responses")
	script := m10_16InlineScript(t, body)
	js16 := m10_16JS(t, script)

	// Nav slots exist (M10-0) and no Conversations view/nav exists:
	// Conversations is cut, Responses is the equivalent.
	if !strings.Contains(body, `data-view="batch"`) || !strings.Contains(body, `data-view="responses"`) {
		t.Error("dashboard nav is missing the batch/responses slots")
	}
	if strings.Contains(body, `id="view-conversations"`) || strings.Contains(body, `data-view="conversations"`) {
		t.Error("dashboard has a Conversations view — it is cut in M10-16; Responses is the equivalent")
	}

	// The section markers sit before their <section> tags, so they
	// are checked against the whole body, not the extracted section.
	for _, want := range []string{"<!-- M10-16 Batch -->", "<!-- M10-16 Responses -->"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing section marker %q", want)
		}
	}

	// Batch section: heading, table, ids, honest states, read-only note.
	for _, want := range []string{
		"<h1>Batch</h1>",
		"read-only list from GET /v1/batches",
		`id="batchPageBody"`, `id="batchPageCount"`,
		`id="batchPageEmpty"`, "No batches yet",
		`id="batchPageKey"`, "The batch list is per gateway key",
		`id="batchPageLoadBtn"`, "Load batches",
		`id="batchPageError"`, `id="batchPageRefreshBtn"`, "Refresh",
		"Read-only: batch creation and cancellation are not wired",
		"the operator-key flow for it is undecided",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("dashboard Batch section missing %q", want)
		}
	}
	// No create/cancel buttons at all in the Batch section: the only
	// buttons are Refresh and Load batches.
	if n := strings.Count(batch, "<button"); n != 2 {
		t.Errorf("Batch section has %d <button elements, want 2 (Refresh + Load batches — no create/cancel)", n)
	}
	if strings.Contains(batch, "batchPageCreate") || strings.Contains(batch, "batchPageCancel") {
		t.Error("Batch section has a create/cancel control id — M10-16 is read-only")
	}
	// No create/cancel POST anywhere in this slice's JS.
	for _, banned := range []string{`"/cancel"`, `method: "POST"`, `method:"POST"`} {
		if strings.Contains(js16, banned) {
			t.Errorf("M10-16 JS contains %q — create/cancel are not wired", banned)
		}
	}

	// Responses section: heading, equivalence label, KPIs, table,
	// tree card, honest states.
	for _, want := range []string{
		"<h1>Responses</h1>",
		"Responses, not conversations — the router stores response identifiers and chains, not chat transcripts",
		"no prompt or response text is ever shown here",
		`id="responsesPageStored"`, `id="responsesPageCreated"`, `id="responsesPageContinuations"`,
		`id="responsesPageBody"`, `id="responsesPageCount"`,
		`id="responsesPageEmpty"`, "No responses stored yet",
		`id="responsesPageError"`, `id="responsesPageMeta"`,
		`id="responsesPageTree"`, `id="responsesPageTreeMeta"`,
		`id="responsesPageTreeEmpty"`, "No chains yet",
		`id="responsesPageTreeError"`, `id="responsesPageRefreshBtn"`,
		"Continuation chains",
	} {
		if !strings.Contains(responses, want) {
			t.Errorf("dashboard Responses section missing %q", want)
		}
	}

	// The section reads the three live feeds through this slice's
	// own loader functions, keyed for Batch via the shared helper.
	for _, want := range []string{
		`fetch("/v1/batches?limit=100"`,
		`fetch("/dashboard/responses")`,
		`fetch("/dashboard/responses/tree")`,
		"m10_16LoadBatch", "m10_16LoadResponses",
		"m10_16BatchRows", "m10_16ResponsesRows", "m10_16ResponsesTreeRows",
		"adminHeaders()",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard M10-16 JS missing %q", want)
		}
	}

	// This slice's JS never reads prompt/response text: no field
	// access to input/output/transcript/message content anywhere
	// in the M10-16 region.
	for _, banned := range []string{".input", ".output", "transcript", "ResponseJSON", "messages", "content_text"} {
		if strings.Contains(js16, banned) {
			t.Errorf("M10-16 JS contains %q — it must only read identifiers, model, status, chain and counts", banned)
		}
	}

	// No fake-data markers, no Playground, in either section.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(batch, banned) {
			t.Errorf("Batch section contains %q", banned)
		}
		if strings.Contains(responses, banned) {
			t.Errorf("Responses section contains %q", banned)
		}
	}
}

// The batch rows the dashboard renders equal the GET /v1/batches
// response: one real batch (2 lines, counting fake engine) run to
// completion, listed back with its status, model and counts.
func TestDashboardM10_16BatchRowsEqualFeed(t *testing.T) {
	eng := okBatchEngine()
	srv := newBatchesServer(t, eng)
	in := seedBatchInput(t, srv, batchJSONL(t, "req-a", "req-b"))
	body, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions",
		"completion_window": "24h",
	})
	rec := doBatches(t, srv, "POST", "/v1/batches", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	created := decodeBatch(t, rec)
	// Model is nil at creation: the worker sets it from the first
	// input line's model when the batch runs (batches_api_test.go
	// asserts it on the completed batch, not the created one).
	done := waitBatchStatus(t, srv, created.ID, "completed")
	if done.Model == nil || *done.Model != "mock-model" {
		t.Fatalf("completed batch model %v, want mock-model", done.Model)
	}
	if done.RequestCounts.Total != 2 || done.RequestCounts.Completed != 2 || done.RequestCounts.Failed != 0 {
		t.Fatalf("completed batch counts = %+v, want total 2 / completed 2 / failed 0", done.RequestCounts)
	}

	rec = doBatches(t, srv, "GET", "/v1/batches", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var list batchListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode batch list: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 1 {
		t.Fatalf("list = %+v, want object list with 1 batch", list)
	}
	got := list.Data[0]
	if got.ID != created.ID || string(got.Status) != "completed" {
		t.Fatalf("list row = %+v, want id %s status completed", got, created.ID)
	}
	if got.Model == nil || *got.Model != "mock-model" {
		t.Fatalf("list row model = %v, want mock-model", got.Model)
	}
	if got.RequestCounts.Total != 2 || got.RequestCounts.Completed != 2 || got.RequestCounts.Failed != 0 {
		t.Fatalf("list row counts = %+v, want total 2 / completed 2 / failed 0", got.RequestCounts)
	}
	if list.HasMore {
		t.Errorf("list has_more = true with 1 batch, want false")
	}
	// The dashboard's Batch rows come from this same shape: the JS
	// pure helper reads d.data with id/status/model/request_counts
	// (asserted under node in TestDashboardM10_16PureHelpersUnderNode).
	srv2 := newTestServer(t, &fakeEngine{})
	script := m10_16InlineScript(t, doReq(t, srv2, http.MethodGet, "/dashboard", "", "").Body.String())
	if !strings.Contains(script, "d.data") || !strings.Contains(script, "request_counts") {
		t.Error("M10-16 Batch JS does not read the list feed's data/request_counts")
	}
}

// Two real Responses creates (the second continuing the first) feed
// GET /dashboard/responses and /dashboard/responses/tree with the
// exact ids, model, status, chain and counts — and neither feed
// carries any prompt or answer text.
func TestDashboardM10_16ResponsesRowsEqualFeed(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "first answer", Model: "m",
		Usage: provider.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"what is 2+2?"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create 1: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	first := decodeResponsesBody(t, rec)
	eng.resp = provider.ChatResponse{Content: "second answer", Model: "m",
		Usage: provider.Usage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8}}
	rec2 := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"and 3+3?","previous_response_id":"`+first.ID+`"}`, "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("create 2: status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
	}
	second := decodeResponsesBody(t, rec2)
	if second.PreviousResponseID == nil || *second.PreviousResponseID != first.ID {
		t.Fatalf("continuation previous_response_id = %+v, want %s", second.PreviousResponseID, first.ID)
	}

	feedRec := doReq(t, srv, http.MethodGet, "/dashboard/responses", "", "")
	if feedRec.Code != http.StatusOK {
		t.Fatalf("feed: status = %d, want 200", feedRec.Code)
	}
	var f responsesFeed
	if err := json.Unmarshal(feedRec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if len(f.Responses) != 2 || f.Created != 2 || f.Continuations != 1 || f.Stored != 2 {
		t.Fatalf("feed = %+v, want 2 responses, created 2, continuations 1, stored 2", f)
	}
	// Newest first: the continuation is row 0 and names the first
	// response as its previous id.
	if f.Responses[0].ID != second.ID || f.Responses[0].PreviousResponseID != first.ID {
		t.Fatalf("feed row 0 = %+v, want id %s continuing %s", f.Responses[0], second.ID, first.ID)
	}
	if f.Responses[1].ID != first.ID || f.Responses[1].PreviousResponseID != "" {
		t.Fatalf("feed row 1 = %+v, want id %s with no previous id (root)", f.Responses[1], first.ID)
	}
	for _, row := range f.Responses {
		if row.Model != "m" || row.Status != "completed" {
			t.Fatalf("feed row = %+v, want model m status completed", row)
		}
	}
	// No prompt or answer text anywhere in the feed JSON.
	for _, leaked := range []string{"what is 2+2?", "and 3+3?", "first answer", "second answer"} {
		if strings.Contains(feedRec.Body.String(), leaked) {
			t.Fatalf("responses feed leaks %q: %s", leaked, feedRec.Body.String())
		}
	}

	treeRec := doReq(t, srv, http.MethodGet, "/dashboard/responses/tree", "", "")
	if treeRec.Code != http.StatusOK {
		t.Fatalf("tree: status = %d, want 200", treeRec.Code)
	}
	var tree responseTreeFeed
	if err := json.Unmarshal(treeRec.Body.Bytes(), &tree); err != nil {
		t.Fatalf("decode tree: %v", err)
	}
	if tree.Total != 2 || tree.Branched != 1 || len(tree.Roots) != 1 {
		t.Fatalf("tree = %+v, want total 2 / branched 1 / 1 root", tree)
	}
	if tree.Roots[0].ID != first.ID || len(tree.Roots[0].Children) != 1 || tree.Roots[0].Children[0].ID != second.ID {
		t.Fatalf("tree root = %+v, want %s with child %s", tree.Roots[0], first.ID, second.ID)
	}
	for _, leaked := range []string{"what is 2+2?", "and 3+3?", "first answer", "second answer"} {
		if strings.Contains(treeRec.Body.String(), leaked) {
			t.Fatalf("responses tree leaks %q: %s", leaked, treeRec.Body.String())
		}
	}
}

// The M10-16 pure helpers run under node, against the exact code
// served in the dashboard, driven with the REAL feed JSON captured
// live in this test (not hand-built fixtures): node --check on the
// whole extracted inline script, then the pure block mapping the
// live batch list, the live responses feed and the live tree.
func TestDashboardM10_16PureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not on PATH — cannot run the inline-script check (install node or set NODE_PATH)")
	}

	// Live batch list JSON: one real batch run to completion.
	eng := okBatchEngine()
	bsrv := newBatchesServer(t, eng)
	in := seedBatchInput(t, bsrv, batchJSONL(t, "req-a", "req-b"))
	bb, _ := json.Marshal(map[string]any{
		"input_file_id": in.ID, "endpoint": "/v1/chat/completions",
		"completion_window": "24h",
	})
	crec := doBatches(t, bsrv, "POST", "/v1/batches", string(bb))
	if crec.Code != http.StatusOK {
		t.Fatalf("batch create: status %d, want 200", crec.Code)
	}
	created := decodeBatch(t, crec)
	waitBatchStatus(t, bsrv, created.ID, "completed")
	listRec := doBatches(t, bsrv, "GET", "/v1/batches", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("batch list: status %d, want 200", listRec.Code)
	}
	batchListRaw := listRec.Body.Bytes()

	// Live responses feed + tree JSON: two creates, one a
	// continuation of the other.
	ceng := &captureEngine{resp: provider.ChatResponse{
		Content: "first answer", Model: "m",
		Usage: provider.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4},
	}}
	rsrv := newTestServer(t, ceng, WithMetrics(metrics.NewRecorder()))
	r1 := doReq(t, rsrv, http.MethodPost, "/v1/responses", `{"model":"m","input":"what is 2+2?"}`, "test-key-123")
	if r1.Code != http.StatusOK {
		t.Fatalf("response create 1: status %d, want 200", r1.Code)
	}
	first := decodeResponsesBody(t, r1)
	ceng.resp = provider.ChatResponse{Content: "second answer", Model: "m",
		Usage: provider.Usage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8}}
	r2 := doReq(t, rsrv, http.MethodPost, "/v1/responses", `{"model":"m","input":"and 3+3?","previous_response_id":"`+first.ID+`"}`, "test-key-123")
	if r2.Code != http.StatusOK {
		t.Fatalf("response create 2: status = %d, want 200", r2.Code)
	}
	second := decodeResponsesBody(t, r2)
	feedRec := doReq(t, rsrv, http.MethodGet, "/dashboard/responses", "", "")
	if feedRec.Code != http.StatusOK {
		t.Fatalf("responses feed: status = %d, want 200", feedRec.Code)
	}
	respRaw := feedRec.Body.Bytes()
	treeRec := doReq(t, rsrv, http.MethodGet, "/dashboard/responses/tree", "", "")
	if treeRec.Code != http.StatusOK {
		t.Fatalf("responses tree: status = %d, want 200", treeRec.Code)
	}
	treeRaw := treeRec.Body.Bytes()

	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := m10_16InlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-16-PURE-START */")
	end := strings.Index(script, "/* M10-16-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-16-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]
	driver := pure + `
var out = {
  batchRows: m10_16BatchRows(` + string(batchListRaw) + `),
  respRows: m10_16ResponsesRows(` + string(respRaw) + `),
  treeRows: m10_16ResponsesTreeRows(` + string(treeRaw) + `),
  pillCompleted: m10_16BatchPill("completed"),
  pillFailed: m10_16BatchPill("failed"),
  pillValidating: m10_16BatchPill("validating"),
  pillCancelled: m10_16BatchPill("cancelled"),
  fmtZero: m10_16FmtTime(0),
  fmtKnown: m10_16FmtTime(1760000000)
};
console.log(JSON.stringify(out));
`
	driverPath := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var got struct {
		BatchRows []struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			Model     *string `json:"model"`
			Completed int     `json:"completed"`
			Failed    int     `json:"failed"`
			Total     int     `json:"total"`
			CreatedAt int64   `json:"createdAt"`
		} `json:"batchRows"`
		RespRows []struct {
			ID            string `json:"id"`
			Model         string `json:"model"`
			Status        string `json:"status"`
			PreviousID    string `json:"previousId"`
			Provider      string `json:"provider"`
			CreatedAt     int64  `json:"createdAt"`
			SessionTokens int    `json:"sessionTokens"`
			Stream        bool   `json:"stream"`
		} `json:"respRows"`
		TreeRows []struct {
			ID            string `json:"id"`
			Model         string `json:"model"`
			Status        string `json:"status"`
			Depth         int    `json:"depth"`
			ParentMissing bool   `json:"parentMissing"`
		} `json:"treeRows"`
		PillCompleted  string `json:"pillCompleted"`
		PillFailed     string `json:"pillFailed"`
		PillValidating string `json:"pillValidating"`
		PillCancelled  string `json:"pillCancelled"`
		FmtZero        string `json:"fmtZero"`
		FmtKnown       string `json:"fmtKnown"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}
	if len(got.BatchRows) != 1 {
		t.Fatalf("batchRows = %d rows, want 1", len(got.BatchRows))
	}
	brow := got.BatchRows[0]
	if brow.ID != created.ID || brow.Status != "completed" {
		t.Errorf("batch row = %+v, want id %s status completed", brow, created.ID)
	}
	if brow.Model == nil || *brow.Model != "mock-model" {
		t.Errorf("batch row model = %v, want mock-model", brow.Model)
	}
	if brow.Completed != 2 || brow.Failed != 0 || brow.Total != 2 {
		t.Errorf("batch row counts = %+v, want completed 2 / failed 0 / total 2", brow)
	}
	if brow.CreatedAt == 0 {
		t.Errorf("batch row createdAt = 0, want the feed's created_at")
	}
	if len(got.RespRows) != 2 {
		t.Fatalf("respRows = %d rows, want 2", len(got.RespRows))
	}
	if got.RespRows[0].ID != second.ID || got.RespRows[0].PreviousID != first.ID {
		t.Errorf("resp row 0 = %+v, want id %s continuing %s", got.RespRows[0], second.ID, first.ID)
	}
	if got.RespRows[1].ID != first.ID || got.RespRows[1].PreviousID != "" {
		t.Errorf("resp row 1 = %+v, want id %s with no previous id", got.RespRows[1], first.ID)
	}
	for _, row := range got.RespRows {
		if row.Model != "m" || row.Status != "completed" {
			t.Errorf("resp row = %+v, want model m status completed", row)
		}
	}
	if len(got.TreeRows) != 2 {
		t.Fatalf("treeRows = %d rows, want 2", len(got.TreeRows))
	}
	if got.TreeRows[0].ID != first.ID || got.TreeRows[0].Depth != 0 {
		t.Errorf("tree row 0 = %+v, want root %s at depth 0", got.TreeRows[0], first.ID)
	}
	if got.TreeRows[1].ID != second.ID || got.TreeRows[1].Depth != 1 {
		t.Errorf("tree row 1 = %+v, want child %s at depth 1", got.TreeRows[1], second.ID)
	}
	if got.TreeRows[0].ParentMissing || got.TreeRows[1].ParentMissing {
		t.Errorf("tree rows flag parent_missing: %+v, want false for a live chain", got.TreeRows)
	}
	if got.PillCompleted != "up" || got.PillFailed != "down" {
		t.Errorf("pills: completed %q failed %q, want up/down", got.PillCompleted, got.PillFailed)
	}
	if got.PillValidating != "flat" || got.PillCancelled != "flat" {
		t.Errorf("pills: validating %q cancelled %q, want flat/flat", got.PillValidating, got.PillCancelled)
	}
	if got.FmtZero != "—" {
		t.Errorf("m10_16FmtTime(0) = %q, want the honest —", got.FmtZero)
	}
	wantKnown := time.Unix(1760000000, 0).UTC().Format("2006-01-02 15:04:05Z")
	if got.FmtKnown != wantKnown {
		t.Errorf("m10_16FmtTime(1760000000) = %q, want %q", got.FmtKnown, wantKnown)
	}
}
