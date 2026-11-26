// Package pty ist der Fallback-Adapter: ein beliebiger Befehl interaktiv in einem Terminal.
// Es gibt keine Timeline, nur den Terminal-Stream (WS /ws/sessions/{id}/pty).
package pty

import (
	"errors"
	"io"
	"os"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

// DefaultShell wird verwendet, wenn weder ein Befehl noch $SHELL gesetzt ist.
const DefaultShell = "bash"

// Adapter ist der PTY-Adapter.
type Adapter struct{}

// New erzeugt den Adapter.
func New() *Adapter { return &Adapter{} }

// Name implementiert adapter.Adapter.
func (*Adapter) Name() string { return "pty" }

// Capabilities implementiert adapter.Adapter.
func (*Adapter) Capabilities() adapter.Capabilities { return adapter.Capabilities{Terminal: true} }

// Command implementiert adapter.Adapter. Ohne Befehl startet die Login-Shell des Nutzers,
// in der Sandbox bash.
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
		return nil, errors.New("leerer Befehl")
	}
	return &adapter.Cmd{Path: argv[0], Args: argv[1:]}, nil
}

// Parse implementiert adapter.Adapter; Terminal-Ausgabe wird nicht geparst.
func (*Adapter) Parse(r io.Reader, _ adapter.Sink) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// EncodeInput implementiert adapter.Adapter; Eingaben gehen direkt ins Terminal.
func (*Adapter) EncodeInput(string) ([]byte, error) {
	return nil, errors.New("der pty-Adapter nimmt Eingaben nur über das Terminal an")
}
