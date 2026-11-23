// Slice 5 (MCP + CLI polish): the `doctor`, `configure` and `mcp`
// subcommands.
//
//   - `doctor` runs real checks (config validation, gateway-key
//     presence, provider-key presence + live reachability probe,
//     listen-port state, dashboard reachability) and prints a
//     PASS/FAIL/WARN line per check. Keys are never printed — a key
//     check reports only "present" or "missing".
//   - `configure` is the first-run setup flow (the reference router's `configure`
//     / `setup` shape, studied in the research log entry 19): it
//     creates the config + first gateway key when missing, applies a
//     --port, and adds a provider by catalog id with a real key test.
//     With no flags it prompts interactively on stdin; piped input
//     (or EOF) answers the prompts, so it works in scripts too.
//   - `mcp` runs the MCP stdio server (internal/mcp) backed by the
//     real registry — stdout is the protocol, so everything human
//     goes to stderr.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/strategy"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// doctorCheck is one doctor result: status is PASS, FAIL or WARN.
type doctorCheck struct {
	status string
	name   string
	detail string
}

func (c doctorCheck) String() string {
	if c.detail == "" {
		return fmt.Sprintf("[%s] %s", c.status, c.name)
	}
	return fmt.Sprintf("[%s] %s — %s", c.status, c.name, c.detail)
}

func runDoctor(opts options) int {
	return runDoctorTo(opts, os.Stdout)
}

// runDoctorTo runs every check it can and reports pass/fail. Exit 1
// when any check FAILs; WARNs never fail the run (a stopped server
// or zero providers is a state to fix, not a broken install).
func runDoctorTo(opts options, out io.Writer) int {
	var checks []doctorCheck
	failures := 0
	add := func(status, name, detail string) {
		checks = append(checks, doctorCheck{status: status, name: name, detail: detail})
		if status == "FAIL" {
			failures++
		}
	}

	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		add("FAIL", "config path", err.Error())
	} else if err := config.ValidateFile(path); err != nil {
		add("FAIL", "config", fmt.Sprintf("%s: %v", path, err))
	} else {
		add("PASS", "config", fmt.Sprintf("%s is valid", path))
	}

	// Everything below needs a loadable config; without one the port
	// and dashboard checks still run against the default listen.
	var cfg *config.Config
	listen := config.DefaultListen
	if path != "" {
		if c, err := config.Load(path); err == nil {
			cfg = c
			listen = c.Listen
			if len(c.GatewayKeys) > 0 {
				add("PASS", "gateway keys", fmt.Sprintf("%d key(s) configured (values never printed)", len(c.GatewayKeys)))
			} else {
				add("FAIL", "gateway keys", "no gateway key — run `codvian-router init` or POST /admin/bootstrap once the server runs")
			}
			if st, err := os.Stat(c.DataDir); err != nil || !st.IsDir() {
				add("FAIL", "data dir", fmt.Sprintf("%s is missing or not a directory: %v", c.DataDir, err))
			} else {
				add("PASS", "data dir", c.DataDir)
			}
		}
	}

	// --port overrides the listen address checked, the same way it
	// overrides the serve address: the port/dashboard checks then
	// probe the server that was actually started with that flag,
	// instead of the port in the config file.
	if opts.port != 0 {
		listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.port))
	}

	// Providers: key present (never printed) + live reachability.
	if cfg != nil {
		reg, _, err := openRegistry(opts)
		if err != nil {
			add("FAIL", "providers", fmt.Sprintf("cannot open registry: %v", err))
		} else {
			providers := reg.List()
			if len(providers) == 0 {
				add("WARN", "providers", "no providers configured — add one with `provider add-catalog <id> <key>`")
			}
			for _, p := range providers {
				key, err := reg.DecryptedKey(p.ID)
				if err != nil || key == "" {
					add("FAIL", "provider "+p.Name+" key", "key is missing or cannot be decrypted (stored key unreadable)")
					continue
				}
				add("PASS", "provider "+p.Name+" key", "present (value never printed)")
			}
			if len(providers) > 0 {
				// Live probe: the health checker's key test is a
				// real /v1/models call with a 5 s per-provider
				// timeout, so this detects dead keys and quota
				// errors, not just config presence.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				results := health.New(reg, nil).CheckAll(ctx)
				cancel()
				byID := make(map[string]health.Status, len(results))
				for _, st := range results {
					byID[st.ProviderID] = st
				}
				for _, p := range providers {
					st, ok := byID[p.ID]
					if !ok {
						add("WARN", "provider "+p.Name+" reachability", "no probe result (provider disabled or skipped)")
						continue
					}
					if st.OK {
						add("PASS", "provider "+p.Name+" reachability", fmt.Sprintf("key test ok in %d ms", st.LatencyMs))
					} else {
						add("FAIL", "provider "+p.Name+" reachability", fmt.Sprintf("key test failed in %d ms: %s", st.LatencyMs, st.LastError))
					}
				}
			}
		}
	}

	// Listen port: a dial success means something is already serving
	// there (usually this router); refused means the port is free.
	_, portStr, splitErr := net.SplitHostPort(listen)
	if splitErr != nil {
		add("FAIL", "listen address", fmt.Sprintf("%q is not a valid host:port: %v", listen, splitErr))
	} else {
		_ = portStr // the port is part of listen; dialling listen checks it
		conn, err := net.DialTimeout("tcp", listen, 2*time.Second)
		serverRunning := false
		if err == nil {
			_ = conn.Close()
			serverRunning = true
			add("WARN", "listen port", fmt.Sprintf("%s is already in use — a server may already be running there", listen))
		} else {
			add("PASS", "listen port", fmt.Sprintf("%s is free", listen))
		}

		// Dashboard: reachable only when a server is actually up.
		dashboardURL := fmt.Sprintf("http://%s/dashboard", listen)
		if !serverRunning {
			add("WARN", "dashboard", fmt.Sprintf("%s not reachable — the server is not running (start it with `codvian-router serve`)", dashboardURL))
		} else {
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(dashboardURL)
			if err != nil {
				add("WARN", "dashboard", fmt.Sprintf("%s did not answer: %v", dashboardURL, err))
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					add("PASS", "dashboard", dashboardURL)
				} else {
					add("WARN", "dashboard", fmt.Sprintf("%s answered HTTP %d", dashboardURL, resp.StatusCode))
				}
			}
		}
	}

	// Agent integration check (CodeVN)
	codevnConfigPath := os.Getenv("CODEVN_CONFIG_PATH")
	if codevnConfigPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			codevnConfigPath = filepath.Join(home, ".codevn", "config.json")
		}
	}
	if codevnConfigPath != "" {
		if data, err := os.ReadFile(codevnConfigPath); err == nil {
			var cvn map[string]any
			if json.Unmarshal(data, &cvn) == nil {
				endpoint, _ := cvn["api_endpoint"].(string)
				model, _ := cvn["model"].(string)
				add("PASS", "agent integration (CodeVN)", fmt.Sprintf("configured at %s (endpoint: %s, model: %s)", codevnConfigPath, endpoint, model))
			} else {
				add("WARN", "agent integration (CodeVN)", fmt.Sprintf("%s exists but has invalid JSON", codevnConfigPath))
			}
		} else {
			add("WARN", "agent integration (CodeVN)", "not configured yet (run `codvian-router setup codevn` to integrate)")
		}
	}

	for _, c := range checks {
		fmt.Fprintln(out, c.String())
	}
	passed, warned := 0, 0
	for _, c := range checks {
		switch c.status {
		case "PASS":
			passed++
		case "WARN":
			warned++
		}
	}
	fmt.Fprintf(out, "doctor: %d passed, %d failed, %d warnings\n", passed, failures, warned)
	if failures > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// configure
// ---------------------------------------------------------------------------

func runConfigure(opts options) int {
	return runConfigureIO(opts, os.Stdin, os.Stdout)
}

// runConfigureIO is the first-run setup flow. It creates the config
// (plus a first gateway key, printed once) when missing, applies
// --port, and adds a provider by catalog id when --provider is given
// (key from --api-key, --api-key-env, or an interactive prompt).
// With no flags it prompts on stdin: an empty answer keeps the
// current value (port) or skips the step (provider); EOF ends the
// prompts the same way, so piping answers — or /dev/null — works.
func runConfigureIO(opts options, in io.Reader, out io.Writer) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(out, "codvian-router: %v\n", err)
		return 1
	}

	created := false
	cfg, err := config.Load(path)
	if err != nil {
		// errors.Is, not os.IsNotExist: config.Load wraps the
		// read error with fmt.Errorf %w, and os.IsNotExist does
		// not unwrap %w chains (it only unwraps *PathError and
		// friends) — it returns false here even for a missing
		// file. errors.Is unwraps properly.
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "codvian-router: cannot load config %s: %v\n", path, err)
			return 1
		}
		dataDir, err := config.DefaultDataDir()
		if err != nil {
			fmt.Fprintf(out, "codvian-router: %v\n", err)
			return 1
		}
		cfg = config.DefaultConfig(dataDir)
		if err := cfg.EnsureDataDir(); err != nil {
			fmt.Fprintf(out, "codvian-router: %v\n", err)
			return 1
		}
		created = true
	}
	if created {
		key, raw, err := config.IssueKey("default", []string{"chat", "models"}, 0)
		if err != nil {
			fmt.Fprintf(out, "codvian-router: cannot issue gateway key: %v\n", err)
			return 1
		}
		if err := cfg.AddGatewayKey(key); err != nil {
			fmt.Fprintf(out, "codvian-router: %v\n", err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(out, "codvian-router: cannot save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "created config: %s\n", path)
		fmt.Fprintln(out, "Gateway key (shown ONCE — save it now, it cannot be recovered):")
		fmt.Fprintln(out, raw)
		fmt.Fprintln(out, "Use it as:  Authorization: Bearer <key>")
		fmt.Fprintln(out)
	}

	// --port (flag mode) or an interactive prompt.
	if opts.port != 0 {
		if err := cfg.SetListenPort(opts.port); err != nil {
			fmt.Fprintf(out, "codvian-router: %v\n", err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(out, "codvian-router: cannot save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "listen: %s\n", cfg.Listen)
	}

	providerID := opts.providerID
	apiKey := opts.apiKey
	if opts.apiKeyEnv != "" {
		if v := os.Getenv(opts.apiKeyEnv); v != "" {
			apiKey = v
		} else {
			fmt.Fprintf(out, "codvian-router: --api-key-env %s is not set in the environment\n", opts.apiKeyEnv)
			return 2
		}
	}
	if providerID != "" && apiKey == "" {
		fmt.Fprintln(out, "codvian-router: --provider needs an API key: pass --api-key <key> or --api-key-env <VAR>")
		return 2
	}
	if providerID == "" && apiKey != "" {
		fmt.Fprintln(out, "codvian-router: an API key without --provider does nothing — pass --provider <catalog-id> too")
		return 2
	}

	// Interactive prompts, only when no provider flag was given and
	// no port flag was given (flag mode is non-interactive by
	// definition). A line of input is read per prompt; empty input
	// keeps/skips, EOF ends prompting.
	if providerID == "" && opts.port == 0 {
		reader := bufio.NewReader(in)
		prompt := func(label string) string {
			fmt.Fprint(out, label)
			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				return ""
			}
			return strings.TrimSpace(line)
		}
		if answer := prompt(fmt.Sprintf("Port [%s]: ", listenPort(cfg))); answer != "" {
			p, err := strconv.Atoi(answer)
			if err != nil || p < 1 || p > 65535 {
				fmt.Fprintf(out, "codvian-router: %q is not a valid port\n", answer)
				return 2
			}
			if err := cfg.SetListenPort(p); err != nil {
				fmt.Fprintf(out, "codvian-router: %v\n", err)
				return 1
			}
			if err := cfg.Save(path); err != nil {
				fmt.Fprintf(out, "codvian-router: cannot save config: %v\n", err)
				return 1
			}
			fmt.Fprintf(out, "listen: %s\n", cfg.Listen)
		}
		providerID = prompt("Provider catalog id (e.g. groq, openai, anthropic — empty to skip): ")
		if providerID != "" {
			apiKey = prompt(fmt.Sprintf("API key for %s (typed key is never printed): ", providerID))
			if apiKey == "" {
				fmt.Fprintln(out, "codvian-router: no API key given — provider not added")
				providerID = ""
			}
		}
	}

	if providerID != "" {
		if _, ok := catalog.ByID(providerID); !ok {
			fmt.Fprintf(out, "codvian-router: unknown catalog provider %q (see `provider add-catalog` for the catalog ids)\n", providerID)
			return 1
		}
		reg, _, err := openRegistry(opts)
		if err != nil {
			fmt.Fprintf(out, "codvian-router: %v\n", err)
			return 1
		}
		p, err := reg.AddCatalogWithOptions(context.Background(), providerID, apiKey, provider.CatalogAddOptions{BaseURL: opts.baseURL})
		if err != nil {
			// AddCatalog tests the key against the upstream, so a
			// failure here is a real rejection (bad key, dead
			// upstream, ToS-grey entry) — reported, not hidden.
			fmt.Fprintf(out, "codvian-router: cannot add provider %q: %v\n", providerID, err)
			return 1
		}
		fmt.Fprintf(out, "added provider %q (%s) — wire %s, %d model(s)\n", p.Name, p.ID, p.WireFormat, len(p.Models))
	}

	// Final summary: reload so the counts are what is on disk, not
	// what this process remembers.
	cfg, err = config.Load(path)
	if err != nil {
		fmt.Fprintf(out, "codvian-router: cannot reload config %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(out, "config: %s\n", path)
	fmt.Fprintf(out, "listening on %s (auth ON)\n", cfg.Listen)
	fmt.Fprintf(out, "providers: %d configured, gateway keys: %d\n", len(cfg.Providers), len(cfg.GatewayKeys))
	fmt.Fprintf(out, "dashboard: http://%s/dashboard\n", cfg.Listen)
	fmt.Fprintln(out, "next: `codvian-router doctor` to check the setup, then `codvian-router serve`")
	return 0
}

func listenPort(cfg *config.Config) string {
	if cfg == nil {
		return strconv.Itoa(config.DefaultPort)
	}
	if p, err := cfg.ListenPort(); err == nil {
		return strconv.Itoa(p)
	}
	return strconv.Itoa(config.DefaultPort)
}

// ---------------------------------------------------------------------------
// mcp
// ---------------------------------------------------------------------------

// registryMCPSource adapts the provider registry + config file to the
// MCP Source interface: every tool call reads live registry state.
// Uptime is -1: the stdio server has no serving loop, so no uptime
// is known — reported honestly, never guessed.
type registryMCPSource struct {
	reg *provider.Registry
	cfg *config.Config
	// configPath is the file the combo/strategy mutations persist
	// to (runMCPTo's openRegistry path). "" means "unknown" — a
	// mutation then refuses instead of writing nowhere.
	configPath string
	// led is the lazily-opened ledger (M9.3; see openLedger).
	// ledOpened sticks the first open outcome, including the
	// error: a missing data dir does not become present.
	ledMu     sync.Mutex
	led       *ledger.Ledger
	ledErr    error
	ledOpened bool
	// prices is the lazily-resolved price map (M9.3).
	pricesMu     sync.Mutex
	prices       ledger.Prices
	pricesErr    error
	pricesLoaded bool
	// activeCombo is the session-active combo (M9.3; see Source).
	activeMu    sync.Mutex
	activeCombo string
}

func (s *registryMCPSource) Providers() []mcp.ProviderInfo {
	if s.reg == nil {
		return []mcp.ProviderInfo{}
	}
	out := []mcp.ProviderInfo{}
	for _, p := range s.reg.List() {
		models := []string{}
		for _, m := range p.Models {
			models = append(models, m.ID)
		}
		out = append(out, mcp.ProviderInfo{
			ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
			Enabled: p.Enabled, Models: models,
			CatalogID: p.CatalogID, Verification: p.Verification,
		})
	}
	return out
}

func (s *registryMCPSource) Models() []mcp.ModelInfo {
	if s.reg == nil {
		return []mcp.ModelInfo{}
	}
	seen := make(map[string]bool)
	out := []mcp.ModelInfo{}
	for _, p := range s.reg.List() {
		for _, m := range p.Models {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, mcp.ModelInfo{ID: m.ID, ProviderID: p.ID, ProviderName: p.Name})
		}
	}
	// Virtual auto/* models, when the auto-router can resolve them —
	// the same rule the request path uses (buildAutoRouter). No
	// ranker here: this only checks resolvability, not order.
	if s.cfg != nil && buildAutoRouter(s.cfg, s.reg, nil, nil) != nil {
		for _, v := range autorouter.VirtualModels {
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, mcp.ModelInfo{ID: v, ProviderID: "auto-router", ProviderName: "auto-router"})
		}
	}
	return out
}

// M9.3 — routing/combo + cost/quota Source methods.
//
// Combos ARE the config routing groups, so combo reads come from
// GroupsSnapshot / ChainStrategy and combo/strategy writes mutate
// Routing under its write lock and persist with cfg.Save (the
// same mirror + persist-or-rollback shape as the /v1/combos API:
// Save validates, so a mutation that breaks validation is rolled
// back and reported, never left half-applied). Save is never
// called while holding the routing lock (see config.Save).
//
// The ledger is opened LAZILY, only when a metrics/cost/quota
// tool runs: the stdio server must stay usable on a fresh config
// with no ledger yet. Reads NEVER create the file (M9.4 rule — a
// read tool must not create database files as a side effect):
// a missing ledger.db means "no usage recorded yet" and the
// read reports the ledger unavailable. Only set_budget_guard
// (a write by definition) opens-or-creates it. The open uses
// ledger.Open (WAL + 5 s busy timeout), so a ledger the serve
// process holds stays readable; a second process writing a
// budget while serve runs is serialised by SQLite, and a lock
// failure surfaces as an honest error, never a silent skip.
//
// The active combo is session state in this process (see Source
// in internal/mcp/mcp.go): the serving router has no single
// active combo, so there is nothing to persist it to.
func (s *registryMCPSource) ledgerPath() (string, bool) {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return "", false
	}
	return filepath.Join(s.cfg.DataDir, "ledger.db"), true
}

// openLedger returns the ledger handle, opening it once and
// caching the result (including a sticky open error — a missing
// data dir does not become present mid-process). create=false
// refuses to create a missing ledger.db (read path); create=true
// opens-or-creates it (set_budget_guard only).
func (s *registryMCPSource) openLedger(create bool) (*ledger.Ledger, error) {
	s.ledMu.Lock()
	defer s.ledMu.Unlock()
	if s.ledOpened {
		return s.led, s.ledErr
	}
	s.ledOpened = true
	path, ok := s.ledgerPath()
	if !ok {
		s.ledErr = errors.New("ledger unavailable: no data dir configured")
		return nil, s.ledErr
	}
	if !create {
		if _, err := os.Stat(path); err != nil {
			s.ledErr = fmt.Errorf("ledger unavailable: no ledger yet at %s (no usage recorded)", path)
			return nil, s.ledErr
		}
	}
	led, err := ledger.Open(path)
	if err != nil {
		s.ledErr = fmt.Errorf("ledger unavailable: cannot open %s: %w", path, err)
		return nil, s.ledErr
	}
	s.led = led
	return led, nil
}

// ledgerPrices resolves the real per-token price map from the
// price book + the registry (the same buildLedgerPrices the serve
// path uses), once per process. A price-book load failure is
// sticky and prices every pair as unknown — never a guessed rate.
func (s *registryMCPSource) ledgerPrices() (ledger.Prices, error) {
	s.pricesMu.Lock()
	defer s.pricesMu.Unlock()
	if s.pricesLoaded {
		return s.prices, s.pricesErr
	}
	s.pricesLoaded = true
	if s.cfg == nil {
		s.pricesErr = errors.New("price book unavailable: no config loaded")
		return nil, s.pricesErr
	}
	book, err := s.cfg.LoadPriceBook()
	if err != nil {
		s.pricesErr = fmt.Errorf("price book unavailable: %w", err)
		return nil, s.pricesErr
	}
	s.prices = buildLedgerPrices(book, s.reg)
	return s.prices, nil
}

// PriceKnown reports whether the real price book prices one
// (provider, model) pair. An unavailable price book prices
// everything as unknown.
func (s *registryMCPSource) PriceKnown(provider, model string) bool {
	prices, err := s.ledgerPrices()
	if err != nil {
		return false
	}
	return prices.Known(provider, model)
}

// promptUSDPer1M returns the prompt rate per 1M tokens for one
// (provider, model) pair from the real price book, and whether
// it is known. Unknown is nil — never a guessed rate, never $0.
func (s *registryMCPSource) promptUSDPer1M(provider, model string) (*float64, bool) {
	prices, err := s.ledgerPrices()
	if err != nil || !prices.Known(provider, model) {
		return nil, false
	}
	rate := prices.Cost(provider, model, 1000000, 0)
	return &rate, true
}

// providerFacts derives one provider's liveness from the live
// config — the same rule as the /v1/combos API (comboStepFacts):
// a key pool entry or a legacy encrypted key counts as a key; a
// disabled provider is unusable; a keyless provider is usable
// only when it is a no-auth local (ollama wire format, or a
// loopback base URL).
func mcpProviderUsable(p config.ProviderConfig) (usable bool, reason string) {
	hasKey := len(p.Keys) > 0 || p.APIKeyEncrypted != ""
	if !p.Enabled {
		return false, "provider disabled"
	}
	if hasKey {
		return true, ""
	}
	if p.WireFormat == "ollama" {
		return true, ""
	}
	if u, err := url.Parse(p.BaseURL); err == nil && u.Hostname() != "" {
		if host := u.Hostname(); host == "localhost" {
			return true, ""
		} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return true, ""
		}
	}
	return false, "provider has no key"
}

// Combos lists every config routing group as a combo, sorted by
// name, with the strategy from routing.chain_strategies
// ("priority" when none is configured) and the session-active
// flag. A nil config yields no combos — never an invented one.
func (s *registryMCPSource) Combos() []mcp.ComboInfo {
	out := []mcp.ComboInfo{}
	if s.cfg == nil {
		return out
	}
	groups := s.cfg.GroupsSnapshot()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	byID := make(map[string]config.ProviderConfig, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		byID[p.ID] = p
	}
	active := s.ActiveCombo()
	for _, name := range names {
		members := groups[name]
		usable := true
		reason := ""
		for _, member := range members {
			provID, _, err := config.ParseGroupMember(member)
			if err != nil {
				usable = false
				reason = "invalid group member " + member
				break
			}
			p, ok := byID[provID]
			if !ok {
				usable = false
				reason = "provider not configured: " + provID
				break
			}
			if okUsable, why := mcpProviderUsable(p); !okUsable {
				usable = false
				reason = provID + ": " + why
				break
			}
		}
		strategy := "priority"
		if st, ok := s.cfg.ChainStrategy(name); ok {
			strategy = st
		}
		out = append(out, mcp.ComboInfo{
			Name: name, Members: append([]string(nil), members...),
			Strategy: strategy, Active: name == active,
			Usable: usable, UnusableReason: reason,
		})
	}
	return out
}

// comboStrategyName validates a combo strategy: a canonical
// strategy name (aliases accepted, normalised), or one of the
// three combo execution modes (fusion/pipeline/jev — the
// /v1/combos API's comboExecutionStrategies, which live in the
// combo record only and are never mirrored into
// routing.chain_strategies).
func comboStrategyName(raw string) (string, bool) {
	if st, ok := strategy.Parse(raw); ok {
		return string(st), true
	}
	norm := strings.ToLower(strings.TrimSpace(raw))
	switch norm {
	case "fusion", "pipeline", "jev":
		return norm, true
	}
	return "", false
}

// comboValidName enforces the routing-group name rule (config
// validation): non-empty, no ":" or "/" (alias syntax). The
// messages match ComboStore.comboValidName in
// internal/server/combos_api.go verbatim, so the stdio and HTTP
// sources reject the same name with the same text (both wrapped
// in mcp.ErrInvalidInput by their CreateCombo).
func comboValidName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return fmt.Errorf("%w: name: must not be empty", mcp.ErrInvalidInput)
	case strings.ContainsAny(name, ":/"):
		return fmt.Errorf("%w: name: %q must not contain \":\" or \"/\" (routing group names use \"group:<name>/<model>\" alias syntax)", mcp.ErrInvalidInput, name)
	}
	return nil
}

// comboChainStep is one validated chain step: the provider, an
// optional model pin and an optional (normalised) reasoning
// effort — the same triple ComboStore.parseChainStep in
// internal/server/combos_api.go produces.
type comboChainStep struct {
	provider string
	model    string
	effort   string
}

// comboStepLabel renders a step the way ComboStore.comboStepLabel
// does: "provider[/model][@effort]".
func comboStepLabel(step comboChainStep) string {
	label := step.provider
	if step.model != "" {
		label += "/" + step.model
	}
	if step.effort != "" {
		label += "@" + step.effort
	}
	return label
}

// validateComboChain validates members exactly like
// ComboStore.validateChain: non-empty, every step a known
// provider with a known model pin (checked only when the
// provider declares models — an undeclared model list cannot
// verify the pin, so the step is accepted) and a known effort,
// no exact duplicate step. Errors use the server's "chain[i]..."
// wording and wrap mcp.ErrInvalidInput, so the stdio source
// rejects the same input with the same text as the HTTP source.
func validateComboChain(members []string, providers []config.ProviderConfig) ([]comboChainStep, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("%w: chain: must not be empty (a combo needs at least one provider)", mcp.ErrInvalidInput)
	}
	byID := make(map[string]config.ProviderConfig, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}
	steps := make([]comboChainStep, 0, len(members))
	seen := map[comboChainStep]int{}
	for i, raw := range members {
		m := strings.TrimSpace(raw)
		if m == "" {
			return nil, fmt.Errorf("%w: chain[%d]: must not be empty", mcp.ErrInvalidInput, i)
		}
		var step comboChainStep
		rest := m
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			step.effort = rest[at+1:]
			rest = rest[:at]
		}
		if prov, model, ok := strings.Cut(rest, "/"); ok {
			step.provider, step.model = strings.TrimSpace(prov), strings.TrimSpace(model)
		} else {
			step.provider = strings.TrimSpace(rest)
		}
		if step.provider == "" {
			return nil, fmt.Errorf("%w: chain[%d].provider: must not be empty", mcp.ErrInvalidInput, i)
		}
		p, ok := byID[step.provider]
		if !ok {
			return nil, fmt.Errorf("%w: chain[%d].provider: unknown provider %q", mcp.ErrInvalidInput, i, step.provider)
		}
		if step.model != "" && len(p.Models) > 0 {
			found := false
			for _, mod := range p.Models {
				if mod.ID == step.model {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: chain[%d].model: unknown model %q for provider %q", mcp.ErrInvalidInput, i, step.model, step.provider)
			}
		}
		if step.effort != "" {
			if !provider.ValidReasoningEffort(step.effort) {
				return nil, fmt.Errorf("%w: chain[%d].effort: unknown reasoning effort %q (one of %s)", mcp.ErrInvalidInput, i, step.effort, strings.Join(provider.ReasoningEfforts, ", "))
			}
			step.effort = provider.NormalizeReasoningEffort(step.effort)
		}
		if j, dup := seen[step]; dup {
			return nil, fmt.Errorf("%w: chain[%d]: duplicate step %q (same provider, model and effort as chain[%d])", mcp.ErrInvalidInput, i, comboStepLabel(step), j)
		}
		seen[step] = i
		steps = append(steps, step)
	}
	return steps, nil
}

// CreateCombo validates a combo and writes it to the config
// routing groups (+ the chain-strategy mirror for strategy
// names), persisting the config file. It converges on
// ComboStore.Create (internal/server/combos_api.go, reached
// from the HTTP path via serverMCPSource.CreateCombo): the same
// validation order (name, strategy, chain, duplicate), the same
// error texts wrapped in mcp.ErrInvalidInput, and the same
// group-mirror store semantics — members are stored as
// "provider[@effort]" (mirrorGroup's form). A model pin
// ("provider/model") is validated against the provider's
// declared model list, exactly like the server's parseChainStep,
// but has no home in a routing group — config validation
// rejects it as an unknown provider — so it is dropped from the
// stored member, as the server's mirror drops it; the pin lives
// in the server's combo record only, which this stdio source has
// no equivalent of. A persist failure rolls the in-memory
// mutation back; nothing is mutated on a denied call — the name,
// strategy and chain validations run before any mutation, and
// the duplicate check runs under the write lock (below), so the
// check and the insert are one atomic step: two concurrent
// same-name creates cannot both pass (see the TOCTOU note in
// the body).
//
// Deliberate divergences from the server path: a persist
// failure reports "unavailable: could not save config: ..."
// (the config file is this source's store) where the server
// reports "could not save combo: ..." — both are plain,
// non-ErrInvalidInput errors. The ctx-cancelled guard DOES
// fire here now (CreateCombo review nit 1): ctx arrives as a
// parameter of the mcp.Source interface, and ctx.Err() is
// checked first — before any validation or mutation, exactly
// where ComboStore.create checks it — so a cancelled call
// aborts with ctx.Err() and stores nothing, the stdio
// equivalent of the server's 503 guard.
//
// Duplicate-check divergence (reviewer nit 1, this slice —
// DOCUMENTED, NOT FIXED, because no correct check is
// implementable from this source): the doctor checks the name
// against the config group mirror (the live Groups map under
// the lock), while ComboStore.createLocked
// (internal/server/combos_api.go) checks s.records[name], the
// in-memory combo records. The two sources can disagree, and
// this source cannot close the gap: combo records live ONLY in
// the serving process's memory (ComboStore.records), seeded
// from the config groups at NewComboStore and never persisted
// — no file, database, or config field holds a record or its
// enabled flag — and this stdio source (runMCPTo) has no
// ComboStore, no server connection, and no record store of its
// own; its only state is cfg + configPath. A correct check
// would need a persisted combo store (a new config schema
// field, a ledger table, or a query to the running server),
// which is a design change outside this file, not a local fix.
// The two cases that diverge, and which side wins on each:
//   - DISABLED server combo: the record exists but
//     mirrorComboLocked removed its group + strategy mirrors
//     (unmirror), so the name is absent from the config file
//     and the doctor does NOT detect a duplicate — it accepts
//     the create and writes a new group mirror, while the
//     server would answer 409. The doctor's write stands in
//     the config file (its own store); the server's in-memory
//     record is untouched by this process.
//   - OPERATOR-CREATED group that is not a combo: the name IS
//     in the config groups, so the doctor rejects the create
//     as a duplicate, while the server (records-only check)
//     accepts it and mirrorGroup silently overwrites the
//     operator's group members. (A group that predates the
//     server's first combos request is seeded as a record at
//     NewComboStore, so this case bites only for groups
//     created after the server started.) Which side wins
//     depends on which path created the combo: stdio rejects,
//     HTTP overwrites.
//
// Within the doctor's own store the check is correct: the
// config groups ARE what this source writes, so a name already
// there is a real collision for this path.
func (s *registryMCPSource) CreateCombo(ctx context.Context, name string, members []string, strategyName string) error {
	// Cancelled first, before any validation or mutation —
	// the same order ComboStore.create uses, so a cancelled
	// call and a bad input agree on which error wins. A nil
	// ctx is a direct Source-level call with no caller
	// context: it proceeds, as before.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s.cfg == nil {
		return errors.New("unavailable: no config loaded")
	}
	if s.configPath == "" {
		return errors.New("unavailable: config path unknown, cannot persist combo")
	}
	if err := comboValidName(name); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	// Strategy before the chain, like ComboStore.create: an
	// empty strategy inherits "priority".
	strat := strings.TrimSpace(strategyName)
	if strat == "" {
		strat = "priority"
	}
	canon, ok := comboStrategyName(strat)
	if !ok {
		return fmt.Errorf("%w: strategy: unknown strategy %q", mcp.ErrInvalidInput, strat)
	}
	steps, err := validateComboChain(members, s.cfg.Providers)
	if err != nil {
		return err
	}
	// Store the server's group-mirror form: "provider[@effort]",
	// the model pin dropped (see the doc comment above).
	clean := make([]string, 0, len(steps))
	for _, st := range steps {
		m := st.provider
		if st.effort != "" {
			m += "@" + st.effort
		}
		clean = append(clean, m)
	}
	// Mutate under the routing write lock, then persist outside
	// it (config.Save must never run under the routing lock).
	// The duplicate check runs UNDER the lock, against the live
	// Groups map — never via GroupsSnapshot, which takes the
	// read lock and would deadlock under the write lock. A
	// pre-lock check would be TOCTOU: two concurrent same-name
	// creates could both pass it and the second would silently
	// overwrite the first, so the check and the insert are one
	// atomic locked step. Validation order is unchanged: name,
	// strategy and chain were already validated above, so a bad
	// chain still beats a duplicate name.
	s.cfg.EnsureRoutingMu()
	s.cfg.LockRouting()
	if s.cfg.Routing == nil {
		s.cfg.Routing = &config.RoutingConfig{}
	}
	if s.cfg.Routing.Groups == nil {
		s.cfg.Routing.Groups = map[string][]string{}
	}
	if _, exists := s.cfg.Routing.Groups[name]; exists {
		s.cfg.UnlockRouting()
		return fmt.Errorf("%w: name: combo %q already exists", mcp.ErrInvalidInput, name)
	}
	oldGroup, hadGroup := s.cfg.Routing.Groups[name]
	s.cfg.Routing.Groups[name] = append([]string(nil), clean...)
	var oldStrat string
	hadStrat := false
	isComboMode := func(s string) bool {
		switch s {
		case "fusion", "pipeline", "jev":
			return true
		}
		return false
	}(canon)
	if _, isStrategy := strategy.Parse(canon); isStrategy && !isComboMode {
		if s.cfg.Routing.ChainStrategies == nil {
			s.cfg.Routing.ChainStrategies = map[string]string{}
		}
		oldStrat, hadStrat = s.cfg.Routing.ChainStrategies[name]
		s.cfg.Routing.ChainStrategies[name] = canon
	} else {
		// Combo execution mode: record-only — drop any stale
		// mirror entry so the config never claims a strategy
		// the combo does not run (mirrors mirrorStrategy).
		if s.cfg.Routing.ChainStrategies != nil {
			oldStrat, hadStrat = s.cfg.Routing.ChainStrategies[name]
			delete(s.cfg.Routing.ChainStrategies, name)
		}
	}
	s.cfg.UnlockRouting()
	if err := s.cfg.Save(s.configPath); err != nil {
		s.cfg.LockRouting()
		if hadGroup {
			s.cfg.Routing.Groups[name] = oldGroup
		} else {
			delete(s.cfg.Routing.Groups, name)
		}
		if hadStrat {
			if s.cfg.Routing.ChainStrategies == nil {
				s.cfg.Routing.ChainStrategies = map[string]string{}
			}
			s.cfg.Routing.ChainStrategies[name] = oldStrat
		} else if s.cfg.Routing.ChainStrategies != nil {
			delete(s.cfg.Routing.ChainStrategies, name)
		}
		s.cfg.UnlockRouting()
		return fmt.Errorf("unavailable: could not save config: %w", err)
	}
	return nil
}

// ActiveCombo returns the session-active combo name ("" = none).
func (s *registryMCPSource) ActiveCombo() string {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.activeCombo
}

// SetActiveCombo selects the session-active combo ("" clears it).
// The name must be an existing combo — activating a name the
// router does not know would invent state.
func (s *registryMCPSource) SetActiveCombo(name string) error {
	if name == "" {
		s.activeMu.Lock()
		s.activeCombo = ""
		s.activeMu.Unlock()
		return nil
	}
	if _, exists := s.cfg.GroupsSnapshot()[name]; !exists {
		return fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	s.activeMu.Lock()
	s.activeCombo = name
	s.activeMu.Unlock()
	return nil
}

// recentEventsCap is the ledger read window for the MCP metrics
// tools (M9.3): ComboMetrics latency and ModelStats read at most
// this many of the most recent usage events. The ledger's
// RecentEvents enforces the same 500 cap server-side, so asking
// for more would not return more; the caveat is documented on
// ComboMetrics/ModelStats and on the ComboMetrics/ModelStat
// types, and pinned by TestRegistryMCPSourceMetrics500EventCap.
const recentEventsCap = 500

// ComboMetrics aggregates the ledger's all-time summary over one
// combo's member providers. Cost is priced rows only; any token
// on an unpriced (provider, model) pair marks the cost UNKNOWN
// (CostKnown false) with the tokens and models listed. Mean
// latency comes from the most recent successful events only —
// at most the 500 most recent events (recentEventsCap): older
// events do not affect the mean, while Requests/tokens/cost are
// all-time. A combo with no such events reports LatencyKnown
// false, never a 0 ms mean.
func (s *registryMCPSource) ComboMetrics(name string) (mcp.ComboMetrics, error) {
	var zero mcp.ComboMetrics
	groups := s.cfg.GroupsSnapshot()
	members, ok := groups[name]
	if !ok {
		return zero, fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	led, err := s.openLedger(false)
	if err != nil {
		return zero, err
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		provID, _, err := config.ParseGroupMember(m)
		if err != nil {
			continue
		}
		memberSet[provID] = true
	}
	ctx := context.Background()
	sum, err := led.Summary(ctx, time.Time{}, time.Time{})
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger summary: %w", err)
	}
	out := mcp.ComboMetrics{Name: name, CostKnown: true, Window: "all_time"}
	for _, p := range sum.ByProvider {
		if !memberSet[p.Provider] {
			continue
		}
		out.Requests += p.Requests
		out.OK += p.OK
		out.Errors += p.Errors
		out.PromptTokens += p.PromptTokens
		out.CompletionTokens += p.CompletionTokens
		out.TotalTokens += p.TotalTokens
		out.CostUSD += p.CostUSD
	}
	prices, _ := s.ledgerPrices()
	unpriced := make(map[string]bool)
	for _, m := range sum.ByModel {
		if !memberSet[m.Provider] {
			continue
		}
		known := prices != nil && prices.Known(m.Provider, m.Model)
		if !known && m.TotalTokens > 0 {
			out.CostKnown = false
			out.UnpricedTokens += m.TotalTokens
			unpriced[m.Provider+"/"+m.Model] = true
		}
	}
	for model := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, model)
	}
	sort.Strings(out.UnpricedModels)
	if out.Requests > 0 {
		rate := float64(out.OK) / float64(out.Requests)
		out.SuccessRate = &rate
	}
	events, err := led.RecentEvents(ctx, recentEventsCap, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	var latSum int64
	for _, ev := range events {
		if !memberSet[ev.Provider] || !ev.OK || ev.LatencyMS <= 0 {
			continue
		}
		latSum += ev.LatencyMS
		out.LatencyEvents++
	}
	if out.LatencyEvents > 0 {
		out.MeanLatencyMS = float64(latSum) / float64(out.LatencyEvents)
		out.LatencyKnown = true
	}
	return out, nil
}

// ComboMetricsForKey implements a2a.KeyScopedComboMetricsSource:
// one combo's measured ledger history for ONE gateway key only —
// the per-key metrics skillSmartRouting serves a gateway-key
// caller, so that caller never sees another key's usage. It
// returns ONLY this key's rows for this combo's member
// providers, never ledger-wide numbers and never another key's
// numbers. Unknown combo and empty key id are invalid input;
// no config / no ledger is an honest unavailable error.
// Validation order (the server's documented order, mirrored
// here): empty key id first, then config, then unknown combo,
// then the ledger.
//
// HOW IT IS SOURCED (same sampling as the server): the ledger
// has NO per-key per-provider aggregate, and UsageSlices(key)
// carries successful, non-shared rows only (no failures, so no
// error count and no success rate). The only ledger read
// returning per-row gateway_key_id values is RecentEvents, so
// this method filters the recentEventsCap (500) most recent
// ledger events (ledger.LogAll) to ev.GatewayKeyID == keyID and
// a member provider — the same sampling precedent the per-key
// CostReport in this file already follows. Every figure comes
// from that one sample, so the figures are mutually consistent
// within it. Window is "recent_500_events", never "all_time":
// usage older than the 500 most recent ledger-wide events is
// invisible, and a busy other key can push this key's rows out
// of the sample entirely.
//
// COUNTING RULES (the server's, mirrored exactly): errors count
// ok=0, non-cancelled rows only (a cancelled hedge loser is a
// request, never a failure); cost is the row's stored cost_usd
// summed unconditionally, so a token-bearing failure DOES
// contribute its stored cost, and a priced failure with no
// reported cost stores 0 — nothing is ever invented. Unpriced
// detection uses the LIVE price book (PriceKnown): a model
// priced now but unpriced when the row was recorded reads as
// priced, and vice versa. A non-member provider's rows are
// excluded BEFORE the unpriced check, so another combo's
// unpriced usage can never flip CostKnown here. Latency is the
// mean over sampled successful events with a recorded latency
// (> 0 ms); LatencyKnown is false when the sample holds none.
func (s *registryMCPSource) ComboMetricsForKey(keyID, name string) (mcp.ComboMetrics, error) {
	var zero mcp.ComboMetrics
	// Normalise before comparing: a padded key id (" key-1 ")
	// must match that key's rows, not silently return zero
	// metrics for a key that does exist.
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return zero, fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	if s.cfg == nil {
		return zero, errors.New("unavailable: no config loaded")
	}
	groups := s.cfg.GroupsSnapshot()
	members, ok := groups[name]
	if !ok {
		return zero, fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	led, err := s.openLedger(false)
	if err != nil {
		return zero, err
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		provID, _, err := config.ParseGroupMember(m)
		if err != nil {
			continue
		}
		memberSet[provID] = true
	}
	ctx := context.Background()
	events, err := led.RecentEvents(ctx, recentEventsCap, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	out := mcp.ComboMetrics{Name: name, CostKnown: true, Window: "recent_500_events"}
	// Total tokens per (provider, model) in the sample, for
	// the unpriced check below (same rule as ComboMetrics).
	tokensByModel := make(map[ledger.PriceKey]int)
	var latSum int64
	for _, ev := range events {
		if ev.GatewayKeyID != keyID || !memberSet[ev.Provider] {
			continue
		}
		out.Requests++
		if ev.OK {
			out.OK++
		} else if !ev.Cancelled {
			out.Errors++
		}
		out.PromptTokens += ev.PromptTokens
		out.CompletionTokens += ev.CompletionTokens
		out.TotalTokens += ev.TotalTokens
		out.CostUSD += ev.CostUSD
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		tokensByModel[k] += ev.TotalTokens
		if ev.OK && ev.LatencyMS > 0 {
			latSum += ev.LatencyMS
			out.LatencyEvents++
		}
	}
	unpriced := make(map[string]bool)
	for k, tokens := range tokensByModel {
		if tokens > 0 && !s.PriceKnown(k.Provider, k.Model) {
			out.CostKnown = false
			out.UnpricedTokens += tokens
			unpriced[k.Provider+"/"+k.Model] = true
		}
	}
	for model := range unpriced {
		out.UnpricedModels = append(out.UnpricedModels, model)
	}
	sort.Strings(out.UnpricedModels)
	if out.Requests > 0 {
		rate := float64(out.OK) / float64(out.Requests)
		out.SuccessRate = &rate
	}
	if out.LatencyEvents > 0 {
		out.MeanLatencyMS = float64(latSum) / float64(out.LatencyEvents)
		out.LatencyKnown = true
	}
	return out, nil
}

// RoutingStrategy returns the effective strategy for a chain
// ("" = global): the chain override when one exists, else the
// global strategy, else "priority". The rule text comes from
// strategy.Describe — the same text the dashboard picker shows.
func (s *registryMCPSource) RoutingStrategy(chain string) mcp.StrategyInfo {
	global := "priority"
	if s.cfg != nil {
		if rs := s.cfg.RoutingSnapshot(); rs != nil && rs.GlobalStrategy != "" {
			global = rs.GlobalStrategy
		}
	}
	if chain == "" {
		name, rule := mcpStrategyDescribe(global)
		return mcp.StrategyInfo{Name: name, Rule: rule, Scope: "global", Inherited: false}
	}
	if s.cfg != nil {
		if st, ok := s.cfg.ChainStrategy(chain); ok {
			name, rule := mcpStrategyDescribe(st)
			return mcp.StrategyInfo{Name: name, Rule: rule, Scope: "chain", Chain: chain, Inherited: false}
		}
	}
	name, rule := mcpStrategyDescribe(global)
	return mcp.StrategyInfo{Name: name, Rule: rule, Scope: "global", Chain: chain, Inherited: true}
}

// mcpStrategyDescribe canonicalises a strategy name (aliases to
// their canonical name) and returns the name + one-line rule.
// An unparseable stored name is reported as-is with no rule —
// the config is validated at load, so this is unreachable in
// production, but a bad row must not crash the tool.
func mcpStrategyDescribe(raw string) (string, string) {
	st, ok := strategy.Parse(raw)
	if !ok {
		return strings.TrimSpace(raw), ""
	}
	_, rule := strategy.Describe(st)
	return string(st), rule
}

// SetRoutingStrategy writes the global strategy (chain == "") or
// one chain's strategy to the config and persists the file. The
// strategy must parse (aliases accepted); a persist failure
// rolls the mutation back. Nothing is mutated on a denied call.
func (s *registryMCPSource) SetRoutingStrategy(chain, strategyName string) error {
	if s.cfg == nil {
		return errors.New("unavailable: no config loaded")
	}
	if s.configPath == "" {
		return errors.New("unavailable: config path unknown, cannot persist strategy")
	}
	st, ok := strategy.Parse(strategyName)
	if !ok {
		return fmt.Errorf("%w: unknown strategy %q", mcp.ErrInvalidInput, strategyName)
	}
	canon := string(st)
	s.cfg.EnsureRoutingMu()
	s.cfg.LockRouting()
	if s.cfg.Routing == nil {
		s.cfg.Routing = &config.RoutingConfig{}
	}
	var oldGlobal string
	var oldChain string
	hadChain := false
	if chain == "" {
		oldGlobal = s.cfg.Routing.GlobalStrategy
		s.cfg.Routing.GlobalStrategy = canon
	} else {
		if s.cfg.Routing.ChainStrategies == nil {
			s.cfg.Routing.ChainStrategies = map[string]string{}
		}
		oldChain, hadChain = s.cfg.Routing.ChainStrategies[chain]
		s.cfg.Routing.ChainStrategies[chain] = canon
	}
	s.cfg.UnlockRouting()
	if err := s.cfg.Save(s.configPath); err != nil {
		s.cfg.LockRouting()
		if chain == "" {
			s.cfg.Routing.GlobalStrategy = oldGlobal
		} else if hadChain {
			s.cfg.Routing.ChainStrategies[chain] = oldChain
		} else {
			delete(s.cfg.Routing.ChainStrategies, chain)
		}
		s.cfg.UnlockRouting()
		return fmt.Errorf("unavailable: could not save config: %w", err)
	}
	return nil
}

// routeTargets returns the providers offering a model, in
// registry (declared) order, with their live price facts. A
// virtual auto/* model has no provider chain — the auto-router
// resolves it, so there is no attempt order to explain.
func (s *registryMCPSource) routeTargets(model string) ([]mcp.RouteAttempt, error) {
	if strings.HasPrefix(model, "auto/") {
		return nil, fmt.Errorf("%w: model %q is a virtual auto-router model; its routing is resolved by the auto-router, not a provider chain", mcp.ErrInvalidInput, model)
	}
	if s.reg == nil {
		return nil, errors.New("unavailable: no provider registry loaded")
	}
	var attempts []mcp.RouteAttempt
	for _, p := range s.reg.List() {
		offers := false
		for _, m := range p.Models {
			if m.ID == model {
				offers = true
				break
			}
		}
		if !offers {
			continue
		}
		att := mcp.RouteAttempt{Provider: p.ID, ProviderName: p.Name, Model: model, Enabled: p.Enabled}
		if rate, known := s.promptUSDPer1M(p.ID, model); known {
			att.PriceKnown = true
			att.PromptUSDPer1M = rate
		}
		attempts = append(attempts, att)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("%w: unknown model %q (no configured provider offers it)", mcp.ErrInvalidInput, model)
	}
	return attempts, nil
}

// ExplainRoute explains how a model would be routed: the
// effective strategy (chain = the model id) and the attempt
// order in declared (priority) order. The order is the
// DECLARED order: a re-ranking strategy re-orders at request
// time from live signals, and the note says so.
func (s *registryMCPSource) ExplainRoute(model string) (mcp.RouteExplanation, error) {
	var zero mcp.RouteExplanation
	attempts, err := s.routeTargets(model)
	if err != nil {
		return zero, err
	}
	info := s.RoutingStrategy(model)
	note := "Order shown is the declared (priority) order over the providers offering this model."
	if info.Name != "priority" {
		note += " Strategy " + info.Name + " re-ranks this order at request time from live latency, success, cost and quota signals, which this dry run does not observe — the first attempt above is not necessarily the first attempt a live request makes."
	} else {
		note += " Priority keeps declared order: no re-ranking."
	}
	return mcp.RouteExplanation{
		Model: model, Strategy: info.Name, Rule: info.Rule,
		Attempts: attempts, Note: note,
	}, nil
}

// SimulateRoute dry-runs a route: the fallback tree (one attempt
// per target, in declared order) plus the first-attempt prompt
// cost from the real price book when promptTokens > 0.
// Completion cost is never estimated — the completion length is
// unknown before the call.
func (s *registryMCPSource) SimulateRoute(model string, promptTokens int) (mcp.RouteSimulation, error) {
	var zero mcp.RouteSimulation
	if promptTokens < 0 {
		return zero, fmt.Errorf("%w: prompt_tokens must be >= 0", mcp.ErrInvalidInput)
	}
	attempts, err := s.routeTargets(model)
	if err != nil {
		return zero, err
	}
	info := s.RoutingStrategy(model)
	sim := mcp.RouteSimulation{
		Model: model, Strategy: info.Name,
		PromptTokens: promptTokens, CostKnown: false,
		Note: "Dry run — no provider was called. The tree is the declared order; a re-ranking strategy re-orders it at request time from live signals. Completion cost is never estimated (completion length is unknown before the call).",
	}
	for i, att := range attempts {
		sa := mcp.SimAttempt{Provider: att.Provider, Model: att.Model, PriceKnown: att.PriceKnown}
		if i == len(attempts)-1 {
			sa.OnFailure = "fail the request (no further targets)"
		} else {
			sa.OnFailure = "fail over to the next target on a retryable failure (429, 502, 503, 504, 5xx, network error); stop the chain on 400/401/403"
		}
		if att.PriceKnown && att.PromptUSDPer1M != nil && promptTokens > 0 {
			cost := float64(promptTokens) * (*att.PromptUSDPer1M) / 1000000
			sa.EstimatedPromptCostUSD = &cost
		}
		sim.Attempts = append(sim.Attempts, sa)
	}
	if len(sim.Attempts) > 0 && sim.Attempts[0].PriceKnown && sim.Attempts[0].EstimatedPromptCostUSD != nil {
		sim.EstimatedPromptCostUSD = sim.Attempts[0].EstimatedPromptCostUSD
		sim.CostKnown = true
	} else if promptTokens == 0 {
		sim.Note += " No cost estimated: pass prompt_tokens to price the first attempt."
	} else {
		sim.Note += " First target's price is unknown, so no cost is estimated (unpriced, never $0)."
	}
	return sim, nil
}

// ModelStatsAvailable reports whether the measured-latency backend
// behind ModelStats is available (N4, stdio side): true when the
// ledger is missing (no events yet — "no measured latency yet" is
// the honest answer) or opens cleanly; false when the ledger file
// exists but cannot be opened (corrupt/unreadable) or no data dir
// is configured, so pick_fastest_model says the backend is
// unavailable instead of claiming no events were measured. It
// never creates the ledger file: a missing file is answered from
// the stat alone, without touching openLedger.
func (s *registryMCPSource) ModelStatsAvailable() bool {
	path, ok := s.ledgerPath()
	if !ok {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	_, err := s.openLedger(false)
	return err == nil
}

// ModelStats aggregates at most the 500 most recent ledger
// events (recentEventsCap — the ledger's RecentEvents cap) per
// (provider, model): requests, successes, and mean latency over
// successful events with a recorded latency. With more than 500
// events recorded, the older events are DROPPED: the counts are
// the last 500, not all-time. No events → no rows; no latency is
// ever guessed from prices or catalog order.
func (s *registryMCPSource) ModelStats() []mcp.ModelStat {
	led, err := s.openLedger(false)
	if err != nil {
		return []mcp.ModelStat{}
	}
	events, err := led.RecentEvents(context.Background(), recentEventsCap, ledger.LogAll)
	if err != nil {
		return []mcp.ModelStat{}
	}
	type agg struct {
		requests, ok int
		latSum       int64
		latEvents    int
	}
	byKey := make(map[ledger.PriceKey]*agg)
	order := []ledger.PriceKey{}
	for _, ev := range events {
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		a, exists := byKey[k]
		if !exists {
			a = &agg{}
			byKey[k] = a
			order = append(order, k)
		}
		a.requests++
		if ev.OK {
			a.ok++
			if ev.LatencyMS > 0 {
				a.latSum += ev.LatencyMS
				a.latEvents++
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Provider != order[j].Provider {
			return order[i].Provider < order[j].Provider
		}
		return order[i].Model < order[j].Model
	})
	out := make([]mcp.ModelStat, 0, len(order))
	for _, k := range order {
		a := byKey[k]
		st := mcp.ModelStat{Provider: k.Provider, Model: k.Model, Requests: a.requests, OK: a.ok, LatencyEvents: a.latEvents}
		if a.requests > 0 {
			rate := float64(a.ok) / float64(a.requests)
			st.SuccessRate = &rate
		}
		if a.latEvents > 0 {
			st.MeanLatencyMS = float64(a.latSum) / float64(a.latEvents)
			st.LatencyKnown = true
		}
		out = append(out, st)
	}
	return out
}

// CostReport reports cost from the real ledger and price book.
// Without a key id it is the authoritative all-window summary.
// With a key id the spend is the authoritative full-window sum
// for that key (SpendFor), while the request/token/by-model
// counts are sampled from the ledger's recent events — the
// ledger has no per-key aggregate query — and the report says
// so. Unpriced usage is UNKNOWN (CostKnown false), never $0.
func (s *registryMCPSource) CostReport(keyID string, days int) (mcp.CostReport, error) {
	var zero mcp.CostReport
	if days < 0 || days > 365 {
		return zero, fmt.Errorf("%w: days must be between 0 (all time) and 365", mcp.ErrInvalidInput)
	}
	led, err := s.openLedger(false)
	if err != nil {
		return zero, err
	}
	ctx := context.Background()
	now := time.Now().UTC()
	since := time.Time{}
	if days > 0 {
		since = now.AddDate(0, 0, -days)
	}
	rep := mcp.CostReport{KeyID: keyID, Days: days, WindowStart: since, WindowEnd: now, CostKnown: true, ByModel: []mcp.ModelCost{}}
	prices, _ := s.ledgerPrices()
	priceKnown := func(provider, model string) bool {
		return prices != nil && prices.Known(provider, model)
	}
	if keyID == "" {
		sum, err := led.Summary(ctx, since, time.Time{})
		if err != nil {
			return zero, fmt.Errorf("unavailable: ledger summary: %w", err)
		}
		rep.Requests, rep.OK, rep.Errors = sum.Requests, sum.OK, sum.Errors
		rep.PromptTokens, rep.CompletionTokens, rep.TotalTokens = sum.PromptTokens, sum.CompletionTokens, sum.TotalTokens
		rep.CostUSD = sum.CostUSD
		unpriced := make(map[string]bool)
		for _, m := range sum.ByModel {
			mc := mcp.ModelCost{
				Provider: m.Provider, Model: m.Model, Requests: m.Requests,
				PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
				TotalTokens: m.TotalTokens, CostUSD: m.CostUSD,
				PriceKnown: priceKnown(m.Provider, m.Model),
			}
			if !mc.PriceKnown && m.TotalTokens > 0 {
				mc.UnpricedTokens = m.TotalTokens
				rep.CostKnown = false
				rep.UnpricedTokens += m.TotalTokens
				unpriced[m.Provider+"/"+m.Model] = true
			}
			rep.ByModel = append(rep.ByModel, mc)
		}
		for model := range unpriced {
			rep.UnpricedModels = append(rep.UnpricedModels, model)
		}
		sort.Strings(rep.UnpricedModels)
		if !rep.CostKnown {
			rep.Note = "cost_usd is a lower bound: some usage in the window is unpriced (see unpriced_tokens / unpriced_models), never $0."
		}
		return rep, nil
	}
	// Per-key: authoritative spend + sampled counts.
	spent, err := led.SpendFor(ctx, keyID, since)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger spend: %w", err)
	}
	rep.CostUSD = spent
	events, err := led.RecentEvents(ctx, 500, ledger.LogAll)
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger recent events: %w", err)
	}
	rep.Sampled = true
	byModel := make(map[ledger.PriceKey]*mcp.ModelCost)
	modelOrder := []ledger.PriceKey{}
	unpriced := make(map[string]bool)
	for _, ev := range events {
		if ev.GatewayKeyID != keyID || ev.TS.Before(since) {
			continue
		}
		rep.Requests++
		if ev.OK {
			rep.OK++
		} else {
			rep.Errors++
		}
		rep.PromptTokens += ev.PromptTokens
		rep.CompletionTokens += ev.CompletionTokens
		rep.TotalTokens += ev.TotalTokens
		k := ledger.PriceKey{Provider: ev.Provider, Model: ev.Model}
		mc, exists := byModel[k]
		if !exists {
			mc = &mcp.ModelCost{Provider: k.Provider, Model: k.Model, PriceKnown: priceKnown(k.Provider, k.Model)}
			byModel[k] = mc
			modelOrder = append(modelOrder, k)
		}
		mc.Requests++
		mc.PromptTokens += ev.PromptTokens
		mc.CompletionTokens += ev.CompletionTokens
		mc.TotalTokens += ev.TotalTokens
		mc.CostUSD += ev.CostUSD
		if !mc.PriceKnown && ev.TotalTokens > 0 {
			mc.UnpricedTokens += ev.TotalTokens
			rep.UnpricedTokens += ev.TotalTokens
			unpriced[k.Provider+"/"+k.Model] = true
		}
	}
	sort.Slice(modelOrder, func(i, j int) bool {
		if modelOrder[i].Provider != modelOrder[j].Provider {
			return modelOrder[i].Provider < modelOrder[j].Provider
		}
		return modelOrder[i].Model < modelOrder[j].Model
	})
	for _, k := range modelOrder {
		rep.ByModel = append(rep.ByModel, *byModel[k])
	}
	for model := range unpriced {
		rep.UnpricedModels = append(rep.UnpricedModels, model)
	}
	sort.Strings(rep.UnpricedModels)
	if len(unpriced) > 0 {
		rep.CostKnown = false
	}
	rep.Note = "cost_usd is the full-window spend for this key; requests, tokens and by_model are sampled from the most recent 500 ledger events (the ledger has no per-key aggregate query)."
	if !rep.CostKnown {
		rep.Note += " cost_usd is a lower bound: some sampled usage is unpriced, never $0; cost_known reflects the sampled events only — unpriced usage outside the sample would not be seen."
	}
	return rep, nil
}

// CheckQuota reports one key's budget position from the
// ledger's KeyBudgetStatus: the rolling 5-hour window plus the
// calendar windows. A key with no budget row is unlimited
// (HasBudget false, RemainingUSD nil) — never an invented cap.
func (s *registryMCPSource) CheckQuota(keyID string) (mcp.QuotaReport, error) {
	var zero mcp.QuotaReport
	if strings.TrimSpace(keyID) == "" {
		return zero, fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	led, err := s.openLedger(false)
	if err != nil {
		return zero, err
	}
	st, err := led.KeyBudgetStatus(context.Background(), keyID, time.Now())
	if err != nil {
		return zero, fmt.Errorf("unavailable: ledger key budget status: %w", err)
	}
	toWindow := func(w ledger.KeyBudgetWindowStatus) mcp.QuotaWindow {
		return mcp.QuotaWindow{LimitUSD: w.LimitUSD, SpentUSD: w.SpentUSD, RemainingUSD: w.RemainingUSD, AtLimit: w.AtLimit}
	}
	return mcp.QuotaReport{
		KeyID: keyID, HasBudget: st.HasBudget, LimitUSD: st.LimitUSD,
		SpentUSD: st.SpentUSD, RemainingUSD: st.RemainingUSD, AtLimit: st.AtLimit,
		UnpricedPolicy: string(st.UnpricedPolicy), ActiveWindow: st.ActiveWindow,
		ActiveLimitUSD: st.ActiveLimitUSD, WarningReached: st.WarningReached,
		UnpricedModels: append([]string(nil), st.UnpricedModels...),
		Daily:          toWindow(st.Daily), Weekly: toWindow(st.Weekly), Monthly: toWindow(st.Monthly),
	}, nil
}

// SetBudgetGuard writes a real key budget into the ledger: the
// rolling 5-hour cap (required) plus optional calendar caps.
// With no calendar cap it uses SetKeyBudget (rolling only);
// with one it uses SetKeyBudgetConfig, preserving the row's
// existing reset interval, reset time and warning threshold
// (or the ledger defaults when the key has no row yet — the
// normalisers map "" to daily / "00:00", and the threshold is
// clamped to [0, 1] by the ledger).
func (s *registryMCPSource) SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, dailyLimitUSD, weeklyLimitUSD, monthlyLimitUSD *float64) error {
	if strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("%w: key_id must not be empty", mcp.ErrInvalidInput)
	}
	if limitUSD < 0 {
		return fmt.Errorf("%w: limit_usd must be >= 0", mcp.ErrInvalidInput)
	}
	policy, ok := ledger.NormalizeUnpricedPolicy(unpricedPolicy)
	if !ok {
		return fmt.Errorf("%w: unknown unpriced_policy %q (want fail_closed or count_as_zero)", mcp.ErrInvalidInput, unpricedPolicy)
	}
	for name, v := range map[string]*float64{"daily": dailyLimitUSD, "weekly": weeklyLimitUSD, "monthly": monthlyLimitUSD} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s_limit_usd must be >= 0", mcp.ErrInvalidInput, name)
		}
	}
	led, err := s.openLedger(true)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if dailyLimitUSD == nil && weeklyLimitUSD == nil && monthlyLimitUSD == nil {
		if err := led.SetKeyBudget(ctx, keyID, limitUSD, policy); err != nil {
			return fmt.Errorf("unavailable: ledger set budget: %w", err)
		}
		return nil
	}
	existing, err := led.GetKeyBudget(ctx, keyID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("unavailable: ledger read budget: %w", err)
	}
	row := ledger.KeyBudget{
		KeyID: keyID, LimitUSD: limitUSD, UnpricedPolicy: policy,
		DailyLimitUSD: dailyLimitUSD, WeeklyLimitUSD: weeklyLimitUSD, MonthlyLimitUSD: monthlyLimitUSD,
		WarningThreshold: 0.8, ResetInterval: ledger.BudgetResetDaily, ResetTime: "00:00",
	}
	if err == nil {
		row.WarningThreshold = existing.WarningThreshold
		row.ResetInterval = existing.ResetInterval
		row.ResetTime = existing.ResetTime
	}
	if err := led.SetKeyBudgetConfig(ctx, row); err != nil {
		return fmt.Errorf("unavailable: ledger set budget: %w", err)
	}
	return nil
}

func (s *registryMCPSource) Status() mcp.StatusInfo {
	st := mcp.StatusInfo{Version: Version, UptimeSeconds: -1}
	if s.cfg != nil {
		st.Listen = s.cfg.Listen
		st.GatewayKeys = len(s.cfg.GatewayKeys)
		st.AutoRouter = buildAutoRouter(s.cfg, s.reg, nil, nil) != nil
	}
	providers := s.Providers()
	st.Providers = len(providers)
	for _, p := range providers {
		if p.Enabled {
			st.EnabledProviders++
		}
		st.Models += len(p.Models)
	}
	return st
}

// M9.4 — ops Source methods (cache / DB / memory tools).
//
// Honesty boundary: this stdio process owns no live cache and no
// live ledger handle — those live in the serve process. What this
// process CAN measure honestly is the data dir on disk: the
// persistent cache SQLite file, the ledger/feedback SQLite files,
// and the memory store file. In-memory exact/semantic layers are
// therefore reported Present=false, never zero-filled. A future
// serve-mode Source (M9.2 remote transports run inside serve
// mode) can back the same interface with the live caches; the
// tools above this seam do not change.
//
// resolveDataPath mirrors runServe: a relative cache path
// resolves against DataDir.
func (s *registryMCPSource) resolveDataPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if s.cfg == nil {
		return p
	}
	return filepath.Join(s.cfg.DataDir, p)
}

// openSQLiteRO opens an existing SQLite file read-only. The file
// must already exist (checked by the caller): a plain sql.Open
// would CREATE a missing file, and a read tool must never create
// database files as a side effect. mode=ro also keeps the open
// from writing WAL checkpoints.
func openSQLiteRO(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func countSQLiteRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *registryMCPSource) CacheStats() mcp.CacheStats {
	st := mcp.CacheStats{
		Note: "exact and semantic in-memory layers live in the serve process and are not visible to this stdio server; only the persistent tier on disk is counted here",
	}
	if s.cfg == nil || s.cfg.Cache == nil {
		return st
	}
	p := s.resolveDataPath(s.cfg.Cache.ExactPath)
	if p == "" {
		return st
	}
	if _, err := os.Stat(p); err != nil {
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := openSQLiteRO(p)
	if err != nil {
		// H4: the driver error is logged server-side in full;
		// the caller gets a fixed note (basename + fixed
		// reason), never the verbatim error.
		fmt.Fprintf(os.Stderr, "codvian-router mcp: cache stats open %s: %v\n", p, err)
		st.Note = fmt.Sprintf("persistent cache file %q could not be opened read-only", filepath.Base(p))
		return st
	}
	defer db.Close()
	var entries int64
	var bytes int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(response)),0) FROM cache_entries`).Scan(&entries, &bytes); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router mcp: cache stats count %s: %v\n", p, err)
		st.Note = fmt.Sprintf("persistent cache file %q could not be counted", filepath.Base(p))
		return st
	}
	st.Persistent = mcp.CacheLayerStats{Present: true, Entries: int(entries), Bytes: int(bytes)}
	return st
}

// CacheFlush evicts the named tenant's entries from the
// persistent cache file on disk. It is a REAL eviction (the disk
// rows are deleted), but only of the persistent tier: the serve
// process keeps its own in-memory copy, and its 1-minute snapshot
// rewrites the table from memory — so a flushed entry can be
// resurrected on disk by a RUNNING serve process until that
// process restarts or its own flush runs. The flush is fully
// durable when serve is not running. This caveat is a property
// of the two-process layout, not of the flush itself; it is
// recorded here rather than hidden behind a bare count.
// stripCachePathError unwraps an os path error to its
// underlying errno ("no such file or directory", "permission
// denied"): *PathError, *LinkError and *SyscallError embed the
// full local path in their Error(), and that path must never
// reach a remote MCP caller in a -32602 message (H4). The full
// error is logged server-side at the call site before stripping,
// so the operator still sees the path in the server log.
// The unwrap is recursive: a *PathError may wrap a
// *SyscallError (or vice versa via %w), and returning the inner
// wrapper would keep the redundant "open: " syscall prefix
// instead of the bare errno the doc comment promises — the
// caller-facing message would read "open: no such file or
// directory" rather than "no such file or directory".
// Mirrors stripPathError (internal/mcp/memory_store.go).
func stripCachePathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return stripCachePathError(pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return stripCachePathError(le.Err)
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return stripCachePathError(se.Err)
	}
	return err
}

// cacheFlushStatError words a CacheFlush os.Stat failure by
// errno: only a missing file is "not present"; a permission
// failure is "not accessible" and anything else is
// "unavailable". Every Stat failure used to read "not present",
// which mislabelled permission failures as a missing file. The
// error carries only the basename and the stripped errno, never
// the full path or the raw error (H4).
func cacheFlushStatError(p string, err error) error {
	base := filepath.Base(p)
	stripped := stripCachePathError(err)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("mcp: persistent cache file %q not present: %w", base, stripped)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("mcp: persistent cache file %q not accessible: %w", base, stripped)
	default:
		return fmt.Errorf("mcp: persistent cache file %q unavailable: %w", base, stripped)
	}
}

func (s *registryMCPSource) CacheFlush(tenant string) (int, error) {
	if s.cfg == nil || s.cfg.Cache == nil || s.cfg.Cache.ExactPath == "" {
		return 0, errors.New("mcp: no persistent cache configured (cache.exact_persist_path is empty); the in-memory cache lives in the serve process and cannot be flushed from this stdio server")
	}
	p := s.resolveDataPath(s.cfg.Cache.ExactPath)
	if _, err := os.Stat(p); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router mcp: cache_flush stat %s: %v\n", p, err)
		return 0, cacheFlushStatError(p, err)
	}
	pc, err := cache.OpenPersistentCache(p, 500)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router mcp: cache_flush open %s: %v\n", p, err)
		return 0, fmt.Errorf("mcp: open persistent cache: %w", stripCachePathError(err))
	}
	defer pc.Close()
	return pc.FlushTenant(context.Background(), tenant), nil
}

// dbFileSpec is one database file the inspector knows how to
// count: name for display, path on disk, and the tables whose
// schema is known (an unknown table is never counted by guessing
// its name — a missing table is an error on that file, not a 0).
type dbFileSpec struct {
	name   string
	path   string
	tables []string
}

func (s *registryMCPSource) inspectDBs() []mcp.DBFileHealth {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil
	}
	specs := []dbFileSpec{
		{name: "ledger.db", path: filepath.Join(s.cfg.DataDir, "ledger.db"), tables: []string{"usage_events"}},
		{name: "feedback.db", path: filepath.Join(s.cfg.DataDir, "feedback.db"), tables: []string{"feedback_decisions", "feedback_events"}},
	}
	if s.cfg.Cache != nil {
		if p := s.resolveDataPath(s.cfg.Cache.ExactPath); p != "" {
			specs = append(specs, dbFileSpec{name: filepath.Base(p), path: p, tables: []string{"cache_entries", "cache_savings"}})
		}
		if p := s.resolveDataPath(s.cfg.Cache.SemanticPath); p != "" {
			specs = append(specs, dbFileSpec{name: filepath.Base(p), path: p, tables: []string{"semantic_entries"}})
		}
	}
	out := make([]mcp.DBFileHealth, 0, len(specs))
	for _, spec := range specs {
		f := mcp.DBFileHealth{Name: spec.name}
		info, err := os.Stat(spec.path)
		if err != nil {
			// Missing is a fact, not an error: these files are
			// created on first use (ledger on first request,
			// cache only when persistence is enabled).
			out = append(out, f)
			continue
		}
		f.Exists = true
		f.SizeBytes = info.Size()
		f.ModifiedAt = info.ModTime().UTC()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		db, err := openSQLiteRO(spec.path)
		if err != nil {
			// H4: the open error embeds the DSN, i.e. the
			// full data-dir path — it is logged server-side
			// (stderr, where the path belongs) and the caller
			// only learns the database is unavailable.
			fmt.Fprintf(os.Stderr, "codvian-router mcp: inspectDBs %s: cannot open database: %v\n", spec.name, err)
			f.Error = "database unavailable"
			cancel()
			out = append(out, f)
			continue
		}
		rows := make(map[string]int64, len(spec.tables))
		for _, table := range spec.tables {
			n, err := countSQLiteRows(ctx, db, table)
			if err != nil {
				// Same rule as the open error: log the
				// detail (which may name the file) and
				// report only that this table's count is
				// unavailable — never the driver's message.
				fmt.Fprintf(os.Stderr, "codvian-router mcp: inspectDBs %s: cannot count table %s: %v\n", spec.name, table, err)
				f.Error = fmt.Sprintf("count %s: unavailable", table)
				break
			}
			rows[table] = n
		}
		db.Close()
		cancel()
		if len(rows) > 0 {
			f.Rows = rows
		}
		out = append(out, f)
	}
	return out
}

func (s *registryMCPSource) DBHealth() mcp.DBHealth {
	files := s.inspectDBs()
	h := mcp.DBHealth{Healthy: true, Files: files}
	if s.cfg == nil || s.cfg.DataDir == "" {
		h.Note = "no data directory configured — no database files to check"
		return h
	}
	for _, f := range files {
		if f.Exists && f.Error != "" {
			h.Healthy = false
		}
	}
	return h
}

func (s *registryMCPSource) DBStats() mcp.DBStats {
	st := mcp.DBStats{Files: s.inspectDBs(), Note: "row counts are live counts from the SQLite files at call time; last write is the file modification time; sizes are the main database file only, excluding -wal/-shm sidecars"}
	if s.cfg == nil || s.cfg.DataDir == "" {
		st.Note = "no data directory configured — no database files to report"
	}
	return st
}

// memoryStore is the M9.4 file-backed memory store: one JSON line
// per entry under <data-dir>/mcp/memory.jsonl, next to the audit
// log. No DataDir means no memory store — the tools fail closed
// with an error, never with an in-memory store that would forget
// everything on exit and pretend otherwise.
func (s *registryMCPSource) memoryStore() (*mcp.MemoryStore, error) {
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil, errors.New("mcp: memory store unavailable — no data directory configured")
	}
	return mcp.NewMemoryStore(filepath.Join(s.cfg.DataDir, "mcp", "memory.jsonl")), nil
}

func (s *registryMCPSource) MemoryAdd(caller, text string) (mcp.MemoryEntry, error) {
	st, err := s.memoryStore()
	if err != nil {
		return mcp.MemoryEntry{}, err
	}
	return st.Add(caller, text)
}

func (s *registryMCPSource) MemorySearch(caller, query string) ([]mcp.MemoryEntry, error) {
	st, err := s.memoryStore()
	if err != nil {
		return nil, err
	}
	return st.Search(caller, query)
}

func (s *registryMCPSource) MemoryClear(caller string) (int, error) {
	st, err := s.memoryStore()
	if err != nil {
		return 0, err
	}
	return st.Clear(caller)
}

func runMCP(opts options) int {
	return runMCPTo(opts, os.Stdin, os.Stdout)
}

// runMCPTo runs the MCP stdio server. Stdout is the protocol —
// nothing human-readable may ever go there — so the startup line
// goes to stderr. The config must already exist (run `init` or
// `configure` first); without a registry there is no live state to
// serve, and a server with no state is exactly what the MCP package
// refuses to build.
func runMCPTo(opts options, in io.Reader, out io.Writer) int {
	reg, path, err := openRegistry(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: cannot load config %s: %v\n", path, err)
		return 1
	}
	// Guardrails seeded from the config file's blocking toggles
	// (all off when absent) — the same seeding serve mode uses, so
	// a tools/call whose arguments trip a blocking guardrail is
	// refused with the policy-refusal code -32001 (tool_args
	// phase; see mcp.WithGuardrail). Log redaction is always on
	// and is not a config value. The store lives in this process
	// only: the stdio server has no dashboard feed, so its
	// tool_args evaluations are counted in-process and not shown
	// on the HTTP dashboard — the dashboard's guardrail counts
	// cover the HTTP request/response phases.
	guard := guardrails.NewStoreWith(guardrails.Config{
		BlockPII:       cfg.Guardrails != nil && cfg.Guardrails.BlockPII,
		BlockSecrets:   cfg.Guardrails != nil && cfg.Guardrails.BlockSecrets,
		BlockInjection: cfg.Guardrails != nil && cfg.Guardrails.BlockInjection,
	})
	// M9.1 SECURITY REVIEW B1 fix: New() now defaults to
	// Anonymous (deny-all, fail-closed), so stdio must opt into
	// the local operator EXPLICITLY. This is the same trust
	// decision as before — stdio's only caller is the operator
	// that spawned the server (same user/machine/filesystem
	// access as the router itself, see internal/mcp/identity.go)
	// — it is just no longer the silent default: any future
	// transport reusing New() + Handle() without opting in gets
	// deny-all, not a full grant.
	srv := mcp.New(&registryMCPSource{reg: reg, cfg: cfg, configPath: path}, Version).WithGuardrail(guard).WithClientIdentity(mcp.LocalOperator())
	// M9.1: audit every stdio tool call to <data-dir>/mcp/audit.jsonl
	// (inputs SHA-256-hashed, outputs truncated — see internal/mcp).
	// A log that cannot be opened warns on stderr and the server
	// still runs unaudited: an audit failure must not take the
	// stdio server down. No DataDir means no audit path.
	if cfg.DataDir != "" {
		if auditLog, err := mcp.OpenAuditLog(filepath.Join(cfg.DataDir, "mcp", "audit.jsonl")); err == nil {
			srv.WithAuditLog(auditLog)
		} else {
			fmt.Fprintf(os.Stderr, "codvian-router mcp: audit log unavailable (%v); tool calls will not be audited\n", err)
		}
	}
	fmt.Fprintf(os.Stderr, "codvian-router mcp: stdio transport, %d tools (config %s)\n", len(mcp.Tools()), path)
	if err := srv.Serve(in, out); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: mcp: %v\n", err)
		return 1
	}
	return 0
}
