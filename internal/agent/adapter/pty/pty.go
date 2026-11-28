// Package pty is the fallback adapter: any command interactively in a terminal.
// There is no timeline, only the terminal stream (WS /ws/sessions/{id}/pty).
package pty

import (
	"errors"
	"io"
	"os"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

// DefaultShell is used if neither a command nor $SHELL is set.
const DefaultShell = "bash"

// Adapter is the PTY adapter.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return "pty" }

// Capabilities implements adapter.Adapter.
func (*Adapter) Capabilities() adapter.Capabilities { return adapter.Capabilities{Terminal: true} }

// Command implements adapter.Adapter. Without a command the user's login shell starts,
// in the sandbox bash.
func (*Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	argv := spec.Command
	if len(argv) == 0 {
		sh := os.Getenv("SHELL")
		if sh == "" || spec.Sandbox {
			sh = DefaultShell
		}
		argv = []string{sh}
	}
	if argv[0] == "" {
		return nil, errors.New("empty command")
	}
	return &adapter.Cmd{Path: argv[0], Args: argv[1:]}, nil
}

// Parse implements adapter.Adapter; terminal output is not parsed.
func (*Adapter) Parse(r io.Reader, _ adapter.Sink) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// EncodeInput implements adapter.Adapter; input goes directly to the terminal.
func (*Adapter) EncodeInput(string) ([]byte, error) {
	return nil, errors.New("the pty adapter only accepts input through the terminal")
}
