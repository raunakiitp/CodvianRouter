// Command codvian-router is the Codvian Router Phase 1 MVP binary.
//
// Usage:
//
//	codvian-router [--config PATH] [--port PORT] serve    # default subcommand
//	codvian-router [--config PATH] init                  # create config + gateway key
//	codvian-router keygen                               # print a fresh gateway key
//
// The router is local-only: it binds 127.0.0.1 only, and auth is ON —
// requests must present a gateway key issued by `init` (or added to the
// config later). `serve` wires the whole stack: config → Vault (AES-256-GCM
// for provider keys) → SQLite ledger → provider registry → routing
// engine → OpenAI-compatible HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
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
}

// parseArgs extracts --config and --port (in "--flag value" or
// "--flag=value" form, before or after the subcommand) and returns the
// remaining positional arguments. The first positional is the subcommand;
// when there is none, "serve" is the default.
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
  codvian-router help                                 show this help

Flags:
  --config PATH   config file (default ~/.config/codvian-router/config.json)
  --port PORT     listen port for serve (binds 127.0.0.1 only)

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
// The router only reports a total token count per attempt, so it is
// recorded as completion tokens with prompt tokens 0; the split is
// unknown at this layer. Cost is 0 in this build because no per-model
// prices are configured (Prices nil) — prices are never invented.
// GatewayKeyID is empty: the engine does not know which gateway key a
// request arrived under. A RecordUsage failure is logged to stderr and
// never fails the request.
type ledgerAdapter struct {
	l *ledger.Ledger
}

func (a *ledgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, tokens int) {
	if a == nil || a.l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.l.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID:     "",
		Provider:         providerName,
		Model:            model,
		PromptTokens:     0,
		CompletionTokens: tokens,
		OK:               ok,
		LatencyMS:        latencyMs,
	}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: ledger record failed: %v\n", err)
	}
}

// runServe loads (or creates) the config, applies --port, opens the
// ledger, unlocks provider keys through the Vault, builds the provider
// registry and the routing engine, and serves the OpenAI-compatible
// gateway on cfg.Listen until SIGINT/SIGTERM.
func runServe(opts options) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrCreate(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	if opts.port != 0 {
		if err := cfg.SetListenPort(opts.port); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
			return 1
		}
	}
	if err := cfg.EnsureDataDir(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}

	// Vault: master secret lives in the data dir (created on first run).
	vault, err := crypto.New(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}

	// Ledger: SQLite in the data dir.
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	defer led.Close()
	led.FailClosed = cfg.FailClosed

	// Provider registry: providers live in the config file, keys sealed
	// with the Vault.
	reg := provider.NewRegistry(
		&configProviderStore{cfg: cfg, path: path},
		provider.WithCipher(provider.NewVaultCipher(vault)),
	)
	if err := reg.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
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
	}

	// Routing engine: fallback chains come from the registry — enabled
	// providers that offer the requested model, in registry order.
	// Providers with no model list (chat-only gateways) are candidates
	// for any model; the upstream rejects unknown models and the
	// engine fails over.
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
	engine := router.NewEngine(clients, &ledgerAdapter{l: led}, router.Config{ChainFor: chainFor})

	srv := server.New(engine, server.WithConfig(cfg), server.WithProviderRegistry(reg))
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv}

	fmt.Printf("codvian-router v%s listening on %s (auth ON)\n", Version, cfg.Listen)
	fmt.Printf("config: %s\n", path)
	fmt.Printf("providers: %d configured, %d clients ready\n", len(reg.List()), len(clients))

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
