// Package fake is an adapter for tests. It starts any command (default: cat) and reads its
// output line by line as JSON events {"type": …, "payload": {…}}.
//
// Special lines:
//
//	{"type":"resume_id","payload":{"id":"…"}}  → Sink.SetResumeID
//	{"type":"turn_done"}                       → Sink.TurnDone
//
// Follow-up instructions are encoded as a message event with role "assistant", so that `cat`
// mirrors them back and tests can check the path input → process → event.
package fake

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

// Adapter is the fake adapter.
type Adapter struct {
	// AdapterName overrides the name (default "fake").
	AdapterName string
	// Caps are the reported capabilities (default: Input and Resume).
	Caps *adapter.Capabilities
	// Argv is the command if the spec names none (default: cat).
	Argv []string

	mu    sync.Mutex
	specs []adapter.Spec
}

// Specs returns the spec of every Command call so far.
func (a *Adapter) Specs() []adapter.Spec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapter.Spec(nil), a.specs...)
}

// Name implements adapter.Adapter.
func (a *Adapter) Name() string {
	if a.AdapterName != "" {
		return a.AdapterName
	}
	return "fake"
}

// Capabilities implements adapter.Adapter.
func (a *Adapter) Capabilities() adapter.Capabilities {
	if a.Caps != nil {
		return *a.Caps
	}
	return adapter.Capabilities{Input: true, Resume: true}
}

// Command implements adapter.Adapter.
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

// Parse implements adapter.Adapter.
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

// EncodeInput implements adapter.Adapter.
func (a *Adapter) EncodeInput(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{"type": adapter.TypeMessage, "payload": adapter.Message{Role: "assistant", Text: "echo: " + text}})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
