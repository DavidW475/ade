// Package fake ist ein Adapter für Tests. Er startet einen beliebigen Befehl (Standard: cat)
// und liest dessen Ausgabe zeilenweise als JSON-Events {"type": …, "payload": {…}}.
//
// Besondere Zeilen:
//
//	{"type":"resume_id","payload":{"id":"…"}}  → Sink.SetResumeID
//	{"type":"turn_done"}                       → Sink.TurnDone
//
// Folgeanweisungen werden als message-Event mit role "assistant" kodiert, so dass `cat` sie
// zurückspiegelt und Tests den Weg Eingabe → Prozess → Event prüfen können.
package fake

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

// Adapter ist der Fake-Adapter.
type Adapter struct {
	// AdapterName überschreibt den Namen (Standard "fake").
	AdapterName string
	// Caps sind die gemeldeten Fähigkeiten (Standard: Input und Resume).
	Caps *adapter.Capabilities
	// Argv ist der Befehl, wenn die Spec keinen nennt (Standard: cat).
	Argv []string

	mu    sync.Mutex
	specs []adapter.Spec
}

// Specs liefert die Spec jedes bisherigen Command-Aufrufs.
func (a *Adapter) Specs() []adapter.Spec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapter.Spec(nil), a.specs...)
}

// Name implementiert adapter.Adapter.
func (a *Adapter) Name() string {
	if a.AdapterName != "" {
		return a.AdapterName
	}
	return "fake"
}

// Capabilities implementiert adapter.Adapter.
func (a *Adapter) Capabilities() adapter.Capabilities {
	if a.Caps != nil {
		return *a.Caps
	}
	return adapter.Capabilities{Input: true, Resume: true}
}

// Command implementiert adapter.Adapter.
func (a *Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	a.mu.Lock()
	a.specs = append(a.specs, spec)
	a.mu.Unlock()
	argv := spec.Command
	if len(argv) == 0 {
		argv = a.Argv
	}
	if len(argv) == 0 {
		argv = []string{"cat"}
	}
	cmd := &adapter.Cmd{Path: argv[0], Args: argv[1:]}
	if spec.Task != "" && a.Capabilities().Input {
		in, err := a.EncodeInput(spec.Task)
		if err != nil {
			return nil, err
		}
		cmd.Input = in
	}
	return cmd, nil
}

type line struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Parse implementiert adapter.Adapter.
func (a *Adapter) Parse(r io.Reader, sink adapter.Sink) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			sink.Emit(adapter.TypeError, adapter.Error{Message: sc.Text()})
			continue
		}
		switch l.Type {
		case "resume_id":
			var p struct{ ID string }
			_ = json.Unmarshal(l.Payload, &p)
			sink.SetResumeID(p.ID)
		case "turn_done":
			sink.TurnDone()
		case adapter.TypeCost:
			var c adapter.Cost
			_ = json.Unmarshal(l.Payload, &c)
			sink.Emit(l.Type, c)
		default:
			if len(l.Payload) == 0 {
				l.Payload = json.RawMessage("{}")
			}
			sink.Emit(l.Type, l.Payload)
		}
	}
	return sc.Err()
}

// EncodeInput implementiert adapter.Adapter.
func (a *Adapter) EncodeInput(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{"type": adapter.TypeMessage, "payload": adapter.Message{Role: "assistant", Text: "echo: " + text}})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
