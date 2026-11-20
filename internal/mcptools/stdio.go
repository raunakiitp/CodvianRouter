package mcptools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// StdioServer is a ToolServer backed by a real MCP server process
// speaking JSON-RPC 2.0 over stdio (newline-delimited, protocol
// 2025-03-26 — the same surface the router's own MCP server
// speaks). The process is spawned lazily on first use from the
// operator's own config (command + args): the config file is
// operator-controlled, so the command is the operator's choice,
// never a value that arrives over HTTP. One process per server,
// one request at a time (stdio is a single stream, so calls are
// serialised with a mutex); a dead process fails honestly instead
// of being silently respawned in a loop.
//
// A start failure is sticky: ListTools/CallTool keep returning the
// same error until the process is replaced — except a
// caller-context cancellation or deadline mid-handshake, which is
// transient, not sticky (see the transient check in startLocked):
// the next call respawns the process and retries. The registry
// records the server as down and the other servers still load.
type StdioServer struct {
	name    string
	command string
	args    []string
	timeout time.Duration

	mu       sync.Mutex
	started  bool
	startErr error
	cmd      *exec.Cmd
	stdin    *bufio.Writer
	stdout   *bufio.Reader
	nextID   int
	tools    []mcp.Tool
}

// NewStdioServer builds a StdioServer. command is the executable,
// args its arguments. timeout bounds each JSON-RPC round-trip
// (0 = 30 s default).
func NewStdioServer(name, command string, args []string) *StdioServer {
	return NewStdioServerWithTimeout(name, command, args, 30*time.Second)
}

// NewStdioServerWithTimeout is NewStdioServer with an explicit
// per-call timeout (tests use a short one).
func NewStdioServerWithTimeout(name, command string, args []string, timeout time.Duration) *StdioServer {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cp := make([]string, len(args))
	copy(cp, args)
	return &StdioServer{name: name, command: command, args: cp, timeout: timeout}
}

// Name returns the server name.
func (s *StdioServer) Name() string { return s.name }

// Close stops the child process, if one was started. It is safe to
// call more than once; a server that never started is a no-op.
func (s *StdioServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Kill()
	err := s.cmd.Wait()
	s.cmd = nil
	s.started = false
	return err
}

// killLocked stops the child process and clears the started state
// so the next call spawns a fresh process. The caller must hold
// s.mu. It is used when a call times out or when the caller's
// context is cancelled mid-round-trip (the orphaned read/write
// goroutine must never share the stream with a later call, and
// killing the process is what ends it — its stdout pipe closes)
// and on the startLocked error paths below: after a timeout kill
// s.cmd is already nil, so those paths must go through here
// instead of dereferencing s.cmd directly.
func (s *StdioServer) killLocked() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	s.cmd = nil
	s.started = false
	s.tools = nil
}

// request is one outbound JSON-RPC request.
type stdioRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// stdioResponse is the inbound shape: result on success, error
// otherwise.
type stdioResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// startLocked spawns the process and runs the initialize handshake.
// The caller must hold s.mu.
func (s *StdioServer) startLocked(ctx context.Context) error {
	if s.started {
		return nil
	}
	if s.startErr != nil {
		return s.startErr
	}
	fail := func(err error) error {
		s.startErr = err
		return err
	}
	if s.command == "" {
		return fail(fmt.Errorf("mcp server %q has no command configured", s.name))
	}
	// The child process is deliberately NOT tied to ctx
	// (exec.Command, not exec.CommandContext): ctx here is the
	// short-lived Discover/call context. Cancelling it between
	// calls — after a round-trip has finished, or before one
	// starts — must never kill the server: the process lives
	// until Close, a call timeout, or a cancellation that lands
	// mid-round-trip. That last case DOES kill it, and it has
	// to: callLocked's read goroutine is blocked on the shared
	// stream, and only the process dying (its stdout pipe
	// closing) ends that goroutine — returning without killing
	// would leave it racing the next call's reader on the same
	// bufio.Reader and able to swallow that call's response
	// line. Per-call timeouts and mid-call cancels are enforced
	// by callLocked, not by the process's context.
	cmd := exec.Command(s.command, s.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("mcp server %q: stdin pipe: %w", s.name, err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(fmt.Errorf("mcp server %q: stdout pipe: %w", s.name, err))
	}
	// The child's stderr is left nil, which connects it to the
	// null device: a noisy server can neither fill a pipe and
	// deadlock the protocol nor pollute the router's log.
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("mcp server %q: starting %q: %w", s.name, s.command, err))
	}
	s.cmd = cmd
	s.stdin = bufio.NewWriter(stdin)
	s.stdout = bufio.NewReader(stdout)
	s.nextID = 0
	// A caller-context cancellation mid-handshake is NOT a start
	// failure: the server was not wedged, the caller just went
	// away (e.g. an HTTP disconnect cancelled the request ctx).
	// callLocked already killed the process, so the next call
	// respawns it; recording the cancellation as sticky (via
	// fail) would brick the server forever on one transient
	// disconnect. Timeouts and other handshake errors stay sticky
	// via fail, as before.
	transient := func(err error) bool {
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	// initialize handshake, then tools/list to cache the tools.
	if _, err := s.callLocked(ctx, "initialize", map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codvian-router", "version": "selector"},
	}); err != nil {
		// callLocked may already have killed the process on
		// timeout (killLocked nils s.cmd), so this must go
		// through killLocked — never s.cmd.Process directly.
		s.killLocked()
		if transient(err) {
			return fmt.Errorf("mcp server %q: initialize: %w", s.name, err)
		}
		return fail(fmt.Errorf("mcp server %q: initialize: %w", s.name, err))
	}
	// notifications/initialized is a notification: no id (a
	// notification with an id is a request, and a server that
	// answers it would leave a stray response line for the next
	// call to misread as its own) and no response is expected,
	// so it is written without waiting for a reply.
	note, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	_, _ = s.stdin.Write(append(note, '\n'))
	_ = s.stdin.Flush()
	var list struct {
		Tools []mcp.Tool `json:"tools"`
	}
	raw, err := s.callLocked(ctx, "tools/list", nil)
	if err != nil {
		s.killLocked()
		if transient(err) {
			return fmt.Errorf("mcp server %q: tools/list: %w", s.name, err)
		}
		return fail(fmt.Errorf("mcp server %q: tools/list: %w", s.name, err))
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		s.killLocked()
		return fail(fmt.Errorf("mcp server %q: tools/list: bad response: %w", s.name, err))
	}
	s.tools = list.Tools
	s.started = true
	return nil
}

// callLocked writes one request and reads its one response line.
// The caller must hold s.mu (stdio is a single stream).
func (s *StdioServer) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	start := time.Now()
	deadline := start.Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// The duration the call actually waited for: min(s.timeout,
	// the ctx's remaining time). The timeout message below
	// reports this, not s.timeout — a call that ended after
	// ~500 ms on its ctx deadline must not log "timed out
	// after 30s".
	effectiveTimeout := deadline.Sub(start)
	s.nextID++
	req := stdioRequest{JSONRPC: "2.0", ID: s.nextID, Method: method, Params: params}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// A write/read that outlives the deadline, or whose caller
	// context is cancelled mid-round-trip, fails the call and
	// kills the process: the orphaned goroutine ends when the
	// stdout pipe closes, so it can never interleave with a later
	// call on the same stream. The next call spawns a fresh
	// process (a timeout is a wedged server, not a start failure,
	// so it is not sticky — but a server that wedges on every call
	// will be killed on every call, and the registry reports it).
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	// Capture the stream endpoints as locals: after a timeout
	// killLocked clears the process and a later call respawns a
	// fresh one, reassigning s.stdin/s.stdout. An orphaned
	// goroutine that read those fields would race with the
	// respawn and could write to / read from the new process's
	// stream; with locals it only ever touches the stream of
	// the call it belongs to (which ends when the killed
	// process's stdout pipe closes).
	stdin := s.stdin
	stdout := s.stdout
	go func() {
		if _, err := stdin.Write(append(raw, '\n')); err != nil {
			done <- result{err: err}
			return
		}
		if err := stdin.Flush(); err != nil {
			done <- result{err: err}
			return
		}
		line, err := stdout.ReadBytes('\n')
		done <- result{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		// Same kill as the timeout path, for the same reason:
		// the read goroutine is still blocked in ReadBytes on
		// the shared stream, and returning without killing it
		// would leave it to race the next call's reader on the
		// same bufio.Reader — and to swallow that call's
		// response line, since responses are not id-checked.
		// Killing the process closes the stdout pipe, which
		// ends the orphan; the next call respawns a fresh
		// process. A cancel between calls (no round-trip in
		// flight) never reaches this branch and never kills
		// anything — see startLocked.
		s.killLocked()
		return nil, ctx.Err()
	case <-time.After(time.Until(deadline)):
		s.killLocked()
		// The timer and ctx.Done() become ready at the same
		// instant when the ctx deadline is the effective
		// deadline, and select picks randomly between them —
		// so a timer win here must not be reported as a plain
		// server timeout: the ctx deadline fired, which is a
		// transient caller expiry (non-sticky), not a wedged
		// server. Return the ctx error, exactly as the
		// ctx.Done() branch does.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Guard for the microsecond race the ctx.Err() check
		// alone leaves: the time.After timer is armed with
		// time.Until(deadline) evaluated at select entry, so
		// it can fire a hair before the ctx's own timer
		// callback has run and set ctx.Err() — observed in
		// testing (a "timed out after 209ms" on a 500 ms ctx
		// deadline with the check above in place). If the ctx
		// deadline is the effective deadline and it has now
		// passed, the ctx deadline factually fired even if
		// its callback has not run yet, so the error is
		// context.DeadlineExceeded. This branch is only
		// reachable when ctx.Err() is nil, so a cancellation
		// (which sets Canceled) is never misreported here;
		// and when the server timeout is the effective
		// deadline, deadline is before the ctx deadline and
		// the guard is skipped.
		if d, ok := ctx.Deadline(); ok && !deadline.After(d) && !time.Now().Before(d) {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("mcp server %q: %s timed out after %s", s.name, method, effectiveTimeout)
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("mcp server %q: %s: %w", s.name, method, res.err)
		}
		var resp stdioResponse
		if err := json.Unmarshal(res.line, &resp); err != nil {
			return nil, fmt.Errorf("mcp server %q: %s: bad response: %w", s.name, method, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp server %q: %s: %s (code %d)", s.name, method, resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	}
}

// ListTools returns the cached tools/list result, starting the
// server process on first use.
func (s *StdioServer) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.startLocked(ctx); err != nil {
		return nil, err
	}
	out := make([]mcp.Tool, len(s.tools))
	copy(out, s.tools)
	return out, nil
}

// CallTool runs one tool on the server process. The result is the
// concatenation of the text content parts, or the structuredContent
// rendered as JSON when there is no text — whichever the server
// actually returned, never an invented summary.
func (s *StdioServer) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.startLocked(ctx); err != nil {
		return "", err
	}
	raw, err := s.callLocked(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp server %q: tools/call %s: bad response: %w", s.name, name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	text := sb.String()
	if text == "" && len(res.StructuredContent) > 0 {
		text = string(res.StructuredContent)
	}
	if res.IsError {
		if text == "" {
			text = "tool reported an error with no message"
		}
		return "", fmt.Errorf("mcp server %q: tool %s failed: %s", s.name, name, text)
	}
	return text, nil
}
