// Package claude bindet Claude Code im Print-Modus mit stream-json an.
//
// Aufruf (geprüft gegen Claude Code 2.1):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//	       [--model M] [--permission-mode P] [--settings HOOK] [--resume ID]
//
// HOOK bindet ade hook pretooluse als PreToolUse-Hook ein (Freigaben, M7).
//
// Anweisungen gehen als JSON-Zeilen über stdin ({"type":"user","message":{…}}); der Prozess
// bleibt nach einem Durchgang ("result") am Leben und wartet auf die nächste Zeile. So sind
// Folgeanweisungen ohne Neustart möglich. Nach einem Prozessende setzt --resume mit der
// session_id aus dem init-Event die Unterhaltung fort.
package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/approval"
)

// Binary ist der Name des CLI-Programms.
const Binary = "claude"

// PermissionModes sind die Werte für --permission-mode, die ADE durchreicht.
var PermissionModes = []string{"default", "acceptEdits", "auto", "bypassPermissions", "dontAsk", "plan"}

// Adapter ist der Claude-Code-Adapter.
type Adapter struct {
	// Binary überschreibt den Programmnamen (Tests).
	Binary string
}

// New erzeugt den Adapter.
func New() *Adapter { return &Adapter{Binary: Binary} }

// Name implementiert adapter.Adapter.
func (a *Adapter) Name() string { return "claude" }

// Capabilities implementiert adapter.Adapter.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Input: true, Resume: true, Approvals: true}
}

// Command implementiert adapter.Adapter.
func (a *Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	bin := a.Binary
	if bin == "" {
		bin = Binary
	}
	if _, err := exec.LookPath(bin); err != nil && !spec.Sandbox {
		return nil, fmt.Errorf("%s nicht gefunden – Claude Code installieren und anmelden (https://docs.claude.com/claude-code)", bin)
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if spec.Model != "" {
		if strings.HasPrefix(spec.Model, "-") {
			return nil, fmt.Errorf("ungültiges Modell %q", spec.Model)
		}
		args = append(args, "--model", spec.Model)
	}
	if spec.PermissionMode != "" {
		if !slices.Contains(PermissionModes, spec.PermissionMode) {
			return nil, fmt.Errorf("unbekannter Berechtigungsmodus %q (erlaubt: %s)", spec.PermissionMode, strings.Join(PermissionModes, ", "))
		}
		args = append(args, "--permission-mode", spec.PermissionMode)
	}
	if spec.Hook != nil && spec.Hook.Command != "" {
		// PreToolUse-Hook für alle Werkzeuge: ADE entscheidet (Regeln oder Freigabe im
		// Dashboard); allow übergeht die Rückfrage der CLI, deny sperrt auch bei bypassPermissions.
		args = append(args, "--settings", approval.HookSettings(spec.Hook.Command, spec.Hook.Timeout))
	}
	if spec.ResumeID != "" {
		if strings.HasPrefix(spec.ResumeID, "-") {
			return nil, fmt.Errorf("ungültige Resume-ID %q", spec.ResumeID)
		}
		args = append(args, "--resume", spec.ResumeID)
	}
	cmd := &adapter.Cmd{Path: bin, Args: args}
	if spec.Task != "" {
		in, err := a.EncodeInput(spec.Task)
		if err != nil {
			return nil, err
		}
		cmd.Input = in
	}
	return cmd, nil
}

// EncodeInput implementiert adapter.Adapter: eine Nutzernachricht im stream-json-Format.
func (a *Adapter) EncodeInput(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("leere Anweisung")
	}
	b, err := json.Marshal(inputMessage{Type: "user", Message: inputBody{Role: "user", Content: text}})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

type inputMessage struct {
	Type    string    `json:"type"`
	Message inputBody `json:"message"`
}

type inputBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// line ist eine Zeile der stream-json-Ausgabe (nur die Felder, die ADE nutzt).
type line struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	// Message ist bei assistant/user ein Objekt mit content, bei manchen system-Zeilen
	// (z. B. permission_denied) ein String – deshalb roh lesen.
	Message         json.RawMessage `json:"message"`
	ParentToolUseID string          `json:"parent_tool_use_id"`

	// result
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	DurationMS   int64    `json:"duration_ms"`
	NumTurns     int      `json:"num_turns"`
	Errors       []string `json:"errors"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// maxLine begrenzt eine Ausgabezeile (große Datei-Inhalte in Tool-Ergebnissen).
const maxLine = 64 << 20

// Parse implementiert adapter.Adapter.
func (a *Adapter) Parse(r io.Reader, sink adapter.Sink) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	p := parser{sink: sink}
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	return sc.Err()
}

type parser struct {
	sink      adapter.Sink
	resumeID  string
	lastTotal float64 // total_cost_usd ist pro Prozess kumuliert
	initDone  bool
}

func (p *parser) line(raw []byte) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.Type == "" {
		// Keine JSON-Zeile (z. B. Warnung auf stdout): als Fehler sichtbar machen.
		text, _ := adapter.Truncate(string(raw), adapter.MaxText)
		p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
		return
	}
	if l.SessionID != "" && l.SessionID != p.resumeID {
		p.resumeID = l.SessionID
		p.sink.SetResumeID(l.SessionID)
	}
	switch l.Type {
	case "system":
		// Weitere Untertypen (thinking_tokens, permission_denied, …) bringen keine neue
		// Information: Abgelehnte Werkzeuge erscheinen als tool_result mit is_error.
		if l.Subtype == "init" && !p.initDone {
			p.initDone = true
			p.sink.Emit(adapter.TypeStatus, adapter.Status{Status: "running", Model: l.Model, Reason: "Claude Code gestartet"})
		}
	case "assistant":
		for _, b := range blocks(messageContent(l.Message)) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					text, _ := adapter.Truncate(b.Text, adapter.MaxText)
					p.sink.Emit(adapter.TypeMessage, adapter.Message{Role: "assistant", Text: text})
				}
			case "thinking":
				// Ohne Freigabe liefert die API das Nachdenken nur als Signatur – leere Blöcke weglassen.
				if strings.TrimSpace(b.Thinking) != "" {
					text, _ := adapter.Truncate(b.Thinking, adapter.MaxText)
					p.sink.Emit(adapter.TypeThinking, adapter.Thinking{Text: text})
				}
			case "tool_use", "server_tool_use":
				p.sink.Emit(adapter.TypeToolCall, adapter.ToolCall{ID: b.ID, Tool: b.Name, Input: b.Input, ParentID: l.ParentToolUseID})
			}
		}
	case "user":
		// Nutzer-Nachrichten im Strom sind Werkzeug-Ergebnisse; eigene Anweisungen meldet der Manager.
		for _, b := range blocks(messageContent(l.Message)) {
			if b.Type != "tool_result" {
				continue
			}
			out, cut := adapter.Truncate(contentText(b.Content), adapter.MaxText)
			p.sink.Emit(adapter.TypeToolResult, adapter.ToolResult{ID: b.ToolUseID, Output: out, IsError: b.IsError, Truncated: cut, ParentID: l.ParentToolUseID})
		}
	case "result":
		p.result(&l)
	}
}

func (p *parser) result(l *line) {
	c := adapter.Cost{
		InputTokens:      l.Usage.InputTokens,
		OutputTokens:     l.Usage.OutputTokens,
		CacheReadTokens:  l.Usage.CacheReadInputTokens,
		CacheWriteTokens: l.Usage.CacheCreationInputTokens,
		DurationMS:       l.DurationMS,
		Turns:            l.NumTurns,
	}
	if l.TotalCostUSD != nil {
		c.TurnUSD = max(*l.TotalCostUSD-p.lastTotal, 0)
		p.lastTotal = *l.TotalCostUSD
	}
	p.sink.Emit(adapter.TypeCost, c)
	if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
		msg := strings.Join(l.Errors, "; ")
		if msg == "" {
			msg = l.Result
		}
		if msg == "" {
			msg = "Durchgang mit Fehler beendet (" + l.Subtype + ")"
		}
		text, _ := adapter.Truncate(msg, adapter.MaxText)
		p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
	}
	p.sink.TurnDone()
}

// messageContent liefert message.content, wenn message ein Objekt ist.
func messageContent(raw json.RawMessage) json.RawMessage {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Content
}

// blocks liest message.content: entweder ein String oder eine Liste von Blöcken.
func blocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var bs []block
	if json.Unmarshal(raw, &bs) == nil {
		return bs
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

// contentText macht aus dem content eines tool_result Text (String oder Blöcke).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[Bild]")
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}
