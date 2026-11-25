// Command codvian-router is the Codvian Router Phase 1 MVP binary.
//
// Usage:
//
//	codvian-router [--config PATH] [--port PORT] serve    # default subcommand
//	codvian-router [--config PATH] init [--key-file PATH]  # create config + gateway key (--key-file: key to a 0600 file, not stdout)
//	codvian-router keygen                               # print a fresh gateway key
//	codvian-router [--config PATH] [--model MODEL] eval  # smoke-eval the live engine
//	codvian-router [--config PATH] validate             # validate the config file
//	codvian-router [--config PATH] [--port PORT] tray   # supervise the gateway headless
//	codvian-router [--config PATH] doctor              # check config, keys, providers, ports, dashboard
//	codvian-router [--config PATH] [--port PORT] configure # first-run setup (interactive or --provider/--api-key)
//	codvian-router [--config PATH] mcp                 # MCP server on stdio (router_status, provider_list, model_list)
//	codvian-router [--config PATH] provider list        # list configured providers
//	codvian-router [--config PATH] provider add <base-url> <api-key> [--name NAME]
//	codvian-router [--config PATH] provider add-catalog <catalog-id> <api-key> [--endpoint E] [--variant V] [--base-url URL]
//	codvian-router [--config PATH] provider login <catalog-id>  # account login (OpenRouter OAuth PKCE; headless code paste)
//	codvian-router [--config PATH] provider remove <provider-id>  # delete a configured provider (persisted)
//	codvian-router verify-release [DIR]  # verify DIR's files against its checksums.txt (integrity only; signature via scripts/verify-release.sh)
//	codvian-router release verify <tag> [--dir DIR]  # verify a published release: cosign signature + checksums + SBOM + provenance (see docs/RELEASE-CHECKLIST.md)
//	codvian-router [--config PATH] db import <export-file> [--confirm IMPORT]  # merge an export file's usage history into the ledger (never resets it)
//	codvian-router [--config PATH] eval-feedback [--feedback-db PATH] [--cutoff RFC3339]  # A/B: feedback heuristic vs no-feedback baseline on recorded traffic
//
// The router is local-only by default: it binds 127.0.0.1 only, and auth
// is ON — requests must present a gateway key issued by `init` (or added
// to the config later). `serve` wires the whole stack: config → Vault
// (AES-256-GCM for provider keys) → SQLite ledger → provider registry →
// routing engine (scoring + quota hints + health watcher + graceful
// degradation) → OpenAI-compatible HTTP server with /metrics.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/capability"
	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/eval"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/gctune"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/modelsync"
	"github.com/raunakiitp/codvian-router/internal/observability"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/providerlogin"
	"github.com/raunakiitp/codvian-router/internal/readiness"
	"github.com/raunakiitp/codvian-router/internal/release"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
	"github.com/raunakiitp/codvian-router/internal/server"
	"github.com/raunakiitp/codvian-router/internal/strategy"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
	"github.com/raunakiitp/codvian-router/internal/tray"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// Version is the binary version printed on the startup line.
const Version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

// options holds the global flags shared by all subcommands.
type options struct {
	configPath string
	port       int // 0 = use the port from the config file
	listen     string
	model      string
	// jsonOut requests machine-readable JSON output for the commands
	// that support it (currently `clients`). P1.3.
	jsonOut bool
	// lowEnd enables the low-end GC profile for serve: GOGC=off plus a
	// memory limit (see internal/gctune). Same as CODVIAN_ROUTER_LOWEND=1.
	lowEnd bool
	// pprofAddr is the loopback listener address for net/http/pprof (e.g. 127.0.0.1:6060).
	pprofAddr string
	// headless enables serve --headless (CHECKLIST 197-198): the
	// gateway as a pure API process — no dashboard server — and it
	// refuses to start without a gateway key configured.
	headless bool
	// wizard enables init --wizard (CHECKLIST 197-198): the
	// interactive first-run walkthrough.
	wizard bool
	// provider add-catalog flags.
	endpoint string
	variant  string
	baseURL  string
	name     string
	// keyFile is the --key-file path for init: write the once-only
	// gateway key to this file (0600) instead of stdout.
	keyFile string
	// configure flags.
	providerID string
	apiKey     string
	apiKeyEnv  string
	// confirm is the --confirm phrase for destructive CLI actions
	// (db import takes "IMPORT"); without the flag the command
	// prompts interactively for the phrase instead.
	confirm string
	// release verify flags (CHECKLIST 261-262): the global parser
	// consumes every --flag before the subcommand sees the args, so
	// a subcommand flag only works when registered here — the
	// provider add-catalog flags above are the precedent.
	releaseDir                string
	releaseRepo               string
	releaseKey                string
	releaseImage              string
	releaseSkipImage          bool
	releaseSkipAttestation    bool
	releaseRequireAttestation bool
	releaseDownloadBaseURL    string
	// catalog audit flags (CHECKLIST 271-272): same precedent —
	// `catalog audit --out-dir` used to be rejected as "unknown
	// flag" because the handler parsed its own flags after the
	// global parser had already refused them.
	catalogOutDir           string
	catalogWebJSON          string
	catalogWebJSONSet       bool // --web-json= (empty) disables the dashboard JSON write
	catalogModelsDevURL     string
	catalogSkipReachability bool
	catalogConcurrency      int // 0 = default (16)
	catalogProbeTimeoutSec  int // 0 = default (15)
	catalogTriageSummary    string
	catalogTriageSummarySet bool // --triage-summary= (empty) disables embedding
	// eval-feedback flags (CHECKLIST 271-272): same precedent.
	evalFeedbackDB string
	evalCutoff     time.Time // zero = default 70/30 split
	// lifecycle flags (status/stop/restart/logs/health/update):
	// same precedent — the global parser consumes every --flag
	// before the subcommand sees the args.
	logsLines    int
	logsLinesSet bool // --lines= (0) means the whole log file
	logsFollow   bool
	updateTag    string
	updateCheck  bool
	updateYes    bool
	// setup flags.
	dryRun bool
	revert bool
}

// parseArgs extracts every registered --flag (in "--flag value"
// or "--flag=value" form, before or after the subcommand) and
// returns the remaining positional arguments. The first positional
// is the subcommand; when there is none, "serve" is the default.
// A flag the parser does not know is an error here — subcommand
// flags must be registered in this function (the provider
// add-catalog flags are the precedent), because the subcommand
// handlers never see them otherwise.
func parseArgs(args []string) (options, []string, error) {
	var opts options
	var positional []string
	afterDashDash := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		// "--" ends flag parsing: everything after it is a
		// positional (run <cli> extra args are passed through
		// to the launched CLI verbatim).
		if afterDashDash {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			afterDashDash = true
			continue
		}
		switch {
		case a == "--config":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--config requires a value")
			}
			i++
			opts.configPath = args[i]
		case strings.HasPrefix(a, "--config="):
			opts.configPath = strings.TrimPrefix(a, "--config=")
		case a == "--port":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--port requires a value")
			}
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil {
				return opts, nil, fmt.Errorf("--port must be a number: %w", err)
			}
			opts.port = p
		case strings.HasPrefix(a, "--port="):
			p, err := strconv.Atoi(strings.TrimPrefix(a, "--port="))
			if err != nil {
				return opts, nil, fmt.Errorf("--port must be a number: %w", err)
			}
			opts.port = p
		case a == "--listen":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--listen requires a value")
			}
			i++
			opts.listen = args[i]
		case strings.HasPrefix(a, "--listen="):
			opts.listen = strings.TrimPrefix(a, "--listen=")
		case a == "--model":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--model requires a value")
			}
			i++
			opts.model = args[i]
		case strings.HasPrefix(a, "--model="):
			opts.model = strings.TrimPrefix(a, "--model=")
		case a == "--endpoint":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--endpoint requires a value")
			}
			i++
			opts.endpoint = args[i]
		case strings.HasPrefix(a, "--endpoint="):
			opts.endpoint = strings.TrimPrefix(a, "--endpoint=")
		case a == "--variant":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--variant requires a value")
			}
			i++
			opts.variant = args[i]
		case strings.HasPrefix(a, "--variant="):
			opts.variant = strings.TrimPrefix(a, "--variant=")
		case a == "--base-url":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--base-url requires a value")
			}
			i++
			opts.baseURL = args[i]
		case strings.HasPrefix(a, "--base-url="):
			opts.baseURL = strings.TrimPrefix(a, "--base-url=")
		case a == "--name":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--name requires a value")
			}
			i++
			opts.name = args[i]
		case strings.HasPrefix(a, "--name="):
			opts.name = strings.TrimPrefix(a, "--name=")
		case a == "--provider":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--provider requires a value")
			}
			i++
			opts.providerID = args[i]
		case strings.HasPrefix(a, "--provider="):
			opts.providerID = strings.TrimPrefix(a, "--provider=")
		case a == "--api-key":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--api-key requires a value")
			}
			i++
			opts.apiKey = args[i]
		case strings.HasPrefix(a, "--api-key="):
			opts.apiKey = strings.TrimPrefix(a, "--api-key=")
		case a == "--api-key-env":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--api-key-env requires a value")
			}
			i++
			opts.apiKeyEnv = args[i]
		case strings.HasPrefix(a, "--api-key-env="):
			opts.apiKeyEnv = strings.TrimPrefix(a, "--api-key-env=")
		case a == "--confirm":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--confirm requires a value")
			}
			i++
			opts.confirm = args[i]
		case strings.HasPrefix(a, "--confirm="):
			opts.confirm = strings.TrimPrefix(a, "--confirm=")
		case a == "--key-file":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--key-file requires a value")
			}
			i++
			opts.keyFile = args[i]
		case strings.HasPrefix(a, "--key-file="):
			opts.keyFile = strings.TrimPrefix(a, "--key-file=")
		case a == "--dir":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--dir requires a value")
			}
			i++
			opts.releaseDir = args[i]
		case strings.HasPrefix(a, "--dir="):
			opts.releaseDir = strings.TrimPrefix(a, "--dir=")
		case a == "--repo":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--repo requires a value")
			}
			i++
			opts.releaseRepo = args[i]
		case strings.HasPrefix(a, "--repo="):
			opts.releaseRepo = strings.TrimPrefix(a, "--repo=")
		case a == "--key":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--key requires a value")
			}
			i++
			opts.releaseKey = args[i]
		case strings.HasPrefix(a, "--key="):
			opts.releaseKey = strings.TrimPrefix(a, "--key=")
		case a == "--image":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--image requires a value")
			}
			i++
			opts.releaseImage = args[i]
		case strings.HasPrefix(a, "--image="):
			opts.releaseImage = strings.TrimPrefix(a, "--image=")
		case a == "--skip-image":
			opts.releaseSkipImage = true
		case strings.HasPrefix(a, "--skip-image="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--skip-image="))
			if err != nil {
				return opts, nil, fmt.Errorf("--skip-image must be true or false: %w", err)
			}
			opts.releaseSkipImage = v
		case a == "--skip-attestation":
			opts.releaseSkipAttestation = true
		case strings.HasPrefix(a, "--skip-attestation="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--skip-attestation="))
			if err != nil {
				return opts, nil, fmt.Errorf("--skip-attestation must be true or false: %w", err)
			}
			opts.releaseSkipAttestation = v
		case a == "--require-attestation":
			opts.releaseRequireAttestation = true
		case strings.HasPrefix(a, "--require-attestation="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--require-attestation="))
			if err != nil {
				return opts, nil, fmt.Errorf("--require-attestation must be true or false: %w", err)
			}
			opts.releaseRequireAttestation = v
		case a == "--download-base-url":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--download-base-url requires a value")
			}
			i++
			opts.releaseDownloadBaseURL = args[i]
		case strings.HasPrefix(a, "--download-base-url="):
			opts.releaseDownloadBaseURL = strings.TrimPrefix(a, "--download-base-url=")
		case a == "--pprof":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				opts.pprofAddr = args[i]
			} else {
				opts.pprofAddr = "127.0.0.1:6060"
			}
		case strings.HasPrefix(a, "--pprof="):
			val := strings.TrimPrefix(a, "--pprof=")
			if val == "" || val == "true" {
				val = "127.0.0.1:6060"
			}
			opts.pprofAddr = val
		case a == "--low-end":
			opts.lowEnd = true
		case strings.HasPrefix(a, "--low-end="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--low-end="))
			if err != nil {
				return opts, nil, fmt.Errorf("--low-end must be true or false: %w", err)
			}
			opts.lowEnd = v
		case a == "--headless":
			opts.headless = true
		case strings.HasPrefix(a, "--headless="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--headless="))
			if err != nil {
				return opts, nil, fmt.Errorf("--headless must be true or false: %w", err)
			}
			opts.headless = v
		case a == "--wizard":
			opts.wizard = true
		case strings.HasPrefix(a, "--wizard="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--wizard="))
			if err != nil {
				return opts, nil, fmt.Errorf("--wizard must be true or false: %w", err)
			}
			opts.wizard = v
		case a == "--out-dir":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--out-dir requires a value")
			}
			i++
			if args[i] == "" {
				return opts, nil, fmt.Errorf("--out-dir requires a non-empty directory")
			}
			opts.catalogOutDir = args[i]
		case strings.HasPrefix(a, "--out-dir="):
			v := strings.TrimPrefix(a, "--out-dir=")
			if v == "" {
				return opts, nil, fmt.Errorf("--out-dir requires a non-empty directory")
			}
			opts.catalogOutDir = v
		case a == "--web-json":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--web-json requires a value")
			}
			i++
			opts.catalogWebJSON = args[i]
			opts.catalogWebJSONSet = true
		case strings.HasPrefix(a, "--web-json="):
			opts.catalogWebJSON = strings.TrimPrefix(a, "--web-json=")
			opts.catalogWebJSONSet = true
		case a == "--models-dev-url":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--models-dev-url requires a value")
			}
			i++
			if args[i] == "" {
				return opts, nil, fmt.Errorf("--models-dev-url requires a non-empty URL")
			}
			opts.catalogModelsDevURL = args[i]
		case strings.HasPrefix(a, "--models-dev-url="):
			v := strings.TrimPrefix(a, "--models-dev-url=")
			if v == "" {
				return opts, nil, fmt.Errorf("--models-dev-url requires a non-empty URL")
			}
			opts.catalogModelsDevURL = v
		case a == "--skip-reachability":
			opts.catalogSkipReachability = true
		case strings.HasPrefix(a, "--skip-reachability="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--skip-reachability="))
			if err != nil {
				return opts, nil, fmt.Errorf("--skip-reachability must be true or false: %w", err)
			}
			opts.catalogSkipReachability = v
		case a == "--concurrency":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--concurrency requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return opts, nil, fmt.Errorf("--concurrency must be a positive integer")
			}
			opts.catalogConcurrency = n
		case strings.HasPrefix(a, "--concurrency="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--concurrency="))
			if err != nil || n < 1 {
				return opts, nil, fmt.Errorf("--concurrency must be a positive integer")
			}
			opts.catalogConcurrency = n
		case a == "--probe-timeout":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--probe-timeout requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				return opts, nil, fmt.Errorf("--probe-timeout must be positive seconds")
			}
			opts.catalogProbeTimeoutSec = n
		case strings.HasPrefix(a, "--probe-timeout="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--probe-timeout="))
			if err != nil || n < 1 {
				return opts, nil, fmt.Errorf("--probe-timeout must be positive seconds")
			}
			opts.catalogProbeTimeoutSec = n
		case a == "--triage-summary":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--triage-summary requires a value")
			}
			i++
			opts.catalogTriageSummary = args[i]
			opts.catalogTriageSummarySet = true
		case strings.HasPrefix(a, "--triage-summary="):
			opts.catalogTriageSummary = strings.TrimPrefix(a, "--triage-summary=")
			opts.catalogTriageSummarySet = true
		case a == "--feedback-db":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--feedback-db requires a value")
			}
			i++
			opts.evalFeedbackDB = args[i]
		case strings.HasPrefix(a, "--feedback-db="):
			opts.evalFeedbackDB = strings.TrimPrefix(a, "--feedback-db=")
		case a == "--cutoff":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--cutoff requires a value")
			}
			i++
			t, err := time.Parse(time.RFC3339, args[i])
			if err != nil {
				return opts, nil, fmt.Errorf("--cutoff must be RFC3339: %w", err)
			}
			opts.evalCutoff = t
		case strings.HasPrefix(a, "--cutoff="):
			t, err := time.Parse(time.RFC3339, strings.TrimPrefix(a, "--cutoff="))
			if err != nil {
				return opts, nil, fmt.Errorf("--cutoff must be RFC3339: %w", err)
			}
			opts.evalCutoff = t
		case a == "--lines":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--lines requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				return opts, nil, fmt.Errorf("--lines must be a non-negative integer")
			}
			opts.logsLines = n
			opts.logsLinesSet = true
		case strings.HasPrefix(a, "--lines="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--lines="))
			if err != nil || n < 0 {
				return opts, nil, fmt.Errorf("--lines must be a non-negative integer")
			}
			opts.logsLines = n
			opts.logsLinesSet = true
		case a == "--follow":
			opts.logsFollow = true
		case strings.HasPrefix(a, "--follow="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--follow="))
			if err != nil {
				return opts, nil, fmt.Errorf("--follow must be true or false: %w", err)
			}
			opts.logsFollow = v
		case a == "--tag":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("--tag requires a value")
			}
			i++
			opts.updateTag = args[i]
		case strings.HasPrefix(a, "--tag="):
			opts.updateTag = strings.TrimPrefix(a, "--tag=")
		case a == "--check":
			opts.updateCheck = true
		case strings.HasPrefix(a, "--check="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--check="))
			if err != nil {
				return opts, nil, fmt.Errorf("--check must be true or false: %w", err)
			}
			opts.updateCheck = v
		case a == "--yes":
			opts.updateYes = true
		case strings.HasPrefix(a, "--yes="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--yes="))
			if err != nil {
				return opts, nil, fmt.Errorf("--yes must be true or false: %w", err)
			}
			opts.updateYes = v
		case a == "--dry-run":
			opts.dryRun = true
		case strings.HasPrefix(a, "--dry-run="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--dry-run="))
			if err != nil {
				return opts, nil, fmt.Errorf("--dry-run must be true or false: %w", err)
			}
			opts.dryRun = v
		case a == "--revert" || a == "--rollback":
			opts.revert = true
		case strings.HasPrefix(a, "--revert="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--revert="))
			if err != nil {
				return opts, nil, fmt.Errorf("--revert must be true or false: %w", err)
			}
			opts.revert = v
		case a == "-h" || a == "--help" || a == "help":
			positional = append(positional, "help")
		case a == "-v" || a == "--version":
			positional = append(positional, "version")
		case a == "--json":
			opts.jsonOut = true
		case strings.HasPrefix(a, "-"):
			return opts, nil, fmt.Errorf("unknown flag %q", a)
		default:
			positional = append(positional, a)
		}
	}
	return opts, positional, nil
}

// resolveConfigPath returns the effective config path: the --config flag
// when given, otherwise the default (~/.config/codvian-router/config.json).
func resolveConfigPath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	return config.DefaultConfigPath()
}

func run(args []string) int {
	opts, positional, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		usage(os.Stderr)
		return 2
	}
	cmd := "serve"
	if len(positional) > 0 {
		cmd = positional[0]
	}
	switch cmd {
	case "serve":
		return runServe(opts)
	case "init":
		if opts.wizard {
			return runWizard(opts)
		}
		return runInit(opts)
	case "wizard":
		return runWizard(opts)
	case "run":
		return runRun(opts, positional[1:])
	case "keygen":
		return runKeygen(opts)
	case "eval":
		return runEval(opts)
	case "eval-feedback":
		return runEvalFeedback(opts, positional[1:])
	case "validate":
		return runValidate(opts)
	case "tray":
		return runTray(opts)
	case "doctor":
		return runDoctor(opts)
	case "configure":
		return runConfigure(opts)
	case "setup":
		return runSetup(opts, positional[1:])
	case "mcp":
		return runMCP(opts)
	case "provider":
		return runProvider(opts, positional[1:])
	case "catalog":
		return runCatalog(opts, positional[1:])
	case "verify-release":
		return runVerifyRelease(positional)
	case "release":
		return runRelease(opts, positional[1:])
	case "db":
		return runDB(opts, positional[1:])
	case "status":
		return runStatus(opts, positional[1:])
	case "stop":
		return runStop(opts, positional[1:])
	case "restart":
		return runRestart(opts, positional[1:])
	case "logs":
		return runLogs(opts, positional[1:])
	case "health":
		return runHealth(opts, positional[1:])
	case "update":
		return runUpdate(opts, positional[1:])
	case "clients":
		return runClients(opts, positional[1:])
	case "help":
		usage(os.Stdout)
		return 0
	case "version":
		fmt.Printf("codvian-router v%s\n", Version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown subcommand %q\n", cmd)
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `codvian-router v%s — local LLM router (Phase 1 MVP)

Usage:
  codvian-router [--config PATH] [--port PORT] serve    start the router (default)
  codvian-router [--config PATH] [--port PORT] serve --headless
                                                start the router as a pure API process: no dashboard
                                                server, no browser/tray behaviour; refuses to start
                                                without a gateway key configured
  codvian-router [--config PATH] init [--key-file PATH]  create config + a gateway key (--key-file writes the once-only key to a 0600 file instead of stdout)
  codvian-router [--config PATH] init --wizard   first-run walkthrough: provider → key → model → real test call → done (every step skippable)
  codvian-router [--config PATH] run <cli> --model MODEL [-- args...]
                                                launch a local CLI (e.g. codex) with the router as its
                                                base URL and a fresh gateway key; starts a headless
                                                gateway when none is running
  codvian-router [--config PATH] wizard          same as init --wizard
  codvian-router keygen                               print a fresh gateway key (not stored)
  codvian-router [--config PATH] [--model MODEL] eval  smoke-eval the live engine
  codvian-router [--config PATH] validate             validate the config file
  codvian-router [--config PATH] [--port PORT] tray   supervise the gateway headless
  codvian-router [--config PATH] [--port PORT] status  show the supervised gateway: pid from <config-dir>/router.pid + GET /healthz (exit 0 when running)
  codvian-router [--config PATH] [--port PORT] stop    stop the supervised gateway (signals the tray supervisor first, so the child stops gracefully)
  codvian-router [--config PATH] [--port PORT] restart stop, then start a detached supervised gateway; its output goes to <config-dir>/router.log
  codvian-router [--config PATH] logs [--lines N] [--follow]
                                                show the gateway log (<config-dir>/router.log — written only when the gateway was started by restart/update; a gateway started by hand logs to its terminal)
  codvian-router [--config PATH] [--port PORT] health probe GET /healthz (exit 0 on 200)
  codvian-router clients [--json]        print the client compatibility matrix (dialect, endpoint, credential, evidence)
  codvian-router [--config PATH] update [--tag TAG] [--check] [--yes]
                                                self-update from the latest GitHub release: cosign signature (when cosign is installed) + SHA-256 against checksums.txt, then an atomic binary swap; stops and restarts the supervised gateway when it is running
  codvian-router [--config PATH] doctor              check config, keys, providers, ports, dashboard
  codvian-router [--config PATH] [--port PORT] configure
                                                first-run setup (interactive, or --provider ID --api-key KEY)
  codvian-router [--config PATH] setup [agent] [--model M] [--endpoint E] [--dry-run] [--revert]
                                                one-command guided setup for AI coding agents (default: codevn; also claude, continue)
  codvian-router [--config PATH] mcp                MCP server on stdio (tools: router_status, provider_list, model_list)
  codvian-router [--config PATH] provider list        list configured providers
  codvian-router [--config PATH] provider add <base-url> <api-key> [--name NAME]
  codvian-router [--config PATH] provider add-catalog <catalog-id> <api-key> [--endpoint E] [--variant V] [--base-url URL]
  codvian-router [--config PATH] provider login <catalog-id>
                                                account login via the provider's documented OAuth flow (supported: openrouter — PKCE, headless code paste; the exchanged API key is live-tested and stored encrypted)
  codvian-router catalog audit [--out-dir DIR] [--web-json PATH] [--models-dev-url URL] [--skip-reachability] [--concurrency N] [--probe-timeout SEC] [--triage-summary PATH]
                                                audit the provider catalog against live models.dev + base-URL/website reachability; reports only, never edits the catalog (docs/catalog-audit-<date>.json/.md + web/catalog-audit.json)
  codvian-router [--config PATH] provider remove <provider-id>  delete a configured provider (persisted)
  codvian-router verify-release [DIR]              verify the files in DIR against its checksums.txt (integrity only — the cosign signature is checked by scripts/verify-release.sh, see docs/RELEASE-SIGNING.md)
  codvian-router release verify <tag> [--dir DIR] [--key PUBKEY] [--skip-image] [--skip-attestation]
                                                verify a published release (download + cosign signature over checksums.txt and SBOMs + SHA-256 + SBOM shape + provenance attestation + Docker image signature); exits non-zero naming the artifact on any failure, see docs/RELEASE-CHECKLIST.md
  codvian-router [--config PATH] db import <export-file> [--confirm IMPORT]
                                                merge a DB export file's usage history into the ledger (never resets it)
  codvian-router [--config PATH] eval-feedback [--feedback-db PATH] [--cutoff RFC3339]
                                                A/B: calibrated feedback heuristic vs no-feedback baseline on recorded local traffic
  codvian-router help                                 show this help
  codvian-router version | --version                  print the version and exit

Flags:
  --config PATH   config file (default ~/.config/codvian-router/config.json)
  --port PORT     listen port for serve (binds 127.0.0.1 only)
  --listen ADDR   explicit listen address for serve, host:port (127.0.0.1, ::1, or
                  0.0.0.0). 0.0.0.0 binds all interfaces — trusted networks only.
                  A runtime override; it is never written to the config file.
  --model MODEL   model for eval
  --key-file PATH write init's once-only gateway key to PATH (0600, atomic, refuses to overwrite) instead of printing it
  --endpoint E    catalog surface for provider add-catalog (default: the entry's default)
  --variant V     catalog surface variant for provider add-catalog (e.g. alt-header, api-key)
  --base-url URL  base-URL override for provider add-catalog (proxy, mirror, per-resource URL, mock)
  --name NAME     display name for provider add
  --provider ID   catalog provider id for configure (with --api-key or --api-key-env)
  --confirm PHRASE  typed-confirmation phrase for a destructive CLI action
                  (db import takes IMPORT; without the flag the command prompts)
  --api-key KEY   provider API key for configure (prefer --api-key-env: a key on
                  the command line lands in shell history)
  --api-key-env VAR  read the configure provider key from environment variable VAR
  --low-end       low-end GC profile for serve: GOGC=off + auto-detected memory
                  limit (same as CODVIAN_ROUTER_LOWEND=1; see docs/gc-tuning.md).
                  Default serve keeps Go's normal GC and only adds the limit.
                  CODVIAN_ROUTER_MEMORY_LIMIT_MB overrides the limit in MiB.
  --headless      serve as a pure API process: no dashboard server, no
                  browser/tray behaviour. Refuses to start without a
                  gateway key configured (auth-on-from-first-boot).
  --wizard        init --wizard: first-run walkthrough (provider → key →
                  model → real test call → done; every step skippable).
  --out-dir DIR   catalog audit: report directory (default docs)
  --web-json PATH catalog audit: dashboard JSON path (default web/catalog-audit.json;
                  empty disables the write)
  --models-dev-url URL  catalog audit: models.dev catalog URL (default https://models.dev/api.json)
  --skip-reachability  catalog audit: skip the base-URL / website probe step (offline runs)
  --concurrency N catalog audit: probe worker count (default 16)
  --probe-timeout SEC  catalog audit: per-attempt probe timeout in seconds (default 15)
  --triage-summary PATH  catalog audit: triage record to embed (default
                  <out-dir>/catalog-triage-<date>.json when it exists; empty disables)
  --feedback-db PATH  eval-feedback: feedback store to A/B (default <data-dir>/feedback.db)
  --cutoff RFC3339  eval-feedback: train/test split time (default: the 70/30 split by count)
  --lines N       logs: show the last N lines (default 50; 0 = the whole file)
  --follow        logs: keep printing lines as they are appended (Ctrl-C to stop)
  --tag TAG       update: install this release tag instead of the latest
  --check         update: only report whether a newer release exists
  --yes           update: update without the interactive confirmation prompt

  run <cli> reads the gateway key from CODVIAN_ROUTER_API_KEY only when a
  gateway is already running (a stored key's raw value cannot be
  recovered from the config — only its hash is stored). When no gateway
  is running, run starts its own headless gateway with a fresh ephemeral
  key and revokes it when the CLI exits.

The router binds localhost only and auth is always ON: clients must send
a gateway key as "Authorization: Bearer <key>".
`, Version)
}

// configProviderStore adapts the config file's Providers list to a
// provider.Store, so the config file stays the single source of truth
// for providers: Registry.Load reads from it, and any later Registry
// mutation (Add/Remove/SetEnabled) persists back through it.
type configProviderStore struct {
	cfg  *config.Config
	path string
}

func (s *configProviderStore) Load() ([]provider.ProviderConfig, error) {
	out := make([]provider.ProviderConfig, len(s.cfg.Providers))
	copy(out, s.cfg.Providers)
	return out, nil
}

func (s *configProviderStore) Save(cfgs []provider.ProviderConfig) error {
	s.cfg.Providers = append([]provider.ProviderConfig(nil), cfgs...)
	return s.cfg.Save(s.path)
}

// ledgerAdapter adapts the ledger to the router's small Ledger
// interface (RecordAttempt). Attempts are recorded through the
// AsyncWriter (ledger.AsyncWriter), which batches rows off the hot
// path — one transaction per batch instead of one synchronous SQLite
// transaction (with fsync) per request; see internal/ledger/async.go
// for the tradeoff (≤50 ms durability window on a crash, graceful
// shutdown flushes). The batch writer keeps the per-key budget spend
// in sync exactly like the synchronous RecordUsage it replaces.
//
// The router passes the full token split reported by the upstream.
// The split is known only when the upstream actually reported
// prompt/completion tokens; a failed or streaming attempt arrives as
// provider.Usage{} (unknown), and an upstream that reports only a
// total arrives with prompt/completion 0 — both are recorded with
// SplitKnown false, total_tokens as reported, and cost 0, never a
// guessed split. Cost comes from the adapter's prices (the operator's
// price book, per-token): a model with no configured price records
// $0 — a lower bound, never an invented cost. GatewayKeyID is empty:
// the engine does not know which gateway key a request arrived
// under. A record failure is logged to stderr and never fails the
// request.
type ledgerAdapter struct {
	w *ledger.AsyncWriter
}

// RecordAttemptForKey records one attempt against the gateway key
// the request arrived under (router.KeyedLedger): per-key
// accounting needs the key on the row, and the engine is the only
// place that sees every attempt, including failed fallback
// attempts (which record cost $0 unless the upstream billed the
// refusal — the reported usage rides in, see failedUsage — and
// consume no quota).
func (a *ledgerAdapter) RecordAttemptForKey(keyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	if a == nil || a.w == nil {
		return
	}
	a.w.Record(ledger.Usage{
		GatewayKeyID: keyID,
		Provider:     providerName,
		Model:        model,
		// Endpoint rides along with the usage: the ledger stores
		// it on the row so the dashboard can label an
		// embeddings/rerank call ("" = chat completions).
		Endpoint: string(usage.Endpoint),
		// Cost/ServiceTier/AutoVariant (B4-11/17, Stage 1):
		// the provider layer already populates them on the
		// wire usage (Cost from xAI cost_in_usd_ticks /
		// OpenRouter usage.cost; ServiceTier from the
		// request; AutoVariant from the auto-routing variant
		// actually used) — the adapter forwards them so the
		// ledger records the gateway-reported cost and the
		// routing variant instead of dropping them. They are
		// dormant (nil/empty) until the engine starts setting
		// them on the live path — no invented values here.
		// Attribution (SessionID/ProjectID/ClientID) is NOT
		// mapped: the engine has no request-attribution
		// concept yet, so there is no source to map from.
		// The ledger fields exist (ledger.Usage + With-
		// Attribution) and the engine-side plumbing is a
		// router-lane follow-up, not a main.go one.
		Cost:             usage.Cost,
		ServiceTier:      usage.ServiceTier,
		AutoVariant:      usage.AutoVariant,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		// Cached tokens ride along with the split: the ledger
		// stores them only alongside a known split (see
		// normalizeUsage) — streaming and failed attempts arrive
		// as Usage{} and record none. Reasoning and cache-write
		// tokens ride the same rule.
		CachedTokens:     usage.CachedTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		// M6.9b: the cache-write tier split rides along with the
		// total — 0/0 when the upstream reported only the legacy
		// total (tier unknown), never a guessed split.
		CacheWriteTokens5m: usage.CacheWriteTokens5m,
		CacheWriteTokens1h: usage.CacheWriteTokens1h,
		// Compression rides along with the usage (CHECKLIST
		// 183-184): the router's honest local counts for the
		// compressed prompt — prompt tokens before compression
		// and tokens saved — so the ledger can total them.
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		// A local-estimate row (upstream reported no usage, the
		// router counted the prompt locally) is NOT a known
		// split even though PromptTokens is non-zero: the
		// ledger stores it with local_estimate=1, split
		// unknown, cost 0. The split is known only when the
		// upstream reported at least one side of it. A zero/zero
		// split with a total (or no usage at all, e.g. a failed
		// attempt) is total-only / unknown, not a 0/0 split.
		LocalEstimate: usage.LocalEstimate,
		// The local estimate of the prompt as sent rides along
		// with the reported split (CHECKLIST 229): the ledger
		// stores it on the row so estimate-vs-reported drift is
		// computable from live data. 0 when the router never
		// computed one for this attempt.
		LocalEstimatePromptTokens: usage.LocalEstimatePrompt,
		SplitKnown:                !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
		OK:                        ok,
		LatencyMS:                 latencyMs,
	})
}

// RecordAttemptWithProviderKey records one attempt against the
// gateway key the request arrived under AND the provider-side key
// that served it (router.ProviderKeyLedger, CHECKLIST 175): the
// ledger row carries both IDs, so per-key spend is attributed to
// the key that actually served — never pooled into one number for
// the provider. A legacy single-key attempt arrives with
// providerKeyID "" and is stored unattributed to a pool key.
func (a *ledgerAdapter) RecordAttemptWithProviderKey(gatewayKeyID, providerName, providerKeyID, model string, ok bool, latencyMs int64, usage provider.Usage) {
	if a == nil || a.w == nil {
		return
	}
	a.w.Record(ledger.Usage{
		GatewayKeyID:  gatewayKeyID,
		ProviderKeyID: providerKeyID,
		Provider:      providerName,
		Model:         model,
		// Endpoint rides along with the usage: the ledger stores
		// it on the row so the dashboard can label an
		// embeddings/rerank call ("" = chat completions).
		Endpoint: string(usage.Endpoint),
		// Cost/ServiceTier/AutoVariant (B4-11/17, Stage 1):
		// see RecordAttemptForKey — the wire usage already
		// carries them; the adapter forwards them so the
		// ledger records the gateway-reported cost and the
		// routing variant instead of dropping them.
		Cost:             usage.Cost,
		ServiceTier:      usage.ServiceTier,
		AutoVariant:      usage.AutoVariant,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		// Cached tokens ride along with the split: the ledger
		// stores them only alongside a known split (see
		// normalizeUsage) — streaming and failed attempts arrive
		// as Usage{} and record none. Reasoning and cache-write
		// tokens ride the same rule.
		CachedTokens:     usage.CachedTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		// M6.9b: the cache-write tier split rides along with the
		// total — 0/0 when the upstream reported only the legacy
		// total (tier unknown), never a guessed split.
		CacheWriteTokens5m: usage.CacheWriteTokens5m,
		CacheWriteTokens1h: usage.CacheWriteTokens1h,
		// Compression rides along with the usage (CHECKLIST
		// 183-184): the router's honest local counts for the
		// compressed prompt — prompt tokens before compression
		// and tokens saved — so the ledger can total them.
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		// A local-estimate row (upstream reported no usage, the
		// router counted the prompt locally) is NOT a known
		// split even though PromptTokens is non-zero: the
		// ledger stores it with local_estimate=1, split
		// unknown, cost 0. The split is known only when the
		// upstream reported at least one side of it. A zero/zero
		// split with a total (or no usage at all, e.g. a failed
		// attempt) is total-only / unknown, not a 0/0 split.
		LocalEstimate: usage.LocalEstimate,
		// The local estimate of the prompt as sent rides along
		// with the reported split (CHECKLIST 229): the ledger
		// stores it on the row so estimate-vs-reported drift is
		// computable from live data. 0 when the router never
		// computed one for this attempt.
		LocalEstimatePromptTokens: usage.LocalEstimatePrompt,
		SplitKnown:                !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
		OK:                        ok,
		LatencyMS:                 latencyMs,
	})
}

func (a *ledgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	a.RecordAttemptForKey("", providerName, model, ok, latencyMs, usage)
}

// RecordCancelledAttempt records a hedge loser (router.
// CancelledLedger, PARITY row B4-15): an attempt that was started
// and then cancelled because the other hedge attempt won. The row
// is stored as cancelled (ledger.Usage.AsCancelled), never as a
// failed attempt, so it is excluded from failure counts while the
// attempt stays visible with its latency. A cancelled attempt
// reports no usage, so the row carries no tokens and no cost.
func (a *ledgerAdapter) RecordCancelledAttempt(keyID, providerName, providerKeyID, model string, latencyMs int64) {
	if a == nil || a.w == nil {
		return
	}
	a.w.Record(ledger.Usage{
		GatewayKeyID:  keyID,
		ProviderKeyID: providerKeyID,
		Provider:      providerName,
		Model:         model,
		LatencyMS:     latencyMs,
	}.AsCancelled())
}

// RecordShared records one waiter served from a shared single-flight
// call (router.SharedLedger): a row marked shared with zero tokens
// and zero cost — the leader's attempt is the only one billed.
// latencyMs is how long the waiter waited for the leader's flight.
func (a *ledgerAdapter) RecordShared(providerName, model string, ok bool, latencyMs int64) {
	if a == nil || a.w == nil {
		return
	}
	a.w.RecordShared(providerName, model, ok, latencyMs)
}

// stack is the fully wired router: config, vault, ledger, registry,
// clients, routing engine (raw and degrade-wrapped), health checker
// and metrics recorder. openStack builds it; runServe serves it,
// runEval evaluates the raw engine against it.
type stack struct {
	cfg      *config.Config
	path     string
	listen   string // effective listen address (--listen wins, else config)
	reg      *provider.Registry
	clients  map[string]provider.Client
	engine   *router.Engine // raw engine: failover, hedging, ledger
	serving  server.Engine  // degrade-wrapped engine: what serve() serves
	exact    cache.ExactStore
	semantic cache.SemanticStore
	// persistExact / persistSemantic are the persistent wrappers
	// behind exact / semantic when persistence is configured
	// (nil = memory-only). Kept so the shutdown path can run a
	// final Sync and close the backing store (see
	// syncCachesOnShutdown); the serving path itself only ever
	// touches the interfaces above.
	persistExact    *cache.PersistentCache
	persistSemantic semanticPersistHandle
	// persistReasoning is the reasoning replay cache when its
	// SQLite tier is attached (G13, DataDir/reasoning_cache.db;
	// nil = memory-only). Kept so the shutdown path can run a
	// final Sync and close the file, like the layers above.
	persistReasoning *cache.ReasoningCache
	autoRouter       *autorouter.Router // nil = no auto-routing (no models configured)
	priceBook        *config.PriceBook  // nil = no prices configured; every model unknown
	// modelsDev is the opt-in models.dev sync (CHECKLIST 179):
	// nil = the sync is off and the router never contacts
	// models.dev. When set, it owns the synced price/window
	// state (models-dev-sync.json in the data dir).
	modelsDev *modelsync.Syncer
	// litellm is the opt-in LiteLLM sync (B4-03/12, Stage
	// 1): nil = the sync is off and the router never contacts
	// the LiteLLM cost map. When set, it owns its own synced
	// price/window state (litellm-sync.json in the data
	// dir) and its own "litellm" slice of the price book's
	// synced layer — it never wipes the models.dev slice.
	litellm *modelsync.Syncer
	// intel is the live model-intelligence store (B2-14): the
	// priority lookup the 16-factor scorer reads (user_override >
	// arena_elo > models_dev_tier). Never nil: built at startup
	// even when every sync is off, so the scorer always has a
	// store to read — empty means "no signal", never an invented
	// score.
	intel *modelsync.Intelligence
	// arenaSyncer is the opt-in Arena ELO syncer filling the
	// store's arena_elo source. Nil when the arena_elo block is
	// off: nothing fetches ELO.
	arenaSyncer *modelsync.ArenaSyncer
	// ledgerPrices is the ledger's per-token price map (the same
	// map the async writer charges with, including
	// provider-reported prices): the server's per-key budget gate
	// consults it alongside the price book.
	ledgerPrices ledger.Prices
	guardrails   *guardrails.Store // never nil: log redaction always on; blocking from config
	// plugins is the in-process plugin registry (CHECKLIST 201):
	// built from cfg.Plugins at startup; nil means no plugins
	// configured. The registry also backs the provider-adapter
	// hook (reg.SetHTTPAdapter), so adapter plugins rewrite the
	// outgoing request for their provider.
	plugins  *plugins.Registry
	checker  *health.Checker
	recorder *metrics.Recorder
	led      *ledger.Ledger
	asyncLed *ledger.AsyncWriter // batched off-hot-path writer over led; Close flushes first
	feedback *feedback.Store     // accept/edit/discard/regenerate signals; never stores content
	vault    *crypto.Vault
}

// effectiveListen returns the listen address for serve: --listen when
// given (an explicit opt-in to a non-loopback bind), otherwise the
// config address (already adjusted by --port). --listen is a runtime
// override only: it is never written back to the config file, so a
// later registry save cannot persist a 0.0.0.0 bind by accident.
//
// Only loopback hosts and 0.0.0.0 are accepted: the router has no TLS
// and no per-client auth beyond the gateway key, so binding an
// arbitrary LAN/WAN address by typo must fail loudly. 0.0.0.0 prints a
// warning: it is for Docker (network_mode: host is the alternative)
// and other trusted networks only.
func effectiveListen(cfg *config.Config, opts options) (string, error) {
	if opts.listen == "" {
		return cfg.Listen, nil
	}
	host, portStr, err := net.SplitHostPort(opts.listen)
	if err != nil {
		return "", fmt.Errorf("--listen must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("--listen has an invalid port %q", portStr)
	}
	switch host {
	case "127.0.0.1", "::1", "0.0.0.0":
	default:
		return "", fmt.Errorf("--listen host %q is not allowed (use 127.0.0.1, ::1, or 0.0.0.0)", host)
	}
	if host == "0.0.0.0" {
		fmt.Fprintln(os.Stderr, "codvian-router: WARNING: --listen 0.0.0.0 binds all interfaces — use only on a trusted network; gateway-key auth is still ON.")
	}
	return opts.listen, nil
}

// openStack loads (or creates) the config, applies --port/--listen,
// opens the ledger, unlocks provider keys through the Vault, builds
// the provider registry and the routing engine, and wires the Phase
// 2/3 packages: scoring (ModeBalanced, driven by health probes +
// quota hints), the health checker, graceful degradation (ON), and
// the metrics recorder. The caller owns st.led (defer Close).
//
// buildAutoRouter builds the local auto-router from the config's
// auto_router block. Tier models come from
// config.AutoRouter.TierModels (tier name -> concrete model IDs);
// when DefaultModel is empty it falls back to the first model of
// the first enabled provider, so a fresh install with providers
// still routes "auto/*" instead of failing. When neither exists
// the router has no model to resolve to and Resolve reports
// ok=false — an honest failure, never an invented model. The
// returned router is also handed to the server, so the dashboard's
// Auto-router section classifies with exactly the same router the
// request path uses.
//
// rankModels, when non-nil, reorders each tier's candidate models
// by real cost + measured latency/success + measured quality
// (see the ranker built in openStack) instead of list order, so
// auto/* tier ordering and the strategies agree on the same live
// signals.
func buildAutoRouter(cfg *config.Config, reg *provider.Registry, priceBook *config.PriceBook, rankModels autorouter.ModelRanker) *autorouter.Router {
	ac := cfg.AutoRouter
	tierModels := make(map[autorouter.Tier][]string, len(ac.TierModels))
	for tier, models := range ac.TierModels {
		if !autorouter.IsTier(tier) || len(models) == 0 {
			continue
		}
		cp := make([]string, len(models))
		copy(cp, models)
		tierModels[autorouter.Tier(tier)] = cp
	}
	defaultModel := ac.DefaultModel
	if defaultModel == "" && reg != nil {
		for _, p := range reg.List() {
			if !p.Enabled || len(p.Models) == 0 {
				continue
			}
			defaultModel = p.Models[0].ID
			break
		}
	}
	// Nothing to resolve to (no tier models, no default model, no
	// provider models, no intent tiers): return nil so the engine
	// leaves "auto/*" and "intent/*" unresolved and the server
	// reports auto-routing as off, instead of building a router
	// that can only fail.
	if len(tierModels) == 0 && defaultModel == "" && len(ac.IntentTiers) == 0 {
		return nil
	}
	// Intent tiers (items 259-260): the operator-defined
	// (model, effort) pair per tier. Config validation already
	// rejects unknown tiers, empty models and unknown efforts;
	// entries that fail here anyway (a direct caller that skipped
	// validation) are dropped, never turned into a mapping.
	intentTiers := make(map[autorouter.IntentTier]autorouter.IntentMapping, len(ac.IntentTiers))
	for name, m := range ac.IntentTiers {
		if !autorouter.IsIntentTier(name) || strings.TrimSpace(m.Model) == "" {
			continue
		}
		effort, err := autorouter.ParseEffort(m.Effort)
		if err != nil {
			continue
		}
		intentTiers[autorouter.IntentTier(name)] = autorouter.IntentMapping{Model: strings.TrimSpace(m.Model), Effort: effort}
	}
	// Auto Code mode (items 247–248): cheapest serving price per
	// model for the cost-per-solved-task scorer, and the pool
	// narrowing from config (allow/exclude lists, capability
	// requirements, operator-declared capabilities) plus the live
	// deprecated-model guardrail. The outcome evidence is wired
	// separately by the server (its OutcomeStore, fed from the
	// feedback POST handler), so a nil provider here simply
	// means "no evidence yet" and the mode ranks by price at the
	// documented neutral default.
	codePrices := codePricesFor(priceBook, reg)
	codePool := autorouter.CodePoolFilter{
		Allow:               ac.CodeAllow,
		Exclude:             ac.CodeExclude,
		RequireCapabilities: ac.CodeRequireCapabilities,
		Capabilities:        ac.CodeCapabilities,
		Guardrail:           codeGuardrailFor(reg),
	}
	// Live variant catalog (M3 Phase D, D2): the engine's auto/*
	// variant dispatch resolves against the registry + price book
	// through router.SetAutoCatalog — the static facts only
	// (provider, model, price, quota hint, tier-membership
	// category). The engine enriches each entry per request with
	// its own live latency/quality signals. One entry per
	// (provider, model) pair, enabled providers only, deprecated
	// models skipped, removed providers skipped (G19 Area 1,
	// CT-2). Category comes from tier membership
	// (coding / reasoning / chat); vision comes from the
	// models.dev sync (provider.Model.Vision, set only when the
	// feed's [modalities] table reports image input — see
	// internal/modelsync) and is false otherwise, never guessed
	// from the model id. Economic tier is classified from the
	// real price via scoring.ClassifyPriceTier — free at $0,
	// cheap at <= $1/M input, premium above (the reference router's
	// tierResolver thresholds, verified against the reference);
	// the input leg decides when known, else the blended price
	// as a proxy. Billing, overage hard-stop and
	// remaining-percent have NO live source in the registry, so
	// they stay unknown: auto/subscription's rung is therefore
	// always empty in production and fails closed, and
	// auto/thrifty's subscription/keyless rungs stay empty —
	// both honest limits, not data. A provider with no model
	// list (chat-only gateway) contributes no entries: its
	// models cannot be enumerated, never invented.
	ar := autorouter.New(autorouter.Config{
		Threshold:    ac.Threshold,
		Thresholds:   ac.Thresholds,
		DefaultTier:  autorouter.Tier(ac.DefaultTier),
		DefaultModel: defaultModel,
		TierModels:   tierModels,
		RankModels:   rankModels,
		CodePrices:   codePrices,
		CodePool:     codePool,
		IntentTiers:  intentTiers,
	})
	router.SetAutoCatalog(func() []autorouter.CatalogModel {
		if reg == nil {
			return nil
		}
		categoryOf := make(map[string]string)
		for _, tier := range autorouter.AllTiers {
			var cat string
			switch tier {
			case autorouter.TierCoding:
				cat = "coding"
			case autorouter.TierReasoning:
				cat = "reasoning"
			case autorouter.TierChat:
				cat = "chat"
			default:
				continue
			}
			for _, m := range ac.TierModels[string(tier)] {
				if _, ok := categoryOf[m]; !ok {
					categoryOf[m] = cat
				}
			}
		}
		var out []autorouter.CatalogModel
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			// Removed providers contribute no catalog entries
			// (G19 Area 1, CT-2).
			if _, ok := p.IsRemovedVariant(); ok {
				continue
			}
			for _, m := range p.Models {
				if m.Deprecated {
					continue
				}
				cm := autorouter.CatalogModel{Provider: p.ID, Model: m.ID, Category: categoryOf[m.ID], Vision: m.Vision}
				if cm.Category == "reasoning" {
					cm.Reasoning = true
				}
				if priceBook != nil {
					if blended, ok := priceBook.BlendedUSDPer1M(p.ID, m.ID); ok {
						cm.PriceUSDPerMTok = blended
						cm.HasPrice = true
						// Economic tier from the real price
						// (G19 Area 1): the input leg decides
						// when known, else the blended price
						// as a proxy. Unknown prices stay
						// unclassified (""), never guessed.
						cm.Tier = string(scoring.ClassifyPriceTier(m.InputPriceUSD*1e6, m.InputPriceUSD > 0, blended))
					}
				}
				if !cm.HasPrice && m.InputPriceUSD > 0 && m.OutputPriceUSD > 0 {
					cm.PriceUSDPerMTok = (m.InputPriceUSD + m.OutputPriceUSD) / 2 * 1e6
					cm.HasPrice = true
					if cm.Tier == "" {
						cm.Tier = string(scoring.ClassifyPriceTier(m.InputPriceUSD*1e6, true, cm.PriceUSDPerMTok))
					}
				}
				if p.QuotaHint != provider.QuotaUnknown && p.QuotaHint >= 0 && p.QuotaHint <= 1 {
					cm.QuotaHeadroom = p.QuotaHint
					cm.HasQuota = true
				}
				out = append(out, cm)
			}
		}
		return out
	})
	// Auto-routing ranker (S1-AUTO-WIRE, Stage 1): the
	// engine's balanced pick scores the live catalog with
	// the autorouter's ranker (the 16-factor scorer) instead
	// of the static pool order. Installed next to
	// SetAutoCatalog; nil-safe (RankModels returns nil for
	// a nil receiver, and the engine falls back to pool[0]
	// when no ranker is installed).
	router.SetAutoRanker(ar.RankModels())
	// LKGP global-last (M3 Phase D, D2X): auto/lkgp needs the most
	// recently recorded last-known-good model across all chains.
	// The server records successes into
	// strategy.DefaultLKGPRegistry keyed by requested model
	// (telemetry RecordSuccessFor), and Latest() reads the most
	// recently updated entry — only what was actually recorded,
	// never invented. "" when nothing was ever recorded, so
	// auto/lkgp errors honestly (D2's fail-closed path). The
	// engine cannot import strategy (import cycle), so the
	// registry reaches it through this supplier func, installed
	// next to SetAutoCatalog.
	router.SetAutoLastGood(func() string {
		model, ok := strategy.DefaultLKGPRegistry.Latest()
		if !ok {
			return ""
		}
		return model
	})
	return ar
}

// codePricesFor builds the auto/code mode's price function: the
// cheapest blended price for a model across every source that
// knows one — the price book (pair, synced models.dev, and
// model-level entries via CheapestBlendedUSDPer1M) plus the
// model's legacy per-token prices on provider entries (the same
// fallback signalsFn uses). A model with no price anywhere is
// unknown (false), never $0.
func codePricesFor(priceBook *config.PriceBook, reg *provider.Registry) autorouter.CodePriceFunc {
	return func(model string) (float64, bool) {
		best, known := 0.0, false
		if priceBook != nil {
			if v, ok := priceBook.CheapestBlendedUSDPer1M(model); ok {
				best, known = v, true
			}
		}
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				if m.ID != model || m.InputPriceUSD <= 0 || m.OutputPriceUSD <= 0 {
					continue
				}
				if v := (m.InputPriceUSD + m.OutputPriceUSD) / 2 * 1e6; !known || v < best {
					best, known = v, true
				}
			}
		}
		return best, known
	}
}

// codeGuardrailFor builds the auto/code pool guardrail from the
// live registry: a model is rejected only when every enabled
// provider offering it marks it deprecated. A model no listed
// provider offers (chat-only gateway, unlisted model) has
// nothing deprecating it and passes — pool membership already
// limits candidates to the tier's configured models.
func codeGuardrailFor(reg *provider.Registry) func(string) bool {
	return func(model string) bool {
		offered, live := false, false
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				if m.ID != model {
					continue
				}
				offered = true
				if !m.Deprecated {
					live = true
				}
			}
		}
		return !offered || live
	}
}

// Scoring signals, honestly: latency and success come from the
// engine's live attempt record first (real chat latency EWMA +
// measured success rate — the freshest signal, and the only one
// that reflects load), falling back to the health checker's last
// key probe (a single probe is a thin sample — OK is 1.0, a failed
// probe is 0.0, no probe yet is unknown); quota comes from the
// registry hint, preferring a probe-measured hint when one exists.
// Cost comes from the operator's price book for the requested model
// (config "prices" over the optional local price file), falling
// back to that model's legacy per-token prices on the provider
// entry; a model with no configured price is unknown (neutral,
// never cheapest). Quality is the measured error-free completion
// rate from real engine attempts only — never from the health
// probe, never a benchmark or claimed score.
// buildLedgerPrices resolves the ledger's per-token price map from
// the price book + the registry's provider-reported prices. The
// book's per-1M prices convert by /1e6; a model's legacy per-token
// prices on its provider entry fill gaps the book does not cover
// (book wins on conflict). Prices are keyed by (provider, model):
// a pair entry charges exactly that provider's serving of the
// model, and a model-level entry (empty Provider) is the fallback
// for every provider serving the model. A model with no price
// anywhere still records $0 — a lower bound, never an invented
// cost.
//
// Synced models.dev prices (CHECKLIST 179) are included the same
// way, except where the operator has a model-level price for the
// model: the ledger's pair key would otherwise beat the operator's
// fallback, and operator values always win. (A synced pair never
// coexists with an operator PAIR price — the sync drops it — so
// only the model-level case needs the guard here.)
func buildLedgerPrices(priceBook *config.PriceBook, reg *provider.Registry) ledger.Prices {
	prices := ledger.Prices{}
	if priceBook != nil {
		addPair := func(key config.PriceKey, p config.ModelPrice) {
			if p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
				return // half-known: no blended price, no ledger price
			}
			mp := ledger.ModelPrice{
				PromptPerToken:     *p.InputUSDPer1M / 1e6,
				CompletionPerToken: *p.OutputUSDPer1M / 1e6,
			}
			// M1.6: carry the cached-input rate — without it the
			// ledger charged cached tokens at the full input price
			// while the dashboard priced them at the cached rate.
			// Nil stays unset (CachedKnown false): cached tokens
			// then fall back to the full prompt rate, never an
			// invented one.
			if p.CachedInputUSDPer1M != nil {
				mp.CachedPromptPerToken = *p.CachedInputUSDPer1M / 1e6
				mp.CachedKnown = true
			}
			// M6.9b: same carry for the cache-write tier rates.
			// Nil stays unset (Known false): the tier then falls
			// back to the Anthropic multiples (1.25x / 2x) for
			// Anthropic-served Claude models, else the plain
			// input price (see ledger.ModelPrice.cacheWriteRate)
			// — never an invented rate. This covers the synced
			// (models.dev) layer too: it flows through addPair.
			if p.CacheWrite5mUSDPer1M != nil {
				mp.CacheWrite5mPerToken = *p.CacheWrite5mUSDPer1M / 1e6
				mp.CacheWrite5mKnown = true
			}
			if p.CacheWrite1hUSDPer1M != nil {
				mp.CacheWrite1hPerToken = *p.CacheWrite1hUSDPer1M / 1e6
				mp.CacheWrite1hKnown = true
			}
			prices[ledger.PriceKey{Provider: key.Provider, Model: key.Model}] = mp
		}
		for _, key := range priceBook.SortedPairs() {
			p, _ := priceBook.LookupPair(key.Provider, key.Model)
			addPair(key, p)
		}
		for _, key := range priceBook.SortedSyncedPairs() {
			if _, ok := priceBook.LookupModel(key.Model); ok {
				continue // operator model-level price wins
			}
			sp, _ := priceBook.SyncedPair(key.Provider, key.Model)
			addPair(key, sp.Price)
		}
		for _, model := range priceBook.SortedModels() {
			p, _ := priceBook.LookupModel(model)
			if p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
				continue // half-known: no blended price, no ledger price
			}
			mp := ledger.ModelPrice{
				PromptPerToken:     *p.InputUSDPer1M / 1e6,
				CompletionPerToken: *p.OutputUSDPer1M / 1e6,
			}
			// M1.6: same cached-rate carry as the pair path above.
			if p.CachedInputUSDPer1M != nil {
				mp.CachedPromptPerToken = *p.CachedInputUSDPer1M / 1e6
				mp.CachedKnown = true
			}
			// M6.9b: same cache-write tier-rate carry as the pair
			// path above.
			if p.CacheWrite5mUSDPer1M != nil {
				mp.CacheWrite5mPerToken = *p.CacheWrite5mUSDPer1M / 1e6
				mp.CacheWrite5mKnown = true
			}
			if p.CacheWrite1hUSDPer1M != nil {
				mp.CacheWrite1hPerToken = *p.CacheWrite1hUSDPer1M / 1e6
				mp.CacheWrite1hKnown = true
			}
			prices[ledger.PriceKey{Model: model}] = mp
		}
	}
	// A nil registry contributes no model prices (the MCP
	// source can run without one): without this guard a
	// registry-less source panics in ComboMetrics via
	// ledgerPrices — found by the fresh-source branch of
	// TestRegistryMCPSourceLazyLedgerStickyError.
	if reg != nil {
		for _, pv := range reg.List() {
			for _, m := range pv.Models {
				if _, ok := prices[ledger.PriceKey{Model: m.ID}]; ok {
					continue
				}
				if m.InputPriceUSD > 0 && m.OutputPriceUSD > 0 {
					prices[ledger.PriceKey{Model: m.ID}] = ledger.ModelPrice{PromptPerToken: m.InputPriceUSD, CompletionPerToken: m.OutputPriceUSD}
				}
			}
		}
	}
	return prices
}

// modelsDevLocalSnapshot takes the registry snapshot the models.dev
// sync plans against: every configured provider with the models the
// router knows it offers. Only listed models can receive synced
// prices/windows — the sync never invents a model.
func modelsDevLocalSnapshot(reg *provider.Registry) []modelsync.LocalProvider {
	out := []modelsync.LocalProvider{}
	for _, p := range reg.List() {
		lp := modelsync.LocalProvider{ID: p.ID, CatalogID: p.CatalogID}
		for _, m := range p.Models {
			if m.ID == "" {
				continue
			}
			lp.Models = append(lp.Models, modelsync.LocalModel{ID: m.ID, ContextWindow: m.ContextWindow})
		}
		out = append(out, lp)
	}
	return out
}

// The arena_elo block lives in config.Config now (Stage 1,
// item 13): the operator's opt-in Arena ELO sync block
// ("arena_elo" in the config file, next to models_dev) round-
// trips through config.Save, so an admin config persist no
// longer drops a hand-written block. The helpers below resolve
// the effective values (configured, else the public defaults).
// The sync itself is off unless enabled is true — with it off
// nothing here touches the network, exactly like the models_dev
// sync.

// arenaEloURL is the leaderboard URL in force: the configured
// one, else the public Arena leaderboard API.
func arenaEloURL(c *config.ArenaEloConfig) string {
	if c != nil && c.URL != "" {
		return c.URL
	}
	return modelsync.DefaultArenaURL
}

// arenaEloInterval is the re-sync interval in force: the
// configured minutes, else the 24 h default. A configured 0
// means "use the default", never "sync continuously".
func arenaEloInterval(c *config.ArenaEloConfig) time.Duration {
	if c != nil && c.IntervalMinutes > 0 {
		return time.Duration(c.IntervalMinutes) * time.Minute
	}
	return modelsync.DefaultArenaInterval
}

// arenaEloTimeout bounds one Arena fetch: the configured
// seconds, else the 30 s default.
func arenaEloTimeout(c *config.ArenaEloConfig) time.Duration {
	if c != nil && c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return modelsync.DefaultArenaTimeoutSeconds * time.Second
}

// modelsDevApplyStateWindows re-applies the persisted synced
// windows to the registry at startup (in memory only — the state
// file stays the durable record). A window is applied only when
// the registry's current window is 0 (unknown) or exactly the
// synced value: a different current window was set by the
// operator (or by hand) and is never overwritten with the stale
// synced value.
func modelsDevApplyStateWindows(syncer *modelsync.Syncer, reg *provider.Registry) {
	st := syncer.Status()
	current := map[string]int{}
	for _, p := range reg.List() {
		for _, m := range p.Models {
			current[modelsync.EntryKey(p.ID, m.ID)] = m.ContextWindow
		}
	}
	for key, e := range st.Entries {
		if e.ContextWindow <= 0 {
			continue
		}
		cur, ok := current[key]
		if !ok || cur == e.ContextWindow {
			continue // provider/model gone, or already applied
		}
		if cur != 0 {
			continue // operator-set window: leave it alone
		}
		if err := reg.ApplySyncedContextWindow(e.Provider, e.Model, e.ContextWindow); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync: cannot apply saved window for %s/%s: %v\n", e.Provider, e.Model, err)
		}
	}
	// Vision (G19 Area 1): re-apply persisted vision flags at
	// startup, in memory only. Only a true flag is re-applied:
	// a false entry may predate vision reporting, and clearing
	// a live flag from stale state would be wrong — a later
	// plan reports absence explicitly and clears it then.
	for key, e := range st.Entries {
		if !e.Vision {
			continue
		}
		if _, ok := current[key]; !ok {
			continue // provider/model gone
		}
		if err := reg.ApplySyncedVision(e.Provider, e.Model, true, true); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync: cannot apply saved vision flag for %s/%s: %v\n", e.Provider, e.Model, err)
		}
	}
	// Audio / video (B3-24 modality bridges): re-apply persisted
	// audio/video flags at startup, same only-true contract as
	// vision — a false entry may predate modality reporting, and
	// clearing a live flag from stale state would be wrong.
	for key, e := range st.Entries {
		if _, ok := current[key]; !ok {
			continue // provider/model gone
		}
		if e.Audio {
			if err := reg.ApplySyncedAudio(e.Provider, e.Model, true, true); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync: cannot apply saved audio flag for %s/%s: %v\n", e.Provider, e.Model, err)
			}
		}
		if e.Video {
			if err := reg.ApplySyncedVideo(e.Provider, e.Model, true, true); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync: cannot apply saved video flag for %s/%s: %v\n", e.Provider, e.Model, err)
			}
		}
	}
}

// applySyncPlan installs one sync plan from either price
// source (models.dev or LiteLLM): windows into the registry
// (in memory only), prices into that source's slice of the
// price book's synced layer, and a rebuilt ledger price map
// onto the async writer and the server — so a periodic re-sync
// changes what the ledger charges, not just the feeds. Windows
// apply first (fail fast): an apply fails only if the
// provider/model vanished between the snapshot and the apply,
// in which case no price moves either. Dropped pairs leave the
// source's slice with the plan; the registry needs no action
// for a dropped window (the operator's value is already the
// one stored, or the provider is gone). The per-source setter
// is load-bearing: the wholesale SetSyncedPrices would wipe
// the other source's entries (see its doc comment).
func applySyncPlan(st *stack, srv *server.Server, syncer *modelsync.Syncer, source string, plan modelsync.PlanResult) error {
	for _, u := range plan.Updates {
		if u.ContextWindow > 0 {
			if err := st.reg.ApplySyncedContextWindow(u.Provider, u.Model, u.ContextWindow); err != nil {
				return err
			}
		}
		// Vision (G19 Area 1): the feed's [modalities] table
		// reported image input (or its absence) for this model
		// — apply it in memory only, like the window. A
		// reported-without-image clears a previously synced
		// flag; an unreported model leaves the flag alone.
		if u.VisionReported {
			if err := st.reg.ApplySyncedVision(u.Provider, u.Model, u.Vision, u.VisionReported); err != nil {
				return err
			}
		}
		// Audio / video (B3-24 modality bridges): the same
		// table reported audio/video input (or its absence)
		// for this model — apply it in memory only, like
		// vision. A reported-without-audio/video clears a
		// previously synced flag; an unreported model leaves
		// the flag alone.
		if u.AudioReported {
			if err := st.reg.ApplySyncedAudio(u.Provider, u.Model, u.Audio, u.AudioReported); err != nil {
				return err
			}
		}
		if u.VideoReported {
			if err := st.reg.ApplySyncedVideo(u.Provider, u.Model, u.Video, u.VideoReported); err != nil {
				return err
			}
		}
	}
	synced := syncer.SyncedPrices()
	for _, k := range plan.DropPrices {
		delete(synced, k)
	}
	for _, u := range plan.Updates {
		if u.Price != nil {
			synced[config.PriceKey{Provider: u.Provider, Model: u.Model}] = config.SyncedPrice{Price: *u.Price, SyncedAt: plan.SyncedAt}
		}
	}
	st.priceBook.SetSyncedPricesForSource(source, synced)
	lp := buildLedgerPrices(st.priceBook, st.reg)
	st.ledgerPrices = lp
	st.asyncLed.SetPrices(lp)
	if srv != nil {
		srv.SetLedgerPrices(lp)
	}
	return nil
}

// modelsDevApplyPlan installs one models.dev sync plan (see
// applySyncPlan).
func modelsDevApplyPlan(st *stack, srv *server.Server, plan modelsync.PlanResult) error {
	return applySyncPlan(st, srv, st.modelsDev, config.SyncedSourceModelsDev, plan)
}

// litellmApplyPlan installs one LiteLLM sync plan (B4-03/12,
// Stage 1; see applySyncPlan).
func litellmApplyPlan(st *stack, srv *server.Server, plan modelsync.PlanResult) error {
	return applySyncPlan(st, srv, st.litellm, config.SyncedSourceLiteLLM, plan)
}

// modelsDevStatusFunc returns the server's models.dev status
// source for a syncer, or nil when the sync is off — the feeds
// then omit the block entirely instead of reporting "unknown".
func modelsDevStatusFunc(syncer *modelsync.Syncer) func() modelsync.Status {
	if syncer == nil {
		return nil
	}
	return syncer.Status
}

// runModelsDevSyncLoop runs the opt-in models.dev sync (CHECKLIST
// 179) in the background: one sync at startup, then every
// Interval when configured (0 = once only). The first sync runs
// after serve starts, never before — startup must never wait on
// models.dev. A failed sync keeps the last good data and logs
// the failure honestly; the loop stops when ctx ends (shutdown).
// The registry is re-snapshotted on every run, so providers added
// or removed live are picked up by the next sync.
func runModelsDevSyncLoop(ctx context.Context, st *stack, srv *server.Server) {
	doSync := func() {
		locals := modelsDevLocalSnapshot(st.reg)
		counts, err := st.modelsDev.Sync(ctx, locals, st.priceBook.OperatorHasPrice, func(plan modelsync.PlanResult) error {
			return modelsDevApplyPlan(st, srv, plan)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync failed (keeping last good data): %v\n", err)
			return
		}
		// Tier source (B2-14): rebuild the models_dev_tier rows
		// from the fresh catalog — the only caller of
		// RebuildTier anywhere — then persist the store, so a
		// restart keeps the tiers without re-fetching. A nil
		// catalog (no successful fetch yet) clears nothing and
		// applies nothing. A failed sync never reaches here, so
		// a failed fetch cannot wipe the last good tiers.
		if st.intel != nil {
			res := st.intel.RebuildTier(st.modelsDev.LastCatalog())
			if err := st.intel.Save(); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: model intelligence state not saved (%v); tiers stay in memory only\n", err)
			} else if res.Upserted > 0 || res.Pruned > 0 {
				fmt.Fprintf(os.Stderr, "codvian-router: model intelligence tiers rebuilt (%d upserted, %d pruned)\n", res.Upserted, res.Pruned)
			}
		}
		fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync ok (%d prices, %d windows applied, %d pairs matched)\n", counts.PricesApplied, counts.WindowsApplied, counts.MatchedPairs)
	}
	doSync()
	interval := st.modelsDev.Interval()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doSync()
		}
	}
}

// runLiteLLMSyncLoop runs the opt-in LiteLLM sync (B4-03/12,
// Stage 1) in the background, mirroring runModelsDevSyncLoop:
// one sync at startup, then every Interval when configured
// (0 = once only). The first sync runs after serve starts,
// never before — startup must never wait on the LiteLLM cost
// map. A failed sync keeps the last good data and logs the
// failure honestly; the loop stops when ctx ends (shutdown).
// The registry is re-snapshotted on every run, so providers
// added or removed live are picked up by the next sync. Unlike
// the models.dev loop it does NOT rebuild intelligence tiers:
// the models_dev_tier source is fed only by the models.dev
// catalog (RebuildTier's only caller is the models.dev loop).
func runLiteLLMSyncLoop(ctx context.Context, st *stack, srv *server.Server) {
	doSync := func() {
		locals := modelsDevLocalSnapshot(st.reg)
		counts, err := st.litellm.Sync(ctx, locals, st.priceBook.OperatorHasPrice, func(plan modelsync.PlanResult) error {
			return litellmApplyPlan(st, srv, plan)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: litellm sync failed (keeping last good data): %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "codvian-router: litellm sync ok (%d prices, %d windows applied, %d pairs matched)\n", counts.PricesApplied, counts.WindowsApplied, counts.MatchedPairs)
	}
	doSync()
	interval := st.litellm.Interval()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doSync()
		}
	}
}

// runScheduledModelRefreshLoop runs the scheduled model-list
// refresh (CT-7) in the background: one refresh at startup, then
// every provider.ModelListRefreshInterval (24h — the interval is
// recorded on that constant; there is no config knob for it, see
// refresh.go). The first refresh runs after serve starts, never
// before — startup must never wait on upstreams. A failed
// provider keeps its last known list (RefreshModels reports it
// per provider and never empties the catalog); failures are
// logged honestly, never silent. The loop stops when ctx ends
// (shutdown).
func runScheduledModelRefreshLoop(ctx context.Context, reg *provider.Registry) {
	doRefresh := func() {
		results, err := reg.RefreshModels(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: scheduled model refresh: persist failed, lists unchanged: %v\n", err)
			return
		}
		ok, failed, skipped := 0, 0, 0
		for _, res := range results {
			switch res.Status {
			case provider.ModelRefreshOK:
				ok++
			case provider.ModelRefreshFailed:
				failed++
				fmt.Fprintf(os.Stderr, "codvian-router: scheduled model refresh: provider %q failed: %s (kept last known list)\n", res.ProviderName, res.Error)
			case provider.ModelRefreshSkipped:
				skipped++
			}
		}
		fmt.Fprintf(os.Stderr, "codvian-router: scheduled model refresh done (%d ok, %d failed, %d skipped; next in %s)\n", ok, failed, skipped, provider.ModelListRefreshInterval)
	}
	doRefresh()
	t := time.NewTicker(provider.ModelListRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doRefresh()
		}
	}
}

// loopbackHost reports whether rawURL's host is a loopback
// address (127.0.0.0/8, ::1) or the name "localhost": requests to
// such a base URL never leave this machine. Used by the
// per-request routing contract (CHECKLIST 157) to treat a local
// provider as training-safe by construction. An unparseable URL
// is not loopback — the verdict fails closed.
func loopbackHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// allowPrivateProviderEndpoints reports whether the operator has
// explicitly opted into local / private provider endpoints — a
// local Ollama, LM Studio or self-hosted gateway on loopback or a
// private range. It is OFF by default: a stored provider base URL
// is attacker-influenceable through the provider-management API,
// and the secure default must not let one reach an internal
// service (SSRF). An operator who genuinely runs a local provider
// opts in with CODVIAN_ROUTER_ALLOW_PRIVATE_ENDPOINTS=1 (or
// true/yes/on); anything else — including unset — keeps the
// secure default.
func allowPrivateProviderEndpoints() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODVIAN_ROUTER_ALLOW_PRIVATE_ENDPOINTS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// buildGuardrailStore seeds the guardrails store from the config
// file (CHECKLIST 185): the three blocking toggles plus the
// external entries, with one HTTP classifier registered per
// external entry. Validation runs here too (defense in depth —
// config.Load already validates, but openStack must not silently
// accept a config that bypassed it): an async entry with a
// blocking kind (pii/secret/injection) fails startup, naming the
// guardrail, instead of letting a secret reach a provider while
// a verdict is in flight. A duplicate name or bad endpoint fails
// startup the same way.
func buildGuardrailStore(cfg *config.Config) (*guardrails.Store, error) {
	gc := guardrails.Config{}
	if cfg.Guardrails != nil {
		gc.BlockPII = cfg.Guardrails.BlockPII
		gc.BlockSecrets = cfg.Guardrails.BlockSecrets
		gc.BlockInjection = cfg.Guardrails.BlockInjection
		// Per-type toggles (B3-17, G18 Area 7): the config
		// file's disabled_types seed the store's disabled
		// set at startup.
		gc.DisabledTypes = cfg.Guardrails.DisabledTypes
		for _, e := range cfg.Guardrails.External {
			gc.External = append(gc.External, guardrails.ExternalConfig{
				Name: e.Name, Kind: e.Kind, Mode: guardrails.Mode(e.Mode),
				Endpoint: e.Endpoint, TimeoutMs: e.TimeoutMs,
			})
		}
	}
	if err := gc.Validate(); err != nil {
		return nil, fmt.Errorf("guardrails config: %w", err)
	}
	store := guardrails.NewStoreWith(gc)
	// Custom regex patterns (B3-18): the config file's
	// custom_patterns seed the store at startup. A bad
	// pattern fails startup naming the label — config.Load
	// already validated them, so this is defense in depth
	// for a config that bypassed it.
	if cfg.Guardrails != nil {
		for _, p := range cfg.Guardrails.CustomPatterns {
			if err := store.AddCustomPattern(guardrails.CustomPattern{
				ID: p.ID, Label: p.Label, Pattern: p.Pattern,
				Kind: p.Kind, Severity: p.Severity,
			}); err != nil {
				return nil, fmt.Errorf("guardrails custom pattern %q: %w", p.Label, err)
			}
		}
	}
	for _, e := range gc.External {
		timeout := time.Duration(e.TimeoutMs) * time.Millisecond
		if err := store.RegisterChecker(e.Name, guardrails.NewHTTPClassifier(e.Endpoint, timeout)); err != nil {
			return nil, fmt.Errorf("guardrails %q: %w", e.Name, err)
		}
	}
	return store, nil
}

// guardrailResponseMode returns the response-sanitization
// mode in force (B3-21/B3-22): the config file's
// guardrails.response_guard_mode, or "redact" when unset.
// An invalid mode was already rejected at config load.
func guardrailResponseMode(cfg *config.Config) string {
	if cfg != nil && cfg.Guardrails != nil && cfg.Guardrails.ResponseGuardMode != "" {
		return cfg.Guardrails.ResponseGuardMode
	}
	return "redact"
}

// reversiblePIIVault builds the reversible PII token vault
// (B3-19, parity tracker:782) from the config flag: nil when the
// flag is off, otherwise a vault sealing under the router's
// master-key vault and persisting to pii_vault.json in the data
// dir. A corrupt vault file fails startup loudly — silently
// dropping sealed PII would strand tokens the provider already
// echoed back.
func reversiblePIIVault(cfg *config.Config, cv *crypto.Vault) (*guardrails.Vault, error) {
	if cfg == nil || cfg.Guardrails == nil || !cfg.Guardrails.ReversibleVault {
		return nil, nil
	}
	if cv == nil {
		return nil, fmt.Errorf("reversible PII vault needs the master-key vault")
	}
	v := guardrails.NewVault(cv, filepath.Join(cfg.DataDir, "pii_vault.json"))
	if err := v.Load(); err != nil {
		return nil, err
	}
	return v, nil
}

// buildPluginRegistry builds the in-process plugin registry from
// the config file (CHECKLIST 201): the named built-ins, in order.
// Validation runs here too (defense in depth — config.Load
// already validates, but openStack must not silently accept a
// config that bypassed it): an unknown name fails startup naming
// it. A nil/empty block means no plugins: the registry is still
// returned (empty), so the server and the adapter hook both see
// "zero plugins" honestly instead of a nil that means "unknown".
//
// The cost-cap plugin (CHECKLIST 239) is built configured here:
// its cap comes from cfg.Plugins.CostCapUSD and its prices from
// the operator's price book (model-level prices only — the plugin
// runs before routing, so no provider is known yet and no pair
// price can apply). A positive cap with no price book still loads
// the plugin, but every priced check then fails closed as
// unpriced; the lookup reports that honestly per request.
func buildPluginRegistry(cfg *config.Config, priceBook *config.PriceBook) (*plugins.Registry, error) {
	reg := plugins.New()
	if cfg.Plugins == nil || len(cfg.Plugins.Enabled) == 0 {
		return reg, nil
	}
	opts := plugins.BuiltinOptions{}
	if cfg.Plugins.CostCapUSD != nil {
		opts.CostCapUSD = *cfg.Plugins.CostCapUSD
	}
	if priceBook != nil {
		book := priceBook
		opts.CostCapPrices = func(model string) (float64, float64, bool) {
			p, ok := book.LookupModel(model)
			if !ok || p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
				return 0, 0, false
			}
			return *p.InputUSDPer1M, *p.OutputUSDPer1M, true
		}
	}
	if err := plugins.LoadBuiltinsConfigured(reg, cfg.Plugins.Enabled, opts); err != nil {
		return nil, fmt.Errorf("plugins config: %w", err)
	}
	return reg, nil
}

// pairQuotaCache memoises ledger.PairQuota for a few seconds per
// (provider, model) pair (M3 Phase C, Phase D wiring): the hook
// calls signalsFn once per chain candidate per request, and each
// PairQuota is a full table scan on a ledger with no
// (provider, model, ts) index. A short TTL keeps routing decisions
// fresh — spend changes slowly relative to a request — while
// collapsing repeated pairs to one scan per TTL window. Errors are
// never cached, so a transient ledger error retries on the next
// request. A dedicated index is a schema change for later.
type pairQuotaCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	query func(context.Context, string, string, time.Time) (ledger.PairQuota, error)
	m     map[string]pairQuotaEntry
}

type pairQuotaEntry struct {
	at time.Time
	q  ledger.PairQuota
}

func newPairQuotaCache(ttl time.Duration, query func(context.Context, string, string, time.Time) (ledger.PairQuota, error)) *pairQuotaCache {
	return &pairQuotaCache{ttl: ttl, query: query, m: make(map[string]pairQuotaEntry)}
}

// get returns the cached PairQuota when it is younger than the TTL,
// otherwise it queries, caches the result on success, and returns
// it. ok is false when the query failed (nothing cached).
func (c *pairQuotaCache) get(ctx context.Context, providerID, model string, now time.Time) (ledger.PairQuota, bool) {
	key := providerID + "\x00" + model
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.q, true
	}
	c.mu.Unlock()
	q, err := c.query(ctx, providerID, model, now)
	if err != nil {
		return ledger.PairQuota{}, false
	}
	c.mu.Lock()
	c.m[key] = pairQuotaEntry{at: now, q: q}
	c.mu.Unlock()
	return q, true
}

// cacheWarmthSnapshot serves strategy.CacheWarmthFunc from a
// short-TTL snapshot of ledger.CachedTokenFraction (M6.5 wiring):
// the hook must never run a fresh ledger query per chain target,
// so fractions are memoized per (provider, model) for the TTL and
// re-read only when stale. The window is the last hour — warmth
// decays with provider prompt-cache TTLs, so an all-time fraction
// would rank a long-cold pair warm. A query error yields ok=false
// (unmeasured, never a guessed 0) and is cached like any other
// result for the TTL; the next window re-reads. Mirrors the
// pairQuotaCache pattern above.
type cacheWarmthSnapshot struct {
	mu    sync.Mutex
	ttl   time.Duration
	query func(provider, model string) (float64, bool)
	m     map[string]warmthEntry
}

type warmthEntry struct {
	at   time.Time
	frac float64
	ok   bool
}

func newCacheWarmthSnapshot(ttl time.Duration, query func(provider, model string) (float64, bool)) *cacheWarmthSnapshot {
	return &cacheWarmthSnapshot{ttl: ttl, query: query, m: make(map[string]warmthEntry)}
}

// warmth returns the memoized fraction for the pair, re-reading
// through query only when the entry is older than the TTL.
func (c *cacheWarmthSnapshot) warmth(provider, model string) (float64, bool) {
	key := provider + "\x00" + model
	c.mu.Lock()
	if e, ok := c.m[key]; ok && time.Since(e.at) < c.ttl {
		frac, ok := e.frac, e.ok
		c.mu.Unlock()
		return frac, ok
	}
	c.mu.Unlock()
	frac, ok := c.query(provider, model)
	c.mu.Lock()
	c.m[key] = warmthEntry{at: time.Now(), frac: frac, ok: ok}
	c.mu.Unlock()
	return frac, ok
}

// wireReasoningCache wires the M6.6 reasoning replay cache onto
// the serving wrapper (M6.6b): a memory-only ReasoningCache with
// the M6.6 defaults (2 h TTL, 200 entries, M6.1 caps —
// cache.NewReasoningCache). A nil wrapper is a no-op. Extracted as a
// helper (like savingsPricesFor) so the wiring itself is
// unit-testable without standing up a whole stack. The DB tier is
// wireReasoningCacheAtPath (G13): serve mode passes
// DataDir/reasoning_cache.db, so a restart reloads entries
// instead of losing them; the no-path form here stays memory-only
// (a restart loses the entries and a later turn misses — never
// wrongly replays).
func wireReasoningCache(se *degrade.StaleEngine) *cache.ReasoningCache {
	return wireReasoningCacheAtPath(se, "")
}

// wireReasoningCacheAtPath is wireReasoningCache with a SQLite
// tier (G13): when path != "" the cache is opened from that file
// (entries survive a restart; expired rows are pruned on open),
// and any open failure falls back to memory-only with a stderr
// note — exactly the fail-open pattern the exact/semantic
// persistence uses above. It returns the wired cache so openStack
// can keep it for the shutdown Sync + Close (and the 1-minute
// Sync ticker, which prunes rows for entries evicted from
// memory since the last sync — a store writes its row
// immediately, but eviction removes only the memory slot).
func wireReasoningCacheAtPath(se *degrade.StaleEngine, path string) *cache.ReasoningCache {
	if se == nil {
		return nil
	}
	if path == "" {
		rc := cache.NewReasoningCache()
		se.SetReasoningCache(rc)
		return rc
	}
	rc, err := cache.OpenPersistentReasoningCache(path, cache.ReasoningCacheConfig{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: opening reasoning cache persistence %q: %v — continuing memory-only\n", path, err)
		rc = cache.NewReasoningCache()
	}
	se.SetReasoningCache(rc)
	return rc
}

// applyNadirConfig wires the operator's routing.nadir block into
// the EXISTING router.Config Nadir fields that nadirSelect reads
// — no parallel set. A nil block leaves the engine untouched,
// which is fail-closed: auto/nadir falls back to priority order
// and never calls any external API. An empty endpoint likewise
// leaves the engine unconfigured. A timeout of 0 keeps the
// engine's 2000 ms default (the engine clamps above 30000). A
// cooldown of 0 keeps the engine's 30 s default. The
// API key comes from the environment ONLY, never the config file
// (see NadirConfig): empty means no X-API-Key header is sent.
// Values were validated at load (endpoint an absolute http(s)
// URL, timeout in [0, 30000], cooldown_ms non-negative).
func applyNadirConfig(nadir *config.NadirConfig, engCfg *router.Config) {
	if nadir == nil {
		return
	}
	engCfg.NadirEndpoint = nadir.Endpoint
	if nadir.TimeoutMs != 0 {
		engCfg.NadirTimeoutMs = nadir.TimeoutMs
	}
	if nadir.CooldownMs != 0 {
		engCfg.NadirCooldownMs = nadir.CooldownMs
	}
	engCfg.NadirAPIKey = os.Getenv("CODVIAN_ROUTER_NADIR_API_KEY")
}

func openStack(opts options) (*stack, error) {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadOrCreate(path)
	if err != nil {
		return nil, err
	}
	if opts.port != 0 {
		if err := cfg.SetListenPort(opts.port); err != nil {
			return nil, err
		}
	}
	listen, err := effectiveListen(cfg, opts)
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, err
	}

	// Vault: master secret lives in the data dir (created on first run).
	vault, err := crypto.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	// Ledger: SQLite in the data dir.
	//
	// AUD-REL-005: a corrupt ledger.db used to abort startup outright,
	// with no quarantine and no operator signal beyond a wrapped error.
	// OpenWithRecovery preserves the unusable file (never deletes or
	// overwrites it) and starts on a fresh ledger, reporting the move.
	ledger.Logf = log.Printf
	led, quarantined, err := ledger.OpenWithRecovery(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		return nil, err
	}
	if quarantined != "" {
		log.Printf("ledger: the previous ledger was unusable and has been preserved at %s; a fresh ledger is in use", quarantined)
	}
	led.FailClosed = cfg.FailClosed
	// Flat-rate opt-in (B4-13, Stage 1): the config file's
	// ledger.flat_rate_as_zero, set after Open exactly as
	// the ledger documents (the operator or main.go sets
	// it; RecordUsage reads it per call). Default false.
	if cfg.Ledger != nil {
		led.FlatRateAsZero = cfg.Ledger.FlatRateAsZero
	}

	// Last-known-good provider/model pairs (M3 Phase C): restored
	// from the previous run so a restart keeps routing to the
	// provider that last worked for each model. A missing file
	// (first run) or a corrupt one starts empty — never fatal,
	// never invented — and the failure is logged loudly.
	lkgpPath := filepath.Join(cfg.DataDir, "lkgp.json")
	if err := strategy.DefaultLKGPRegistry.Store().Load(lkgpPath); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: lkgp store load failed (%v); starting with empty last-known-good state\n", err)
	}

	// Rolling per-key 5-hour budgets from the config file's
	// key_budgets block: applied into the ledger (the durable
	// store) at startup, so a budget survives a restart even if
	// the config file is later edited. A row that fails to apply
	// is logged loudly, never fatal: the ledger itself just
	// opened fine, so this is a data error — but a silently
	// missing budget would let a key spend uncapped, so it must
	// not fail quiet.
	for keyID, b := range cfg.KeyBudgets {
		policy, _ := ledger.NormalizeUnpricedPolicy(b.UnpricedPolicy)
		// M5.1: the full windowed config, not just the 5-hour
		// cap — daily/weekly/monthly limits + the active-window
		// selector ride in the same row.
		kb := ledger.KeyBudget{
			KeyID:            keyID,
			LimitUSD:         b.LimitUSD,
			UnpricedPolicy:   policy,
			DailyLimitUSD:    b.DailyLimitUSD,
			WeeklyLimitUSD:   b.WeeklyLimitUSD,
			MonthlyLimitUSD:  b.MonthlyLimitUSD,
			WarningThreshold: b.WarningThreshold,
			ResetTime:        b.ResetTime,
		}
		if iv, ok := ledger.NormalizeBudgetResetInterval(b.ResetInterval); ok {
			kb.ResetInterval = iv
		}
		if kb.WarningThreshold == 0 {
			kb.WarningThreshold = 0.8
		}
		if kb.ResetTime == "" {
			kb.ResetTime = "00:00"
		}
		if err := led.SetKeyBudgetConfig(context.Background(), kb); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: applying key budget for %s: %v\n", keyID, err)
		}
	}
	// M5.4: fair-share quota pools from the config file into the
	// ledger (the durable store), same pattern as the key budgets
	// above — a sync failure is logged loudly, never fatal, and
	// never silent: a missing pool would let its keys spend
	// unshared.
	for _, p := range cfg.QuotaPools {
		pool := ledger.QuotaPool{
			ID:                  p.ID,
			Name:                p.Name,
			SaturationThreshold: p.SaturationThreshold,
			AccountCount:        p.AccountCount,
			Dimensions:          []ledger.QuotaDimension{},
			Allocations:         []ledger.PoolAllocation{},
		}
		for _, d := range p.Dimensions {
			pool.Dimensions = append(pool.Dimensions, ledger.QuotaDimension{Unit: d.Unit, Window: d.Window, Limit: d.Limit})
		}
		for _, a := range p.Allocations {
			pool.Allocations = append(pool.Allocations, ledger.PoolAllocation{
				KeyID: a.KeyID, Weight: a.Weight, CapValue: a.CapValue, CapUnit: a.CapUnit, Policy: a.Policy,
			})
		}
		if err := led.SetQuotaPool(context.Background(), pool); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: applying quota pool %s: %v\n", p.ID, err)
		}
	}
	// M5.9: quota schedules from the config file into the ledger
	// (the durable store), same pattern as the pools above — a
	// sync failure is logged loudly, never fatal, and never
	// silent: a missing schedule would leave its pool open when
	// it should be closed (or closed when it should be open).
	for _, s := range cfg.QuotaSchedules {
		sched := ledger.QuotaSchedule{
			ID: s.ID, PoolID: s.PoolID, Label: s.Label,
			Days: s.Days, StartMinute: s.StartMinute, EndMinute: s.EndMinute,
			Timezone: s.Timezone, Mode: s.Mode,
			BudgetValue: s.BudgetValue, BudgetUnit: s.BudgetUnit, BudgetWindow: s.BudgetWindow,
			Priority: s.Priority, Enabled: s.Enabled,
			Reserves: []ledger.ReserveRule{},
		}
		for _, r := range s.Reserves {
			sched.Reserves = append(sched.Reserves, ledger.ReserveRule{Window: r.Window, Percent: r.Percent})
		}
		if err := led.SetQuotaSchedule(context.Background(), sched); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: applying quota schedule %s: %v\n", s.ID, err)
		}
	}
	// M5.11: internal budgets from the config file into the
	// ledger (the durable store), same pattern as the pools
	// above — a sync failure is logged loudly, never fatal, and
	// never silent: a missing budget would let spend run past
	// the operator's cap.
	for _, b := range cfg.InternalBudgets {
		budget := ledger.InternalBudget{
			ID: b.ID, Scope: b.Scope, ProviderID: b.ProviderID, ModelID: b.ModelID, PoolID: b.PoolID,
			Period: b.Period, LimitType: b.LimitType, LimitValue: b.LimitValue,
			WarningThreshold: b.WarningThreshold, Enabled: b.Enabled,
		}
		if err := led.SetInternalBudget(context.Background(), budget); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: applying internal budget %s: %v\n", b.ID, err)
		}
	}
	// M5.12: teams from the config file into the ledger (the
	// durable store), same pattern as the pools above — a sync
	// failure is logged loudly, never fatal, and never silent: a
	// missing team would let its keys spend past the shared cap.
	for _, t := range cfg.Teams {
		team := ledger.Team{
			ID: t.ID, Name: t.Name, Description: t.Description, Status: t.Status,
			MaxBudgetUSD: t.MaxBudgetUSD, BudgetDuration: t.BudgetDuration,
		}
		if err := led.SetTeam(context.Background(), team); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: applying team %s: %v\n", t.ID, err)
			continue
		}
		for _, member := range t.Members {
			if err := led.AssignKeyToTeam(context.Background(), member, t.ID, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: assigning key %s to team %s: %v\n", member, t.ID, err)
			}
		}
	}

	// Feedback store: SQLite alongside the ledger (feedback.db).
	// Learning is behind auto_router.feedback_learned_weights
	// (default off): signals are always recorded, but the nudges
	// apply to routing only when the flag is on.
	fb, err := feedback.Open(filepath.Join(cfg.DataDir, "feedback.db"))
	if err != nil {
		led.Close()
		return nil, err
	}
	fb.SetEnabled(cfg.AutoRouter.FeedbackLearnedWeights)

	// Provider registry: providers live in the config file, keys sealed
	// with the Vault. TCP Fast Open is on by default for the clients
	// it builds; CODVIAN_ROUTER_TFO=0 disables it (measuring the
	// with/without difference only — the dialer falls back
	// gracefully either way). Warm HTTP/2 pools are sized from the
	// config "pools" block (Little's Law: ceil(expected_rps ×
	// expected_latency_ms); nil block = defaults 20 req/s × 1000 ms
	// → 20 idle connections per provider+key).
	tfoEnabled := true
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("CODVIAN_ROUTER_TFO"))); v == "0" || v == "false" || v == "off" {
		tfoEnabled = false
	}
	poolCfg := provider.PoolConfig{}
	if cfg.Pools != nil {
		poolCfg = *cfg.Pools
	}
	reg := provider.NewRegistry(
		&configProviderStore{cfg: cfg, path: path},
		provider.WithCipher(provider.NewVaultCipher(vault)),
		provider.WithTFOEnabled(tfoEnabled),
		provider.WithPoolConfig(poolCfg),
		provider.WithRegistryAllowPrivateDestinations(allowPrivateProviderEndpoints()),
	)
	if err := reg.Load(); err != nil {
		led.Close()
		return nil, err
	}

	// Real price source: the operator's price book — the config
	// file's "prices" block over the optional local price file
	// (config wins on a conflict). A set price file that cannot
	// be read or fails validation fails startup: the operator
	// explicitly asked for those prices, so routing on silently
	// missing prices would be worse than not starting. With
	// neither source set the book is empty and every model is
	// price-unknown (neutral in scoring, $0 in the ledger).
	priceBook, err := cfg.LoadPriceBook()
	if err != nil {
		led.Close()
		return nil, err
	}

	// Opt-in models.dev sync (CHECKLIST 179): off by default —
	// with no models_dev block (or enabled=false) the router
	// never contacts models.dev. When enabled, the persisted
	// state from the last run is loaded first (last good prices
	// + windows, so a restart keeps them without re-fetching)
	// and installed into the price book + registry; the live
	// fetch runs in the background after serve starts (see
	// runModelsDevSyncLoop), so a slow or offline catalog never
	// delays startup. A corrupt state file only warns — startup
	// must never fail on a corrupt sync cache.
	var modelsDev *modelsync.Syncer
	if md := cfg.ModelsDev; md != nil && md.Enabled {
		modelsDev = modelsync.New(modelsync.SyncerConfig{
			Enabled:  true,
			URL:      md.EffectiveURL(),
			Interval: time.Duration(md.IntervalMinutes) * time.Minute,
			Timeout:  time.Duration(md.EffectiveTimeoutSeconds()) * time.Second,
		}, filepath.Join(cfg.DataDir, modelsync.StateFileName))
		if err := modelsDev.LoadState(); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: models.dev sync state ignored (%v); starting with no synced data\n", err)
		}
		// The state file may hold a synced price for a pair the
		// operator priced AFTER the last sync (and before this
		// restart): it never reaches the book — operator values
		// always win, and the next sync drops it from the state
		// too. The window side is unaffected (prices and windows
		// are independent entries).
		synced := modelsDev.SyncedPrices()
		for k := range synced {
			if priceBook.OperatorHasPrice(k.Provider, k.Model) {
				delete(synced, k)
			}
		}
		priceBook.SetSyncedPricesForSource(config.SyncedSourceModelsDev, synced)
		modelsDevApplyStateWindows(modelsDev, reg)
	}

	// Opt-in LiteLLM sync (B4-03/12, Stage 1): off by
	// default, mirroring the models.dev sync above — own
	// state file (litellm-sync.json), own "litellm" slice of
	// the synced price layer, own background loop (see
	// runLiteLLMSyncLoop). An empty URL means the public
	// LiteLLM cost map (NewLiteLLMSyncer defaults it).
	// A corrupt state file only warns — startup must never
	// fail on a corrupt sync cache.
	var litellm *modelsync.Syncer
	if ll := cfg.LiteLLM; ll != nil && ll.Enabled {
		timeout := ll.TimeoutSeconds
		if timeout <= 0 {
			timeout = config.DefaultModelsDevTimeoutSeconds
		}
		litellm = modelsync.NewLiteLLMSyncer(modelsync.SyncerConfig{
			Enabled:  true,
			URL:      ll.URL,
			Interval: time.Duration(ll.IntervalMinutes) * time.Minute,
			Timeout:  time.Duration(timeout) * time.Second,
		}, filepath.Join(cfg.DataDir, modelsync.LiteLLMStateFileName))
		if err := litellm.LoadState(); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: litellm sync state ignored (%v); starting with no synced data\n", err)
		}
		synced := litellm.SyncedPrices()
		for k := range synced {
			if priceBook.OperatorHasPrice(k.Provider, k.Model) {
				delete(synced, k)
			}
		}
		priceBook.SetSyncedPricesForSource(config.SyncedSourceLiteLLM, synced)
		modelsDevApplyStateWindows(litellm, reg)
	}

	// Live model intelligence (B2-14 follow-up): the Intelligence
	// store the 16-factor scorer reads (TaskFit) and the opt-in
	// Arena ELO syncer that fills it. The store is ALWAYS built,
	// even with every sync off: the models_dev_tier source is
	// rebuilt from the models.dev catalog on every sync (see
	// runModelsDevSyncLoop), so the scorer gets tier signal
	// without any Arena fetch, and the persisted state
	// (model-intelligence.json) keeps the last good rows across
	// restarts. A corrupt state file only warns — startup must
	// never fail on a sync cache. The Arena fetch itself runs
	// only when the operator enables the arena_elo block (off by
	// default); with it off nothing here touches the network.
	intel := modelsync.NewIntelligence(filepath.Join(cfg.DataDir, modelsync.IntelligenceStateFileName))
	if err := intel.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: model intelligence state ignored (%v); starting with no stored intelligence\n", err)
	}
	var arenaSyncer *modelsync.ArenaSyncer
	if ae := cfg.ArenaElo; ae != nil && ae.Enabled {
		arenaSyncer = modelsync.NewArenaSyncer(modelsync.ArenaSyncerConfig{
			Enabled:  true,
			URL:      arenaEloURL(ae),
			Interval: arenaEloInterval(ae),
			Timeout:  arenaEloTimeout(ae),
		}, intel)
		fmt.Fprintf(os.Stderr, "codvian-router: Arena ELO sync enabled\n")
	}

	// Ledger prices (per-token) from the same book, so spend is
	// real where the operator configured a price: the book's
	// per-1M prices convert by /1e6, and a model's legacy
	// per-token prices on its provider entry fill gaps the book
	// does not cover (book wins on conflict). Prices are keyed
	// by (provider, model): a pair entry charges exactly that
	// provider's serving of the model, and a model-level entry
	// (empty Provider) is the fallback for every provider
	// serving the model. A model with no price anywhere still
	// records $0 — a lower bound, never an invented cost.
	// Synced models.dev prices (CHECKLIST 179) are included the
	// same way, except where the operator has a model-level price
	// for the model — the ledger's pair key would otherwise beat
	// the operator's fallback, and operator values always win.
	ledgerPrices := buildLedgerPrices(priceBook, reg)

	// Upstream clients, one per enabled provider AND per key in
	// its pool (CHECKLIST 175): the engine's client map is keyed
	// by (provider, key) identity (router.ClientKey), so a pool
	// of N keys registers N clients and the engine can fail over
	// from one key to the next without ever touching another
	// provider. A provider whose key cannot be decrypted (e.g.
	// sealed under a different master secret) or whose base URL
	// is invalid is skipped with a warning — it must not take
	// the whole gateway down. A single bad key in a pool skips
	// only that key: the provider's other keys still register.
	clients := make(map[string]provider.Client)
	for _, p := range reg.List() {
		if !p.Enabled {
			continue
		}
		keys := p.EffectiveKeys()
		if len(keys) == 0 {
			// No-auth provider (empty legacy key, e.g. a local
			// provider): no key to pool — one client under the
			// legacy identity (bare provider ID), exactly as
			// before pools existed.
			client, err := reg.ClientFor(p.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: skipping provider %q (%s): %v\n", p.Name, p.ID, err)
				continue
			}
			clients[router.ClientKey(p.ID, "")] = client
		}
		for _, key := range keys {
			client, err := reg.ClientForKey(p.ID, key.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: skipping key %q of provider %q (%s): %v\n", key.ID, p.Name, p.ID, err)
				continue
			}
			clients[router.ClientKey(p.ID, key.ID)] = client
		}
		if p.CatalogID != "" {
			// Surface the catalog facts to the operator: which
			// entry this provider came from, how its facts were
			// established, and which surface the credentials are
			// for. Verification is never hidden behind a bare
			// provider count.
			auth := p.AuthEndpoint
			if p.AuthVariant != "" {
				auth += " (variant " + p.AuthVariant + ")"
			}
			fmt.Fprintf(os.Stderr, "codvian-router: provider %q catalog %s verification %s wire %s auth %s\n",
				p.Name, p.CatalogID, p.Verification, p.WireFormat, auth)
		}
	}

	// Warm the HTTP/2 pools: one real request (TestKey) per provider
	// through its pooled transport, so the first user request never
	// pays a cold dial. Best-effort — a provider whose key test
	// fails is logged and skipped, never fatal at startup. (The
	// health watcher's startup probe would warm the same pools a
	// moment later; this explicit warm is what the startup log line
	// and the dashboard "warmed" state report.)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		warmed, failures := reg.WarmPools(ctx)
		cancel()
		for id, err := range failures {
			fmt.Fprintf(os.Stderr, "codvian-router: pool warm-up skipped provider %q: %v\n", id, err)
		}
		if warmed > 0 {
			fmt.Fprintf(os.Stderr, "codvian-router: warmed %d provider pool(s)\n", warmed)
		}
	}

	// Key health / quota intelligence: probes provider keys and applies
	// quota hints (0 on quota errors) to the registry. The watcher is
	// started by runServe; runEval does one synchronous probe instead.
	// Probe history is persisted to health-history.json in the data
	// dir and reloaded at startup, so uptime/latency history
	// survives restarts (see internal/health/history.go).
	checker := health.NewWithDataDir(reg, nil, cfg.DataDir)

	// Scoring signals, honestly: latency and success come from the
	// engine's live attempt record first (real chat latency EWMA +
	// measured success rate — the freshest signal, and the only one
	// that reflects load), falling back to the health checker's last
	// key probe (a single probe is a thin sample — OK is 1.0, a
	// failed probe is 0.0, no probe yet is unknown); quota comes
	// from the registry hint, preferring a probe-measured hint when
	// one exists. Cost comes from the price book for the requested
	// (provider, model) pair (blended USD per 1M tokens: the pair
	// price first, the model-level fallback second), falling back
	// to that model's legacy per-token prices on the provider
	// entry; a pair with no configured price stays -1 (unknown —
	// neutral, never cheapest). Quality is the measured error-free
	// completion rate from real engine attempts only (the same
	// live record as success rate): it is set ONLY when the engine
	// has real attempts for the provider, never from the health
	// probe — a single probe is not an attempt record, and a
	// provider with no attempts yet has unknown quality.
	//
	// The engine is created below; signalsFn reads it through the
	// package-level forward declaration so the closure can be
	// built before the engine exists. It is nil-safe: before the
	// engine is assigned, the health-probe fallback applies.
	var engine *router.Engine
	quotaHint := router.QuotaHintFunc(reg)
	// NOTE (slice 8, 2026-11-17): a parallel slice changed
	// scoring.SignalsFunc to (providerID, model string) — cost is
	// per model, not per provider — while this file still had the
	// old one-arg form, so the tree did not build. This closure was
	// adapted to the new signature only: when a model is named its
	// own prices are used, otherwise the cheapest model's price as
	// before. No scoring behaviour changed for empty/unknown models.
	// One quota cache for the process (10s TTL, see
	// pairQuotaCache): the signals closure below reads it on every
	// routing decision.
	pqCache := newPairQuotaCache(10*time.Second, func(ctx context.Context, providerID, model string, now time.Time) (ledger.PairQuota, error) {
		return led.PairQuota(ctx, providerID, model, now)
	})

	signalsFn := func(providerID, model string) scoring.Signals {
		sig := scoring.Signals{QuotaHint: quotaHint(providerID), CostUSDPer1M: -1, Util5h: -1, Util7d: -1, Attempts: -1, QuotaRemainingPct: -1, ConnectionCount: -1}
		// 16-factor producer fields (G19 Area 1): the four
		// Signals fields the scorer reads that no producer
		// filled before — BreakerClosed, QuotaRemainingPct,
		// ConnectionCount, ContextWindow. Each is populated
		// HERE, before the ProviderStats early return below,
		// so every path (live stats, probe fallback) carries
		// them. Unknown stays unknown: QuotaRemainingPct -1
		// (never QuotaHintFunc's 100-for-unknown — a measured
		// fraction only), ConnectionCount -1 (never 0, which
		// would score as a known single connection),
		// ContextWindow 0 (never a guessed window).
		if engine != nil {
			sig.BreakerClosed = engine.BreakerClosedForProvider(providerID)
		}
		if p, ok := reg.Get(providerID); ok {
			// Remaining quota: the registry hint as a real
			// fraction, only when one was actually measured
			// (QuotaUnknown -1 means no hint; out-of-range
			// values are not trusted either).
			if p.QuotaHint >= 0 && p.QuotaHint <= 1 {
				sig.QuotaRemainingPct = p.QuotaHint * 100
			}
			// Live connection pool: one per registered client
			// key, or 1 for a bare (no-auth) client. Counted
			// from the clients map directly — hasClient is
			// declared below this closure and cannot be
			// referenced here.
			if _, ok := clients[router.ClientKey(p.ID, "")]; ok {
				sig.ConnectionCount = 1
			} else {
				n := 0
				for _, k := range p.EffectiveKeys() {
					if _, ok := clients[router.ClientKey(p.ID, k.ID)]; ok {
						n++
					}
				}
				if n > 0 {
					sig.ConnectionCount = n
				}
			}
			// Context window: the registry model entry
			// (operator-set or models.dev synced). Inlined
			// here because the contextWindow helper is
			// declared below this closure.
			for _, m := range p.Models {
				if m.ID == model {
					sig.ContextWindow = m.ContextWindow
					break
				}
			}
		}
		// Learned quota overlay (PARITY rows :495/:497): the
		// engine's learned remaining-quota fraction — measured
		// from real upstream rate-limit headers on every
		// response (learnRateLimit) — and the window-reset
		// instant it expires at, overlaid on the signal so the
		// strategy layer finally sees a measured positive
		// fraction instead of only the registry/probe hints.
		// Measured beats configured: the learned fraction
		// overrides the registry hint above and the probe hint
		// below, and the learned reset instant becomes the
		// signal's ExpiresAt — the deadline expiry-first
		// spends against. The upstream window reset is when
		// the current window's remaining budget is replaced,
		// so it IS the deadline for that remaining quota
		// (the reference router's own deadline is the window reset).
		// The overlay runs on BOTH return paths below (the
		// live-stats early return and the probe fallback), so
		// no path ships a signal without it. No entry — or an
		// expired one — leaves the signal untouched: unknown
		// stays unknown, never invented. A zero expiry
		// (operator override / startup restore) carries no
		// deadline, so ExpiresAt stays zero and the
		// reset-time fallback stands. Per provider, not per
		// key: the engine learns at provider granularity (the
		// most restrictive signal across the provider's keys
		// wins); the per-key rows persist in the ledger.
		applyLearnedQuota := func() {
			if engine == nil {
				return
			}
			if hint, expiresAt, ok := engine.LearnedQuotaHint(providerID); ok {
				sig.QuotaRemainingPct = float64(hint)
				sig.QuotaHint = hint
				if !expiresAt.IsZero() {
					sig.ExpiresAt = expiresAt
				}
			}
		}
		// Real cost for THIS (provider, model) pair: the price
		// book first (pair price, else the model-level
		// fallback), then the model's legacy per-token prices
		// on the provider entry. Unknown stays -1: neutral,
		// never cheapest.
		if blended, ok := priceBook.BlendedUSDPer1M(providerID, model); ok {
			sig.CostUSDPer1M = blended
		} else if p, ok := reg.Get(providerID); ok {
			for _, m := range p.Models {
				if m.ID != model || m.InputPriceUSD <= 0 || m.OutputPriceUSD <= 0 {
					continue
				}
				sig.CostUSDPer1M = (m.InputPriceUSD + m.OutputPriceUSD) / 2 * 1e6
				break
			}
		}
		// Quota telemetry (M3 Phase C, ledger.PairQuota): measured
		// priced spend for THIS (provider, model) pair in the
		// rolling 5-hour and 7-day windows, and the pair's next
		// roll-off (oldest in-window priced spend + 5h). Util5h /
		// Util7d stay -1: the system holds no per-pair limit to
		// divide the spend by (limits exist only per gateway
		// key), so utilisation is unknown, never invented.
		// ExpiresAt stays zero: no quota-expiry data exists
		// anywhere. A ledger error leaves the fields unknown —
		// a telemetry miss must never fail routing.
		if led != nil {
			// Memoised (10s TTL, see pairQuotaCache): one ledger
			// scan per pair per window, not one per candidate
			// per request.
			if pq, ok := pqCache.get(context.Background(), providerID, model, time.Now()); ok {
				sig.Util5h = pq.Util5h
				sig.Util7d = pq.Util7d
				sig.ResetAt = pq.ResetAt
			}
		}
		// Load (M3 Phase C): the engine's adaptive limiter state
		// for this provider — in-flight / live limit, 0..1. No
		// limiter state (or a non-positive limit) leaves Load 0:
		// unknown-or-idle, which scores the same (no penalty).
		if engine != nil {
			for _, st := range engine.ConcurrencyStats() {
				if st.Provider != providerID || st.Limit <= 0 {
					continue
				}
				if st.InFlight > 0 {
					sig.Load = float64(st.InFlight) / st.Limit
					if sig.Load > 1 {
						sig.Load = 1
					}
				}
				break
			}
		}
		// // Breaker state (M3 Area 1, PARITY row 394): the
		// engine's live breaker for this provider, so the
		// sla-aware hook can filter OPEN candidates before
		// scoring (and the 16-factor health factor can read
		// it), and so the LKGP hook can refuse to hoist a
		// pin to a DEGRADED provider (RT-2 follow-up:
		// BreakerDegraded is breakerStateOf's DEGRADED
		// state — slowing/failing, not yet OPEN). Guarded
		// on a live engine exactly like the Load block
		// above; false means "not open / not degraded OR
		// unknown" and scores normally.
		if engine != nil {
			sig.BreakerOpen = engine.BreakerOpenForProvider(providerID)
			sig.BreakerDegraded = engine.BreakerDegradedForProvider(providerID)
		}
		// Live attempt stats from the engine (M3 Phase C):
		// success rate, EWMA latency (p50 proxy), true p95
		// from histogram (B9), attempts. When samples > 0
		// these are authoritative — skip the probe fallback.
		if engine != nil {
			if st, ok := engine.ProviderStats(providerID); ok && st.Samples > 0 {
				if st.LatencyMs > 0 {
					sig.LatencyMs = st.LatencyMs
				}
				if st.LatencyP95Ms > 0 {
					sig.LatencyP95Ms = st.LatencyP95Ms
				}
				sig.SuccessRate = st.SuccessRate
				// Measured quality: the error-free completion
				// rate over real attempts. Same record as the
				// success component — the two components weigh
				// it differently per strategy, and both are
				// measured, never claimed.
				sig.QualityScore = st.SuccessRate
				// Attempt count from the same live record as
				// SuccessRate (M3 Phase C): the least-used
				// strategy's input. Per provider, not per
				// model — the engine's attempt record is
				// keyed by provider, exactly like SuccessRate.
				sig.Attempts = st.Attempts
				// Live attempt stats are authoritative: skip the
				// probe fallback below entirely.
				applyLearnedQuota()
				return sig
			}
		}
		// Probe fallback: no live stats yet (or engine nil).
		// The checker's last probe for this provider gives
		// latency and a success/failure hint. It does NOT
		// provide a true p95 or a measured success rate —
		// those are only available from the engine's attempt
		// record. Unknown stays unknown.
		for _, st := range checker.Last() {
			if st.ProviderID != providerID {
				continue
			}
			if st.LatencyMs > 0 {
				sig.LatencyMs = float64(st.LatencyMs)
			}
			if !st.CheckedAt.IsZero() {
				if st.OK {
					sig.SuccessRate = 1.0
				} else {
					sig.SuccessRate = 0
				}
			}
			if st.QuotaHint >= 0 {
				sig.QuotaHint = int(st.QuotaHint*100 + 0.5)
				// Same measured fraction as a percentage for
				// the 16-factor quota factor (G19 Area 1):
				// the probe hint is the only quota source
				// when the registry holds none.
				if sig.QuotaRemainingPct < 0 {
					sig.QuotaRemainingPct = st.QuotaHint * 100
				}
			}
			break
		}
		applyLearnedQuota()
		return sig
	}
	// Cost/latency/quality-aware auto-router tier ordering: the
	// per-tier candidate models are ranked by the same live signals
	// the strategies use — real cost from the price book, measured
	// latency/success from the engine's live provider stats, and
	// the measured quality signal (error-free completion rate from
	// real attempts) — under the 16-factor weights
	// (DefaultWeights16), instead of list order. 16-factor, not
	// cheapest/fastest: the tier already encodes the virtual
	// model's axis (auto/cheap restricts to the cheap tiers,
	// auto/fast to the fast ones), so the ranking itself weighs
	// quota, health, cost, latency, reliability and quality
	// across all 16 factors. A model with no price is neutral on
	// cost; a model whose providers have no attempts yet is
	// unknown on latency/success/quality.
	//
	// The ranking is per (provider, model) PAIR, not per model:
	// each model is scored by its best-scoring pair (see
	// scoring.RankModelsByBestPair16), so a model that is cheap at
	// one provider and expensive at another ranks by its cheap
	// pair — the pair the router would actually serve it through.
	// A provider with no model list (chat-only gateway) is a
	// candidate for any model, as in chain building; a provider
	// with no live client is not a candidate at all. A model no
	// enabled provider offers is scored on its own signals (the
	// model-level price fallback, no provider stats) instead of
	// being dropped.
	// hasClient reports whether any key of the provider has a
	// registered engine client (CHECKLIST 175): a provider is
	// routable while at least one of its keys has a client, even
	// when other keys failed to build.
	hasClient := func(p *provider.Provider) bool {
		if _, ok := clients[router.ClientKey(p.ID, "")]; ok {
			return true
		}
		for _, k := range p.EffectiveKeys() {
			if _, ok := clients[router.ClientKey(p.ID, k.ID)]; ok {
				return true
			}
		}
		return false
	}
	// attemptsForProvider expands one provider into one attempt
	// per registered key (CHECKLIST 175): preferred keys in pool
	// order, then reserve keys, each marked Reserve so the
	// engine's tail partition holds the reserve back until every
	// preferred key is exhausted. Keys with no registered client
	// are skipped — a key that failed to build never appears in
	// a chain. A no-auth provider (empty pool) yields one legacy
	// attempt under the bare provider identity.
	attemptsForProvider := func(p *provider.Provider, model string) []router.Attempt {
		var out []router.Attempt
		for _, k := range p.EffectiveKeys() {
			if _, ok := clients[router.ClientKey(p.ID, k.ID)]; !ok {
				continue
			}
			out = append(out, router.Attempt{
				ProviderID: p.ID,
				Model:      model,
				KeyID:      k.ID,
				Reserve:    k.Role == provider.KeyRoleReserve,
			})
		}
		if len(out) == 0 {
			if _, ok := clients[router.ClientKey(p.ID, "")]; ok {
				out = append(out, router.Attempt{ProviderID: p.ID, Model: model})
			}
		}
		return out
	}
	rankModels := func(tier autorouter.Tier, models []string) []string {
		offers := func(model string) []scoring.Candidate {
			var out []scoring.Candidate
			for _, p := range reg.List() {
				if !p.Enabled {
					continue
				}
				// Removed providers never rank (G19 Area 1,
				// CT-2): a removed variant cannot win the
				// auto pool even if it still offers the model.
				if _, ok := p.IsRemovedVariant(); ok {
					continue
				}
				if !hasClient(p) {
					continue
				}
				if len(p.Models) == 0 {
					out = append(out, scoring.Candidate{ProviderID: p.ID, Model: model})
					continue
				}
				for _, m := range p.Models {
					if m.ID == model {
						out = append(out, scoring.Candidate{ProviderID: p.ID, Model: model})
						break
					}
				}
			}
			return out
		}
		// 16-factor ranking (G19 Area 1, PARITY rows \"auto
		// 16-factor scoring\" / \"Router strategy rules\" /
		// \"Router strategy score\"): each model inherits its
		// best pair's Score16 under DefaultWeights16 — quota,
		// health, cost, latency, taskFit, stability, tiers,
		// specificity, context/cache/session affinity,
		// reset-window affinity, connection density, quality,
		// reliability — instead of the 5-factor balanced
		// score. The inputs come from the live signals
		// closure, so the producer fields signalsFn fills
		// (breaker state, remaining quota, connection pool,
		// context window) all vote here. Session affinity
		// comes from the LKGP record: the last-known-good
		// provider/model scores 1.0 on the same pair, 0.7 on
		// the same provider, 0.3 elsewhere; no record leaves
		// the factor neutral. The LKGP record is filtered
		// here, at the call site below (B7 RT-2):
		// LatestHealthyRecord drops expired pins (TTL) and
		// skips breaker-OPEN/DEGRADED pairs per signalsFn,
		// so a dead or degrading provider never wins
		// stickiness here. (An earlier comment claimed the
		// record was "already breaker-filtered at lookup
		// time" — that was wrong: LatestRecord checked
		// neither breaker state nor the strategy hook, which
		// guards a different path. G19 nit 1.) Cache warmth is NOT fed here: the
		// warmth snapshot is constructed below this closure
		// in source order, and a fresh ledger read per pair
		// would defeat its purpose — the per-request chain
		// hook (StrategyHookForRequest) carries warmth for
		// the chain that actually runs.
		return scoring.RankModelsByBestPair16(models, offers, func(c scoring.Candidate) scoring.SixteenInputs {
			in := strategy.SixteenInputsFromSignals(signalsFn(c.ProviderID, c.Model))
			// Live intelligence (B2-14): the TaskFit factor reads
			// the Intelligence store — user_override > arena_elo
			// > models_dev_tier — for this model at the default
			// task category. The ranking closure has no
			// per-request task category, so the default row
			// votes (the text leaderboard and every tier both
			// carry one); the per-request hook layers a category
			// when one exists. A miss leaves the factor on its
			// tag Jaccard, never on an invented score.
			if score, _, ok := intel.TaskFit(c.Model, modelsync.CategoryDefault); ok {
				in.TaskFitScore = score
				in.TaskFitKnown = true
			}
			// Session affinity comes from the last-known-good
			// record that is still HEALTHY (B7 RT-2): a DEGRADED
			// or OPEN pin is skipped, falling back to the most
			// recent healthy record — a degrading provider must
			// not hoist above healthy targets on the
			// session-affinity boost. TTL filtering is inside
			// LatestHealthyRecord, as in LatestRecord.
			if rec, ok := strategy.DefaultLKGPRegistry.LatestHealthyRecord(signalsFn); ok {
				in.SessionProvider = rec.Provider
				in.SessionModel = rec.Model
			}
			return in
		}, scoring.DefaultWeights16)
	}

	// Routing engine: fallback chains come from the registry — enabled
	// providers that offer the requested model, in registry order.
	// Providers with no model list (chat-only gateways) are candidates
	// for any model; the upstream rejects unknown models and the
	// engine fails over. The strategy hook reorders the chain by the
	// resolved strategy (global, per-chain override, or per-request
	// "auto:<axis>" alias); it never drops or duplicates an attempt.
	//
	// Auto-router ("auto/*" virtual models): the local auto-router
	// (internal/autorouter) embeds the prompt once with the same
	// deterministic hashed bag-of-words embedding as the semantic
	// cache, cosine-matches it against curated per-tier example
	// utterances, and resolves the virtual model to a concrete
	// model — in milliseconds, no LLM call, no network call. Below
	// the configured threshold it falls back to the default tier /
	// strong model instead of guessing; with no concrete model
	// configured for a tier it fails honestly. Tier models come
	// from config auto_router.tier_models; when DefaultModel is
	// empty it falls back to the first model of the first enabled
	// provider, so a fresh install with providers still routes.
	autoRouter := buildAutoRouter(cfg, reg, priceBook, rankModels)
	// Region-aware failover order (CHECKLIST 257): after the
	// same-model providers, the chain appends the operator's
	// declared equivalents for this model
	// (routing.model_equivalents, in preference order), each
	// resolved against the providers that offer it — so the
	// engine's tier order (same model in another region, then
	// equivalent model, then self-hosted) has targets to step
	// to. A model with no declared equivalents appends nothing:
	// equivalence is never guessed. Attempts are deduped by
	// exact (provider, model), so the same target is never
	// tried twice in one chain.
	chainFor := func(model string) []router.Attempt {
		var chain []router.Attempt
		seen := make(map[router.Attempt]bool)
		add := func(p *provider.Provider, m string) {
			// Removed providers never enter a chain (G19
			// Area 1, CT-2): a removed variant cannot be
			// served even if it still offers the model.
			// This is the choke point for both the direct
			// loop and the equivalents loop below.
			if _, ok := p.IsRemovedVariant(); ok {
				return
			}
			for _, att := range attemptsForProvider(p, m) {
				if !seen[att] {
					seen[att] = true
					chain = append(chain, att)
				}
			}
		}
		offers := func(p *provider.Provider, m string) bool {
			if len(p.Models) == 0 {
				return true
			}
			for _, mm := range p.Models {
				if mm.ID == m {
					return true
				}
			}
			return false
		}
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			if !hasClient(p) {
				continue
			}
			if offers(p, model) {
				add(p, model)
			}
		}
		// M8.14 race fix, round 2: read the equivalents from a
		// RoutingSnapshot taken under the routing read lock —
		// the old code derefed cfg.Routing directly, racing
		// routingPersist's pointer swap.
		if rc := cfg.RoutingSnapshot(); rc != nil {
			for _, equiv := range rc.ModelEquivalents[model] {
				for _, p := range reg.List() {
					if !p.Enabled {
						continue
					}
					if !hasClient(p) {
						continue
					}
					if offers(p, equiv) {
						add(p, equiv)
					}
				}
			}
		}
		// Region-aware failover order (CHECKLIST 257-258), step
		// 3 — the self-hosted last resort: a self-hosted
		// open-weight model is NOT an equivalent of the
		// requested model, so the loops above never include
		// it — without this append the last tier could never
		// fire. Every model offered by a self-hosted provider
		// (catalog local, or Ollama wire — see
		// isLocalProvider) is appended here, deduped, in
		// registry order. The region tiering in the engine
		// (applyRegionTiers, tier 3) keeps them last and the
		// primary first whatever order they land in, and
		// MaxAttempts bounds how many are actually tried.
		// A self-hosted provider with no model list already
		// serves the requested model via the first loop
		// (offers() is true for any model), so there is
		// nothing to enumerate for it.
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			if !hasClient(p) {
				continue
			}
			if !isLocalProvider(p) {
				continue
			}
			for _, m := range p.Models {
				add(p, m.ID)
			}
		}
		return chain
	}
	// Context-window pre-call fit check (CHECKLIST 159): the
	// engine compares the locally counted prompt (+ a response
	// reserve) against each candidate's context window before
	// dialling. Windows come from the registry's model metadata
	// (0 = unknown — an unknown window never removes an
	// attempt). The dedicated fallback chain is the operator's
	// configured long-context models, resolved against the
	// providers that offer each one, in config order; a model
	// no provider offers contributes nothing.
	contextWindow := func(providerID, model string) (int, bool) {
		p, ok := reg.Get(providerID)
		if !ok {
			return 0, false
		}
		for _, m := range p.Models {
			if m.ID == model {
				return m.ContextWindow, m.ContextWindow > 0
			}
		}
		return 0, false
	}
	contextFallbackChain := func(model string) []router.Attempt {
		_ = model // the dedicated chain is global, not per-model
		var chain []router.Attempt
		// M8.14 race fix, round 2: read the fallback models
		// from a RoutingSnapshot taken under the routing read
		// lock — the old code derefed cfg.Routing directly,
		// racing routingPersist's pointer swap.
		rc := cfg.RoutingSnapshot()
		if rc == nil {
			return chain
		}
		for _, fbModel := range rc.ContextFallbackModels {
			for _, p := range reg.List() {
				if !p.Enabled {
					continue
				}
				if !hasClient(p) {
					continue
				}
				if len(p.Models) == 0 {
					continue
				}
				for _, m := range p.Models {
					if m.ID == fbModel {
						chain = append(chain, attemptsForProvider(p, fbModel)...)
						break
					}
				}
			}
		}
		return chain
	}
	// Routing strategies (slice 1): the global strategy, per-chain
	// overrides and the per-combo deadline budget come from the
	// config file's routing block; the per-request "auto:<axis>"
	// alias still wins over both (resolved in the engine). The
	// StrategyHook factory ranks each resolved strategy by the
	// live signals above (strategy.HookFactory); the old fixed
	// ModeBalanced OrderHook is gone — balanced is now just one
	// strategy name among the family, and the default when
	// nothing is configured.
	routing := cfg.Routing
	customWeights := scoring.Weights{}
	if routing != nil && routing.WeightedWeights != nil {
		w := routing.WeightedWeights
		customWeights = scoring.Weights{Latency: w.Latency, Success: w.Success, Cost: w.Cost, Quality: w.Quality, Quota: w.Quota}
	}
	// Per-target chain weights for the "weighted" strategy (M3
	// Phase C): installed process-wide before the engine is built,
	// so every weighted draw reads the operator's table.
	if routing != nil {
		strategy.SetChainWeights(routing.ChainWeights)
	}
	// Cache warmth for the cache-optimized strategy (M6.5 wiring):
	// a 1-minute snapshot of ledger.CachedTokenFraction over the
	// last hour, so the hook serves memoized fractions and never
	// runs a fresh ledger query per chain target (see
	// cacheWarmthSnapshot). The per-request hook is preferred over
	// StrategyHook when set; StrategyHook stays as the fallback
	// for context-free callers.
	warmthSnap := newCacheWarmthSnapshot(time.Minute, func(provider, model string) (float64, bool) {
		frac, ok, err := led.CachedTokenFraction(context.Background(), provider, model, time.Now().Add(-time.Hour))
		if err != nil {
			return 0, false
		}
		return frac, ok
	})
	// relayClientFor returns the live client for a provider: the
	// bare (no-auth) client first, else the first key with a
	// registered client. Mirrors hasClient below, which only
	// reports existence.
	relayClientFor := func(clients map[string]provider.Client, p *provider.Provider) (provider.Client, bool) {
		if c, ok := clients[router.ClientKey(p.ID, "")]; ok {
			return c, true
		}
		for _, k := range p.EffectiveKeys() {
			if c, ok := clients[router.ClientKey(p.ID, k.ID)]; ok {
				return c, true
			}
		}
		return nil, false
	}
	// parseRelayHandoffSummary splits a summariser reply into
	// handoff fields on its section headers. Lenient on purpose:
	// unknown lines join the current section, a missing header
	// leaves that field empty, and a reply with no headers at
	// all lands whole as the summary — it is the model's own
	// words either way, never invented here.
	parseRelayHandoffSummary := func(text string) router.RelayHandoff {
		var h router.RelayHandoff
		section := "summary"
		var summary, progress []string
		var decisions, entities []string
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if idx := strings.Index(trimmed, ":"); idx > 0 {
				switch strings.ToUpper(trimmed[:idx]) {
				case "SUMMARY":
					section = "summary"
					trimmed = strings.TrimSpace(trimmed[idx+1:])
				case "PROGRESS":
					section = "progress"
					trimmed = strings.TrimSpace(trimmed[idx+1:])
				case "DECISIONS":
					section = "decisions"
					trimmed = strings.TrimSpace(trimmed[idx+1:])
				case "ENTITIES":
					section = "entities"
					trimmed = strings.TrimSpace(trimmed[idx+1:])
				}
			}
			switch section {
			case "summary":
				summary = append(summary, trimmed)
			case "progress":
				progress = append(progress, trimmed)
			case "decisions":
				if trimmed != "" {
					decisions = append(decisions, strings.TrimSpace(strings.TrimLeft(trimmed, "- ")))
				}
			case "entities":
				for _, e := range strings.Split(trimmed, ",") {
					if e = strings.TrimSpace(e); e != "" {
						entities = append(entities, e)
					}
				}
			}
		}
		h.Summary = strings.TrimSpace(strings.Join(summary, "\n"))
		h.TaskProgress = strings.TrimSpace(strings.Join(progress, "\n"))
		h.KeyDecisions = decisions
		h.ActiveEntities = entities
		if h.Summary == "" && strings.TrimSpace(text) != "" {
			h.Summary = strings.TrimSpace(text)
		}
		return h
	}
	// Production context-relay summariser (Phase D, D5 — B2-01):
	// the cheapest priced model with a live client summarises the
	// relayed messages through a direct provider call — the
	// router's own cheapest available model path, picked fresh on
	// every call from the price book so runtime provider changes
	// apply immediately. Unpriced models are never cheapest; a
	// provider with no live client is skipped. No candidate, a
	// failed call, or an empty reply is an error, so the handoff
	// degrades honestly to the raw transcript — never a stub,
	// never a canned summary. The call is bounded (30 s, 512
	// output tokens) and dials the provider directly, never the
	// engine, so it cannot re-enter the attempt loop that called
	// it.
	relaySummariser := func(ctx context.Context, messages []router.RelayMessage) (router.RelayHandoff, error) {
		var (
			bestClient provider.Client
			bestModel  string
			bestPrice  float64
		)
		if priceBook != nil {
			for _, p := range reg.List() {
				if !p.Enabled {
					continue
				}
				client, ok := relayClientFor(clients, p)
				if !ok {
					continue
				}
				for _, m := range p.Models {
					price, ok := priceBook.BlendedUSDPer1M(p.ID, m.ID)
					if !ok || price <= 0 {
						continue
					}
					if bestClient == nil || price < bestPrice {
						bestClient, bestModel, bestPrice = client, m.ID, price
					}
				}
			}
		}
		if bestClient == nil {
			return router.RelayHandoff{}, fmt.Errorf("relay summariser: no priced model with a live client")
		}
		var sb strings.Builder
		for i, m := range messages {
			content := strings.TrimSpace(m.Content)
			if content == "" {
				continue
			}
			role := m.Role
			if role == "" {
				role = "unknown"
			}
			fmt.Fprintf(&sb, "[%d] %s:\n%s\n\n", i+1, strings.ToUpper(role), content)
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		resp, err := bestClient.Chat(callCtx, provider.ChatRequest{
			Model: bestModel,
			Messages: []provider.Message{
				{Role: "system", Content: "You are a concise conversation summariser. Summarise the conversation below for a handoff to another model that will continue the work. Reply ONLY with these sections, in this order:\nSUMMARY: <2-4 sentences: what the user is doing and where things stand>\nPROGRESS: <what is done and what remains>\nDECISIONS: <one key decision per line, each starting with '- '>\nENTITIES: <names, files, identifiers that matter, comma-separated>\nDo not invent anything not in the conversation. If a section has nothing to say, leave it empty."},
				{Role: "user", Content: sb.String()},
			},
			MaxTokens: 512,
		})
		if err != nil {
			return router.RelayHandoff{}, fmt.Errorf("relay summariser: %s: %w", bestModel, err)
		}
		handoff := parseRelayHandoffSummary(resp.Content)
		if strings.TrimSpace(handoff.Summary) == "" {
			return router.RelayHandoff{}, fmt.Errorf("relay summariser: %s returned an empty summary", bestModel)
		}
		handoff.MessageCount = len(messages)
		handoff.Model = bestModel
		return handoff, nil
	}
	engCfg := router.Config{
		ChainFor:  chainFor,
		QuotaHint: quotaHint,
		// Context-relay production quota signal (Phase D, D5 —
		// B2-01): the engine's learned remaining-quota
		// fraction — measured from real upstream rate-limit
		// headers on every response (LearnedQuotaHint, the
		// same measured source :495/:497 wired into
		// signalsFn) — converted to the used fraction the
		// relay's success trigger needs ([0.85, 0.95)). No
		// new signal, no invented number: unknown (no learned
		// entry) stays unknown.
		RelayQuotaUsedFor: func(providerID, model string) (float64, bool) {
			if engine == nil {
				return 0, false
			}
			hint, _, ok := engine.LearnedQuotaHint(providerID)
			if !ok {
				return 0, false
			}
			used := (100 - float64(hint)) / 100
			if used < 0 {
				used = 0
			}
			if used > 1 {
				used = 1
			}
			return used, true
		},
		// Context-relay production summariser (Phase D, D5 —
		// B2-01): the cheapest priced model with a live
		// client, called directly — see relaySummariser
		// above. Never a stub, never a canned summary.
		RelaySummariser:    relaySummariser,
		VisionCapability:   router.VisionCapabilityFunc(reg),
		VisionPool:         router.VisionPoolFunc(reg),
		ModalityCapability: router.ModalityCapabilityFunc(reg),
		ModalityPool:       router.ModalityPoolFunc(reg),
		// P1.5 capability gate: a live lookup (never a stale snapshot)
		// plus the operator's verified-capability requirements from
		// config. Empty requirements leave the chain untouched.
		CapabilityLookup:       router.CapabilityLookupFromRegistry(reg),
		RequireCapabilities:    requiredCapabilities(cfg),
		StrategyHook:           strategy.HookFactory(signalsFn, customWeights),
		StrategyHookForRequest: strategy.HookFactoryWithCacheAffinity(signalsFn, customWeights, warmthSnap.warmth),
		// LKGP failure-clear (RT-2): a hard failure against
		// the pinned provider drops that chain's pin, so the
		// next request stops hoisting a dead target. The
		// clear is provider-gated inside ClearIfProvider —
		// a failure against one provider never drops
		// another's pin — and slow degradation does NOT
		// clear (undecided: see the slice report).
		LKGPInvalidator: func(chainKey, provider string) {
			strategy.DefaultLKGPRegistry.Store().ClearIfProvider(chainKey, provider)
		},
		ContextWindow:        contextWindow,
		ContextFallbackChain: contextFallbackChain,
		// Region-aware failover order (CHECKLIST 257): the
		// region comes from the registry's operator-declared
		// provider/model entries (never guessed — see
		// provider.Provider.RegionFor), and a provider counts
		// as self-hosted only on positive identification: a
		// catalog local provider (ollama-local, lm-studio,
		// vllm, llama-cpp, ...) or Ollama wire format (see
		// isLocalProvider). A loopback base URL alone does NOT
		// make a provider self-hosted: a loopback
		// OpenAI-compatible endpoint may be a local proxy
		// forwarding to a hosted provider, and the tier asks
		// "is this an open-weight model served locally", not
		// "does data leave this machine" (the
		// data_collection=deny rule's looser loopback test
		// answers that different question). A custom loopback
		// server with no catalog entry and no Ollama wire is
		// an ordinary provider — tried in its model tier,
		// never demoted to last on a URL guess.
		RegionFor: func(providerID, model string) string {
			p, ok := reg.Get(providerID)
			if !ok {
				return ""
			}
			return p.RegionFor(model)
		},
		// Effort→budget table (CHECKLIST 285-286, Vertex 303-304): the
		// operator-declared `reasoning.effort_budgets` from the
		// config file, consulted only for the Anthropic/Gemini/
		// Vertex wires. No table in the file means no budget is ever
		// resolved — those wires send no thinking field.
		EffortBudgetFor: func(wire, effort string) (int, bool) {
			if cfg.Reasoning == nil || cfg.Reasoning.EffortBudgets == nil {
				return 0, false
			}
			return cfg.Reasoning.EffortBudgets.Lookup(wire, effort)
		},
		// Forced-reasoning policy (CHECKLIST 311-312): the
		// operator-declared `reasoning.forced_effort` rules from
		// the config file. No block in the file means no rule
		// ever matches — tier, group-entry and client efforts
		// apply as before.
		ForcedEffortFor: func(model string) (router.ForcedEffort, bool) {
			if cfg.Reasoning == nil {
				return router.ForcedEffort{}, false
			}
			pol, ok := cfg.Reasoning.ForcedEffortFor(model)
			if !ok {
				return router.ForcedEffort{}, false
			}
			return router.ForcedEffort{Model: pol.Model, Effort: pol.Effort, AllowClientOverride: pol.AllowClientOverride}, true
		},
		SelfHosted: func(providerID string) bool {
			p, ok := reg.Get(providerID)
			if !ok {
				return false
			}
			return isLocalProvider(p)
		},
		// Breaker auth profiles (M4.2/M4.9): the profile table
		// is selected per provider from the registry's auth
		// kind — OAuth providers trip at 8, API-key at 12,
		// local runtimes at 2. A local runtime (catalog local
		// category or Ollama wire) always takes the local
		// profile, whatever its auth kind says; an unknown
		// provider takes the API-key profile.
		AuthProfileFor: func(providerID string) router.AuthProfile {
			p, ok := reg.Get(providerID)
			if !ok {
				return router.AuthProfileAPIKey
			}
			if isLocalProvider(p) {
				return router.AuthProfileLocal
			}
			return router.AuthProfileForKind(provider.AuthKindFor(p))
		},
		// Per-key token limits (M5.3): the engine consults
		// this before each attempt — the provider/model are
		// only known per attempt, so the check cannot live in
		// the server's pre-call gate (which covers global +
		// model scopes for the requested model). A key with no
		// token limits returns nil: the engine skips the check.
		TokenLimitCheck: func(ctx context.Context, keyID, providerID, model string) (*router.TokenLimitBreach, error) {
			key, ok := cfg.FindGatewayKey(keyID)
			if !ok || len(key.TokenLimits) == 0 {
				return nil, nil
			}
			limits := make([]ledger.TokenLimitConfig, 0, len(key.TokenLimits))
			for _, tl := range key.TokenLimits {
				limits = append(limits, ledger.TokenLimitConfig{
					Scope:         tl.Scope,
					ScopeValue:    tl.ScopeValue,
					Limit:         tl.Limit,
					ResetInterval: tl.ResetInterval,
					ResetTime:     tl.ResetTime,
					Enabled:       tl.IsEnabled(),
				})
			}
			breach, err := led.CheckKeyTokenLimits(ctx, keyID, providerID, model, limits, time.Now())
			if err != nil || breach == nil {
				return nil, err
			}
			return &router.TokenLimitBreach{
				Scope:      breach.Scope,
				ScopeValue: breach.ScopeValue,
				Limit:      breach.Limit,
				Used:       breach.Used,
				NextReset:  breach.NextReset,
			}, nil
		},
		// Internal budgets, provider scope (M5.11): the engine
		// consults this before each attempt — the provider is
		// only known per attempt, so the check cannot live in
		// the server's pre-call gate (which covers global /
		// model / pool scopes). Applies to anonymous requests
		// too: the cap is on the operator's total spend.
		InternalBudgetCheck: func(ctx context.Context, providerID string) (*router.InternalBudgetBreach, error) {
			ev, err := led.CheckInternalBudgetForProvider(ctx, providerID, time.Now())
			if err != nil || ev == nil || ev.Decision != ledger.InternalBudgetDeny {
				return nil, err
			}
			return &router.InternalBudgetBreach{
				BudgetID:  ev.Budget.ID,
				Provider:  providerID,
				LimitType: ev.Budget.LimitType,
				Limit:     ev.Budget.LimitValue,
				Used:      ev.Used,
				Remaining: ev.Remaining,
				Period:    ev.Budget.Period,
			}, nil
		},
		// Upstream rate-limit learning (M5.13): the ledger
		// persists learned signals per (provider, key); the
		// engine paces the adaptive limiter from them live and
		// reloads the fresh ones at startup below.
		RateLimitStore: led,
	}
	// Breaker persistence (G11 Area 2, parity tracker:524): the
	// consecutive-breaker state persists to DataDir on every
	// state change and reloads at engine start, so a restart no
	// longer forgets every trip — the store machinery in
	// internal/router/breaker_store.go was never enabled in
	// production because no path was ever configured. Empty
	// DataDir means no file (the old in-memory-only behaviour)
	// — never a relative path into the working directory.
	if cfg.DataDir != "" {
		engCfg.BreakerStorePath = filepath.Join(cfg.DataDir, "breaker_state.json")
	}
	// E1 limiter verdict (CHECKLIST 233): CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1
	// disables the Gradient2 adaptive limiter for measurement only
	// (limiter-off vs limiter-on back-to-back on the same tree, via
	// scripts/bench-e1-limiter.sh). With the limiter off, overload
	// queues instead of shedding — this is a bench switch, not a
	// tuning recommendation, and it is never set by default.
	if os.Getenv("CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER") == "1" {
		engCfg.DisableAdaptiveLimiter = true
	}
	// Per-request routing contract (CHECKLIST 157): the price
	// source for routing.max_price and the training-data
	// verdict for routing.data_collection=deny.
	//
	// ContractPriceFunc is the operator-configured price book
	// (pair price first, model-level fallback — the Lookup
	// precedence): a pair with no entry is unknown, never
	// assumed cheap, so an unpriced pair is excluded by a
	// ceiling rather than admitted under it.
	if priceBook != nil {
		engCfg.ContractPriceFunc = func(providerID, model string) (config.ModelPrice, bool) {
			return priceBook.Lookup(providerID, model)
		}
	}
	// TrainingSafe vouches for a provider only on one of two
	// grounds the router can actually verify: the operator
	// asserted no_training_data for it in the config, or the
	// data never leaves this machine — a catalog local provider,
	// or a base URL on a loopback host. Everything else is
	// "not verified" and routing.data_collection=deny excludes
	// it: the router ships no vendor training-policy database,
	// so no vendor is ever guessed safe from its name or docs.
	engCfg.TrainingSafe = func(providerID string) bool {
		p, ok := reg.Get(providerID)
		if !ok {
			return false
		}
		if p.NoTrainingData {
			return true
		}
		if p.CatalogID != "" {
			if entry, ok := catalog.ByID(p.CatalogID); ok && entry.Category == catalog.CategoryLocal {
				return true
			}
		}
		return loopbackHost(p.BaseURL)
	}
	// ResolveProvider maps the names clients use in a
	// per-request routing contract to registry IDs (CHECKLIST
	// 157): an exact ID, then an exact display name, then a
	// case-insensitive display name. Only enabled providers
	// with a live client resolve — a disabled provider has no
	// chain to be ordered into, so it resolves to nothing and
	// the contract fails with "unknown provider" instead of
	// silently dropping it. An ambiguous display name (two
	// providers sharing it) is an error, never a guess.
	engCfg.ResolveProvider = func(name string) (string, error) {
		for _, p := range reg.List() {
			if p.ID == name {
				if hasClient(p) && p.Enabled {
					return p.ID, nil
				}
				return "", fmt.Errorf("unknown provider %q", name)
			}
		}
		var matches []string
		for _, p := range reg.List() {
			if !hasClient(p) || !p.Enabled {
				continue
			}
			if p.Name == name {
				matches = append(matches, p.ID)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("ambiguous provider name %q (%d providers share it — use the provider ID)", name, len(matches))
		}
		lower := strings.ToLower(name)
		matches = matches[:0]
		for _, p := range reg.List() {
			if !hasClient(p) || !p.Enabled {
				continue
			}
			if strings.ToLower(p.Name) == lower {
				matches = append(matches, p.ID)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("ambiguous provider name %q (%d providers share it — use the provider ID)", name, len(matches))
		}
		return "", fmt.Errorf("unknown provider %q", name)
	}
	if routing != nil && routing.ContextReserveTokens > 0 {
		engCfg.ContextReserveTokens = routing.ContextReserveTokens
	}
	// Lossless-first compression (CHECKLIST 183): the
	// operator's per-model lossless list, from the config
	// file's compression block. Nil means no model compresses
	// by default; a per-request opt-in still applies to any
	// model, and lossy is never configured here.
	if cfg.Compression != nil {
		engCfg.CompressionLosslessModels = cfg.Compression.LosslessModels
	}
	if routing != nil {
		engCfg.Strategy = routing.GlobalStrategy
		engCfg.ChainStrategy = func(model string) string {
			// M8.14 race fix: read through the shared
			// routing read lock (ChainStrategy locks
			// internally) — a combo mirror may rewrite
			// the map on this request path. This closure
			// is a reviewer-missed reader.
			if st, ok := cfg.ChainStrategy(model); ok {
				return st
			}
			return ""
		}
		// Nestable strategy trees (CHECKLIST 177): the global
		// tree orders every chain, a per-chain tree wins for
		// its model. The config was already validated at load,
		// so the engine receives known-good trees.
		engCfg.StrategyTree = routing.StrategyTree
		if routing.StrategyTrees != nil {
			trees := routing.StrategyTrees
			engCfg.ChainStrategyTree = func(model string) *strategytree.Node {
				return trees[model]
			}
		}
		if routing.DeadlineBudgetMs > 0 {
			engCfg.DeadlineBudget = time.Duration(routing.DeadlineBudgetMs) * time.Millisecond
		}
		// Stream-lifetime cap (CHECKLIST 255): the config value
		// maps 1:1 onto the engine's — 0 means the 30-minute
		// default, negative disables the cap. It is set
		// unconditionally (not only when positive) so an explicit
		// negative in the config file actually disables the cap
		// instead of silently keeping the default.
		engCfg.StreamMaxLifetime = time.Duration(routing.StreamMaxLifetimeMs) * time.Millisecond
		// M4.9 failover knobs: the routing block maps 1:1 onto
		// the engine's config. Zero values keep the engine
		// defaults (MaxAttempts 5 via withDefaults, per-target
		// timeout 120 s, TTFT prediction off, lockout off,
		// wait budgets off, preferred failover off); the config
		// was validated at load, so the values here are
		// known-good.
		if routing.MaxAttempts != 0 {
			engCfg.MaxAttempts = routing.MaxAttempts
		}
		if routing.PerTargetTimeoutMs != 0 {
			engCfg.PerTargetTimeout = time.Duration(routing.PerTargetTimeoutMs) * time.Millisecond
		}
		if routing.PredictiveTtftMs != 0 {
			engCfg.PredictiveTtftMs = routing.PredictiveTtftMs
		}
		if routing.PreferredFailoverAfterMs != 0 {
			engCfg.PreferredFailoverAfter = time.Duration(routing.PreferredFailoverAfterMs) * time.Millisecond
		}
		// 200-OK body validation predicate (:523, G11
		// Area 2): the config file's routing.response_
		// validation maps 1:1 onto the engine's. A nil
		// block keeps the built-in error-payload predicate,
		// enabled.
		if rv := routing.ResponseValidation; rv != nil {
			engCfg.ResponseValidation = router.ResponseValidationConfig{
				Disabled:            rv.Disabled,
				ForbiddenSubstrings: rv.ForbiddenSubstrings,
				RequiredSubstrings:  rv.RequiredSubstrings,
				MinContentLength:    rv.MinContentLength,
			}
		}
		// Readiness-deadline failure sub-budget (B2-28):
		// 0 keeps the engine default (3); a negative value
		// disables the sub-budget (see router.Config).
		if routing.DeadlineFailureMax != 0 {
			engCfg.DeadlineFailureMax = routing.DeadlineFailureMax
		}
		// Cross-provider default (B2-29): 0 keeps the
		// engine default (3 distinct providers).
		if routing.MaxProvidersPerRequest != 0 {
			engCfg.MaxProvidersPerRequest = routing.MaxProvidersPerRequest
		}
		// Failover knobs (B2-30, G11 Area 2): 0 keeps the
		// engine defaults (3 retries, 200 ms delay, no set
		// cap, same-target-first). The config was validated
		// at load, so the values here are known-good.
		if routing.MaxRetries != 0 {
			engCfg.FailoverMaxRetries = routing.MaxRetries
		}
		if routing.RetryDelayMs != 0 {
			engCfg.FailoverRetryDelay = time.Duration(routing.RetryDelayMs) * time.Millisecond
		}
		if routing.MaxSetRetries != 0 {
			engCfg.FailoverMaxSetRetries = routing.MaxSetRetries
		}
		engCfg.FailoverBeforeRetry = routing.FailoverBeforeRetry
		// Hedging (B2-26): 0 keeps the engine default
		// (150 ms); a negative value disables hedging.
		// The 150 ms default is deliberate and is NOT
		// changed here (see the Stage 1 queue).
		if routing.HedgeDelayMs != 0 {
			engCfg.HedgeDelay = time.Duration(routing.HedgeDelayMs) * time.Millisecond
		}
		// Context-relay thresholds/TTL (B2-01): 0 keeps
		// the engine defaults (0.85 / 0.95 / 5h / 30
		// messages). The config was validated at load,
		// so the values here are known-good — including
		// the threshold < exhaustion ordering, checked
		// on the effective values.
		if routing.RelayHandoffThreshold != 0 {
			engCfg.RelayHandoffThreshold = routing.RelayHandoffThreshold
		}
		if routing.RelayExhaustionThreshold != 0 {
			engCfg.RelayExhaustionThreshold = routing.RelayExhaustionThreshold
		}
		if routing.RelayTTLMs != 0 {
			engCfg.RelayTTL = time.Duration(routing.RelayTTLMs) * time.Millisecond
		}
		if routing.RelayMaxMessages != 0 {
			engCfg.RelayMaxMessages = routing.RelayMaxMessages
		}
		// Quota-share operator table (B2-08): provider ->
		// model -> share weight. A missing entry means
		// share 1 (equal); the table was validated at
		// load, so every share here is positive.
		if len(routing.QuotaShares) > 0 {
			shares := routing.QuotaShares
			engCfg.QuotaShareFor = func(providerID, model string) float64 {
				if byModel, ok := shares[providerID]; ok {
					if s, ok := byModel[model]; ok {
						return s
					}
				}
				return 1
			}
		}
		// Account-level fallback (M3 Area 1, PARITY row 388):
		// the operator's global + per-provider account
		// ordering, from the config file. "" means the
		// engine's fill-first default; both values were
		// validated at load (unknown names, including
		// "expiry-first", fail there).
		engCfg.AccountFallbackStrategy = routing.AccountFallbackStrategy
		engCfg.AccountFallbackStrategies = routing.AccountFallbackStrategies
		// SLA objectives (M3 Area 1, PARITY row 394): the
		// operator's SLOs from routing.sla, into the
		// EXISTING router.Config SLA fields that
		// Engine.slaPolicy() and strategyHookContextFor
		// read — no parallel set. Each field is a pointer
		// in the file config, so only set fields are
		// assigned; an unset field leaves the engine
		// default. An explicit max_error_rate of 0 sets
		// SLAMaxErrorRateSet too, so the 0 survives to the
		// hook as zero-tolerance instead of being
		// rewritten to the 0.05 default. Values were
		// validated at load (target > 0, error rate in
		// [0, 1], cost cap >= 0).
		if sla := routing.SLA; sla != nil {
			if sla.TargetP95Ms != nil {
				engCfg.SLATargetP95Ms = *sla.TargetP95Ms
			}
			if sla.MaxErrorRate != nil {
				engCfg.SLAMaxErrorRate = *sla.MaxErrorRate
				engCfg.SLAMaxErrorRateSet = true
			}
			if sla.MaxCostPer1M != nil {
				engCfg.SLAMaxCostPer1M = *sla.MaxCostPer1M
			}
			if sla.HardConstraints != nil {
				engCfg.SLAHardConstraints = *sla.HardConstraints
			}
		}
		// Nadir decision endpoint (M3 Area 1, PARITY row
		// 396): see applyNadirConfig.
		applyNadirConfig(routing.Nadir, &engCfg)
		if ml := routing.ModelLockout; ml != nil {
			engCfg.ModelLockout = router.LockoutConfig{
				Enabled:      ml.Enabled,
				ErrorCodes:   ml.ErrorCodes,
				BaseCooldown: time.Duration(ml.BaseCooldownMs) * time.Millisecond,
				MaxCooldown:  time.Duration(ml.MaxCooldownMs) * time.Millisecond,
				MaxSteps:     ml.MaxSteps,
			}
		}
		if wb := routing.WaitBudgets; wb != nil {
			engCfg.WaitBudgets = router.WaitBudgetConfig{
				Enabled:         wb.Enabled,
				MaxWaitPerRetry: time.Duration(wb.MaxWaitPerRetryMs) * time.Millisecond,
				MaxRetries:      wb.MaxRetries,
				TotalBudget:     time.Duration(wb.TotalBudgetMs) * time.Millisecond,
			}
		}
		// Cooldown mode: "" (unset) means hard — the engine
		// default. A per-chain mode wins over the global one
		// for that chain's model.
		engCfg.CooldownMode = routing.CooldownMode
		engCfg.ChainCooldownMode = func(model string) string {
			if routing.ChainCooldownModes == nil {
				return ""
			}
			return routing.ChainCooldownModes[model]
		}
		// User-named routing groups (CHECKLIST 173): group name
		// -> ordered members, expanded to (provider, model)
		// attempts with the same rules as chainFor — enabled
		// providers with a live client that offer the model (or
		// list no models, i.e. chat-only gateways that take any
		// model). Members that fail any of those are skipped,
		// never dialled; the engine fails honestly when the
		// expansion is empty for a model. The same expansion
		// serves refusal chains: a "group:<name>" entry there
		// expands to the group's members, in order, for the
		// refused model.
		// (CHECKLIST 255): a member may carry a reasoning
		// effort — "provider@low" — so the same provider can
		// appear twice at different efforts, each expanding to
		// its own attempts with that effort set. The effort is
		// stamped on every attempt the member expands to
		// (including per-key attempts), so the engine runs that
		// entry at that effort. Members were validated at config
		// load, so ParseGroupMember cannot fail here; a failure
		// would be a config the loader let through, and the
		// member is skipped rather than routed at a guessed
		// effort.
		groupChainFor := func(group, model string) ([]router.Attempt, bool) {
			// M8.14 race fix: members come from a copy
			// taken under the shared routing read lock
			// (GroupMembers locks internally) — a combo
			// mirror may rewrite the live map on this
			// request path. The copy is ranged after the
			// lock is released.
			members, ok := cfg.GroupMembers(group)
			if !ok {
				return nil, false
			}
			var chain []router.Attempt
			for _, id := range members {
				providerID, effort, err := config.ParseGroupMember(id)
				if err != nil {
					continue
				}
				p, ok := reg.Get(providerID)
				if !ok || !p.Enabled {
					continue
				}
				if !hasClient(p) {
					continue
				}
				withEffort := func(atts []router.Attempt) []router.Attempt {
					if effort == "" {
						return atts
					}
					for i := range atts {
						atts[i].Effort = effort
					}
					return atts
				}
				if len(p.Models) == 0 {
					chain = append(chain, withEffort(attemptsForProvider(p, model))...)
					continue
				}
				for _, m := range p.Models {
					if m.ID == model {
						chain = append(chain, withEffort(attemptsForProvider(p, model))...)
						break
					}
				}
			}
			return chain, true
		}
		// Installed unconditionally (M8.14): a combo created
		// through POST /v1/combos on a zero-group install must
		// route immediately, not 400 "unknown routing group"
		// until restart. groupChainFor answers exists=false
		// for unknown groups, so an empty group map is safe.
		engCfg.GroupChain = groupChainFor
		// Content-policy refusal chains (CHECKLIST 161): the
		// operator-configured per-model chains from
		// routing.refusal_chains. A model with no entry returns
		// nil, so the engine default applies (the failure
		// chain's remaining targets, in order, each once); an
		// explicitly empty list returns an empty non-nil slice,
		// which the engine reads as "no refusal routing for
		// this model". The hook is only set when the config
		// names at least one chain, so an unconfigured install
		// keeps the plain default with no hook at all.
		if len(routing.RefusalChains) > 0 {
			chains := routing.RefusalChains
			engCfg.RefusalChainFor = func(model string) []router.Attempt {
				ids, ok := chains[model]
				if !ok {
					return nil
				}
				out := make([]router.Attempt, 0, len(ids))
				for _, id := range ids {
					if strings.HasPrefix(id, "group:") {
						// A group entry expands to the group's
						// members, in order, for this model
						// (CHECKLIST 173). The group name was
						// validated at config load, so an
						// unknown name cannot reach here; a
						// group with no member serving this
						// model contributes nothing.
						members, _ := groupChainFor(strings.TrimSpace(id[len("group:"):]), model)
						out = append(out, members...)
						continue
					}
					// A refusal-chain entry names a provider,
					// not a key: expand it into that provider's
					// per-key attempts (CHECKLIST 175) — the
					// preferred keys first, reserve held back
					// by the engine's tail partition. An
					// unknown or disabled provider, or one
					// with no live client, contributes nothing.
					if p, ok := reg.Get(id); ok && p.Enabled && hasClient(p) {
						out = append(out, attemptsForProvider(p, model)...)
					}
				}
				return out
			}
		}
	}
	// A nil autoRouter (nothing to resolve to) leaves AutoRoute
	// unset: "auto/*" then behaves like any other unknown model
	// instead of panicking on a nil-receiver method value.
	if autoRouter != nil {
		engCfg.AutoRoute = autoRouter.Resolve
	}
	// Local-vs-reported token counts: a material divergence is
	// logged to stderr with both numbers, never silently resolved
	// — the ledger keeps the provider's count (see
	// router.reconcileUsage), and this line is the flag that it
	// disagreed with the local estimate.
	engCfg.OnTokenDivergence = func(d router.TokenDivergence) {
		fmt.Fprintf(os.Stderr, "token count divergence: provider=%s model=%s local=%d reported=%d (local tiktoken estimate vs provider-reported prompt tokens; the provider's number is kept)\n",
			d.Provider, d.Model, d.Local, d.Reported)
	}
	// Per-request budget price source (CHECKLIST 155): the
	// engine's pre-attempt gate prices each attempt at its real
	// (provider, model) pair from the same book the ledger
	// charges with — pair first, model-level fallback, per leg
	// (a leg with no configured price contributes $0, never a
	// guessed price). Unlike ledger.Prices.Cost it does not
	// require both legs to be known: a prompt-only estimate
	// needs only the input leg, and a half-known pair must
	// still be gateable on the leg that is priced.
	engCfg.PriceFunc = func(providerID, model string, promptTokens, completionTokens int) float64 {
		if priceBook == nil {
			return 0
		}
		var in, out float64
		if p, ok := priceBook.LookupPair(providerID, model); ok {
			if p.InputUSDPer1M != nil {
				in = *p.InputUSDPer1M
			}
			if p.OutputUSDPer1M != nil {
				out = *p.OutputUSDPer1M
			}
		}
		if in == 0 || out == 0 {
			if p, ok := priceBook.LookupModel(model); ok {
				if in == 0 && p.InputUSDPer1M != nil {
					in = *p.InputUSDPer1M
				}
				if out == 0 && p.OutputUSDPer1M != nil {
					out = *p.OutputUSDPer1M
				}
			}
		}
		return float64(promptTokens)*in/1e6 + float64(completionTokens)*out/1e6
	}
	asyncLed := ledger.NewAsyncWriter(led, ledgerPrices, ledger.AsyncWriterConfig{})
	engine = router.NewEngine(clients, &ledgerAdapter{w: asyncLed}, engCfg)

	// Cache-aware compression, prefix freeze (row :939): the
	// engine observes each request's system prompt once (see
	// maybeCompress) and preserves a prompt that has recurred
	// at least DefaultPrefixFreezeThreshold times from
	// compression. The observer is owned here — the engine
	// cannot name its type (internal/degrade imports the
	// router package) — and installed once at startup, before
	// the engine serves. A frozen prefix is only ever
	// PRESERVED from compression; it never rewrites, drops,
	// or otherwise changes a payload.
	prefixFreeze := degrade.NewPrefixFreeze(true, degrade.DefaultPrefixFreezeThreshold)
	engine.SetPrefixFreezeHooks(prefixFreeze.Observe, prefixFreeze.IsFrozen)

	// Upstream rate-limit learning (M5.13): pace the adaptive
	// limiter from the fresh learned signals persisted in the
	// ledger, so a restart does not forget how close each
	// provider is to its limit. Grouped per provider (the
	// limiter's granularity — the tightest key's signal paces
	// the provider); a load failure is logged loudly, never
	// fatal — the next response re-learns. The same signals
	// also restore the engine's measured quota hint (the
	// fraction learnRateLimit records in-memory), so the first
	// requests after a restart order with a real remaining
	// budget instead of the unknown default.
	if learned, err := led.LearnedRateLimits(context.Background(), time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: loading learned rate limits: %v\n", err)
	} else {
		byProvider := map[string]adaptive.RateLimitSignals{}
		for _, e := range learned {
			sig := byProvider[e.ProviderID]
			sig.ObservedAt = e.UpdatedAt
			switch e.Kind {
			case "requests":
				sig.HasRequests = true
				sig.RequestsLimit = e.Limit
				sig.RequestsRemaining = e.Remaining
				sig.RequestsResetAt = e.ResetAt
			case "tokens":
				sig.HasTokens = true
				sig.TokensLimit = e.Limit
				sig.TokensRemaining = e.Remaining
				sig.TokensResetAt = e.ResetAt
			}
			byProvider[e.ProviderID] = sig
		}
		for providerID, sig := range byProvider {
			engine.ApplyLearnedRateLimit(providerID, sig, time.Now())
			// Restore the measured remaining-quota fraction the
			// live path learns (learnRateLimit in ratelimit.go):
			// same fraction formula, int(RemainingFraction()*100).
			// A 0 fraction is deliberately NOT restored: the
			// only exported setter (Engine.SetQuotaHint) never
			// expires, and a 0 hint makes filterZeroQuota skip
			// the provider — a skipped provider never answers,
			// so it could never re-learn and would stay skipped
			// until the next restart. A 0 provider is retried
			// after restart instead: its first response 429s
			// and the live path re-learns 0 with a proper
			// expiry. A positive fraction is safe to restore —
			// the provider still gets traffic, and its next
			// response overwrites this entry with an expiring
			// learned fraction.
			if hint := int(sig.RemainingFraction() * 100); hint > 0 {
				engine.SetQuotaHint(providerID, hint)
			}
		}
		// Per-key twin of the restore above (B2-09): the same
		// ledger rows carry the key, so the per-key fraction
		// restores too — the account-rotation cap survives a
		// restart. A 0 IS restored here, unlike the provider
		// form: RestoreLearnedKeyQuotaHint carries the
		// window-reset expiry, so the key is retried after
		// its window refills instead of staying skipped.
		byKey := map[[2]string]adaptive.RateLimitSignals{}
		for _, e := range learned {
			k := [2]string{e.ProviderID, e.KeyID}
			sig := byKey[k]
			sig.ObservedAt = e.UpdatedAt
			switch e.Kind {
			case "requests":
				sig.HasRequests = true
				sig.RequestsLimit = e.Limit
				sig.RequestsRemaining = e.Remaining
				sig.RequestsResetAt = e.ResetAt
			case "tokens":
				sig.HasTokens = true
				sig.TokensLimit = e.Limit
				sig.TokensRemaining = e.Remaining
				sig.TokensResetAt = e.ResetAt
			}
			byKey[k] = sig
		}
		for k, sig := range byKey {
			engine.RestoreLearnedKeyQuotaHint(k[0], k[1], sig, time.Now())
		}
	}

	// Graceful degradation: successful responses are cached in the
	// exact-match cache (500 entries, 1h store TTL); when every
	// provider fails, a cached response is served instead, marked
	// X-Codvian-Stale by the server. Enabled in serve mode.
	//
	// Semantic cache (layer 3): successful responses are also stored
	// under a deterministic hashed bag-of-words vector of the last
	// 3 non-system turns (500 entries, 1h TTL, the configured
	// semantic threshold, default 0.85 cosine) — scoped by the
	// model the request named, so a cached answer is served only
	// to a request for the same model (CHECKLIST 187).
	// ServeCache is ON, so cacheable requests are served proactively
	// from exact first, then semantic, without calling any provider
	// — marked X-Codvian-Cache by the server and counted in
	// codvian_router_cache_hits_total{layer="semantic"}, never billed
	// to a provider. Honest description: neural embeddings when a
	// provider offers the embedding model, falling back to
	// token-overlap similarity (hashed bag-of-words) when none
	// does or the call fails.
	var exact cache.ExactStore = cache.NewExactCache(500)
	// Neural embeddings (B2-31 follow-up): the semantic cache
	// compares meaning, not token overlap, once a provider
	// embedding client is wired into LocalSemanticCacheConfig —
	// the seam cache.go defines. The closure routes one
	// embeddings call through the engine (chain resolution,
	// budgets, ledger) for the default neural model
	// (text-embedding-3-small). The cache bounds the call at 3 s
	// and fails open to the hashed bag-of-words vector on any
	// error, timeout, or malformed vector, so a missing or dead
	// embedding provider can never break serving. CallerID is
	// left empty: an internal call has no gateway key, and an
	// empty CallerID records the same way in the ledger (see
	// router.KeyedLedger). One config value feeds all three
	// tiers (memory, Redis, SQLite) so every tier embeds the
	// same way — a neural vector (1536 dims) and a bag-of-words
	// vector (256 dims) never match each other.
	neuralEmbedder := func(ctx context.Context, text string) ([]float32, error) {
		input, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		resp, err := engine.EmbedInternal(ctx, provider.EmbeddingRequest{
			Model: cache.DefaultNeuralEmbeddingModel,
			Input: input,
		})
		if err != nil {
			return nil, err
		}
		if len(resp.Data) == 0 {
			return nil, fmt.Errorf("codvian-router: embeddings response has no data")
		}
		var vec []float32
		if err := json.Unmarshal(resp.Data[0].Embedding, &vec); err != nil {
			return nil, err
		}
		return vec, nil
	}
	// Semantic threshold (M6.7): the operator-configured floor
	// from cache.semantic_threshold (default 0.85 when unset;
	// validated to [0.85, 1.0] at load) seeds both the memory-only
	// cache and the persistent one below, so serve mode honours
	// the configured floor instead of always running at 0.85.
	semanticCfg := cache.LocalSemanticCacheConfig{
		Threshold:      float32(cfg.Cache.EffectiveSemanticThreshold()),
		NeuralEmbedder: neuralEmbedder,
		NeuralModel:    cache.DefaultNeuralEmbeddingModel,
	}
	var semantic cache.SemanticStore = cache.NewLocalSemanticCacheWithConfig(semanticCfg)
	var persistExact *cache.PersistentCache
	var persistSemantic semanticPersistHandle
	var persistReasoning *cache.ReasoningCache
	// M6.2 persistence tiers (config cache.exact_persist_path /
	// cache.semantic_persist_path; empty = memory-only, the
	// previous default). A relative path resolves against
	// DataDir. A path that cannot be opened logs a warning and
	// that layer keeps running memory-only; a bad cache path
	// never fails startup.
	//
	// Write-through (M6.2b): the degrade wrapper and the relay
	// take the ExactStore / SemanticStore interfaces, so the
	// persistent wrappers themselves are passed in — every Set
	// reaches disk synchronously, and an admin flush deletes
	// the disk rows at once. The 1-minute snapshot goroutines
	// stay as a safety net: they rewrite each table from the
	// live entries, which also prunes rows for entries evicted
	// from memory since the last sync (a Set writes its row
	// immediately, but eviction removes only the memory slot).
	// A final sync runs on graceful shutdown too (see
	// syncCachesOnShutdown, called from runServe's shutdown
	// branches), so a clean stop loses nothing and leaves no
	// evicted rows behind.
	if cfg.Cache != nil {
		resolveCachePath := func(p string) string {
			if p == "" || filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(cfg.DataDir, p)
		}
		if p := resolveCachePath(cfg.Cache.ExactPath); p != "" {
			if pc, err := cache.OpenPersistentCache(p, 500); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: opening exact cache persistence %q: %v — continuing memory-only\n", p, err)
			} else {
				exact = pc
				persistExact = pc
				go func() {
					t := time.NewTicker(time.Minute)
					defer t.Stop()
					for range t.C {
						_, _ = pc.Sync(context.Background())
					}
				}()
			}
		}
		// B2-32 Redis tier: when cache.redis_addr is set, the
		// semantic layer persists to Redis instead of SQLite —
		// the SQLite file is not opened at all. A Redis that
		// cannot be reached logs a warning and the layer keeps
		// running memory-only; a bad Redis address never fails
		// startup, exactly like a bad SQLite path above.
		if cfg.Cache.RedisAddr != "" {
			if rsc, err := cache.OpenRedisSemanticCache(cache.RedisOptions{Addr: cfg.Cache.RedisAddr, Password: cfg.Cache.RedisPassword, DB: cfg.Cache.RedisDB, TLS: cfg.Cache.RedisTLS}, "", semanticCfg); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: opening semantic cache Redis persistence %q: %v — continuing memory-only\n", cfg.Cache.RedisAddr, err)
			} else {
				semantic = rsc.AsStore()
				persistSemantic = rsc
				go func() {
					t := time.NewTicker(time.Minute)
					defer t.Stop()
					for range t.C {
						_, _ = rsc.Sync(context.Background())
					}
				}()
			}
		} else if p := resolveCachePath(cfg.Cache.SemanticPath); p != "" {
			if psc, err := cache.OpenPersistentSemanticCache(p, semanticCfg); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: opening semantic cache persistence %q: %v — continuing memory-only\n", p, err)
			} else {
				semantic = psc.AsStore()
				persistSemantic = psc
				go func() {
					t := time.NewTicker(time.Minute)
					defer t.Stop()
					for range t.C {
						_, _ = psc.Sync(context.Background())
					}
				}()
			}
		}
	}
	degradePolicy := degrade.DefaultPolicy()
	degradePolicy.Enabled = true
	degradePolicy.ServeCache = true
	// Semantic threshold (M6.7): same configured floor as the
	// cache above — the degrade layer takes the max of this, the
	// 0.85 floor, and the per-request header floor.
	degradePolicy.SemanticThreshold = float32(cfg.Cache.EffectiveSemanticThreshold())
	recorder := metrics.NewRecorder()
	// Coalescing counters: every waiter served from a shared flight
	// increments codvian_router_coalesced_total (one upstream call
	// avoided per shared request); the dashboard's coalescing
	// section reads the same numbers from the engine.
	engine.SetCoalescingMetrics(recorder)
	// T7 usage/cost metrics: every attempt's provider-reported token
	// split and exact reported cost reach /metrics from the engine's
	// single recordAttempt path (chat and streaming alike). Computed
	// pricing stays in the ledger analytics API, never duplicated here.
	engine.SetUsageMetrics(recorder)
	// Adaptive concurrency: the limiter's per-provider cap is
	// exposed as codvian_router_concurrency_limit{provider} and
	// every shed call as
	// codvian_router_concurrency_rejected_total{provider}; the
	// dashboard's concurrency section reads the same state from
	// the engine.
	engine.SetConcurrencyMetrics(recorder)
	// Recovered panics (CHECKLIST 165): every panic recovered on
	// the request/stream path increments
	// codvian_router_panics_total{location}; the dashboard's
	// Provider health card reads the total from the engine.
	engine.SetPanicMetrics(recorder)
	// Content-policy refusals (CHECKLIST 161): every refusal,
	// refusal-chain serve and exhausted chain increments the
	// refusal counters on /metrics; the dashboard's guardrail
	// section reads the same numbers from the engine.
	engine.SetRefusalMetrics(recorder)
	serving := degrade.WrapWithSemantic(engine, exact, semantic, degradePolicy)
	if se, ok := serving.(*degrade.StaleEngine); ok {
		se.SetCacheMetrics(recorder)
		// Reasoning replay (M6.6b): wire the reasoning cache so
		// the serving wrapper restores cached reasoning onto
		// follow-up turns (see wireReasoningCache). Without this
		// call the degrade reasoning paths were no-ops in serve
		// mode and only the router's own store replayed.
		// G13: the DB tier lives at DataDir/reasoning_cache.db
		// (no config knob — config.go is Wave 3's; the path
		// follows the ledger.db/feedback.db convention of
		// deriving straight from DataDir). An empty DataDir
		// means memory-only, never a relative-path file.
		reasonPath := ""
		if cfg.DataDir != "" {
			reasonPath = filepath.Join(cfg.DataDir, "reasoning_cache.db")
		}
		persistReasoning = wireReasoningCacheAtPath(se, reasonPath)
		if persistReasoning != nil && persistReasoning.Persistent() {
			pr := persistReasoning
			go func() {
				t := time.NewTicker(time.Minute)
				defer t.Stop()
				for range t.C {
					_, _ = pr.Sync(context.Background())
				}
			}()
		}
		// Savings recording (M6.2b / M6.8): when the exact layer
		// is persistent, every proactive own-cache hit is
		// recorded into the same SQLite file (tokens + dollars
		// at the model-level prices below), and the serving
		// engine is wrapped so GET /api/cache/stats reads the
		// live totals. Memory-only serve mode wires nothing:
		// the bare engine stays the serving engine, so the
		// endpoint keeps its honest savings_note instead of a
		// zero-filled savings block (see degrade.WithSavings).
		if persistExact != nil {
			se.SetSavingsRecorder(persistExact)
			se.SetSavingsPrices(savingsPricesFor(priceBook))
			serving = degrade.WithSavings(se, persistExact.SavingsTotals)
		}
		// Ledger recording (B4-51): every proactive own-cache hit
		// is also recorded as a no-charge ledger row carrying the
		// measured tokens saved (CacheTokensSaved). Unlike savings
		// recording it needs no persistent cache — asyncLed is the
		// engine's ledger writer — so it wires whenever the
		// serving engine is a StaleEngine, memory-only mode
		// included. Until now the hook was nil and hits recorded
		// nothing to the ledger.
		se.SetLedgerRecorder(asyncLed)
	}

	// Guardrails seeded from the config file (CHECKLIST 185):
	// the three blocking toggles plus the external entries, with
	// one HTTP classifier per external entry. Log redaction is
	// always on and is not a config value. A bad external entry
	// fails startup (see buildGuardrailStore), never silently.
	guardrailStore, err := buildGuardrailStore(cfg)
	if err != nil {
		return nil, err
	}

	// Plugins (CHECKLIST 201): the in-process registry from the
	// config file's plugins.enabled list, in order. An unknown
	// name fails startup (see buildPluginRegistry), never
	// silently. The registry also backs the provider-adapter
	// hook: a client built for a provider with a registered
	// adapter rewrites its outgoing request through it.
	pluginRegistry, err := buildPluginRegistry(cfg, priceBook)
	if err != nil {
		return nil, err
	}
	reg.SetHTTPAdapter(func(ctx context.Context, providerID string, req *http.Request) (*http.Request, error) {
		return pluginRegistry.AdaptHTTP(ctx, providerID, req)
	})

	return &stack{
		cfg: cfg, path: path, listen: listen,
		reg: reg, clients: clients,
		engine: engine, serving: serving, exact: exact, semantic: semantic,
		persistExact: persistExact, persistSemantic: persistSemantic, persistReasoning: persistReasoning,
		autoRouter:   autoRouter,
		priceBook:    priceBook,
		modelsDev:    modelsDev,
		litellm:      litellm,
		intel:        intel,
		arenaSyncer:  arenaSyncer,
		ledgerPrices: ledgerPrices,
		guardrails:   guardrailStore,
		plugins:      pluginRegistry,
		checker:      checker, recorder: recorder,
		led: led, asyncLed: asyncLed, vault: vault, feedback: fb,
	}, nil
}

// requiredCapabilities converts the operator's
// routing.require_capabilities config into the router's capability
// vocabulary (P1.5). Unknown names are already rejected by
// config.Validate, so anything dropped here is unreachable — but the
// function still drops rather than guesses, so a config edited after
// validation can never inject an invented capability.
func requiredCapabilities(cfg *config.Config) []capability.Capability {
	if cfg == nil || cfg.Routing == nil || len(cfg.Routing.RequireCapabilities) == 0 {
		return nil
	}
	out := make([]capability.Capability, 0, len(cfg.Routing.RequireCapabilities))
	for _, name := range cfg.Routing.RequireCapabilities {
		if c, ok := capability.ParseCapability(name); ok {
			out = append(out, c)
		}
	}
	return out
}

// savingsPricesFor returns the per-token USD price lookup the
// savings recorder uses (M6.2b): model -> (prompt $/token,
// completion $/token) from the model-level price book. Only the
// model-level price is consulted: a cached hit records no
// provider, so a per-(provider, model) pair price cannot be
// attributed honestly. An unknown model, or a model priced
// without the side in question, yields $0 for that side — the
// dollar saving is then $0 and only tokens are recorded, never
// an invented price. A nil book yields $0 for every model.
func savingsPricesFor(pb *config.PriceBook) func(model string) (promptPerToken, completionPerToken float64) {
	return func(model string) (float64, float64) {
		if pb == nil {
			return 0, 0
		}
		p, ok := pb.LookupModel(model)
		if !ok {
			return 0, 0
		}
		var pp, cp float64
		if p.InputUSDPer1M != nil {
			pp = *p.InputUSDPer1M / 1e6
		}
		if p.OutputUSDPer1M != nil {
			cp = *p.OutputUSDPer1M / 1e6
		}
		return pp, cp
	}
}

// semanticPersistHandle is the shutdown-sync contract shared by
// the semantic persistence tiers: the SQLite wrapper
// (*cache.PersistentSemanticCache) and the Redis wrapper
// (*cache.RedisSemanticCache) both satisfy it. Sync rewrites the
// backing store from the live entries; Close releases it. The
// serving path only ever touches cache.SemanticStore; this handle
// exists so syncCachesOnShutdown can final-sync and close
// whichever tier is attached.
type semanticPersistHandle interface {
	Sync(context.Context) (int, error)
	Close() error
}

// syncCachesOnShutdown snapshots both persistent caches to disk
// one final time and closes their backing stores (M6.2b). Every
// store already wrote through, so the sync is a prune + safety
// net: it drops stored rows for entries evicted from memory since
// the last 1-minute snapshot, so a restart never reloads an
// entry the live cache already evicted. Memory-only layers
// (nil handles) are skipped. Errors are logged, never fatal —
// the process is exiting, and a failed final sync loses at most
// the pruning, not entries (they are already stored).
func (st *stack) syncCachesOnShutdown(ctx context.Context) {
	if st == nil {
		return
	}
	if st.persistExact != nil {
		if n, err := st.persistExact.Sync(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: exact cache shutdown sync failed: %v\n", err)
		} else {
			fmt.Printf("cache: exact shutdown sync wrote %d entries\n", n)
		}
		_ = st.persistExact.Close()
	}
	if st.persistSemantic != nil {
		if n, err := st.persistSemantic.Sync(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: semantic cache shutdown sync failed: %v\n", err)
		} else {
			fmt.Printf("cache: semantic shutdown sync wrote %d entries\n", n)
		}
		_ = st.persistSemantic.Close()
	}
	if st.persistReasoning != nil && st.persistReasoning.Persistent() {
		if n, err := st.persistReasoning.Sync(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: reasoning cache shutdown sync failed: %v\n", err)
		} else {
			fmt.Printf("cache: reasoning shutdown sync wrote %d entries\n", n)
		}
		_ = st.persistReasoning.Close()
	}
}

// bootstrapIssuer returns the server.BootstrapIssuer for serve mode:
// it issues the first gateway key and persists it to the config file.
// The raw key is returned to the caller exactly once; only its hash is
// stored (see config.IssueKey / AddGatewayKey). The key carries the
// admin scope: it is the local operator's first key, and without it
// a fresh install's dashboard cannot call admin routes.
func bootstrapIssuer(cfg *config.Config, path string) server.BootstrapIssuer {
	return func(_ context.Context) (string, error) {
		key, raw, err := config.IssueKey("default", []string{"chat", "models", "admin"}, 0)
		if err != nil {
			return "", fmt.Errorf("cannot issue gateway key: %w", err)
		}
		if err := cfg.AddGatewayKey(key); err != nil {
			return "", fmt.Errorf("cannot store gateway key: %w", err)
		}
		if err := cfg.Save(path); err != nil {
			return "", fmt.Errorf("cannot save config: %w", err)
		}
		return raw, nil
	}
}

// routingPersist returns the server.RoutingPersistFunc for serve
// mode: it applies a dashboard routing update to the config file's
// routing block and saves the file. The in-memory pointer is
// swapped to a fresh copy first and rolled back if the save fails,
// so a failed save can never leave runtime and file disagreeing.
// The engine's runtime setters run after this returns nil (the
// server applies them), so a restart reads back exactly what the
// dashboard showed.
func routingPersist(cfg *config.Config, path string) server.RoutingPersistFunc {
	return func(update server.RoutingUpdate) error {
		// M8.14 race fix: the copy of the live routing block,
		// the application of the update, and the pointer swap
		// all run under the shared routing write lock, as one
		// CPU-only section — a combo mirror cannot write the
		// live ChainStrategies map mid-copy (which would race
		// the range below) or land a mirror write on the old
		// block after the swap (which would lose it). The
		// lock is released before Save runs, so it is never
		// held across disk I/O; Save takes the routing read
		// lock itself for Validate + Marshal.
		cfg.LockRouting()
		rc := &config.RoutingConfig{}
		if cfg.Routing != nil {
			*rc = *cfg.Routing
			if cfg.Routing.ChainStrategies != nil {
				rc.ChainStrategies = make(map[string]string, len(cfg.Routing.ChainStrategies))
				for k, v := range cfg.Routing.ChainStrategies {
					rc.ChainStrategies[k] = v
				}
			}
			if cfg.Routing.WeightedWeights != nil {
				w := *cfg.Routing.WeightedWeights
				rc.WeightedWeights = &w
			}
			if cfg.Routing.ChainCooldownModes != nil {
				rc.ChainCooldownModes = make(map[string]string, len(cfg.Routing.ChainCooldownModes))
				for k, v := range cfg.Routing.ChainCooldownModes {
					rc.ChainCooldownModes[k] = v
				}
			}
		}
		if update.Global != nil {
			rc.GlobalStrategy = *update.Global
		}
		for model, st := range update.Chains {
			if rc.ChainStrategies == nil {
				rc.ChainStrategies = map[string]string{}
			}
			if st == "" {
				delete(rc.ChainStrategies, model)
			} else {
				rc.ChainStrategies[model] = st
			}
		}
		if update.CooldownMode != nil {
			rc.CooldownMode = *update.CooldownMode
		}
		for model, m := range update.ChainCooldownModes {
			if rc.ChainCooldownModes == nil {
				rc.ChainCooldownModes = map[string]string{}
			}
			if m == "" {
				delete(rc.ChainCooldownModes, model)
			} else {
				rc.ChainCooldownModes[model] = m
			}
		}
		old := cfg.Routing
		cfg.Routing = rc
		cfg.UnlockRouting()
		if err := cfg.Save(path); err != nil {
			cfg.LockRouting()
			cfg.Routing = old
			cfg.UnlockRouting()
			return err
		}
		return nil
	}
}

// runServe builds the stack, starts the health watcher, and serves
// the OpenAI-compatible gateway on the effective listen address until
// SIGINT/SIGTERM.
// sseBatchingEnabled reads the hybrid-SSE-batching switch
// (items 135-136): on by default, off only when
// CODVIAN_ROUTER_SSE_BATCH is "0", "false" or "off".
// Measurement only — scripts/bench-sse.sh sets it to 0 for
// the "before" run so the same binary measures both modes.
func sseBatchingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODVIAN_ROUTER_SSE_BATCH"))) {
	case "0", "false", "off":
		return false
	default:
		return true
	}
}

// responsesSessionTokenCap returns the configured per-session
// token cap for Responses continuations (CHECKLIST 263–264):
// 0 when the responses block (or the config) is absent —
// counting-only, never refused.
func responsesSessionTokenCap(cfg *config.Config) int {
	if cfg == nil || cfg.Responses == nil {
		return 0
	}
	return cfg.Responses.SessionTokenCap
}

// webhookDispatcher builds the outbound webhook dispatcher from
// the config file (M5.9). Nil when no webhooks are configured —
// the server treats a nil dispatcher as "no delivery", and
// schedule blocks are still refused the same way.
func webhookDispatcher(cfg *config.Config) *webhooks.Dispatcher {
	if cfg == nil || len(cfg.Webhooks) == 0 {
		return nil
	}
	endpoints := make([]webhooks.Endpoint, 0, len(cfg.Webhooks))
	for _, w := range cfg.Webhooks {
		endpoints = append(endpoints, webhooks.Endpoint{ID: w.ID, URL: w.URL, Events: w.Events, Secret: w.Secret})
	}
	return webhooks.New(endpoints, nil, log.New(os.Stderr, "codvian-router: ", log.LstdFlags))
}

// isLocalProvider reports whether a configured provider is a local
// runtime (a gateway-provided local capability for the readiness
// gate): the Ollama wire, or a catalog entry in the local category
// (ollama-local, llamafile, llama.cpp, ...). The wire alone cannot
// identify one — no provider carries WireFormat "ollama" today —
// so the catalog category is the deciding signal for catalog-linked
// providers.
func isLocalProvider(p *provider.Provider) bool {
	if p == nil {
		return false
	}
	if p.WireFormat == "ollama" {
		return true
	}
	if p.CatalogID == "" {
		return false
	}
	entry, ok := catalog.ByID(p.CatalogID)
	if !ok {
		entry, ok = catalog.ByAlias(p.CatalogID)
	}
	return ok && entry.Category == catalog.CategoryLocal
}

// probeLocalRuntime checks whether a local runtime answers at base:
// GET /api/tags first (Ollama's native liveness endpoint). When the
// runtime answers /api/tags at all, that answer IS the verdict: 2xx
// means ready, any other status means not ready — falling back to
// another endpoint after a 500 would report a half-dead runtime as
// ready. Only a 404 (the runtime has no /api/tags endpoint, e.g.
// llamafile / llama.cpp) falls back to GET /v1/models, the endpoint
// every OpenAI-compatible local runtime serves. Each attempt is
// bounded at 2 s; a runtime that answers neither is not ready, never
// assumed ready.
func probeLocalRuntime(ctx context.Context, base string) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true
	case resp.StatusCode == http.StatusNotFound:
		// Not an Ollama runtime: fall back to the OpenAI-shaped
		// model list as the liveness signal.
		mctx, mcancel := context.WithTimeout(ctx, 2*time.Second)
		defer mcancel()
		mreq, err := http.NewRequestWithContext(mctx, http.MethodGet, base+"/v1/models", nil)
		if err != nil {
			return false
		}
		mresp, err := http.DefaultClient.Do(mreq)
		if err != nil {
			return false
		}
		defer mresp.Body.Close()
		return mresp.StatusCode >= 200 && mresp.StatusCode < 300
	default:
		return false
	}
}

// newMCPRegistry builds the production MCP client registry for
// the configured mcp block (CHECKLIST 392, M9.6 follow-up — the
// sole production registry construction; internal/server only
// receives the registry built here via WithMCPTools).
//
//   - mc.Controls == nil (no "controls" section): legacy
//     mcptools.New — allow-all, exactly as before M9.6. The
//     operator has not opted into restrictions. This is the
//     operator's explicit choice (absence), NOT a fail-open hole,
//     and it does not touch the server-side M9.1 scopes.
//   - mc.Controls != nil: mcptools.NewWithControls with the
//     configured Controls, enforced from construction —
//     fail-closed, so an empty Allow list denies every tool.
//
// Failed dispatches consume budget (M9.6 review nit 1 —
// DOCUMENTED, not refunded): the reservation is made before
// dispatch inside ExecuteDetailed and is not refunded when the
// server call fails, so a failed dispatch counts as one spent
// call against MaxCalls. See MCPConfig in internal/config.
func newMCPRegistry(mc *config.MCPConfig, toolServers []mcptools.ToolServer) *mcptools.Registry {
	if mc != nil && mc.ControlsEngaged() {
		return mcptools.NewWithControls(toolServers, mc.EffectiveMode(), mc.EffectiveMaxIterations(), *mc.Controls)
	}
	return mcptools.New(toolServers, mc.EffectiveMode(), mc.EffectiveMaxIterations())
}

func runServe(opts options) int {
	// Headless gate (CHECKLIST 197-198): serve --headless refuses
	// to start without a gateway key configured — checked BEFORE
	// openStack, which would otherwise create a fresh empty
	// config and serve it unauthenticated. Auth-on-from-first-boot.
	if opts.headless {
		if err := requireGatewayKey(opts); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: serve --headless refused: %v\n", err)
			return 1
		}
	}

	// GC tuning first, before any heavy allocation: auto-detect RAM,
	// set the memory limit, and (low-end only) GOGC=off. The line is
	// printed honestly — profile, limit, detection source, GOGC state.
	// See internal/gctune and docs/gc-tuning.md.
	fmt.Println(gctune.Apply(gctune.ApplyOptions{LowEndFlag: opts.lowEnd}).LogLine())

	// Operator live pprof HTTP server (hot-path diagnosis, off by default):
	// --pprof [addr] or CODVIAN_PPROF_ADDR starts a loopback HTTP listener
	// serving net/http/pprof endpoints.
	pprofAddr := opts.pprofAddr
	if pprofAddr == "" {
		pprofAddr = os.Getenv("CODVIAN_PPROF_ADDR")
	}
	if pprofAddr != "" {
		go func() {
			fmt.Printf("pprof: listening on http://%s/debug/pprof/\n", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "codvian-router: pprof listener failed: %v\n", err)
			}
		}()
	}

	// Operator CPU profiling (hot-path diagnosis, off by default):
	// CODVIAN_CPU_PROFILE=<path> writes a pprof CPU profile of the
	// serve run to that local file until shutdown (SIGINT/SIGTERM
	// or /admin/shutdown). No network surface: the file is written
	// by the operator's own process to the operator's own path.
	// CODVIAN_MEM_PROFILE=<path> likewise writes a heap profile at
	// shutdown. Both are empty/unset in normal operation.
	if cpuPath := os.Getenv("CODVIAN_CPU_PROFILE"); cpuPath != "" {
		f, err := os.Create(cpuPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cpu profile: %v\n", err)
			return 1
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cpu profile: %v\n", err)
			return 1
		}
		defer pprof.StopCPUProfile()
		fmt.Printf("cpu profile: writing to %s\n", cpuPath)
	}
	memPath := os.Getenv("CODVIAN_MEM_PROFILE")
	defer func() {
		if memPath == "" {
			return
		}
		f, err := os.Create(memPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: mem profile: %v\n", err)
			return
		}
		defer f.Close()
		_ = pprof.WriteHeapProfile(f)
	}()

	st, err := openStack(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	defer st.led.Close()
	defer st.feedback.Close()
	// Flush the async ledger writer before the DB closes (LIFO:
	// this runs first) so a graceful stop loses no usage rows.
	defer st.asyncLed.Close()

	// JSON trace log to stderr when enabled (off by default: it would
	// otherwise pollute the server log).
	if os.Getenv("CODVIAN_ROUTER_TRACE") == "1" {
		metrics.SetTraceWriter(os.Stderr)
	}

	// Health watcher: probe all providers immediately, then every 60s,
	// applying quota hints to the registry so the router skips
	// exhausted providers and scoring sees fresh latency/success.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go st.checker.StartWatcher(watchCtx, 60*time.Second)

	// MCP selector mode (CHECKLIST 141): with `mcp.servers`
	// configured, build the registry of stdio MCP servers and
	// discover their tools now, at startup. A server that fails
	// to start or answer tools/list is logged and recorded as
	// down — it contributes no tools, and the healthy servers
	// still load. With no `mcp` block (or no servers) there is no
	// registry and chat completions flow exactly as before.
	// When `mcp.controls` is configured the registry is built
	// hardened (CHECKLIST 392, newMCPRegistry); without it the
	// legacy allow-all registry is preserved.
	var mcpRegistry *mcptools.Registry
	if mc := st.cfg.MCP; mc != nil && len(mc.Servers) > 0 {
		toolServers := make([]mcptools.ToolServer, 0, len(mc.Servers))
		for _, s := range mc.Servers {
			toolServers = append(toolServers, mcptools.NewStdioServer(s.Name, s.Command, s.Args))
		}
		mcpRegistry = newMCPRegistry(mc, toolServers)
		discCtx, discCancel := context.WithTimeout(context.Background(), 30*time.Second)
		mcpRegistry.Discover(discCtx)
		discCancel()
		snap := mcpRegistry.Snapshot()
		up, down := 0, 0
		for _, st := range snap.Servers {
			if st.OK {
				up++
			} else {
				down++
				fmt.Fprintf(os.Stderr, "codvian-router: mcp server %q is down: %s\n", st.Name, st.LastError)
			}
		}
		fmt.Printf("mcp selector: mode=%s servers=%d up=%d down=%d tools=%d loop cap=%d controls_set=%v (selector injects 2 meta-tools; direct would inject all %d)\n",
			snap.Mode, len(snap.Servers), up, down, snap.DirectCount, snap.MaxIterations, snap.ControlsSet, snap.DirectCount)
	}

	// Local-capability readiness gate (CHECKLIST 143-144,
	// research-log entry 179): every configured provider that is a
	// local runtime (Ollama wire, or a catalog local-category entry
	// such as ollama-local / llamafile / llama.cpp) is a
	// gateway-provided local capability — its runtime is the local
	// daemon, probed here (GET {base}/api/tags, falling back to
	// GET {base}/v1/models, 2 s each). While the probe fails,
	// requests for its models answer 503 with Retry-After: 30
	// instead of failing over silently or returning a 502 from a
	// dead local daemon. A provider with no registered capability is
	// never gated. (The wire alone cannot identify a local runtime:
	// no provider carries WireFormat "ollama" today — the native
	// Ollama transport is not built yet, so those providers speak
	// OpenAI wire to the local daemon.)
	localGate := &readiness.Gate{}
	for _, p := range st.reg.List() {
		if p == nil || !p.Enabled || !isLocalProvider(p) {
			continue
		}
		base := strings.TrimRight(p.BaseURL, "/")
		id := p.ID
		localGate.Register("local:"+id, 30*time.Second, func(ctx context.Context) bool {
			return probeLocalRuntime(ctx, base)
		})
	}

	// MCP tool-call audit log (M9.1/M9.7): the same
	// <data-dir>/mcp/audit.jsonl the stdio server records into
	// (see runMCPTo) — wired here so the HTTP MCP transports
	// and GET /api/mcp/audit read exactly what was logged
	// (inputs SHA-256-hashed, outputs truncated). A log that
	// cannot be opened warns and serve still runs unaudited:
	// an audit failure must not take the gateway down. No
	// DataDir means no audit path.
	var mcpAuditLog *mcp.AuditLog
	if st.cfg.DataDir != "" {
		if auditLog, err := mcp.OpenAuditLog(filepath.Join(st.cfg.DataDir, "mcp", "audit.jsonl")); err == nil {
			mcpAuditLog = auditLog
		} else {
			fmt.Fprintf(os.Stderr, "codvian-router: mcp audit log unavailable (%v); tool calls will not be audited\n", err)
		}
	}

	// doShutdown is set after httpSrv exists; the server's shutdown
	// hook (POST /admin/shutdown) calls it through the closure above.
	var doShutdown func()
	// restartRequested is set by the POST /admin/restart hook.
	var restartRequested atomic.Bool
	// Reversible PII token vault (B3-19, parity tracker:782): nil
	// unless the config flag is on; a corrupt vault file fails
	// startup loudly (see reversiblePIIVault).
	piiVault, err := reversiblePIIVault(st.cfg, st.vault)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	srv := server.New(st.serving,
		server.WithVersion(Version),
		// Request tracing (Phase 2 observability): the build-tagged
		// tracer; an operator-set tracing_otlp_endpoint exports root
		// spans to that OTLP/HTTP collector. Empty endpoint = local
		// ring only (or a no-op in a default build).
		server.WithTracer(observability.NewConfiguredTracer(observability.TracerConfig{
			Endpoint:  st.cfg.TracingOTLPEndpoint,
			Headers:   st.cfg.TracingOTLPHeaders,
			TimeoutMs: st.cfg.TracingOTLPTimeoutMs,
		})),
		server.WithHeadless(opts.headless),
		server.WithConfig(st.cfg),
		server.WithProviderRegistry(st.reg),
		// P1.5 capability API: the same registry the routing gate
		// reads (built from it on demand), so the two can never
		// disagree about a model's recorded capabilities.
		server.WithCapabilityRegistry(st.reg),
		server.WithShortlist(st.cfg.Shortlist),
		// Native relay + readiness (CHECKLIST 143-144): the
		// readiness gate above; cache-serving on, with the gateway
		// caches wired in — the relay consults them in canonical
		// form first (hit served in the Anthropic shape, miss
		// relayed verbatim) and stores verbatim responses back.
		server.WithReadinessGate(localGate),
		server.WithCacheServing(true),
		server.WithCaches(st.exact, st.semantic),
		// Benchmark feed: the last measured bench run, recorded by
		// scripts/bench-geekflare.sh via CODVIAN_BENCH_OUT into
		// the data dir (benchmark.json). Absent file = the
		// endpoint answers recorded=false, never invented numbers.
		server.WithBenchmarkFile(filepath.Join(st.cfg.DataDir, "benchmark.json")),
		// Benchmark history feed (CHECKLIST 309-310): every
		// recorded bench file (benchmark-*.json) in the
		// directory named by CODVIAN_BENCH_HISTORY_DIR — the
		// repo's docs/ in practice. Unset = the endpoint
		// answers recorded=false, never invented numbers.
		server.WithBenchmarkHistoryDir(os.Getenv("CODVIAN_BENCH_HISTORY_DIR")),
		// Streaming-delivery feed: the last measured SSE run,
		// recorded by scripts/bench-sse.sh via CODVIAN_SSE_OUT
		// into the data dir (streaming.json). Same honest-empty
		// rule as the benchmark feed.
		server.WithStreamingFile(filepath.Join(st.cfg.DataDir, "streaming.json")),
		// Responses transcripts (CHECKLIST 315–316): the store
		// persists to responses.json in the same data dir as
		// the ledger (ledger.db), on every mutation, and is
		// restored at startup (RestoreResponses below) — a
		// restart no longer loses transcripts, so a
		// continuation of a pre-restart response works.
		server.WithResponsesFile(filepath.Join(st.cfg.DataDir, "responses.json")),
		// Hybrid SSE batching (items 135-136; chat-completions
		// stream handleChatStream + Responses stream
		// handleResponsesStream only): flush at most
		// every 8 ms or every 3 tokens. CODVIAN_ROUTER_SSE_BATCH=0
		// disables it — measurement only, for the before/after
		// comparison in scripts/bench-sse.sh.
		server.WithSSEBatching(sseBatchingEnabled()),
		// Provider-add hook: after POST /api/providers stores a
		// provider, build one upstream client per key in its pool
		// (CHECKLIST 175) and register each with the live
		// engine, so the new provider routes immediately with
		// no restart. st.clients is the map the openStack
		// chainFor closure reads live (fallback chains); the
		// map is keyed by (provider, key) identity
		// (router.ClientKey), and AddClientForKey updates the
		// engine's own copied client map per key. A failure
		// here does not fail the add — the server answers 201
		// with routing_live=false instead.
		server.WithProviderAddedHook(func(ctx context.Context, p *provider.Provider) error {
			keys := p.EffectiveKeys()
			if len(keys) == 0 {
				// No-auth provider: one legacy client under the
				// bare provider identity.
				client, err := st.reg.ClientFor(p.ID)
				if err != nil {
					return err
				}
				st.clients[router.ClientKey(p.ID, "")] = client
				st.engine.AddClientForKey(p.ID, "", client)
			}
			for _, key := range keys {
				client, err := st.reg.ClientForKey(p.ID, key.ID)
				if err != nil {
					return err
				}
				st.clients[router.ClientKey(p.ID, key.ID)] = client
				st.engine.AddClientForKey(p.ID, key.ID, client)
			}
			// A live-added local provider is a local capability
			// too: register it in the readiness gate (same probe
			// as at startup) so it is gated without a restart.
			if isLocalProvider(p) {
				base := strings.TrimRight(p.BaseURL, "/")
				id := p.ID
				localGate.Register("local:"+id, 30*time.Second, func(ctx context.Context) bool {
					return probeLocalRuntime(ctx, base)
				})
			}
			return nil
		}),
		// Provider-removed hook: after DELETE /api/providers/{id}
		// removes a provider from the live registry and persists
		// it, drop its client (engine + openStack map) so the next
		// request never routes to it, unregister its models from
		// the auto-router tiers (only models no remaining provider
		// still serves — tiers hold model IDs, not pairs), and
		// prune its probe history. A hook failure does not fail
		// the delete — the server answers live_removed=false.
		server.WithProviderRemovedHook(func(ctx context.Context, p *provider.Provider) error {
			// Drop every key's client for the provider — the
			// map is keyed by (provider, key) identity
			// (CHECKLIST 175), so the bare provider ID alone
			// would leave pooled clients behind.
			for key := range st.clients {
				if providerID, _ := router.SplitClientKey(key); providerID == p.ID {
					delete(st.clients, key)
				}
			}
			st.engine.RemoveClient(p.ID)
			// Drop its readiness capability too, so the feed stops
			// listing a deleted local runtime as not-ready.
			localGate.Unregister("local:" + p.ID)
			remaining := make(map[string]bool)
			for _, rp := range st.reg.List() {
				for _, m := range rp.Models {
					if m.ID != "" {
						remaining[m.ID] = true
					}
				}
			}
			var drop []string
			for _, m := range p.Models {
				if m.ID != "" && !remaining[m.ID] {
					drop = append(drop, m.ID)
				}
			}
			if st.autoRouter != nil && len(drop) > 0 {
				st.autoRouter.UnregisterTierModels(drop)
			}
			if st.checker != nil {
				st.checker.PruneProvider(p.ID)
			}
			return nil
		}),
		// Provider-key-changed hook: after a key is added,
		// removed or re-roled, re-sync that provider's engine
		// clients from its current pool (CHECKLIST 175) — build
		// clients for new or changed keys, drop clients for
		// removed keys — so the change routes immediately with
		// no restart. Chains read the registry live, so no
		// chain rebuild is needed. A failure here does not fail
		// the request — the server answers with
		// routing_live=false instead.
		server.WithProviderKeyChangedHook(func(ctx context.Context, p *provider.Provider) error {
			current := map[string]bool{}
			for _, key := range p.EffectiveKeys() {
				current[key.ID] = true
				client, err := st.reg.ClientForKey(p.ID, key.ID)
				if err != nil {
					return err
				}
				st.clients[router.ClientKey(p.ID, key.ID)] = client
				st.engine.AddClientForKey(p.ID, key.ID, client)
			}
			for key := range st.clients {
				if pid, kid := router.SplitClientKey(key); pid == p.ID && kid != "" && !current[kid] {
					delete(st.clients, key)
					st.engine.RemoveClientForKey(p.ID, kid)
				}
			}
			return nil
		}),
		server.WithAutoRouter(st.autoRouter),
		server.WithPriceBook(st.priceBook),
		// M5.9: outbound webhooks from the config file (today
		// only "quota.exceeded"); nil when none are configured,
		// in which case schedule blocks are refused without any
		// delivery.
		server.WithWebhooks(webhookDispatcher(st.cfg)),
		// Request-level compliance audit log (Workstream A1):
		// hash-chained, append-only SQLite store persisted in the
		// data dir. A real request through the production server
		// now produces a durable audit event; failures are logged
		// by the option itself, never silently dropped.
		server.WithAuditStore(filepath.Join(st.cfg.DataDir, "audit.db")),
		server.WithModelsDevStatus(modelsDevStatusFunc(st.modelsDev)),
		server.WithLedgerPrices(st.ledgerPrices),
		server.WithGuardrails(st.guardrails),
		// Guardrail request/response options (B3-20/21/22,
		// Stage 1): wired from the config file's guardrails
		// block. All off by default — the check still runs,
		// only the rewrite/sanitization are opt-in. The mode
		// defaults to "redact" when sanitization is on and
		// no mode is configured; an invalid mode was already
		// rejected at config load, so the cast here is safe.
		server.WithRequestRewrite(st.cfg.Guardrails != nil && st.cfg.Guardrails.RequestRewrite),
		server.WithResponseSanitization(st.cfg.Guardrails != nil && st.cfg.Guardrails.ResponseSanitization),
		server.WithResponseGuardMode(guardrails.ResponseGuardMode(guardrailResponseMode(st.cfg))),
		// Reversible PII token vault (B3-19): nil when the flag
		// is off — the rewrite then uses plain placeholders.
		server.WithReversibleVault(piiVault),
		// Responses session token cap (CHECKLIST 263–264):
		// read from config at startup; 0 (unset) means
		// counting-only — turns are counted and reported,
		// never refused.
		server.WithResponsesSessionTokenCap(responsesSessionTokenCap(st.cfg)),
		server.WithPlugins(st.plugins),
		server.WithHealthChecker(st.checker),
		server.WithMetrics(st.recorder),
		server.WithLedger(st.led),
		// MCP selector mode (CHECKLIST 141): nil when no MCP
		// servers are configured, in which case chat completions
		// flow exactly as before.
		server.WithMCPTools(mcpRegistry),
		// MCP tool-call audit log (M9.1/M9.7): the same log the
		// stdio server records into, so GET /api/mcp/audit
		// reads what the HTTP transports logged. Nil when the
		// log could not be opened — the feed then honestly
		// answers available:false with an empty list.
		server.WithMCPAuditLog(mcpAuditLog),
		server.WithFeedback(st.feedback),
		// Learned auto-router (items 181-182, RouteLLM
		// pattern): off by default; even when on, it activates
		// only on real evidence (50+ labelled outcomes, held-out
		// calibration error <= 0.15 beating the base rate).
		server.WithLearnedRouter(st.cfg.AutoRouter.LearnedRouter),
		// DB wipe: the probe-history clearer (ring + file +
		// backup) and the async-ledger flush, so a wipe deletes
		// queued rows before they can land after it.
		server.WithHealthHistory(st.checker),
		server.WithLedgerFlushHook(func() { st.asyncLed.Flush() }),
		server.WithRoutingPersist(routingPersist(st.cfg, st.path)),
		// Danger zone: persist config mutations to the file, and
		// drop reset providers from the in-memory registry so a
		// reset takes effect without a restart (the registry's
		// Remove re-saves the already-reset config — same content).
		server.WithConfigPersist(func() error { return st.cfg.Save(st.path) }),
		server.WithDangerResetHook(func() error {
			for _, p := range st.reg.List() {
				if err := st.reg.Remove(p.ID); err != nil {
					return err
				}
			}
			// Registry removal persists via the config store,
			// which writes a nil provider slice when the last
			// provider goes; restore the empty (not null) shape
			// so the saved file stays canonical.
			if st.cfg.Providers == nil {
				st.cfg.Providers = []provider.ProviderConfig{}
			}
			return st.cfg.Save(st.path)
		}),
		// API log capture (M8.16): GET /api/logs serves the
		// captured lines (ring of 1000, double-redacted);
		// without this line the endpoint honestly answers
		// captured:false with an empty list.
		server.WithAPILogCapture(server.NewAPILogRing(1000)),
		server.WithListenAddr(st.listen),
		server.WithBootstrapPending(len(st.cfg.GatewayKeys) == 0),
		server.WithBootstrapIssuer(bootstrapIssuer(st.cfg, st.path)),
		server.WithShutdownHook(func() {
			if doShutdown != nil {
				doShutdown()
			}
		}),
		// POST /admin/restart: mark the restart, then stop the
		// HTTP server exactly like shutdown. runServe's normal
		// shutdown branch sees the mark after the final cache
		// sync and starts the replacement (relaunchAfterRestart).
		server.WithRestartHook(func() {
			restartRequested.Store(true)
			if doShutdown != nil {
				doShutdown()
			}
		}),
		// POST /admin/update: the same GitHub releases lookup as
		// `update --check`. A check only; installing stays in the
		// CLI, which verifies signature + checksum.
		server.WithUpdateCheck(Version, func(ctx context.Context) (string, error) {
			client := &http.Client{Timeout: 15 * time.Second}
			return release.LatestTag(ctx, client, updateAPIBase(), release.DefaultRepo)
		}),
	)
	// Workstream A2: wire the OAuth connection store into the provider
	// registry's auth resolution, so an OAuth-connected catalog provider
	// executes with its real stored OAuth access token (Bearer) instead
	// of being refused as an unsupported exchange. The resolver is
	// called lazily per client build; a missing connection surfaces as
	// an honest error, never a fabricated header.
	st.reg.SetOAuthTokenResolver(srv.OAuthAccessToken)
	// Responses transcripts (CHECKLIST 315–316): restore the
	// persisted store before serving — a restart resumes the
	// pre-restart conversations instead of 404ing them.
	srv.RestoreResponses()
	// T7 provider-health webhooks: the engine's genuine circuit-breaker
	// transitions (CLOSED->OPEN / OPEN->CLOSED) become the
	// "provider.degraded" / "provider.recovered" webhook events,
	// delivered best-effort to subscribed endpoints with no secret or
	// payload content in the body.
	st.engine.SetBreakerEvents(srv.BreakerEventsWire())
	httpSrv := &http.Server{Addr: st.listen, Handler: srv}
	// The listener is created explicitly (instead of
	// ListenAndServe) so it can be wrapped with
	// server.NoDelayListener: TCP_NODELAY is set on every
	// accepted connection (items 135-136). Go's stdlib already
	// sets it by default — the wrapper makes it an enforced
	// property of this server, proven by the GetsockoptInt
	// test in internal/server/nodelay_listener_test.go.
	ln, err := net.Listen("tcp", st.listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: listen %s: %v\n", st.listen, err)
		return 1
	}
	ln = server.NoDelayListener(ln)
	// The shutdown hook (POST /admin/shutdown, Tier 1 LOCAL_ONLY)
	// stops this server; it is set here because it needs httpSrv.
	doShutdown = func() {
		// Persist the last-known-good store (M3 Phase C) before
		// the HTTP server stops: a restart keeps routing to the
		// provider that last worked for each model. A save
		// failure is logged, never fatal — the store rebuilds
		// from new successes.
		if err := strategy.DefaultLKGPRegistry.Store().Save(filepath.Join(st.cfg.DataDir, "lkgp.json")); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: lkgp store save failed (%v)\n", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}

	fmt.Printf("codvian-router v%s listening on %s (auth ON)\n", Version, st.listen)
	fmt.Printf("config: %s\n", st.path)
	// Clients are keyed by (provider, key) identity (CHECKLIST
	// 175): the count is provider-key clients, so a pooled
	// provider counts once per key. The distinct-provider count
	// is reported alongside so the line stays honest.
	readyProviders := map[string]bool{}
	for key := range st.clients {
		if pid, _ := router.SplitClientKey(key); pid != "" {
			readyProviders[pid] = true
		}
	}
	fmt.Printf("providers: %d configured, %d with clients ready, %d provider-key clients\n", len(st.reg.List()), len(readyProviders), len(st.clients))
	// Async guardrails (CHECKLIST 185): named at startup so the
	// operator can see which slow classifiers run concurrently
	// with the provider call — a BLOCK verdict from one cuts the
	// live stream with the -32001 refusal frame.
	if st.guardrails != nil {
		var asyncNames []string
		for _, a := range st.guardrails.Stats().Async {
			if a.Mode == string(guardrails.ModeAsync) {
				asyncNames = append(asyncNames, a.Name+" ("+a.Kind+")")
			}
		}
		if len(asyncNames) > 0 {
			fmt.Printf("async guardrails: %s — BLOCK verdicts cut live streams with -32001\n", strings.Join(asyncNames, ", "))
		}
	}
	fmt.Println("degradation: ON (stale responses served with X-Codvian-Stale)")
	// Responses session token cap (CHECKLIST 263–264): named
	// at startup so the operator can see whether continuations
	// are refused or only counted.
	if cap := responsesSessionTokenCap(st.cfg); cap > 0 {
		fmt.Printf("responses session token cap: %d tokens per session (over-cap turns refused with 429 session_token_limit)\n", cap)
	} else {
		fmt.Println("responses session token cap: unset (counting only — sessions are counted, never refused)")
	}
	fmt.Printf("semantic cache: ON (token-overlap similarity, threshold %.2f, 500 entries, 1h TTL — not neural embeddings; hits marked X-Codvian-Cache)\n", st.cfg.Cache.EffectiveSemanticThreshold())
	fmt.Println("single-flight coalescing: ON (identical non-streaming requests share one upstream call; streaming and tool-call turns never coalesce)")
	tfo := provider.TFOStatus()
	if tfo.Active {
		fmt.Printf("tcp fast open: ACTIVE (%s; %s)\n", tfo.Platform, tfo.Reason)
	} else {
		fmt.Printf("tcp fast open: OFF (%s; %s) — dials fall back to a normal handshake\n", tfo.Platform, tfo.Reason)
	}
	// Drive-by compile fix (cost-cap slice, items 239–240): the E1
	// limiter hunk (items 233–234, uncommitted) reads engCfg here
	// in runServe, but engCfg is declared in openStack — the tree
	// did not build (`undefined: engCfg`). The print must reflect
	// the same bit openStack sets, so it reads the same env var
	// directly; the E1 slice owns the behaviour, this only restores
	// compilation.
	if os.Getenv("CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER") == "1" {
		fmt.Println("adaptive concurrency: OFF (CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1 — measurement only for the E1 limiter verdict; overload queues instead of shedding)")
	} else {
		fmt.Println("adaptive concurrency: ON (Gradient2 per provider: cap starts at 20, floor 2, ceiling 100; a slowing provider is shed before 429s appear; limiter fails open)")
	}
	{
		// Warm HTTP/2 pools: one transport per (provider, key),
		// created at startup and warmed with a real request, sized
		// by Little's Law (see internal/provider/pool.go).
		pc := st.reg.PoolConfig()
		fmt.Printf("warm http/2 pools: ON (Little's Law N=ceil(λ×W): λ=%.0f req/s, W=%.0f ms → %d idle connections per provider+key, min %d, max %d; warmed at boot, TFO on cold dials)\n",
			pc.ExpectedRPS, pc.ExpectedLatencyMs, pc.Size(), pc.MinConns, pc.MaxConns)
	}
	// SSE delivery (items 135-136): TCP_NODELAY is explicit on
	// both socket paths (accepted connections via
	// NoDelayListener above, provider dials via the dialer
	// wrapper — Go's stdlib already sets it by default, the
	// GetsockoptInt tests prove it is set), and token events
	// are hybrid-batched on the two streaming response handlers
	// (handleChatStream + handleResponsesStream): one Flush per 3 tokens or 8 ms.
	if sseBatchingEnabled() {
		fmt.Println("sse delivery: TCP_NODELAY on (inbound listener + provider dialer, verified by socket-option probe) + hybrid batching on the chat + Responses streams (flush ≤8ms or ≤3 tokens; CODVIAN_ROUTER_SSE_BATCH=0 disables for measurement)")
	} else {
		fmt.Println("sse delivery: TCP_NODELAY on (inbound listener + provider dialer, verified by socket-option probe) + hybrid batching OFF on the chat + Responses streams (CODVIAN_ROUTER_SSE_BATCH=0 — flush per token, measurement mode)")
	}
	if st.cfg.AutoRouter.FeedbackLearnedWeights {
		fmt.Println("feedback-learned routing weights: ON (min 10 signals per tier, nudge bounded to ±0.05; signals stored in feedback.db — decision id, tier, model, latency only, never prompts or answers)")
	} else {
		fmt.Println("feedback-learned routing weights: OFF (signals still recorded to feedback.db; enable with auto_router.feedback_learned_weights)")
	}
	if st.cfg.AutoRouter.LearnedRouter {
		fmt.Println("learned auto-router: ON (per-tier win probabilities from recorded outcomes, bounded to ±0.05; activates only on real evidence — 50+ labelled outcomes and held-out calibration error ≤ 0.15 beating the base rate — otherwise routing is unchanged)")
	} else {
		fmt.Println("learned auto-router: OFF (status still reported on GET /dashboard/learned; enable with auto_router.learned_router)")
	}
	// Opt-in models.dev sync (CHECKLIST 179): off by default —
	// with it off the router never contacts models.dev. When on,
	// prices + context windows sync from the public catalog for
	// pairs the operator has not priced/set, and the dashboard's
	// Models view shows the provenance (operator vs models.dev +
	// sync time). The live fetch runs in the background after
	// this line prints, so startup never waits on it.
	if st.modelsDev != nil {
		fmt.Printf("models.dev sync: ON (catalog %s, operator prices/windows always win, last good data kept on failure)\n", st.modelsDev.URL())
	} else {
		fmt.Println("models.dev sync: OFF (no network call for prices; enable with models_dev.enabled in the config)")
	}
	{
		global := "balanced"
		deadline := "off"
		if st.cfg.Routing != nil {
			if st.cfg.Routing.GlobalStrategy != "" {
				global = st.cfg.Routing.GlobalStrategy
			}
			if st.cfg.Routing.DeadlineBudgetMs > 0 {
				deadline = fmt.Sprintf("%dms", st.cfg.Routing.DeadlineBudgetMs)
			}
		}
		fmt.Printf("routing strategies: ON (global %s, per-chain overrides + auto:<axis> aliases, deadline budget %s)\n", global, deadline)
	}
	if st.autoRouter != nil {
		fmt.Println("auto-router: ON (local hashed bag-of-words matching, threshold 0.5, no LLM call, no network — virtual models auto, auto/fast, auto/cheap, auto/coding, auto/reasoning)")
	} else {
		fmt.Println("auto-router: OFF (no model configured to resolve auto/* to)")
	}
	fmt.Println("metrics: GET /metrics (gateway-key auth required)")
	if opts.headless {
		// Headless mode (CHECKLIST 197-198): pure API process —
		// no dashboard server, no browser/tray behaviour. The
		// /dashboard* routes answer 404 (see server.register).
		fmt.Println("headless: ON (API only — no dashboard server; every /dashboard* route answers 404)")
		fmt.Println("dashboard: disabled in headless mode — run without --headless for the dashboard")
	} else {
		fmt.Printf("dashboard: http://%s/dashboard\n", st.listen)
		fmt.Println("headless: OFF (dashboard served; run `serve --headless` for an API-only process — it refuses to start without a gateway key)")
	}
	fmt.Println("mcp: stdio server available via `codvian-router mcp` (tools: router_status, provider_list, model_list)")
	fmt.Println("route guards: ON (Tier 1 LOCAL_ONLY enforced before auth by real TCP peer — no Host/X-Forwarded-For, no manage-scope bypass; Tier 2 ALWAYS_PROTECTED always needs a gateway key; Tier 3 MANAGEMENT auth required)")
	fmt.Println("guardrails: ON (log + spend-log redaction ALWAYS ON — prompts and provider keys never reach any log stream; blocking is opt-in per guardrail, all off by default — see GET /dashboard/guardrails)")
	// Plugins (CHECKLIST 201): the startup line names what is
	// actually registered, in order — or says none, honestly.
	// Dynamic loading is intentionally unsupported (in-process
	// only), so there is nothing else to report.
	if st.plugins != nil && st.plugins.Len() > 0 {
		names := []string{}
		for _, p := range st.plugins.Snapshot() {
			names = append(names, p.Name)
		}
		fmt.Printf("plugins: %d registered (%s) — in-process only, no dynamic loading; see GET /dashboard/plugins\n", len(names), strings.Join(names, ", "))
	} else {
		fmt.Println("plugins: none registered (enable built-ins in the config file under plugins.enabled — header-add, response-log; see GET /dashboard/plugins)")
	}
	if len(st.cfg.GatewayKeys) == 0 {
		// AK-2: the one-shot bootstrap token is printed to stdout
		// only — never to a log file, never in an API response.
		// The operator claims the first admin key once with it
		// (POST /admin/bootstrap, loopback-only); after use it
		// is dead.
		fmt.Println("bootstrap: PENDING (no gateway key yet — POST /admin/bootstrap from loopback with the one-shot token below; it is shown once and cannot be recovered)")
		if tok := srv.BootstrapToken(); tok != "" {
			fmt.Printf("bootstrap token (single-use): %s\n", tok)
		} else {
			fmt.Println("bootstrap token: unavailable (bootstrap is pending but no token was issued — restart the router)")
		}
	} else {
		fmt.Println("bootstrap: complete (a gateway key already exists)")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	// Opt-in models.dev sync (CHECKLIST 179): the live fetch runs
	// in the background, after the server is up, so a slow or
	// offline catalog never delays startup or a first request.
	// It stops when the watcher context ends (shutdown).
	if st.modelsDev != nil {
		go runModelsDevSyncLoop(watchCtx, st, srv)
	}

	// Opt-in LiteLLM sync (B4-03/12, Stage 1): the live
	// fetch runs in the background, after the server is up,
	// so a slow or offline cost map never delays startup or
	// a first request. It stops when the watcher context
	// ends (shutdown).
	if st.litellm != nil {
		go runLiteLLMSyncLoop(watchCtx, st, srv)
	}

	// Opt-in Arena ELO sync (B2-14): the background fetch that
	// fills the Intelligence store's arena_elo source — one sync
	// at startup, then every interval. It stops when the watcher
	// context ends (shutdown). The known-model supplier
	// re-snapshots the registry on every tick, so providers added
	// or removed live are picked up by the next sync. Nil when
	// the arena_elo block is off: nothing fetches ELO.
	if st.arenaSyncer != nil {
		st.arenaSyncer.Start(watchCtx, func() map[string]bool {
			known := make(map[string]bool)
			for _, lp := range modelsDevLocalSnapshot(st.reg) {
				for _, m := range lp.Models {
					if id := modelsync.NormalizeModelName(m.ID); id != "" {
						known[id] = true
					}
				}
			}
			return known
		})
	}

	// Scheduled model-list refresh (CT-7): the background
	// re-probe that keeps stored model lists fresh — one run at
	// startup, then every 24h (provider.ModelListRefreshInterval).
	// It stops when the watcher context ends (shutdown).
	if st.reg != nil {
		go runScheduledModelRefreshLoop(watchCtx, st.reg)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigCh:
		fmt.Println("codvian-router: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: shutdown error: %v\n", err)
			return 1
		}
		// Final cache sync (M6.2b): the HTTP server has stopped,
		// so no request is still storing; snapshot both
		// persistent layers to disk one last time and close the
		// SQLite files. Write-through already persisted every
		// entry, so this is a prune + safety net, not a rescue.
		st.syncCachesOnShutdown(ctx)
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		// Normal shutdown (POST /admin/shutdown, or Serve
		// returning): the doShutdown hook already stopped the
		// HTTP server, so the same final sync runs here — both
		// shutdown paths end in this branch or the SIGTERM one
		// above, never neither.
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer syncCancel()
		st.syncCachesOnShutdown(syncCtx)
		if restartRequested.Load() {
			return relaunchAfterRestart()
		}
		return 0
	}
}

// relaunchAfterRestart finishes POST /admin/restart once the HTTP
// server has stopped (so the port is free) and caches are synced.
// Under the tray supervisor (tray.SupervisedEnv set) the process
// exits with tray.RestartExitCode and the supervisor starts a fresh
// gateway. Unsupervised, it starts the same binary with the same
// arguments, detached, inheriting stdout/stderr, and exits 0. A
// failed relaunch exits 1 with the reason: the gateway has stopped
// and says so, instead of claiming it restarted.
func relaunchAfterRestart() int {
	if os.Getenv(tray.SupervisedEnv) == "1" {
		fmt.Println("codvian-router: restarting (handing back to the supervisor)")
		return tray.RestartExitCode
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: cannot resolve own binary (%v); the gateway has stopped\n", err)
		return 1
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: cannot start replacement (%v); the gateway has stopped\n", err)
		return 1
	}
	fmt.Printf("codvian-router: restarted as pid %d\n", cmd.Process.Pid)
	return 0
}

// runEval smoke-evaluates the live engine: one synchronous health
// probe (so scoring has signals), then the eval harness smoke cases
// against the requested model. It uses the RAW engine, not the
// degrade wrapper — a stale cache hit would mask provider failures
// and make the eval meaningless. Exit 1 when any case fails.
func runEval(opts options) int {
	if opts.model == "" {
		fmt.Fprintln(os.Stderr, "codvian-router: eval requires --model MODEL")
		return 2
	}
	st, err := openStack(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	defer st.led.Close()
	defer st.feedback.Close()
	// Flush the async ledger writer before the DB closes (LIFO:
	// this runs first) so a graceful stop loses no usage rows.
	defer st.asyncLed.Close()

	ctx := context.Background()
	st.checker.CheckAll(ctx)
	st.checker.ApplyHints(st.reg)

	h := &eval.Harness{Engine: st.engine}
	report := h.Run(ctx, eval.SmokeCases(opts.model))
	for _, r := range report.Results {
		status := "PASS"
		if !r.OK {
			status = "FAIL"
		}
		line := fmt.Sprintf("[%s] %s (%d ms)", status, r.CaseID, r.LatencyMs)
		if r.Error != "" {
			line += ": " + r.Error
		} else if len(r.Missing) > 0 {
			line += ": missing " + strings.Join(r.Missing, ", ")
		}
		fmt.Println(line)
	}
	fmt.Printf("eval: %d/%d passed (%.0f%%)\n", report.Passed, report.Total, report.PassRate*100)
	if report.Failed > 0 {
		return 1
	}
	return 0
}

// runEvalFeedback is the A/B comparison for feedback routing
// (item 115): the calibrated heuristic vs the no-feedback baseline
// on the recorded local traffic in feedback.db. It prints the
// measured result and the verdict, and exits 0 whatever the
// verdict — a baseline win is a real result, not a failure; the
// operator keeps whichever wins (learning stays OFF unless the
// feedback policy wins AND auto_router.feedback_learned_weights
// is enabled). --feedback-db overrides the data-dir store;
// --cutoff RFC3339 overrides the default 70/30 time split. Both
// flags are registered in parseArgs (CHECKLIST 271-272); args here
// is only the leftover positionals, which are all rejected.
func runEvalFeedback(opts options, args []string) int {
	dbPath := opts.evalFeedbackDB
	cutoff := opts.evalCutoff
	for _, a := range args {
		fmt.Fprintf(os.Stderr, "codvian-router: eval-feedback: unknown argument %q\n", a)
		return 2
	}
	if dbPath == "" {
		path, err := resolveConfigPath(opts.configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		cfg, err := config.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot load config %s (run `init` first): %v\n", path, err)
			return 1
		}
		dbPath = filepath.Join(cfg.DataDir, "feedback.db")
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("eval-feedback: no feedback recorded yet (no store at %s)\nverdict: no-evidence — serve auto-routed requests and record signals first\n", dbPath)
			return 0
		}
		fmt.Fprintf(os.Stderr, "codvian-router: cannot read feedback store %s: %v\n", dbPath, err)
		return 1
	}
	fb, err := feedback.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot open feedback store %s: %v\n", dbPath, err)
		return 1
	}
	defer fb.Close()
	ctx := context.Background()
	var res feedback.EvalResult
	if cutoff.IsZero() {
		res, err = fb.Evaluate(ctx)
	} else {
		res, err = fb.EvaluateAt(ctx, cutoff)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: eval-feedback: %v\n", err)
		return 1
	}
	fmt.Printf("eval-feedback: cutoff %s\n", res.Cutoff.Format(time.RFC3339))
	fmt.Printf("  train signals: %d · test decisions: %d (evaluated %d, excluded %d%s)\n", res.TrainSignals, res.TestDecisions, res.Evaluated, res.Excluded, cappedSuffix(res.Capped))
	nudgeLine := ""
	for _, tier := range []string{"coding", "chat", "reasoning", "quick-qa"} {
		if nudgeLine != "" {
			nudgeLine += ", "
		}
		nudgeLine += fmt.Sprintf("%s %+.3f", tier, res.TrainNudges[tier])
	}
	fmt.Printf("  train nudges: %s\n", nudgeLine)
	fmt.Printf("  flips: %d (%d away from bad outcomes, %d away from good ones) · avoided points: %.3f (%.3f per flip)\n",
		res.Flips, res.FlipsAwayFromBad, res.FlipsAwayFromGood, res.AvoidedPoints, res.AvoidedPerFlip)
	fmt.Printf("verdict: %s — %s\n", res.Verdict, res.Note)
	fmt.Println("limit: the eval observes only the tier actually served; it cannot measure what the alternative tier would have done. Evidence, not proof.")
	return 0
}

func cappedSuffix(capped bool) string {
	if capped {
		return ", capped"
	}
	return ""
}

// runValidate checks the config file's schema without starting
// anything. Exit 1 when the file is missing or invalid.
func runValidate(opts options) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	if err := config.ValidateFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: invalid config: %v\n", err)
		return 1
	}
	fmt.Printf("config OK: %s\n", path)
	return 0
}

// runTray supervises the gateway headless: it starts
// `codvian-router --port PORT --config PATH serve` as a child process
// and restarts it once if it dies unexpectedly. It blocks until
// SIGINT/SIGTERM, then stops the child. (The GUI tray icon is not
// built — see internal/tray/README.md.)
func runTray(opts options) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := opts.port
	if port == 0 {
		if cfg, err := config.Load(path); err == nil {
			if p, err := cfg.ListenPort(); err == nil {
				port = p
			}
		}
	}
	if port == 0 {
		port = 20128
	}
	sup := tray.NewWithConfig(path, port)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("codvian-router tray supervisor: config %s, port %d\n", path, port)
	if err := tray.RunHeadless(ctx, sup); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "codvian-router: tray: %v\n", err)
		return 1
	}
	return 0
}

// runInit creates a fresh config file (refusing when one already exists)
// plus a first gateway key, and prints the raw key exactly once.
// writeKeyFile writes the once-only gateway key to path atomically
// (temp file + rename, 0600) for scripted installs. It refuses to
// overwrite an existing file — clobbering a saved key would lock
// the operator out — and creates the parent directory (0700) when
// missing. The caller removes the file again if a later init step
// fails, so a failed init never leaves a key file whose hash is in
// no config.
func writeKeyFile(path, raw string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("key file already exists at %s (delete it first if you really want to start over)", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot check %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create key file directory %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codvian-key-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(raw + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write key file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot set key file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot write key file: %w", err)
	}
	return nil
}

func runInit(opts options) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "codvian-router: config already exists at %s (delete it first if you really want to start over)\n", path)
		return 1
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot check %s: %v\n", path, err)
		return 1
	}
	dataDir, err := config.DefaultDataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	cfg := config.DefaultConfig(dataDir)
	if err := cfg.EnsureDataDir(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	key, raw, err := config.IssueKey("default", []string{"chat", "models", "admin"}, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot issue gateway key: %v\n", err)
		return 1
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	// --key-file: write the key first, so a config whose raw key
	// is lost can never be saved; if the save then fails, the
	// just-written file is removed again.
	keyFileWritten := false
	if opts.keyFile != "" {
		if err := writeKeyFile(opts.keyFile, raw); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		keyFileWritten = true
	}
	if err := cfg.Save(path); err != nil {
		if keyFileWritten {
			_ = os.Remove(opts.keyFile)
		}
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	fmt.Printf("codvian-router v%s initialised\n", Version)
	fmt.Printf("config: %s\n", path)
	fmt.Printf("data dir: %s\n", cfg.DataDir)
	fmt.Printf("listening on %s (auth ON)\n", cfg.Listen)
	fmt.Println()
	if opts.keyFile != "" {
		// The once-only key lives in the file, never on stdout:
		// a scripted install must not leak it into logs.
		fmt.Printf("Gateway key written ONCE to %s (0600) — save it now, it cannot be recovered.\n", opts.keyFile)
		fmt.Println()
		fmt.Printf("Use it as:  Authorization: Bearer $(cat %s)\n", opts.keyFile)
		return 0
	}
	fmt.Println("Gateway key (shown ONCE — save it now, it cannot be recovered):")
	fmt.Println(raw)
	fmt.Println()
	fmt.Println("Use it as:  Authorization: Bearer " + raw)
	return 0
}

// runKeygen prints a fresh gateway key to stdout. The key is NOT stored —
// add its hash to the config (via `init` for the first key, or by issuing
// through the config later). The explanatory note goes to stderr so the
// raw key alone can be captured from stdout.
func runKeygen(_ options) int {
	_, raw, err := config.IssueKey("keygen", nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot generate gateway key: %v\n", err)
		return 1
	}
	fmt.Println(raw)
	fmt.Fprintln(os.Stderr, "note: this key was not stored anywhere — add it to your config to use it.")
	return 0
}

// openRegistry loads the config and opens the provider registry backed
// by it (Vault-sealed keys), for the provider management subcommands.
// It builds no engine and starts nothing. The config must already
// exist — run `init` first.
func openRegistry(opts options) (*provider.Registry, string, error) {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", fmt.Errorf("cannot load config %s (run `init` first): %w", path, err)
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, "", err
	}
	vault, err := crypto.New(cfg.DataDir)
	if err != nil {
		return nil, "", err
	}
	reg := provider.NewRegistry(
		&configProviderStore{cfg: cfg, path: path},
		provider.WithCipher(provider.NewVaultCipher(vault)),
		provider.WithRegistryAllowPrivateDestinations(allowPrivateProviderEndpoints()),
	)
	if err := reg.Load(); err != nil {
		return nil, "", err
	}
	return reg, path, nil
}

// runProvider manages providers in the config file:
//
//	provider list
//	provider add <base-url> <api-key> [--name NAME]
//	provider add-catalog <catalog-id> <api-key> [--endpoint E] [--variant V] [--base-url URL]
//	provider login <catalog-id>
//	provider remove <provider-id>
//
// add-catalog resolves the base URL, wire format and auth scheme from
// the catalog entry, so the operator only supplies the ID and their
// own key. The key is tested against the upstream before the provider
// is stored (see Registry.AddCatalog); a bad key or an unreachable
// upstream rejects the add.
//
// login runs the provider's documented account-login flow headlessly
// (Phase 1: OpenRouter OAuth PKCE — see runProviderLogin).
func runProvider(opts options, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "codvian-router: provider requires an action: list, add, add-catalog, login, remove")
		return 2
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		reg, _, err := openRegistry(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		for _, p := range reg.List() {
			line := fmt.Sprintf("%s  %s  %s  wire=%s  models=%d  enabled=%v", p.ID, p.Name, p.BaseURL, p.WireFormat, len(p.Models), p.Enabled)
			if p.CatalogID != "" {
				line += fmt.Sprintf("  catalog=%s verification=%s auth=%s", p.CatalogID, p.Verification, p.AuthEndpoint)
				if p.AuthVariant != "" {
					line += " variant=" + p.AuthVariant
				}
			}
			fmt.Println(line)
		}
		return 0
	case "add":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "codvian-router: usage: provider add <base-url> <api-key> [--name NAME]")
			return 2
		}
		reg, _, err := openRegistry(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		p, err := reg.AddContext(ctx, opts.name, args[1], args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot add provider: %v\n", err)
			return 1
		}
		fmt.Printf("added provider %q (%s)\n  base_url: %s\n  wire: %s\n  models: %d\n", p.Name, p.ID, p.BaseURL, p.WireFormat, len(p.Models))
		return 0
	case "add-catalog":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "codvian-router: usage: provider add-catalog <catalog-id> <api-key> [--endpoint ENDPOINT] [--variant VARIANT] [--base-url URL]")
			return 2
		}
		reg, _, err := openRegistry(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		p, err := reg.AddCatalogWithOptions(ctx, args[1], args[2], provider.CatalogAddOptions{
			Endpoint: opts.endpoint,
			Variant:  opts.variant,
			BaseURL:  opts.baseURL,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot add catalog provider: %v\n", err)
			return 1
		}
		auth := p.AuthEndpoint
		if p.AuthVariant != "" {
			auth += " (variant " + p.AuthVariant + ")"
		}
		fmt.Printf("added catalog provider %q (%s)\n  catalog: %s (verification: %s)\n  base_url: %s\n  wire: %s\n  auth endpoint: %s\n  models: %d\n", p.Name, p.ID, p.CatalogID, p.Verification, p.BaseURL, p.WireFormat, auth, len(p.Models))
		return 0
	case "login":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "codvian-router: usage: provider login <catalog-id> (supported: openrouter)")
			return 2
		}
		return runProviderLogin(opts, args[1])
	case "remove":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "codvian-router: usage: provider remove <provider-id>")
			return 2
		}
		reg, _, err := openRegistry(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		p, ok := reg.Get(args[1])
		if !ok {
			fmt.Fprintf(os.Stderr, "codvian-router: provider %q not found\n", args[1])
			return 1
		}
		if err := reg.Remove(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot remove provider: %v\n", err)
			return 1
		}
		// The config file is the tiers' source of truth, so a
		// restart rebuilds them without the removed provider's
		// models; the running router (if any) needs DELETE
		// /api/providers/{id} instead for the live removal.
		fmt.Printf("removed provider %q (%s)\n  models removed from config: %d (restart the router to drop it live, or use DELETE /api/providers/%s on a running router)\n", p.Name, p.ID, len(p.Models), p.ID)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown provider action %q (want list, add, add-catalog, login, remove)\n", args[0])
		return 2
	}
}

// runProviderLogin runs a provider account login headlessly: it
// prints the authorization URL, the operator authorizes in a browser,
// OpenRouter displays a single-use code (headless PKCE — no callback
// listener needed, works over SSH), the operator pastes the code, and
// the exchanged API key is stored through the registry: live-tested
// against the upstream, encrypted at rest, models discovered. When an
// OpenRouter provider is already configured the key joins its pool as
// another account instead of duplicating the provider.
//
// Phase 1 supports OpenRouter only. Its official flow returns an API
// key, not an access/refresh token pair — there is no refresh token to
// renew; an expired/revoked key is rotated with `provider login` again
// or the dashboard's key rotation.
func runProviderLogin(opts options, catalogID string) int {
	if catalogID != "openrouter" {
		fmt.Fprintf(os.Stderr, "codvian-router: account login is supported for \"openrouter\" only in this build; for %q use `provider add-catalog %s <api-key>`\n", catalogID, catalogID)
		return 2
	}
	reg, _, err := openRegistry(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	flow := providerlogin.NewOpenRouterFlow()
	started, err := flow.Start(providerlogin.OpenRouterStartOptions{KeyLabel: "Codvian Router"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot start login: %v\n", err)
		return 1
	}
	fmt.Println("Open this URL in a browser, sign in to OpenRouter and authorize:")
	fmt.Println()
	fmt.Println("  " + started.AuthorizationURL)
	fmt.Println()
	fmt.Println("After authorizing, the page displays a single-use code (expires in 10 minutes).")
	fmt.Print("Paste the authorization code: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot read the authorization code: %v\n", err)
		return 1
	}
	code := strings.TrimSpace(line)
	if code == "" {
		fmt.Fprintln(os.Stderr, "codvian-router: no authorization code entered; nothing was stored")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	key, err := flow.Exchange(ctx, started.State, code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: login failed: %v\n", err)
		return 1
	}
	var p *provider.Provider
	asKey := false
	for _, existing := range reg.List() {
		if existing.CatalogID == "openrouter" {
			if _, err := reg.AddKey(ctx, existing.ID, key, provider.KeyRolePreferred); err != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: cannot store the account key: %v\n", err)
				return 1
			}
			p, asKey = existing, true
			break
		}
	}
	if p == nil {
		p, err = reg.AddCatalogWithOptions(ctx, "openrouter", key, provider.CatalogAddOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot add OpenRouter provider: %v\n", err)
			return 1
		}
	}
	if asKey {
		fmt.Printf("added an OpenRouter account key to provider %q (%s); the pool now holds %d accounts (the router fails over across them per key)\n", p.Name, p.ID, len(p.Keys))
	} else {
		fmt.Printf("added provider %q (%s)\n  wire: %s\n  models: %d\n", p.Name, p.ID, p.WireFormat, len(p.Models))
	}
	fmt.Println("note: the key is stored encrypted in the config; a running router picks it up after restart (or use the dashboard's live provider add).")
	return 0
}

// dbImportConfirm is the exact phrase the operator must type to
// import a DB export file — the same phrase POST /admin/db/import
// requires, so the CLI and the route can never drift apart.
const dbImportConfirm = "IMPORT"

// runDB manages the local database:
//
//	db import <export-file> [--confirm IMPORT]
//
// It merges the export file's usage history into the local ledger
// (ledger.db in the config's data dir) using the same code the
// server route uses (ledger.ParseExport + Ledger.ImportEvents):
// existing history is preserved, never reset; duplicate events
// are skipped, never double-counted; a corrupt file imports
// nothing. It works on the ledger file directly, so it needs no
// running server and no gateway key — the operator already has
// filesystem access to the data dir. While a server is running,
// SQLite (WAL + busy timeout) serialises the import against the
// live writer.
func runDB(opts options, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "codvian-router: db requires an action: import")
		return 2
	}
	switch args[0] {
	case "import":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "codvian-router: usage: db import <export-file> [--confirm IMPORT]")
			return 2
		}
		return runDBImport(opts, args[1])
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown db action %q (want import)\n", args[0])
		return 2
	}
}

func runDBImport(opts options, file string) int {
	phrase := opts.confirm
	if phrase == "" {
		// Interactive typed confirmation, mirroring the server
		// route's phrase check: the phrase is read from stdin,
		// never defaulted, and a non-terminal stdin that cannot
		// supply it fails instead of importing.
		fmt.Printf("Type %s to confirm merging this export file's usage history into the ledger (existing history is preserved, never reset): ", dbImportConfirm)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: cannot read confirmation: %v\n", err)
			return 1
		}
		phrase = strings.TrimSpace(line)
	}
	if phrase != dbImportConfirm {
		fmt.Fprintf(os.Stderr, "codvian-router: confirmation phrase does not match: type %s to confirm\n", dbImportConfirm)
		return 1
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot read export file %s: %v\n", file, err)
		return 1
	}
	exp, err := ledger.ParseExport(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot load config %s (run `init` first): %v\n", path, err)
		return 1
	}
	if err := cfg.EnsureDataDir(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot open ledger: %v\n", err)
		return 1
	}
	defer led.Close()
	ctx := context.Background()
	imported, dups, err := led.ImportEvents(ctx, exp.Usage.Events)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: import failed (nothing was imported): %v\n", err)
		return 1
	}
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: imported %d event(s) but the new totals could not be read: %v\n", imported, err)
		return 1
	}
	fmt.Printf("imported %d event(s), skipped %d duplicate(s) (never double-counted)\n", imported, dups)
	fmt.Printf("ledger now holds %d request(s), $%.6g total spend — existing history was preserved, nothing was reset\n", sum.Requests, sum.CostUSD)
	return 0
}
