// Command codvian-bench is the Geekflare-style benchmark rig for
// Codvian Router (CHECKLIST items 105-106): an instant stand-in
// provider, a sequential-latency driver and a fixed-duration RPS
// driver. It is a separate binary on purpose — the router binary
// itself is never modified for benchmarking.
//
// Subcommands:
//
//	codvian-bench mock --port PORT
//	  Instant stand-in provider (the Geekflare NGINX role): serves
//	  GET /v1/models and POST /v1/chat/completions with a fixed
//	  tiny OpenAI-compatible reply and zero artificial delay.
//	  Written in Go (net/http) so the mock itself is never the
//	  bottleneck the way a Python mock would be at 32 clients.
//
//	codvian-bench latency --base URL [--key KEY] [--model MODEL]
//	    [--requests 1000] [--warmup 25]
//	  Sequential requests, one at a time, each with a distinct
//	  prompt (topic x question-frame, 50 x 20 = 1,000 unique
//	  combinations, so the router's exact/semantic cache and
//	  single-flight coalescing cannot collapse the load into
//	  cache hits). Prints one JSON object: request count, ok,
//	  errors (by kind), cache_hits (X-Codvian-Cache responses),
//	  mean/p50/p95/p99/min/max latency in ms over HTTP 200s.
//
//	codvian-bench rps --base URL [--key KEY] [--model MODEL]
//	    [--concurrency 32] [--duration 20s]
//	  Fixed-duration load: N workers loop requests until the
//	  deadline. Prompts cycle the same 1,000 unique combinations;
//	  repeats past 1,000 can hit the router cache, so cache_hits
//	  is reported alongside, plus upstream_rps (non-cache
//	  completions per second) so a cache-inflated headline can
//	  never be mistaken for gateway throughput. Prints one JSON
//	  object.
//
// Latency is measured from just before the request is sent until
// the full response body has been read, over a keep-alive client.
// Exit code is 0 even when requests error — errors are data for
// the harness to report, not a reason to abort the run; only flag
// errors exit non-zero.
//
// No sample/fake data: every number printed comes from a run this
// binary just performed.
//
// Pollution gate and provenance: a latency/RPS run is refused
// (non-zero exit, no JSON, no numbers) when the shared VM is busy
// at measurement start — 1-min loadavg >= 3, or a `go test` /
// `go build` process running — because a stormed run's numbers
// would measure the storm, not the router. Every run also stamps
// its exact command line, Go version, git HEAD (from the build's
// embedded VCS info, else `git rev-parse HEAD`, else "unknown")
// and loadavg at start/end onto stderr and into the JSON result,
// so a result can never be mistaken for a clean one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	modelsBody = `{"object":"list","data":[{"id":"model-a","object":"model"}]}`
	// Fixed tiny reply, matching the shape the Python mock in
	// scripts/bench-mock-upstream.py returns.
	replyBody = `{"id":"chatcmpl-mock","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}}`
)

// topics and frames combine into 50 x 20 = 1,000 lexically distinct
// prompts. Two prompts sharing a topic differ in the frame (and vice
// versa), so token overlap stays far below the router's 0.85
// semantic-cache threshold; no prompt repeats inside one latency run.
var topics = []string{
	"photosynthesis in desert cacti", "tidal locking of exoplanets",
	"sourdough starter hydration ratios", "Byzantine fault tolerance proofs",
	"migrating monarch butterflies", "welding aluminium bicycle frames",
	"Old Norse saga manuscripts", "fermentation of kimchi at altitude",
	"orbital rendezvous mathematics", "handloom weaving in Kanchipuram",
	"quantum error correction codes", "composting kitchen scraps safely",
	"medieval siege engine design", "deep-sea hydrothermal vent ecology",
	"restoring vintage fountain pens", "solar sail navigation principles",
	"beekeeping in rooftop apiaries", "cartography of uncharted islands",
	"ferrofluid sculpture techniques", "Andean potato domestication history",
	"luthier wood selection secrets", "glacier retreat measurements",
	"Morse code transmission speed", "bonsai pruning seasonal schedules",
	"ant colony optimisation algorithms", "Victorian greenhouse heating",
	"kayak rolling Eskimo technique", "fungal mycelium network signalling",
	"brass instrument mouthpiece acoustics", "Saharan caravan trade routes",
	"origami tessellation folding patterns", "coral bleaching recovery studies",
	"blacksmith forge temperature control", "Arctic permafrost thaw feedback",
	"calligraphy ink grinding methods", "seismic wave propagation models",
	"urban pigeon navigation abilities", "tapestry weaving loom setups",
	"volcanic ash soil fertility", "lighthouse lens Fresnel design",
	"mushroom foraging safety rules", "river delta sediment deposition",
	"typewriter ribbon replacement steps", "penguin huddling thermodynamics",
	"stained glass leading techniques", "monsoon rainfall prediction models",
	"harpsichord tuning temperaments", "cave painting pigment chemistry",
	"saffron harvesting labour costs", "wind turbine blade erosion",
}

var frames = []string{
	"Explain %s in one sentence.",
	"What causes %s?",
	"Give one little-known fact about %s.",
	"Summarise %s briefly.",
	"How does %s work?",
	"Why does %s matter in practice?",
	"What is the hardest part of %s?",
	"Describe %s to a curious beginner.",
	"What would surprise most people about %s?",
	"List the three key ideas behind %s.",
	"How is %s measured or tested?",
	"What is a common misconception about %s?",
	"Where did %s originate?",
	"What are the risks of getting %s wrong?",
	"Compare %s with its closest alternative.",
	"When is %s the right approach?",
	"What tools are used for %s?",
	"What does the future hold for %s?",
	"How long does %s usually take?",
	"Who relies on %s every day?",
}

func prompt(i int) string {
	return fmt.Sprintf(frames[(i/len(topics))%len(frames)], topics[i%len(topics)])
}

// warmupPrompt is deliberately a different prompt family from prompt():
// a warmup request must warm connection pools and the limiter's
// baseline WITHOUT pre-populating the router's exact/semantic cache
// for the measured requests (the first 25 measured prompts would
// otherwise return as cache hits, as the first rig run showed:
// exactly 25 cache_hits in each 1,000-request latency run).
func warmupPrompt(i int) string {
	return fmt.Sprintf("Warmup probe %d: explain %s in one sentence.", i, topics[i%len(topics)])
}

func newClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 128,
			IdleConnTimeout:     60 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
}

// oneRequest POSTs a single chat completion with the given prompt
// content and returns the latency in ms, whether it was a cache hit
// (X-Codvian-Cache header set), and an error string ("" on HTTP 200).
func oneRequest(client *http.Client, base, key, model, content string) (float64, bool, string) {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"stream":false}`,
		model, content)
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(base, "/")+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, false, "request-build: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(t0).Seconds() * 1000, false, "transport: " + err.Error()
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	lat := time.Since(t0).Seconds() * 1000
	if resp.StatusCode != http.StatusOK {
		return lat, false, fmt.Sprintf("http-%d", resp.StatusCode)
	}
	return lat, resp.Header.Get("X-Codvian-Cache") != "", ""
}

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(pct/100*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func round3(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return math.Round(f*1000) / 1000
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "codvian-bench: encode:", err)
		os.Exit(1)
	}
}

// --- run provenance and pollution gate ---
//
// Every measurement must be attributable and clean, or it must not
// happen at all. The gate refuses a latency/rps run when the shared
// VM is busy at measurement start (1-min loadavg >= 3, or a
// `go test` / `go build` process running); a stormed run's numbers
// would measure the storm, not the router. Provenance (exact
// command line, Go version, git HEAD, loadavg at start/end) is
// printed to stderr and embedded in the JSON result so the durable
// record carries it. Stdout stays pure JSON — the rig parses it.

// loadavgThreshold is the 1-min loadavg at or above which a
// measurement is refused: on this 1.5-shared-vCPU VM a loadavg
// >= 3 means other work is competing for CPU.
const loadavgThreshold = 3.0

// parseLoadavg parses the first three fields of /proc/loadavg.
func parseLoadavg(s string) ([3]float64, error) {
	var la [3]float64
	f := strings.Fields(s)
	if len(f) < 3 {
		return la, fmt.Errorf("loadavg %q has fewer than 3 fields", s)
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return la, fmt.Errorf("loadavg field %d %q: %v", i+1, f[i], err)
		}
		la[i] = v
	}
	return la, nil
}

func readLoadavg() ([3]float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoadavg(string(b))
}

// ancestorPIDs returns our own PID plus every ancestor up to PID 1,
// so the process scan never flags our own invocation chain.
func ancestorPIDs(pid int) map[int]bool {
	seen := map[int]bool{pid: true}
	for pid > 1 {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		// stat is "pid (comm) state ppid ..."; comm may contain
		// spaces or parens, so split after the LAST ')'.
		s := string(b)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			break
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			break
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil || ppid <= 0 || seen[ppid] {
			break
		}
		seen[ppid] = true
		pid = ppid
	}
	return seen
}

// scanGoProcs lists running `go test` / `go build` processes and
// stray Go test binaries (*.test) — anything that competes for CPU
// while a measurement runs. Our own process tree is excluded.
// procDir is /proc in production; tests pass a fake tree.
func scanGoProcs(procDir string, self int, ancestors map[int]bool) ([]string, error) {
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || ancestors[pid] {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procDir, e.Name(), "cmdline"))
		if err != nil || len(b) == 0 {
			continue
		}
		var argv []string
		for _, p := range strings.Split(string(b), "\x00") {
			if p != "" {
				argv = append(argv, p)
			}
		}
		if len(argv) == 0 {
			continue
		}
		base := filepath.Base(argv[0])
		hit := false
		if base == "go" && len(argv) > 1 && (argv[1] == "test" || argv[1] == "build") {
			hit = true
		}
		if strings.HasSuffix(base, ".test") {
			hit = true
		}
		if hit {
			desc := strings.Join(argv, " ")
			if len(desc) > 160 {
				desc = desc[:157] + "..."
			}
			found = append(found, fmt.Sprintf("pid %d: %s", pid, desc))
		}
	}
	return found, nil
}

// pollutionReasons returns one human-readable reason per pollution
// source, or nil when the machine is quiet.
func pollutionReasons(la [3]float64, procs []string) []string {
	var reasons []string
	if la[0] >= loadavgThreshold {
		reasons = append(reasons, fmt.Sprintf("1-min loadavg %.2f >= %.1f", la[0], loadavgThreshold))
	}
	for _, p := range procs {
		reasons = append(reasons, "competing process: "+p)
	}
	return reasons
}

// gateCheck refuses a measurement on a busy machine. It returns the
// loadavg at check time (zeros when unreadable) and a non-nil error
// describing every pollution source found. Unreadable loadavg or an
// unreadable process table also refuses: without a quiet check
// there is no clean measurement.
func gateCheck() ([3]float64, error) {
	la, err := readLoadavg()
	if err != nil {
		return la, fmt.Errorf("codvian-bench: cannot read /proc/loadavg (%v); refusing to measure without a quiet check", err)
	}
	procs, err := scanGoProcs("/proc", os.Getpid(), ancestorPIDs(os.Getpid()))
	if err != nil {
		return la, fmt.Errorf("codvian-bench: cannot scan processes (%v); refusing to measure without a quiet check", err)
	}
	if reasons := pollutionReasons(la, procs); len(reasons) > 0 {
		return la, fmt.Errorf("codvian-bench: POLLUTED RUN REFUSED — %s; wait for a quiet window (scripts/bench-quiet.sh) and retry; no measurement was taken", strings.Join(reasons, "; "))
	}
	return la, nil
}

// runProvenance is the attribution stamped on every run.
type runProvenance struct {
	Command   string
	GoVersion string
	GitHead   string
	GitDirty  *bool
}

// collectProvenance gathers the exact command line, Go version and
// git HEAD. HEAD comes from the build's embedded VCS info first; if
// the binary was built without it, `git rev-parse HEAD` in the
// current directory is tried; otherwise "unknown" — never guessed.
func collectProvenance() runProvenance {
	p := runProvenance{
		Command:   strings.Join(os.Args, " "),
		GoVersion: runtime.Version(),
		GitHead:   "unknown",
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if s.Value != "" {
					p.GitHead = s.Value
				}
			case "vcs.modified":
				b := s.Value == "true"
				p.GitDirty = &b
			}
		}
	}
	if p.GitHead == "unknown" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
			if head := strings.TrimSpace(string(out)); head != "" {
				p.GitHead = head
			}
		}
	}
	return p
}

// formatProvenance renders one stderr line identifying a run.
func formatProvenance(sub string, p runProvenance, la [3]float64) string {
	dirty := ""
	if p.GitDirty != nil && *p.GitDirty {
		dirty = " (dirty tree)"
	}
	return fmt.Sprintf("codvian-bench %s: cmd=%q go=%s git=%s%s loadavg=%.2f %.2f %.2f",
		sub, p.Command, p.GoVersion, p.GitHead, dirty, la[0], la[1], la[2])
}

// --- mock ---

func runMock(args []string) int {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	port := fs.Int("port", 0, "port to listen on (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *port <= 0 {
		fmt.Fprintln(os.Stderr, "codvian-bench mock: --port is required")
		return 2
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsBody))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replyBody))
	})
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", *port), Handler: mux}
	prov := collectProvenance()
	la, _ := readLoadavg()
	fmt.Fprintln(os.Stderr, formatProvenance("mock", prov, la))
	fmt.Fprintf(os.Stderr, "codvian-bench mock listening on 127.0.0.1:%d (instant stand-in, zero delay)\n", *port)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "codvian-bench mock:", err)
		return 1
	}
	return 0
}

// --- latency ---

type latencyResult struct {
	Mode           string         `json:"mode"`
	Base           string         `json:"base"`
	Model          string         `json:"model"`
	Requests       int            `json:"requests"`
	OK             int            `json:"ok"`
	Errors         int            `json:"errors"`
	ErrorKinds     map[string]int `json:"error_kinds,omitempty"`
	CacheHits      int            `json:"cache_hits"`
	ElapsedS       float64        `json:"elapsed_s"`
	MeanMs         float64        `json:"mean_ms"`
	P50Ms          float64        `json:"p50_ms"`
	P95Ms          float64        `json:"p95_ms"`
	P99Ms          float64        `json:"p99_ms"`
	MinMs          float64        `json:"min_ms"`
	MaxMs          float64        `json:"max_ms"`
	Command        string         `json:"command"`
	GoVersion      string         `json:"go_version"`
	GitHead        string         `json:"git_head"`
	GitDirty       *bool          `json:"git_dirty,omitempty"`
	LoadavgStart   [3]float64     `json:"loadavg_start"`
	LoadavgEnd     [3]float64     `json:"loadavg_end"`
	LoadavgEndHigh bool           `json:"loadavg_end_high"`
	Polluted       bool           `json:"polluted"`
}

func runLatency(args []string) int {
	fs := flag.NewFlagSet("latency", flag.ContinueOnError)
	base := fs.String("base", "", "base URL of the target (mock or router), required")
	key := fs.String("key", "", "gateway key (Bearer); empty = no auth header (direct-to-mock)")
	model := fs.String("model", "model-a", "model to request")
	requests := fs.Int("requests", 1000, "sequential requests")
	warmup := fs.Int("warmup", 25, "warmup requests (not counted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *requests <= 0 {
		fmt.Fprintln(os.Stderr, "codvian-bench latency: --base is required and --requests must be > 0")
		return 2
	}
	// Pollution gate: refuse to measure on a busy VM. A refused run
	// prints the reason and exits non-zero before any request, so
	// no number from a stormed run can ever be quoted.
	prov := collectProvenance()
	laStart, err := gateCheck()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, formatProvenance("latency", prov, laStart))
	client := newClient()
	for i := 0; i < *warmup; i++ {
		_, _, _ = oneRequest(client, *base, *key, *model, warmupPrompt(i))
	}
	lat := make([]float64, 0, *requests)
	errKinds := map[string]int{}
	cacheHits := 0
	t0 := time.Now()
	for i := 0; i < *requests; i++ {
		ms, hit, errStr := oneRequest(client, *base, *key, *model, prompt(i))
		if errStr != "" {
			errKinds[errStr]++
			continue
		}
		if hit {
			cacheHits++
		}
		lat = append(lat, ms)
	}
	elapsed := time.Since(t0).Seconds()
	sort.Float64s(lat)
	res := latencyResult{
		Mode: "latency", Base: *base, Model: *model,
		Requests: *requests, OK: len(lat), CacheHits: cacheHits,
		ElapsedS: round3(elapsed),
	}
	for _, n := range errKinds {
		res.Errors += n
	}
	if len(errKinds) > 0 {
		res.ErrorKinds = errKinds
	}
	if len(lat) > 0 {
		var sum float64
		for _, v := range lat {
			sum += v
		}
		res.MeanMs = round3(sum / float64(len(lat)))
		res.P50Ms = round3(percentile(lat, 50))
		res.P95Ms = round3(percentile(lat, 95))
		res.P99Ms = round3(percentile(lat, 99))
		res.MinMs = round3(lat[0])
		res.MaxMs = round3(lat[len(lat)-1])
	}
	// Provenance + end-of-run pollution check. A sequential latency
	// run loads the VM itself only negligibly, so a hot loadavg at
	// the end means outside load arrived mid-run: the JSON is
	// emitted marked polluted and the run fails loudly.
	laEnd, _ := readLoadavg()
	endProcs, _ := scanGoProcs("/proc", os.Getpid(), ancestorPIDs(os.Getpid()))
	res.Command = prov.Command
	res.GoVersion = prov.GoVersion
	res.GitHead = prov.GitHead
	res.GitDirty = prov.GitDirty
	res.LoadavgStart = laStart
	res.LoadavgEnd = laEnd
	res.LoadavgEndHigh = laEnd[0] >= loadavgThreshold
	res.Polluted = len(endProcs) > 0 || res.LoadavgEndHigh
	emit(res)
	fmt.Fprintf(os.Stderr, "codvian-bench latency: loadavg at end: %.2f %.2f %.2f\n", laEnd[0], laEnd[1], laEnd[2])
	if len(endProcs) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-bench latency: POLLUTED RUN — competing go test/build started during the run: %s; the JSON above is marked polluted and is NOT quotable\n", strings.Join(endProcs, "; "))
		return 1
	}
	if res.LoadavgEndHigh {
		fmt.Fprintf(os.Stderr, "codvian-bench latency: POLLUTED RUN — 1-min loadavg at end %.2f >= %.1f; the JSON above is marked loadavg_end_high and is NOT quotable\n", laEnd[0], loadavgThreshold)
		return 1
	}
	return 0
}

// --- rps ---

type rpsResult struct {
	Mode           string         `json:"mode"`
	Base           string         `json:"base"`
	Model          string         `json:"model"`
	Concurrency    int            `json:"concurrency"`
	DurationS      float64        `json:"duration_s"`
	ElapsedS       float64        `json:"elapsed_s"`
	Completed      int            `json:"completed"`
	OK             int            `json:"ok"`
	Errors         int            `json:"errors"`
	ErrorKinds     map[string]int `json:"error_kinds,omitempty"`
	CacheHits      int            `json:"cache_hits"`
	RPS            float64        `json:"rps"`
	UpstreamRPS    float64        `json:"upstream_rps"`
	MeanMs         float64        `json:"mean_ms"`
	P50Ms          float64        `json:"p50_ms"`
	P95Ms          float64        `json:"p95_ms"`
	P99Ms          float64        `json:"p99_ms"`
	Command        string         `json:"command"`
	GoVersion      string         `json:"go_version"`
	GitHead        string         `json:"git_head"`
	GitDirty       *bool          `json:"git_dirty,omitempty"`
	LoadavgStart   [3]float64     `json:"loadavg_start"`
	LoadavgEnd     [3]float64     `json:"loadavg_end"`
	LoadavgEndHigh bool           `json:"loadavg_end_high"`
	Polluted       bool           `json:"polluted"`
}

func runRPS(args []string) int {
	fs := flag.NewFlagSet("rps", flag.ContinueOnError)
	base := fs.String("base", "", "base URL of the target (mock or router), required")
	key := fs.String("key", "", "gateway key (Bearer); empty = no auth header (direct-to-mock)")
	model := fs.String("model", "model-a", "model to request")
	concurrency := fs.Int("concurrency", 32, "concurrent clients")
	duration := fs.Duration("duration", 20*time.Second, "load duration")
	warmup := fs.Int("warmup", 25, "warmup requests (not counted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *concurrency <= 0 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "codvian-bench rps: --base is required, --concurrency and --duration must be > 0")
		return 2
	}
	// Pollution gate: see runLatency. A refused run exits non-zero
	// before any request, so no stormed number can be quoted.
	prov := collectProvenance()
	laStart, err := gateCheck()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintln(os.Stderr, formatProvenance("rps", prov, laStart))
	client := newClient()
	for i := 0; i < *warmup; i++ {
		_, _, _ = oneRequest(client, *base, *key, *model, warmupPrompt(i))
	}
	deadline := time.Now().Add(*duration)
	var idx atomic.Int64
	var ok, errs, cacheHits atomic.Int64
	errKinds := map[string]int{}
	var mu sync.Mutex
	latCh := make(chan float64, 65536)
	t0 := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				i := int(idx.Add(1) - 1)
				ms, hit, errStr := oneRequest(client, *base, *key, *model, prompt(i))
				if errStr != "" {
					errs.Add(1)
					mu.Lock()
					errKinds[errStr]++
					mu.Unlock()
					continue
				}
				ok.Add(1)
				if hit {
					cacheHits.Add(1)
				}
				select {
				case latCh <- ms:
				default:
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(t0).Seconds()
	close(latCh)
	lat := make([]float64, 0, 65536)
	for v := range latCh {
		lat = append(lat, v)
	}
	sort.Float64s(lat)
	res := rpsResult{
		Mode: "rps", Base: *base, Model: *model,
		Concurrency: *concurrency, DurationS: duration.Seconds(),
		ElapsedS: round3(elapsed),
		OK:       int(ok.Load()), Errors: int(errs.Load()),
		CacheHits: int(cacheHits.Load()),
	}
	res.Completed = res.OK + res.Errors
	if len(errKinds) > 0 {
		res.ErrorKinds = errKinds
	}
	if elapsed > 0 {
		res.RPS = round3(float64(res.OK) / elapsed)
		res.UpstreamRPS = round3(float64(res.OK-res.CacheHits) / elapsed)
	}
	if len(lat) > 0 {
		var sum float64
		for _, v := range lat {
			sum += v
		}
		res.MeanMs = round3(sum / float64(len(lat)))
		res.P50Ms = round3(percentile(lat, 50))
		res.P95Ms = round3(percentile(lat, 95))
		res.P99Ms = round3(percentile(lat, 99))
	}
	// Provenance + end-of-run pollution check. Unlike the sequential
	// latency run, a 32-client RPS run loads this small VM itself,
	// so a hot loadavg at the end is expected and only warns (the
	// JSON is still marked loadavg_end_high — check it before
	// quoting). A competing go test/build that started mid-run is
	// outside load and fails the run loudly.
	laEnd, _ := readLoadavg()
	endProcs, _ := scanGoProcs("/proc", os.Getpid(), ancestorPIDs(os.Getpid()))
	res.Command = prov.Command
	res.GoVersion = prov.GoVersion
	res.GitHead = prov.GitHead
	res.GitDirty = prov.GitDirty
	res.LoadavgStart = laStart
	res.LoadavgEnd = laEnd
	res.LoadavgEndHigh = laEnd[0] >= loadavgThreshold
	res.Polluted = len(endProcs) > 0
	emit(res)
	fmt.Fprintf(os.Stderr, "codvian-bench rps: loadavg at end: %.2f %.2f %.2f\n", laEnd[0], laEnd[1], laEnd[2])
	if len(endProcs) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-bench rps: POLLUTED RUN — competing go test/build started during the run: %s; the JSON above is marked polluted and is NOT quotable\n", strings.Join(endProcs, "; "))
		return 1
	}
	if res.LoadavgEndHigh {
		fmt.Fprintf(os.Stderr, "codvian-bench rps: WARNING — 1-min loadavg at end %.2f >= %.1f (a 32-client RPS run loads the VM itself, so this alone does not fail the run); the JSON above is marked loadavg_end_high — check it before quoting\n", laEnd[0], loadavgThreshold)
	}
	return 0
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: codvian-bench <mock|latency|rps> [flags]")
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "mock":
		code = runMock(os.Args[2:])
	case "latency":
		code = runLatency(os.Args[2:])
	case "rps":
		code = runRPS(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "codvian-bench: unknown subcommand %q (want mock|latency|rps)\n", os.Args[1])
		code = 2
	}
	os.Exit(code)
}
