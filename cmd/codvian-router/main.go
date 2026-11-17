// Command codvian-router is the Codvian Router Phase 1 MVP binary.
//
// Usage:
//
//	codvian-router [--config PATH] [--port PORT] serve    # default subcommand
//	codvian-router [--config PATH] init                  # create config + gateway key
//	codvian-router keygen                               # print a fresh gateway key
//	codvian-router [--config PATH] [--model MODEL] eval  # smoke-eval the live engine
//	codvian-router [--config PATH] validate             # validate the config file
//	codvian-router [--config PATH] [--port PORT] tray   # supervise the gateway headless
//	codvian-router [--config PATH] provider list        # list configured providers
//	codvian-router [--config PATH] provider add <base-url> <api-key> [--name NAME]
//	codvian-router [--config PATH] provider add-catalog <catalog-id> <api-key> [--endpoint E] [--variant V] [--base-url URL]
//
// The router is local-only by default: it binds 127.0.0.1 only, and auth
// is ON — requests must present a gateway key issued by `init` (or added
// to the config later). `serve` wires the whole stack: config → Vault
// (AES-256-GCM for provider keys) → SQLite ledger → provider registry →
// routing engine (scoring + quota hints + health watcher + graceful
// degradation) → OpenAI-compatible HTTP server with /metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/eval"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
	"github.com/raunakiitp/codvian-router/internal/server"
	"github.com/raunakiitp/codvian-router/internal/tray"
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
	// provider add-catalog flags.
	endpoint string
	variant  string
	baseURL  string
	name     string
}

// parseArgs extracts --config, --port, --listen and --model (in "--flag
// value" or "--flag=value" form, before or after the subcommand) and
// returns the remaining positional arguments. The first positional is
// the subcommand; when there is none, "serve" is the default.
func parseArgs(args []string) (options, []string, error) {
	var opts options
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
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
		case a == "-h" || a == "--help" || a == "help":
			positional = append(positional, "help")
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
		return runInit(opts)
	case "keygen":
		return runKeygen(opts)
	case "eval":
		return runEval(opts)
	case "validate":
		return runValidate(opts)
	case "tray":
		return runTray(opts)
	case "provider":
		return runProvider(opts, positional[1:])
	case "help":
		usage(os.Stdout)
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
  codvian-router [--config PATH] init                  create config + a gateway key
  codvian-router keygen                               print a fresh gateway key (not stored)
  codvian-router [--config PATH] [--model MODEL] eval  smoke-eval the live engine
  codvian-router [--config PATH] validate             validate the config file
  codvian-router [--config PATH] [--port PORT] tray   supervise the gateway headless
  codvian-router [--config PATH] provider list        list configured providers
  codvian-router [--config PATH] provider add <base-url> <api-key> [--name NAME]
  codvian-router [--config PATH] provider add-catalog <catalog-id> <api-key> [--endpoint E] [--variant V] [--base-url URL]
  codvian-router help                                 show this help

Flags:
  --config PATH   config file (default ~/.config/codvian-router/config.json)
  --port PORT     listen port for serve (binds 127.0.0.1 only)
  --listen ADDR   explicit listen address for serve, host:port (127.0.0.1, ::1, or
                  0.0.0.0). 0.0.0.0 binds all interfaces — trusted networks only.
                  A runtime override; it is never written to the config file.
  --model MODEL   model for eval
  --endpoint E    catalog surface for provider add-catalog (default: the entry's default)
  --variant V     catalog surface variant for provider add-catalog (e.g. alt-header, api-key)
  --base-url URL  base-URL override for provider add-catalog (proxy, mirror, per-resource URL, mock)
  --name NAME     display name for provider add

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

// ledgerAdapter adapts *ledger.Ledger to the router's small Ledger
// interface (RecordAttempt). Attempts are recorded through
// Ledger.RecordUsage, which also keeps the per-key budget spend in sync.
//
// The router passes the full token split reported by the upstream.
// The split is known only when the upstream actually reported
// prompt/completion tokens; a failed or streaming attempt arrives as
// provider.Usage{} (unknown), and an upstream that reports only a
// total arrives with prompt/completion 0 — both are recorded with
// SplitKnown false, total_tokens as reported, and cost 0, never a
// guessed split. Cost is 0 in this build because no per-model prices
// are configured (Prices nil) — prices are never invented.
// GatewayKeyID is empty: the engine does not know which gateway key a
// request arrived under. A RecordUsage failure is logged to stderr and
// never fails the request.
type ledgerAdapter struct {
	l *ledger.Ledger
}

func (a *ledgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	if a == nil || a.l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.l.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID:     "",
		Provider:         providerName,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		// The split is known only when the upstream reported at
		// least one side of it. A zero/zero split with a total (or
		// no usage at all, e.g. a failed or streaming attempt) is
		// total-only / unknown, not a 0/0 split.
		SplitKnown: usage.PromptTokens != 0 || usage.CompletionTokens != 0,
		OK:         ok,
		LatencyMS:  latencyMs,
	}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: ledger record failed: %v\n", err)
	}
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
	exact    *cache.ExactCache
	checker  *health.Checker
	recorder *metrics.Recorder
	led      *ledger.Ledger
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
// Scoring signals, honestly: latency and success come from the health
// checker's last key probe (a single probe is a thin sample — OK is
// 1.0, a failed probe is 0.0, no probe yet is unknown); quota comes
// from the registry hint, preferring a probe-measured hint when one
// exists. Cost and quality stay 0 (unknown): no per-model prices are
// configured and no quality signal exists yet, so both components
// score equally for every candidate and ordering is driven by
// latency, success and quota alone.
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
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		return nil, err
	}
	led.FailClosed = cfg.FailClosed

	// Provider registry: providers live in the config file, keys sealed
	// with the Vault.
	reg := provider.NewRegistry(
		&configProviderStore{cfg: cfg, path: path},
		provider.WithCipher(provider.NewVaultCipher(vault)),
	)
	if err := reg.Load(); err != nil {
		led.Close()
		return nil, err
	}

	// Upstream clients, one per enabled provider. A provider whose key
	// cannot be decrypted (e.g. sealed under a different master secret)
	// or whose base URL is invalid is skipped with a warning — it must
	// not take the whole gateway down.
	clients := make(map[string]provider.Client)
	for _, p := range reg.List() {
		if !p.Enabled {
			continue
		}
		client, err := reg.ClientFor(p.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: skipping provider %q (%s): %v\n", p.Name, p.ID, err)
			continue
		}
		clients[p.ID] = client
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

	// Key health / quota intelligence: probes provider keys and applies
	// quota hints (0 on quota errors) to the registry. The watcher is
	// started by runServe; runEval does one synchronous probe instead.
	checker := health.New(reg, nil)

	// Scoring signals from the checker's last probe + registry hints.
	quotaHint := router.QuotaHintFunc(reg)
	signalsFn := func(providerID string) scoring.Signals {
		sig := scoring.Signals{QuotaHint: quotaHint(providerID)}
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
			}
			break
		}
		return sig
	}

	// Routing engine: fallback chains come from the registry — enabled
	// providers that offer the requested model, in registry order.
	// Providers with no model list (chat-only gateways) are candidates
	// for any model; the upstream rejects unknown models and the
	// engine fails over. The scoring hook reorders the chain by
	// ModeBalanced; it never drops or duplicates an attempt.
	chainFor := func(model string) []router.Attempt {
		var chain []router.Attempt
		for _, p := range reg.List() {
			if !p.Enabled {
				continue
			}
			if _, ok := clients[p.ID]; !ok {
				continue
			}
			if len(p.Models) == 0 {
				chain = append(chain, router.Attempt{ProviderID: p.ID, Model: model})
				continue
			}
			for _, m := range p.Models {
				if m.ID == model {
					chain = append(chain, router.Attempt{ProviderID: p.ID, Model: model})
					break
				}
			}
		}
		return chain
	}
	engine := router.NewEngine(clients, &ledgerAdapter{l: led}, router.Config{
		ChainFor:  chainFor,
		QuotaHint: quotaHint,
		OrderHook: scoring.Hook(signalsFn, scoring.ModeBalanced),
	})

	// Graceful degradation: successful responses are cached in the
	// exact-match cache (500 entries, 1h store TTL); when every
	// provider fails, a cached response is served instead, marked
	// X-Codvian-Stale by the server. Enabled in serve mode.
	exact := cache.NewExactCache(500)
	degradePolicy := degrade.DefaultPolicy()
	degradePolicy.Enabled = true
	serving := degrade.Wrap(engine, exact, degradePolicy)

	return &stack{
		cfg: cfg, path: path, listen: listen,
		reg: reg, clients: clients,
		engine: engine, serving: serving, exact: exact,
		checker: checker, recorder: metrics.NewRecorder(),
		led: led, vault: vault,
	}, nil
}

// runServe builds the stack, starts the health watcher, and serves
// the OpenAI-compatible gateway on the effective listen address until
// SIGINT/SIGTERM.
func runServe(opts options) int {
	st, err := openStack(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	defer st.led.Close()

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

	srv := server.New(st.serving,
		server.WithConfig(st.cfg),
		server.WithProviderRegistry(st.reg),
		server.WithMetrics(st.recorder),
	)
	httpSrv := &http.Server{Addr: st.listen, Handler: srv}

	fmt.Printf("codvian-router v%s listening on %s (auth ON)\n", Version, st.listen)
	fmt.Printf("config: %s\n", st.path)
	fmt.Printf("providers: %d configured, %d clients ready\n", len(st.reg.List()), len(st.clients))
	fmt.Println("degradation: ON (stale responses served with X-Codvian-Stale)")
	fmt.Println("metrics: GET /metrics (gateway-key auth required)")
	fmt.Printf("dashboard: http://%s/dashboard\n", st.listen)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

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
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
		return 0
	}
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
	key, raw, err := config.IssueKey("default", []string{"chat", "models"}, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot issue gateway key: %v\n", err)
		return 1
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	if err := cfg.Save(path); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	fmt.Printf("codvian-router v%s initialised\n", Version)
	fmt.Printf("config: %s\n", path)
	fmt.Printf("data dir: %s\n", cfg.DataDir)
	fmt.Printf("listening on %s (auth ON)\n", cfg.Listen)
	fmt.Println()
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
//
// add-catalog resolves the base URL, wire format and auth scheme from
// the catalog entry, so the operator only supplies the ID and their
// own key. The key is tested against the upstream before the provider
// is stored (see Registry.AddCatalog); a bad key or an unreachable
// upstream rejects the add.
func runProvider(opts options, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "codvian-router: provider requires an action: list, add, add-catalog")
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
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown provider action %q (want list, add, add-catalog)\n", args[0])
		return 2
	}
}
