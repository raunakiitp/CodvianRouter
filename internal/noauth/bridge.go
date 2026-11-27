package noauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrBridgeDisabled is returned when a CLI bridge is invoked while
// disabled (NoAuthDisabled set in the environment) or when the
// mechanism for the ID is not a runnable bridge. Every noauth test
// must fail when the mechanism is disabled — this error is the
// mechanism's fail-closed answer.
var ErrBridgeDisabled = errors.New("noauth: CLI bridge disabled")

// disabledEnv is the kill switch: setting NOAUTH_DISABLED=1 (any
// non-empty value) disables every bridge spawn and every anonymous
// dial this package performs, so operators and tests can prove the
// mechanism off is the mechanism off.
const disabledEnv = "NOAUTH_DISABLED"

func disabled() bool { return os.Getenv(disabledEnv) != "" }

// RunBridge spawns the provider's own CLI for a one-shot prompt and
// returns its stdout. The CLI owns its credentials (see
// Mechanism.AuthNote): the router supplies only the prompt and the
// inherited environment, never a credential. A "{prompt}" placeholder
// in Mechanism.Command is replaced with prompt; a command without the
// placeholder receives the prompt on stdin.
//
// The bridge fails closed: NOAUTH_DISABLED=1 (or a non-CLIBridge
// mechanism) returns ErrBridgeDisabled without spawning anything.
func (m Mechanism) RunBridge(ctx context.Context, prompt string, extraEnv ...string) (string, error) {
	if disabled() {
		return "", ErrBridgeDisabled
	}
	if m.Kind != KindCLIBridge || len(m.Command) == 0 {
		return "", fmt.Errorf("%w: %s has no runnable bridge command", ErrBridgeDisabled, m.ID)
	}
	argv := make([]string, 0, len(m.Command))
	for _, a := range m.Command {
		if a == "{prompt}" {
			argv = append(argv, prompt)
			continue
		}
		argv = append(argv, a)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stdinBuf bytes.Buffer
	stdinBuf.WriteString(prompt)
	cmd.Stdout = &stdout
	cmd.Stdin = &stdinBuf
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if err := cmd.Run(); err != nil {
		// Surface the CLI's stderr fragment when available so the
		// operator sees the provider's own diagnosis (missing
		// binary, not logged in, ...).
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("noauth: %s bridge exited: %w: %s", m.ID, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("noauth: %s bridge: %w", m.ID, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
