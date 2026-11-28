// Package adapter defines the common abstraction for agent CLIs (doc/architecture.md#23-agent-adapters).
//
// An adapter knows a specific CLI: it builds the process call, reads its output and
// translates it into ADE events. The agent manager (internal/agent) starts and supervises
// processes; adapters are stateless and may be used by several sessions at the same time.
// The state of a run (e.g. costs since the process started) only lives within one Parse
// call.
package adapter

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"time"
)

// Capabilities describes what an adapter can do.
type Capabilities struct {
	// Terminal: the process runs in a PTY; its output is a terminal stream
	// (WS /ws/sessions/{id}/pty) instead of structured events.
	Terminal bool `json:"terminal"`
	// Input: follow-up instructions can be sent to the running process (EncodeInput).
	Input bool `json:"input"`
	// Resume: an ended or interrupted session can be resumed with Spec.ResumeID.
	Resume bool `json:"resume"`
	// Approvals: the CLI asks ADE through Spec.Hook before tool calls (M7).
	Approvals bool `json:"approvals"`
}

// Hook is a session's approval hook (ade hook pretooluse).
type Hook struct {
	// Command is the shell command the CLI runs before every tool call.
	Command string
	// Timeout is the waiting time for an approval.
	Timeout time.Duration
}

// Spec describes a process start.
type Spec struct {
	SessionID string
	// Workdir is the workspace directory; the process runs in it.
	Workdir string
	// Task is the (first) instruction to the agent. When resuming, the new instruction.
	Task string
	// Command is the command for terminal adapters (empty: the user's login shell).
	Command []string
	// Model and PermissionMode are passed to the CLI if it knows them.
	Model          string
	PermissionMode string
	// ResumeID is the CLI's session ID from an earlier run (Sink.SetResumeID).
	ResumeID string
	// Sandbox: the process runs in the workspace's Docker container (Workdir /workspace).
	// Programs are then looked up in the image, not on the host.
	Sandbox bool
	// ReadOnly: read-only role of a factory. ADE itself makes sure the workspace is not
	// changed (reader container or snapshot); adapters additionally block their write tools
	// so the agent does not even try.
	ReadOnly bool
	// Hook is the approval hook (nil: none, the CLI decides via PermissionMode).
	Hook *Hook
	// Provider describes the access to the provider if ADE sets it (nil: the CLI's own
	// login). Adapters that cannot set a base URL through the environment (codex) need it as
	// an argument.
	Provider *ProviderAccess
}

// ProviderAccess is the access to a provider API.
type ProviderAccess struct {
	// BaseURL is the base URL of the API (in the sandbox the egress proxy).
	BaseURL string
	// KeyEnv is the environment variable with the key (in the sandbox a placeholder).
	KeyEnv string
}

// Tokens are token counts (same counting as Cost).
type Tokens struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Cmd is a process call. Path is resolved through $PATH.
type Cmd struct {
	Path string
	Args []string
	// Env are additional environment variables (KEY=VALUE) on top of the controlled base environment.
	Env []string
	// Input is written to stdin right after the start (e.g. the first instruction).
	Input []byte
	// CloseStdin closes stdin after Input (CLIs that read until EOF).
	CloseStdin bool
}

// Sink receives the results of Parse. Calls only come from the Parse goroutine.
type Sink interface {
	// Emit creates an event; payload is one of the payload types from events.go.
	Emit(typ string, payload any)
	// SetResumeID reports the CLI's session ID with which the run can be resumed later.
	SetResumeID(id string)
	// TurnDone reports that the agent is done and waits for the next instruction.
	TurnDone()
	// Baseline returns the tokens that were already counted for the session when the process
	// started. Adapters whose CLI reports cumulative numbers per conversation (codex when
	// resuming) subtract them.
	Baseline() Tokens
}

// Adapter connects an agent CLI.
type Adapter interface {
	// Name is the identifier in project.yaml and the API (e.g. "claude").
	Name() string
	// Capabilities describes the capabilities.
	Capabilities() Capabilities
	// Command builds the process call.
	Command(spec Spec) (*Cmd, error)
	// Parse reads the output (stdout) until EOF and reports events to sink.
	// Terminal adapters are not parsed.
	Parse(r io.Reader, sink Sink) error
	// EncodeInput translates a follow-up instruction into bytes for stdin (only with Capabilities.Input).
	EncodeInput(text string) ([]byte, error)
}

// Registry holds the available adapters.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry creates a registry with the given adapters.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range as {
		r.Register(a)
	}
	return r
}

// Register adds an adapter; an existing one with the same name is replaced.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get returns the adapter name.
func (r *Registry) Get(name string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q (available: %v)", name, r.namesLocked())
	}
	return a, nil
}

// Names returns the names of all adapters, sorted.
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

// Info describes an adapter for the API.
type Info struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
}

// Infos returns name and capabilities of all adapters.
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

// Has reports whether an adapter name exists.
func (r *Registry) Has(name string) bool {
	return slices.Contains(r.Names(), name)
}
