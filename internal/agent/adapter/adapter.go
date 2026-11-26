// Package adapter definiert die gemeinsame Abstraktion für Agent-CLIs (doc/architecture.md#23-agent-adapter).
//
// Ein Adapter kennt eine konkrete CLI: Er baut den Prozessaufruf, liest dessen Ausgabe und
// übersetzt sie in ADE-Events. Prozesse startet und überwacht der Agent-Manager
// (internal/agent); Adapter sind zustandslos und dürfen von mehreren Sessions gleichzeitig
// benutzt werden. Zustand eines Laufs (z. B. Kosten seit Prozessstart) lebt nur innerhalb
// eines Parse-Aufrufs.
package adapter

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
)

// Capabilities beschreibt, was ein Adapter kann.
type Capabilities struct {
	// Terminal: Der Prozess läuft in einem PTY; seine Ausgabe ist ein Terminal-Stream
	// (WS /ws/sessions/{id}/pty) statt strukturierter Events.
	Terminal bool `json:"terminal"`
	// Input: Folgeanweisungen lassen sich an den laufenden Prozess schicken (EncodeInput).
	Input bool `json:"input"`
	// Resume: Eine beendete oder unterbrochene Session lässt sich mit Spec.ResumeID fortsetzen.
	Resume bool `json:"resume"`
}

// Spec beschreibt einen Prozessstart.
type Spec struct {
	SessionID string
	// Workdir ist das Workspace-Verzeichnis; der Prozess läuft darin.
	Workdir string
	// Task ist die (erste) Anweisung an den Agenten. Beim Fortsetzen die neue Anweisung.
	Task string
	// Command ist der Befehl für Terminal-Adapter (leer: Login-Shell des Nutzers).
	Command []string
	// Model und PermissionMode werden an die CLI durchgereicht, falls sie sie kennt.
	Model          string
	PermissionMode string
	// ResumeID ist die Session-ID der CLI aus einem früheren Lauf (Sink.SetResumeID).
	ResumeID string
	// Sandbox: Der Prozess läuft im Docker-Container des Workspaces (Workdir /workspace).
	// Programme werden dann im Image gesucht, nicht auf dem Host.
	Sandbox bool
}

// Cmd ist ein Prozessaufruf. Path wird über $PATH aufgelöst.
type Cmd struct {
	Path string
	Args []string
	// Env sind zusätzliche Umgebungsvariablen (KEY=VALUE) zur kontrollierten Basisumgebung.
	Env []string
	// Input wird direkt nach dem Start auf stdin geschrieben (z. B. die erste Anweisung).
	Input []byte
}

// Sink nimmt die Ergebnisse von Parse entgegen. Aufrufe kommen nur aus der Parse-Goroutine.
type Sink interface {
	// Emit erzeugt ein Event; payload ist einer der Payload-Typen aus events.go.
	Emit(typ string, payload any)
	// SetResumeID meldet die Session-ID der CLI, mit der sich der Lauf später fortsetzen lässt.
	SetResumeID(id string)
	// TurnDone meldet, dass der Agent fertig ist und auf die nächste Anweisung wartet.
	TurnDone()
}

// Adapter bindet eine Agent-CLI an.
type Adapter interface {
	// Name ist der Bezeichner in project.yaml und der API (z. B. "claude").
	Name() string
	// Capabilities beschreibt die Fähigkeiten.
	Capabilities() Capabilities
	// Command baut den Prozessaufruf.
	Command(spec Spec) (*Cmd, error)
	// Parse liest die Ausgabe (stdout) bis EOF und meldet Events an sink.
	// Terminal-Adapter werden nicht geparst.
	Parse(r io.Reader, sink Sink) error
	// EncodeInput übersetzt eine Folgeanweisung in Bytes für stdin (nur mit Capabilities.Input).
	EncodeInput(text string) ([]byte, error)
}

// Registry hält die verfügbaren Adapter.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry erzeugt eine Registry mit den gegebenen Adaptern.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range as {
		r.Register(a)
	}
	return r
}

// Register fügt einen Adapter hinzu; ein vorhandener gleichen Namens wird ersetzt.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get liefert den Adapter name.
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unbekannter Adapter %q (verfügbar: %v)", name, r.namesLocked())
	}
	return a, nil
}

// Names liefert die Namen aller Adapter, sortiert.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *Registry) namesLocked() []string {
	names := make([]string, 0, len(r.adapters))
	for n := range r.adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Info beschreibt einen Adapter für die API.
type Info struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
}

// Infos liefert Name und Fähigkeiten aller Adapter.
func (r *Registry) Infos() []Info {
	names := r.Names()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(names))
	for _, n := range names {
		out = append(out, Info{Name: n, Capabilities: r.adapters[n].Capabilities()})
	}
	return out
}

// Has meldet, ob es einen Adapter name gibt.
func (r *Registry) Has(name string) bool {
	return slices.Contains(r.Names(), name)
}
