# Writing an adapter

An **adapter** connects one agent CLI (Claude Code, Codex, …) to ADE. It builds the command
line, reads the CLI's output and translates it into ADE events. Everything else – processes,
workspaces, sandbox, approvals, costs, the timeline – is done by ADE. This guide explains the
interface and walks through a new adapter step by step.

Code: [`internal/agent/adapter`](../internal/agent/adapter) (interface, events, JSON schema) and
the existing adapters [`claude/`](../internal/agent/adapter/claude),
[`codex/`](../internal/agent/adapter/codex), [`pty/`](../internal/agent/adapter/pty) and
[`fake/`](../internal/agent/adapter/fake) (tests). Background: [architecture.md](architecture.md#23-agent-adapter).

## 1. The interface

```go
type Adapter interface {
    Name() string                          // identifier in project.yaml and the API
    Capabilities() Capabilities            // what the adapter can do
    Command(spec Spec) (*Cmd, error)       // build the process call
    Parse(r io.Reader, sink Sink) error    // read stdout until EOF, report events
    EncodeInput(text string) ([]byte, error) // follow-up instruction → bytes for stdin
}
```

Adapters are **stateless** and used by many sessions at once. State of one process (for
example cumulative costs) lives only inside a `Parse` call. The agent manager
(`internal/agent`) starts the process, feeds stdin, calls `Parse` in its own goroutine and
handles status, stopping and restarts.

### Capabilities

| Field | Meaning |
|---|---|
| `Terminal` | The process runs in a PTY and its output is a terminal stream; `Parse` is not called (like `pty`). |
| `Input` | Follow-up instructions can be written to the running process via `EncodeInput` (like `claude` with `stream-json`). |
| `Resume` | A finished or interrupted session can be continued with `Spec.ResumeID`. Without `Input` but with `Resume`, every follow-up instruction starts a new process that resumes (like `codex`). |
| `Approvals` | The CLI asks ADE before tool calls via `Spec.Hook`. |

### Spec

`Command` receives a `Spec` and must respect all of its fields:

| Field | What the adapter does with it |
|---|---|
| `Workdir` | Nothing; the manager starts the process there (`/workspace` in the sandbox). |
| `Task` | First instruction, or the new instruction when resuming. Usually goes to stdin (`Cmd.Input`), never into an argument that could be mistaken for a flag. |
| `Model`, `PermissionMode` | Pass them to the CLI if it knows them. Reject values that start with `-`. Validate permission modes against a list and expose it in `internal/api/agents.go` (`Modes`), so the dashboard offers them. |
| `ResumeID` | Pass to the CLI's resume option. The ID comes from `Sink.SetResumeID` of an earlier run. |
| `Sandbox` | The process runs inside the workspace container. Look up binaries in the image, not on the host (skip `exec.LookPath`). The container is the sandbox: disable the CLI's own sandbox if it does not work in containers. |
| `ReadOnly` | Read-only factory role. ADE already protects the workspace (read-only container or snapshot); disable write tools as well so the agent does not even try. |
| `Hook` | If not nil, register `Hook.Command` as the CLI's pre-tool hook with `Hook.Timeout`. The hook blocks until a decision and prints the CLI's expected answer. |
| `Provider` | If not nil, ADE injects the API key through the egress proxy. CLIs that read a base URL from the environment get it automatically; others need `Provider.BaseURL` and `Provider.KeyEnv` as arguments (like `codex`). |
| `Command` | Only for terminal adapters: the command to run. |

### Cmd

```go
type Cmd struct {
    Path       string   // looked up in PATH (on the host or in the image)
    Args       []string
    Env        []string // extra KEY=VALUE on top of the controlled base environment
    Input      []byte   // written to stdin right after start
    CloseStdin bool     // close stdin after Input (CLIs that read until EOF)
}
```

The manager starts the process with a **controlled environment**: an allowlist of the daemon's
variables (`HOME`, `PATH`, `LANG`, …) plus `ADE_SESSION_ID`, `ADE_WORKSPACE_ID` and
`ADE_WORKSPACE_PATH`. Tokens like `GH_TOKEN` or `SSH_AUTH_SOCK` are never inherited. Do not try to
pass them through `Env`.

## 2. Events

`Parse` reports what happens through the `Sink`:

```go
type Sink interface {
    Emit(typ string, payload any) // one event, payload is a type from events.go
    SetResumeID(id string)        // session ID of the CLI, for Resume
    TurnDone()                    // agent finished and waits → status "idle"
    Baseline() Tokens             // tokens already counted when the process started
}
```

| Type | Payload | When |
|---|---|---|
| `message` | `Message{Role, Text}` | Text of the agent (`assistant`) or an echoed instruction (`user`). |
| `thinking` | `Thinking{Text}` | Visible reasoning, if the CLI reports it. |
| `tool_call` | `ToolCall{ID, Tool, Input, ParentID}` | The agent calls a tool. `ID` links it to the result; `ParentID` is set for sub-agents. Use the CLI's tool names (`Bash`, `Edit`, …) where they exist; approval rules match on them. |
| `tool_result` | `ToolResult{ID, Output, IsError, Truncated, ParentID}` | Result of a call. Shorten long output with `adapter.Truncate(s, adapter.MaxText)` and set `Truncated`. |
| `cost` | `Cost{TurnUSD, …Tokens, Model, Estimated}` | End of a turn. If the CLI reports no USD, set only the tokens and `Estimated: true`; the manager computes `TurnUSD` from the price table. |
| `error` | `Error{Message}` | Error message of the CLI. |

`status` events (and `usage`, `approval_*`, `egress`) are created by the manager, not by
adapters. Payloads and required fields are described in
[`event.schema.json`](../internal/agent/adapter/event.schema.json); a new event type needs an
entry there (a test checks that every type is covered) and is shown generically by the
dashboard until it gets its own view.

Rules for `Parse`:

- Read until EOF and return `nil` at EOF; return an error only for a broken stream. A CLI that
  crashes ends with EOF too; the manager sees the exit code.
- Lines that are not valid JSON (warnings, progress bars) are not an error. Skip them or report
  them as `error` events if they look like an error.
- Call `TurnDone` exactly when the agent waits for the next instruction. For CLIs with one
  instruction per process, the end of the process is enough.
- Call `SetResumeID` as soon as the CLI reports its session ID, so an interrupted session can
  be resumed.
- CLIs that report cumulative token counts per conversation (like `codex` when resuming)
  subtract `sink.Baseline()`, so costs are not counted twice.

## 3. Sandbox, approvals and costs

**Sandbox.** In the sandbox the manager runs the command with `docker exec` in the workspace
container. The CLI must therefore be installed in
[`images/agent-base`](../images/agent-base/Dockerfile) (or in a project's own image). Add it
there with a pinned or configurable version and make sure it works without a TTY, under the
UID of the daemon (which may have no home of its own in the image; ADE sets `HOME`), without
capabilities, and with network access only through the proxy (`HTTP(S)_PROXY` is set, external
DNS does not resolve). See [sandbox.md](sandbox.md#2-agent-container). Add its API
hosts as a preset in [`internal/sandbox/egress.go`](../internal/sandbox/egress.go) if they
are not covered yet.

**Secrets.** ADE never puts API keys into the container. For a CLI that talks to a known
provider, map the adapter to the provider in [`internal/provider`](../internal/provider)
(`ForAdapter`) and to the key variable in `internal/daemon` (`adapterEnv` for local sessions,
`injectedProviders` and `providerAccess` for the proxy). The proxy then injects the key and
measures token usage. A CLI that cannot use a different base URL can only run in the sandbox
with its own login.

**Approvals.** If the CLI has a hook that runs before tool calls and can block, set
`Capabilities.Approvals` and register `Spec.Hook.Command` in `Command` (see
`claude.Command`: `--settings` with a `PreToolUse` hook). `ade hook pretooluse` reads the
Claude Code hook format; another format needs a variant of the hook command in
[`internal/approval`](../internal/approval) and `cmd/ade/hook.go`. Without a hook, document
how the CLI limits itself (Codex: `--sandbox workspace-write` locally).

**Costs.** Report `cost` at the end of every turn. If the proxy measures the session, the
manager ignores the CLI's numbers; otherwise they are what the dashboard shows. Add the CLI's
models to the price table in `internal/provider` if they are missing.

## 4. Registering the adapter

1. Package `internal/agent/adapter/<name>/` with `New()` and the interface.
2. Register it in `internal/daemon/daemon.go` (`adapter.NewRegistry(claude.New(), codex.New(), …)`).
3. Allow it in `project.yaml`: `KnownAdapters` in [`internal/project/config.go`](../internal/project/config.go).
4. For factories (structured adapters only): `Adapters` in [`internal/factory/definition.go`](../internal/factory/definition.go).
5. Permission modes: `Modes` in [`internal/api/agents.go`](../internal/api/agents.go).
6. Provider mapping, egress preset and image as described above.
7. Documentation: adapter table in [architecture.md](architecture.md#23-agent-adapter),
   `adapter` values in [configuration.md](configuration.md), [`CHANGELOG.md`](../CHANGELOG.md).

## 5. Testing

- **Recorded output.** Run the real CLI once with a small task and save its stdout to
  `testdata/*.jsonl` (remove personal data). Tests parse the file with a recording sink and check
  the event types, payloads, resume ID and `TurnDone` calls (see `claude_test.go`,
  `TestParseRecordedTask`). Record a resume as well, and odd lines (errors, non-JSON).
- **Command.** Table tests for `Command`: model, permission mode, `Sandbox`, `ReadOnly`, `Hook`,
  `Provider`, `ResumeID`, and rejected values (`-x` as model or resume ID).
- **Truncation.** A large tool result is shortened to `MaxText` and marked.
- **Everything around the adapter** (manager, API, factory runner) is tested with the
  [`fake`](../internal/agent/adapter/fake/fake.go) adapter: it runs any command (default `cat`)
  and reads JSON lines `{"type": …, "payload": {…}}`, plus `{"type":"resume_id",…}` and
  `{"type":"turn_done"}`. A new adapter does not need its own manager tests unless it adds a
  capability the fake cannot express.
- Before opening the PR, try the adapter for real: locally and in the sandbox, with a follow-up
  instruction, after a daemon restart (resume), and in a factory role with `permissions: read`.

## 6. Example: a minimal adapter

A CLI `mycli` that takes the task on stdin, prints one JSON object per line and exits after
the task:

```go
// Package mycli connects the (fictional) mycli CLI.
package mycli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "mycli" }

func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Resume: true}
}

func (a *Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	if _, err := exec.LookPath("mycli"); err != nil && !spec.Sandbox {
		return nil, errors.New("mycli not found – install it with npm install -g mycli")
	}
	args := []string{"run", "--json"}
	if spec.Model != "" {
		if strings.HasPrefix(spec.Model, "-") {
			return nil, fmt.Errorf("invalid model %q", spec.Model)
		}
		args = append(args, "--model", spec.Model)
	}
	if spec.ReadOnly {
		args = append(args, "--no-write")
	}
	if spec.ResumeID != "" {
		args = append(args, "--resume", spec.ResumeID)
	}
	in, err := a.EncodeInput(spec.Task)
	if err != nil {
		return nil, err
	}
	return &adapter.Cmd{Path: "mycli", Args: args, Input: in, CloseStdin: true}, nil
}

func (a *Adapter) EncodeInput(text string) ([]byte, error) { return []byte(text), nil }

// line is one output line of mycli.
type line struct {
	Kind    string          `json:"kind"`
	Session string          `json:"session"`
	Text    string          `json:"text"`
	CallID  string          `json:"call_id"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Output  string          `json:"output"`
	Tokens  struct{ In, Out int64 }
}

func (a *Adapter) Parse(r io.Reader, sink adapter.Sink) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue // not JSON: progress output
		}
		switch l.Kind {
		case "start":
			sink.SetResumeID(l.Session)
		case "text":
			sink.Emit(adapter.TypeMessage, adapter.Message{Role: "assistant", Text: l.Text})
		case "call":
			sink.Emit(adapter.TypeToolCall, adapter.ToolCall{ID: l.CallID, Tool: l.Tool, Input: l.Args})
		case "result":
			out, cut := adapter.Truncate(l.Output, adapter.MaxText)
			sink.Emit(adapter.TypeToolResult, adapter.ToolResult{ID: l.CallID, Output: out, Truncated: cut})
		case "done":
			sink.Emit(adapter.TypeCost, adapter.Cost{
				InputTokens: l.Tokens.In, OutputTokens: l.Tokens.Out, Estimated: true,
			})
			sink.TurnDone()
		case "error":
			sink.Emit(adapter.TypeError, adapter.Error{Message: l.Text})
		}
	}
	return sc.Err()
}
```

And its test with a recorded run:

```go
func TestParseRecorded(t *testing.T) {
	f, err := os.Open("testdata/task.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := &recSink{}
	if err := New().Parse(f, s); err != nil {
		t.Fatal(err)
	}
	want := []string{"tool_call", "tool_result", "message", "cost"}
	if !slices.Equal(s.types(), want) || s.turns != 1 || s.resume[0] == "" {
		t.Fatalf("events %v, turns %d, resume %v", s.types(), s.turns, s.resume)
	}
}
```

`recSink` is a small `adapter.Sink` that records the calls (copy it from `claude_test.go`).
