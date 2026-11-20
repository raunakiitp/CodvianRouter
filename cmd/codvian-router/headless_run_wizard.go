// Slice 197-198 (competitor-sweep P2 gaps): serve --headless, the
// `run <cli> --model` launcher, and the first-run wizard.
//
//   - serve --headless: the gateway as a pure API process — no
//     dashboard server (internal/server answers every /dashboard*
//     route 404 while headless). It refuses to start without a
//     gateway key configured (auth-on-from-first-boot): the
//     refusal is checked in runServe BEFORE openStack builds
//     anything, so a headless unauthenticated gateway can never
//     listen. The check is requireGatewayKey below.
//   - run <cli> --model <model>: launches a named local CLI (a
//     coding-agent binary on PATH, or any program) with the router
//     as its base URL. A missing binary is an honest error naming
//     it, never a silent no-op. When no gateway is running, run
//     starts its own headless gateway with a FRESH ephemeral
//     gateway key, waits for /healthz, launches the CLI with
//     OPENAI_BASE_URL/OPENAI_API_KEY (+ CODVIAN_ROUTER_* vars)
//     pointed at the router, and revokes the key + stops the
//     gateway when the CLI exits. When a gateway IS already
//     running, run reuses it — but a stored gateway key's raw
//     value is unrecoverable from the config (only its hash is
//     stored), so reuse needs the operator's key in
//     CODVIAN_ROUTER_API_KEY; without it run refuses honestly
//     instead of launching a CLI that cannot authenticate.
//   - init --wizard: the first-run walkthrough (provider choice
//     → key → model → real test call → done). Every step is
//     skippable (empty answer, or EOF on piped input), every
//     failure is reported honestly and never aborts the rest of
//     the wizard. On an existing config it adds a provider and
//     tests — it never overwrites anything already configured.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ---------------------------------------------------------------------------
// serve --headless gate
// ---------------------------------------------------------------------------

// requireGatewayKey refuses headless startup without a gateway key
// configured. A missing config file counts as no key: openStack
// would otherwise create a fresh empty config and serve it, which
// is exactly the unauthenticated headless gateway the
// auth-on-from-first-boot rule forbids.
func requireGatewayKey(opts options) error {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config at %s and no gateway key configured — run `codvian-router init` first (or start a normal server once and POST /admin/bootstrap); headless mode never starts an unauthenticated gateway", path)
		}
		return fmt.Errorf("cannot load config %s: %w", path, err)
	}
	if len(cfg.GatewayKeys) == 0 {
		return fmt.Errorf("no gateway key configured in %s — run `codvian-router init` to create one first (or POST /admin/bootstrap on a running normal server); headless mode never starts an unauthenticated gateway", path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// run <cli> --model
// ---------------------------------------------------------------------------

// resolveRunCLI finds the named CLI on PATH. A missing binary is an
// honest error naming it — the launcher never silently does nothing.
func resolveRunCLI(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("run needs a CLI name: codvian-router run <cli> --model <model>")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("CLI %q not found on PATH — install it first (nothing was launched)", name)
	}
	return path, nil
}

// buildRunEnv is the environment a launched CLI runs with: the
// router as its OpenAI-compatible base URL, the gateway key as its
// API key, and the requested model. Only OpenAI-wire CLIs pick
// these up automatically; a CLI that speaks another wire needs its
// own configuration — the CODVIAN_ROUTER_* vars name the router
// explicitly so a wrapper can use them.
func buildRunEnv(baseURL, rawKey, model string) []string {
	return []string{
		"OPENAI_BASE_URL=" + baseURL,
		"OPENAI_API_KEY=" + rawKey,
		"CODVIAN_ROUTER_BASE_URL=" + baseURL,
		"CODVIAN_ROUTER_API_KEY=" + rawKey,
		"CODVIAN_ROUTER_MODEL=" + model,
	}
}

// gatewayPort parses the port out of a listen address
// (127.0.0.1:20128 / 0.0.0.0:20128 / [::1]:20128); the health
// probe always dials 127.0.0.1, whatever the bind host is.
func gatewayPort(listen string) (int, error) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(portStr)
}

// gatewayRunning reports whether a gateway answers /healthz on the
// configured port (2 s timeout — a probe, not a wait).
func gatewayRunning(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// waitGatewayHealthy polls /healthz until it answers 200 or the
// deadline passes — used after spawning a gateway for run.
func waitGatewayHealthy(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gatewayRunning(port) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// revokeEphemeralKey removes one gateway key (by ID) from the
// config file. Best-effort at run exit: a failure is reported, and
// the key ID is printed so the operator can remove it by hand —
// the raw key is never printed.
func revokeEphemeralKey(path, keyID string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	kept := cfg.GatewayKeys[:0]
	removed := false
	for _, k := range cfg.GatewayKeys {
		if k.ID == keyID {
			removed = true
			continue
		}
		kept = append(kept, k)
	}
	if !removed {
		return fmt.Errorf("ephemeral key %s was already gone", keyID)
	}
	cfg.GatewayKeys = kept
	return cfg.Save(path)
}

func runRun(opts options, args []string) int {
	return runRunIO(opts, args, os.Stdin, os.Stdout, os.Stderr)
}

func runRunIO(opts options, args []string, _ io.Reader, out io.Writer, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "codvian-router: run needs a CLI name: codvian-router run <cli> --model <model>")
		return 2
	}
	cliName := args[0]
	extra := args[1:]
	if opts.model == "" {
		fmt.Fprintln(errOut, "codvian-router: run needs --model <model> (which model the launched CLI should use through the router)")
		return 2
	}
	cliPath, err := resolveRunCLI(cliName)
	if err != nil {
		fmt.Fprintf(errOut, "codvian-router: %v\n", err)
		return 1
	}

	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(errOut, "codvian-router: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(errOut, "codvian-router: cannot load config %s: %v\n", path, err)
			return 1
		}
		dataDir, err := config.DefaultDataDir()
		if err != nil {
			fmt.Fprintf(errOut, "codvian-router: %v\n", err)
			return 1
		}
		cfg = config.DefaultConfig(dataDir)
		if err := cfg.EnsureDataDir(); err != nil {
			fmt.Fprintf(errOut, "codvian-router: %v\n", err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot save config: %v\n", err)
			return 1
		}
	}
	if opts.port != 0 {
		if err := cfg.SetListenPort(opts.port); err != nil {
			fmt.Fprintf(errOut, "codvian-router: %v\n", err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot save config: %v\n", err)
			return 1
		}
	}
	port, err := gatewayPort(cfg.Listen)
	if err != nil {
		fmt.Fprintf(errOut, "codvian-router: cannot parse listen address %q: %v\n", cfg.Listen, err)
		return 1
	}
	if len(cfg.Providers) == 0 {
		fmt.Fprintf(errOut, "codvian-router: no providers configured — the launched CLI's requests would fail; run `codvian-router init --wizard` first to add one\n")
		return 1
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/v1", port)

	// Reuse or spawn? A gateway already on the port is reused —
	// but only with a key the operator supplies, because a stored
	// key's raw value cannot be recovered from the config (only
	// its hash is stored) and a key issued now would be unknown
	// to the already-running process.
	var rawKey string
	var spawned *exec.Cmd
	var ephemeralKeyID string
	if gatewayRunning(port) {
		rawKey = os.Getenv("CODVIAN_ROUTER_API_KEY")
		if rawKey == "" {
			fmt.Fprintf(errOut, "codvian-router: a gateway is already running on 127.0.0.1:%d and its key cannot be recovered from the config (only the hash is stored) — either stop it first (run will start its own headless gateway with a fresh key) or set CODVIAN_ROUTER_API_KEY to an existing gateway key\n", port)
			return 1
		}
		fmt.Fprintf(out, "gateway already running on 127.0.0.1:%d — reusing it (key from CODVIAN_ROUTER_API_KEY)\n", port)
	} else {
		// Fresh ephemeral key for this run only: issued, saved,
		// revoked when the CLI exits. The spawned headless
		// gateway loads it from the config file at startup.
		key, raw, err := config.IssueKey("run-"+cliName, []string{"chat", "models"}, 0)
		if err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot issue gateway key: %v\n", err)
			return 1
		}
		if err := cfg.AddGatewayKey(key); err != nil {
			fmt.Fprintf(errOut, "codvian-router: %v\n", err)
			return 1
		}
		if err := cfg.Save(path); err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot save config: %v\n", err)
			return 1
		}
		rawKey = raw
		ephemeralKeyID = key.ID
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot find own binary: %v\n", err)
			_ = revokeEphemeralKey(path, ephemeralKeyID)
			return 1
		}
		spawned = exec.Command(self, "--config", path, "serve", "--headless")
		spawned.Stdout = out
		spawned.Stderr = errOut
		spawned.Stdin = nil
		if err := spawned.Start(); err != nil {
			fmt.Fprintf(errOut, "codvian-router: cannot start headless gateway: %v\n", err)
			_ = revokeEphemeralKey(path, ephemeralKeyID)
			return 1
		}
		if !waitGatewayHealthy(port, 15*time.Second) {
			fmt.Fprintf(errOut, "codvian-router: headless gateway did not become healthy on 127.0.0.1:%d within 15s — not launching %q\n", port, cliName)
			_ = spawned.Process.Kill()
			_ = spawned.Wait()
			_ = revokeEphemeralKey(path, ephemeralKeyID)
			return 1
		}
		fmt.Fprintf(out, "headless gateway started on 127.0.0.1:%d (ephemeral key, revoked when %s exits)\n", port, cliName)
	}

	// Launch the CLI with the router as its base URL. Stdio is
	// inherited: this is the operator's own interactive session.
	cmd := exec.Command(cliPath, extra...)
	cmd.Env = append(os.Environ(), buildRunEnv(baseURL, rawKey, opts.model)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			if exitCode < 0 {
				exitCode = 1
			}
		} else {
			fmt.Fprintf(errOut, "codvian-router: cannot run %q: %v\n", cliName, runErr)
			exitCode = 1
		}
	}

	if spawned != nil {
		// Stop the gateway we started, then revoke the ephemeral key.
		if spawned.Process != nil {
			_ = spawned.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _ = spawned.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = spawned.Process.Kill()
				_ = spawned.Wait()
			}
		}
		if err := revokeEphemeralKey(path, ephemeralKeyID); err != nil {
			fmt.Fprintf(errOut, "codvian-router: WARNING: could not revoke the ephemeral gateway key %s: %v — remove it by hand (it is only an ID, never a raw key)\n", ephemeralKeyID, err)
		} else {
			fmt.Fprintf(out, "ephemeral gateway key revoked; headless gateway stopped\n")
		}
	}
	return exitCode
}

// ---------------------------------------------------------------------------
// init --wizard
// ---------------------------------------------------------------------------

// wizardTestCall runs one real chat call against the live engine and
// returns the model's reply text. It is a variable in the IO
// function so tests can stub it (scripted stdin, no network); the
// production value is realWizardTestCall below.
type wizardTestCallFunc func(ctx context.Context, opts options, model string) (string, error)

// realWizardTestCall builds the stack from the just-written config
// and sends one short real request through the engine — a genuine
// end-to-end test of provider + key + model, not a key-presence
// check. The prompt is fixed and trivial; the reply is the
// operator's own test output, shown back to them.
func realWizardTestCall(ctx context.Context, opts options, model string) (string, error) {
	st, err := openStack(opts)
	if err != nil {
		return "", fmt.Errorf("cannot build router stack: %w", err)
	}
	defer st.led.Close()
	defer st.feedback.Close()
	defer st.asyncLed.Close()
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := st.engine.Chat(callCtx, provider.ChatRequest{
		Model:     model,
		Messages:  []provider.Message{{Role: "user", Content: "Say hello in one short sentence."}},
		MaxTokens: 64,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func runWizard(opts options) int {
	return runWizardIO(opts, os.Stdin, os.Stdout, realWizardTestCall)
}

// runWizardIO is the first-run walkthrough. Every prompt accepts an
// empty answer (or EOF) as "skip this step"; a failed step is
// reported and the wizard continues with the rest. The config is
// written before the test call, so a failed test never loses the
// setup — the summary says so honestly.
func runWizardIO(opts options, in io.Reader, out io.Writer, testCall wizardTestCallFunc) int {
	path, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(out, "codvian-router: %v\n", err)
		return 1
	}
	reader := bufio.NewReader(in)
	prompt := func(label string) string {
		fmt.Fprint(out, label)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return ""
		}
		return strings.TrimSpace(line)
	}

	fmt.Fprintln(out, "Codvian Router — first-run wizard")
	fmt.Fprintln(out, "Every step can be skipped with an empty answer. Nothing here is sent anywhere except the provider you choose, and only for the test call.")
	fmt.Fprintln(out)

	cfg, err := config.Load(path)
	existing := true
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "codvian-router: cannot load config %s: %v\n", path, err)
			return 1
		}
		existing = false
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
	} else {
		fmt.Fprintf(out, "config already exists at %s — the wizard will only add a provider and test; nothing already configured is changed.\n\n", path)
	}

	// Step 1 — port.
	if answer := prompt(fmt.Sprintf("Step 1/4 — Port [%s, empty to keep]: ", listenPort(cfg))); answer != "" {
		p, err := strconv.Atoi(answer)
		if err != nil || p < 1 || p > 65535 {
			fmt.Fprintf(out, "not a valid port %q — keeping %s\n", answer, cfg.Listen)
		} else if err := cfg.SetListenPort(p); err != nil {
			fmt.Fprintf(out, "cannot set port: %v — keeping %s\n", err, cfg.Listen)
		} else if err := cfg.Save(path); err != nil {
			fmt.Fprintf(out, "cannot save config: %v\n", err)
			return 1
		} else {
			fmt.Fprintf(out, "listening on %s\n", cfg.Listen)
		}
	}

	// Step 2 — provider choice.
	providerID := prompt("Step 2/4 — Provider catalog id (e.g. groq, openai, anthropic, deepseek — empty to skip): ")
	for providerID != "" {
		if _, ok := catalog.ByID(providerID); ok {
			break
		}
		fmt.Fprintf(out, "unknown catalog provider %q — see `codvian-router provider add-catalog` for the ids.\n", providerID)
		providerID = prompt("Provider catalog id (empty to skip): ")
	}

	// Step 3 — key, then a real add (live key test inside).
	var added *provider.Provider
	if providerID != "" {
		apiKey := prompt(fmt.Sprintf("Step 3/4 — API key for %s (empty to skip — typed key is never printed): ", providerID))
		if apiKey == "" {
			fmt.Fprintln(out, "no API key given — provider not added")
		} else {
			reg, _, err := openRegistry(opts)
			if err != nil {
				fmt.Fprintf(out, "cannot open registry: %v — provider not added\n", err)
			} else {
				p, err := reg.AddCatalogWithOptions(context.Background(), providerID, apiKey, provider.CatalogAddOptions{})
				if err != nil {
					// A real rejection (bad key, dead upstream,
					// ToS-grey entry) — reported, not hidden,
					// and the wizard continues to the summary.
					fmt.Fprintf(out, "provider NOT added: %v\n", err)
				} else {
					added = p
					fmt.Fprintf(out, "added provider %q (%s) — wire %s, %d model(s)\n", p.Name, p.ID, p.WireFormat, len(p.Models))
				}
			}
		}
	} else {
		fmt.Fprintln(out, "Step 3/4 — skipped (no provider chosen)")
	}

	// Step 4 — model + real test call.
	model := ""
	if added != nil && len(added.Models) > 0 {
		def := added.Models[0].ID
		if answer := prompt(fmt.Sprintf("Step 4/4 — Model to test [%s, empty to use the default]: ", def)); answer != "" {
			model = answer
		} else {
			model = def
		}
		fmt.Fprintf(out, "testing %s through the router…\n", model)
		if testCall == nil {
			fmt.Fprintln(out, "test call unavailable (no test function) — config is saved; test with `codvian-router eval --model "+model+"`")
		} else {
			reply, err := testCall(context.Background(), opts, model)
			if err != nil {
				fmt.Fprintf(out, "test call FAILED: %v — the config is still saved; check the key/model and re-run the wizard\n", err)
			} else {
				reply = strings.TrimSpace(reply)
				if len(reply) > 200 {
					reply = reply[:200] + "…"
				}
				fmt.Fprintf(out, "test call OK — %s answered: %q\n", model, reply)
			}
		}
	} else {
		fmt.Fprintln(out, "Step 4/4 — skipped (no provider added, so there is nothing to test)")
	}

	// Done summary — reloaded from disk, so the counts are what is
	// actually saved.
	cfg, err = config.Load(path)
	if err != nil {
		fmt.Fprintf(out, "codvian-router: cannot reload config %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Wizard done.")
	fmt.Fprintf(out, "config: %s\n", path)
	fmt.Fprintf(out, "listening on %s (auth ON)\n", cfg.Listen)
	fmt.Fprintf(out, "providers: %d configured, gateway keys: %d\n", len(cfg.Providers), len(cfg.GatewayKeys))
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(out, "no provider yet — re-run `codvian-router init --wizard` any time to add one")
	} else {
		fmt.Fprintln(out, "next: `codvian-router serve` (or `serve --headless` for servers), then `codvian-router run <cli> --model <model>`")
	}
	_ = existing
	return 0
}
