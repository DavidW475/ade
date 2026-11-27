// Package codex bindet die Codex CLI (OpenAI) im exec-Modus mit JSONL-Ausgabe an.
//
// Aufruf (geprüft gegen Codex CLI 0.172):
//
//	codex exec --json --skip-git-repo-check [-m M] (-s MODE | --dangerously-bypass-approvals-and-sandbox)
//	           [-c model_provider=ade -c model_providers.ade={…}] [resume ID] -
//
// Die Anweisung kommt über stdin („-“); Codex liest bis EOF, ADE schließt stdin danach. Ein
// Prozess bearbeitet genau eine Anweisung. Folgeanweisungen setzen die Unterhaltung mit
// `resume <thread_id>` in einem neuen Prozess fort.
//
// Eine eigene Basis-URL (Secret-Injection über den Egress-Proxy) geht nur über einen
// eigenen Provider in der Konfiguration; OPENAI_BASE_URL beachtet Codex nicht.
//
// Die Token-Zahlen in turn.completed sind pro Unterhaltung kumuliert: Beim Fortsetzen
// enthalten sie die früheren Durchgänge, die der Parser über Sink.Baseline abzieht.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"slices"
	"strings"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

// Binary ist der Name des CLI-Programms.
const Binary = "codex"

// SandboxModes sind die Werte für -s, die ADE als permission_mode durchreicht.
var SandboxModes = []string{"read-only", "workspace-write", "danger-full-access"}

// DefaultSandboxMode gilt ohne permission_mode außerhalb der ADE-Sandbox.
const DefaultSandboxMode = "workspace-write"

// providerID ist der Name des eigenen Providers in der Codex-Konfiguration.
const providerID = "ade"

// Adapter ist der Codex-Adapter.
type Adapter struct {
	// Binary überschreibt den Programmnamen (Tests).
	Binary string
}

// New erzeugt den Adapter.
func New() *Adapter { return &Adapter{Binary: Binary} }

// Name implementiert adapter.Adapter.
func (a *Adapter) Name() string { return "codex" }

// Capabilities implementiert adapter.Adapter: Folgeanweisungen nur über Fortsetzen.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Resume: true}
}

// Command implementiert adapter.Adapter.
func (a *Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	bin := a.Binary
	if bin == "" {
		bin = Binary
	}
	if _, err := exec.LookPath(bin); err != nil && !spec.Sandbox {
		return nil, fmt.Errorf("%s nicht gefunden – Codex CLI installieren und anmelden (npm install -g @openai/codex)", bin)
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--color", "never"}
	if spec.Model != "" {
		if strings.HasPrefix(spec.Model, "-") {
			return nil, fmt.Errorf("ungültiges Modell %q", spec.Model)
		}
		args = append(args, "--model", spec.Model)
	}
	switch {
	case spec.PermissionMode != "" && !slices.Contains(SandboxModes, spec.PermissionMode):
		return nil, fmt.Errorf("unbekannter Sandbox-Modus %q für codex (erlaubt: %s)", spec.PermissionMode, strings.Join(SandboxModes, ", "))
	case spec.Sandbox:
		// Der Container ist die Sandbox; die eigene (Landlock/seccomp) von Codex läuft darin
		// nicht zuverlässig. Rückfragen gibt es im exec-Modus ohnehin nicht.
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case spec.PermissionMode != "":
		args = append(args, "--sandbox", spec.PermissionMode)
	default:
		args = append(args, "--sandbox", DefaultSandboxMode)
	}
	if p := spec.Provider; p != nil {
		cfg, err := providerConfig(p)
		if err != nil {
			return nil, err
		}
		args = append(args, "-c", "model_provider="+providerID, "-c", "model_providers."+providerID+"="+cfg)
	}
	if spec.ResumeID != "" {
		if strings.HasPrefix(spec.ResumeID, "-") || strings.ContainsAny(spec.ResumeID, " \t\n") {
			return nil, fmt.Errorf("ungültige Resume-ID %q", spec.ResumeID)
		}
		args = append(args, "resume", spec.ResumeID)
	}
	args = append(args, "-")
	cmd := &adapter.Cmd{Path: bin, Args: args, CloseStdin: true}
	in, err := a.EncodeInput(spec.Task)
	if err != nil {
		return nil, err
	}
	cmd.Input = in
	return cmd, nil
}

// providerConfig baut den Provider-Eintrag als TOML-Inline-Tabelle.
func providerConfig(p *adapter.ProviderAccess) (string, error) {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(p.BaseURL, "\"\\\n") {
		return "", fmt.Errorf("ungültige Basis-URL %q", p.BaseURL)
	}
	if p.KeyEnv == "" || strings.ContainsAny(p.KeyEnv, "\"\\\n =") {
		return "", fmt.Errorf("ungültige Key-Variable %q", p.KeyEnv)
	}
	return fmt.Sprintf(`{name="ADE", base_url="%s", env_key="%s", wire_api="responses"}`, p.BaseURL, p.KeyEnv), nil
}

// EncodeInput implementiert adapter.Adapter: die Anweisung als Text für stdin.
func (a *Adapter) EncodeInput(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("leere Anweisung")
	}
	return []byte(text), nil
}

// line ist eine Zeile der JSONL-Ausgabe.
type line struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Message  string          `json:"message"`
	Item     json.RawMessage `json:"item"`
	Usage    *usage          `json:"usage"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// usage ist die Nutzung in turn.completed (OpenAI-Zählweise: input enthält cached).
type usage struct {
	Input       int64 `json:"input_tokens"`
	Cached      int64 `json:"cached_input_tokens"`
	CacheWrite  int64 `json:"cache_write_input_tokens"`
	Output      int64 `json:"output_tokens"`
	ReasoningOu int64 `json:"reasoning_output_tokens"`
}

type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Message          string          `json:"message"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Changes          json.RawMessage `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string          `json:"query"`
	Items json.RawMessage `json:"items"`
}

// maxLine begrenzt eine Ausgabezeile (große Befehlsausgaben).
const maxLine = 64 << 20

// Parse implementiert adapter.Adapter.
func (a *Adapter) Parse(r io.Reader, sink adapter.Sink) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	p := &parser{sink: sink, started: map[string]bool{}}
	base := sink.Baseline()
	p.last = usage{Input: base.Input + base.CacheRead, Cached: base.CacheRead, CacheWrite: base.CacheWrite, Output: base.Output}
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	return sc.Err()
}

type parser struct {
	sink     adapter.Sink
	started  map[string]bool // Werkzeug-Aufrufe, für die schon ein tool_call kam
	last     usage           // kumulierte Nutzung bis zum vorigen Durchgang
	initDone bool
}

func (p *parser) line(raw []byte) {
	if strings.TrimSpace(string(raw)) == "" {
		return
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.Type == "" {
		text, _ := adapter.Truncate(string(raw), adapter.MaxText)
		p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
		return
	}
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			p.sink.SetResumeID(l.ThreadID)
		}
		if !p.initDone {
			p.initDone = true
			p.sink.Emit(adapter.TypeStatus, adapter.Status{Status: "running", Reason: "Codex gestartet"})
		}
	case "item.started", "item.updated":
		var it item
		if json.Unmarshal(l.Item, &it) == nil {
			p.itemStarted(&it)
		}
	case "item.completed":
		var it item
		if json.Unmarshal(l.Item, &it) == nil {
			p.itemCompleted(&it)
		}
	case "turn.completed":
		p.turnDone(l.Usage, "")
	case "turn.failed":
		msg := "Durchgang fehlgeschlagen"
		if l.Error != nil && l.Error.Message != "" {
			msg = l.Error.Message
		}
		p.turnDone(l.Usage, msg)
	case "error":
		// Wiederholungsversuche meldet Codex als error; erst turn.failed ist endgültig.
		if l.Message != "" && !strings.HasPrefix(l.Message, "Reconnecting...") {
			text, _ := adapter.Truncate(l.Message, adapter.MaxText)
			p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
		}
	}
}

func (p *parser) itemStarted(it *item) {
	if p.started[it.ID] {
		return
	}
	if tool, input := toolCall(it); tool != "" {
		p.started[it.ID] = true
		p.sink.Emit(adapter.TypeToolCall, adapter.ToolCall{ID: it.ID, Tool: tool, Input: input})
	}
}

func (p *parser) itemCompleted(it *item) {
	switch it.Type {
	case "agent_message":
		if strings.TrimSpace(it.Text) != "" {
			text, _ := adapter.Truncate(it.Text, adapter.MaxText)
			p.sink.Emit(adapter.TypeMessage, adapter.Message{Role: "assistant", Text: text})
		}
		return
	case "reasoning":
		if strings.TrimSpace(it.Text) != "" {
			text, _ := adapter.Truncate(it.Text, adapter.MaxText)
			p.sink.Emit(adapter.TypeThinking, adapter.Thinking{Text: text})
		}
		return
	case "error":
		if it.Message != "" {
			text, _ := adapter.Truncate(it.Message, adapter.MaxText)
			p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
		}
		return
	}
	tool, _ := toolCall(it)
	if tool == "" {
		return
	}
	p.itemStarted(it)
	delete(p.started, it.ID)
	out, isErr := toolResult(it)
	text, cut := adapter.Truncate(out, adapter.MaxText)
	p.sink.Emit(adapter.TypeToolResult, adapter.ToolResult{ID: it.ID, Output: text, IsError: isErr, Truncated: cut})
}

// toolCall übersetzt ein Item in Werkzeug-Name und Eingabe (leer: kein Werkzeug). Die Namen
// lehnen sich an Claude Code an, damit die Timeline sie gleich darstellt.
func toolCall(it *item) (string, json.RawMessage) {
	mustJSON := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	switch it.Type {
	case "command_execution":
		return "Bash", mustJSON(map[string]string{"command": it.Command})
	case "file_change":
		var changes []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(it.Changes, &changes)
		in := map[string]any{"changes": changes}
		if len(changes) > 0 {
			in["path"] = changes[0].Path
		}
		return "Edit", mustJSON(in)
	case "mcp_tool_call":
		args := it.Arguments
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		return "mcp__" + it.Server + "__" + it.Tool, args
	case "web_search":
		return "WebSearch", mustJSON(map[string]string{"query": it.Query})
	case "todo_list":
		return "TodoList", mustJSON(map[string]json.RawMessage{"items": it.Items})
	}
	return "", nil
}

func toolResult(it *item) (string, bool) {
	failed := it.Status == "failed" || it.Status == "declined"
	switch it.Type {
	case "command_execution":
		out := it.AggregatedOutput
		if it.ExitCode != nil && *it.ExitCode != 0 {
			failed = true
			out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n[Exit-Code %d]", *it.ExitCode)
		}
		return out, failed
	case "file_change":
		var changes []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(it.Changes, &changes)
		lines := make([]string, 0, len(changes))
		for _, c := range changes {
			lines = append(lines, c.Kind+" "+c.Path)
		}
		return strings.Join(lines, "\n"), failed
	case "mcp_tool_call":
		if it.Error != nil && it.Error.Message != "" {
			return it.Error.Message, true
		}
		return string(it.Result), failed
	}
	return it.Status, failed
}

func (p *parser) turnDone(u *usage, errMsg string) {
	c := adapter.Cost{Estimated: true, Turns: 1}
	if u != nil {
		cur := *u
		d := usage{
			Input: max(cur.Input-p.last.Input, 0), Cached: max(cur.Cached-p.last.Cached, 0),
			CacheWrite: max(cur.CacheWrite-p.last.CacheWrite, 0), Output: max(cur.Output-p.last.Output, 0),
		}
		// Kumulierte Zahlen, die kleiner sind als die Basis (andere Zählung als erwartet):
		// dann sind es wohl schon die Zahlen dieses Durchgangs.
		if cur.Input < p.last.Input || cur.Output < p.last.Output {
			d = cur
		}
		p.last = cur
		c.InputTokens = max(d.Input-d.Cached, 0)
		c.CacheReadTokens = d.Cached
		c.CacheWriteTokens = d.CacheWrite
		c.OutputTokens = d.Output
	}
	p.sink.Emit(adapter.TypeCost, c)
	if errMsg != "" {
		text, _ := adapter.Truncate(errMsg, adapter.MaxText)
		p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
	}
	p.sink.TurnDone()
}
