// Package claude connects Claude Code in print mode with stream-json.
//
// Call (checked against Claude Code 2.1):
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//	       [--model M] [--permission-mode P] [--settings HOOK] [--resume ID]
//
// HOOK registers ade hook pretooluse as a PreToolUse hook (approvals, M7).
//
// Instructions go to stdin as JSON lines ({"type":"user","message":{…}}); the process stays
// alive after a turn ("result") and waits for the next line. That makes follow-up
// instructions possible without a restart. After the process ends, --resume with the
// session_id from the init event continues the conversation.
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

// Binary is the name of the CLI program.
const Binary = "claude"

// ReadOnlyDenied are the tools a read-only role does not get.
var ReadOnlyDenied = []string{"Edit", "Write", "NotebookEdit"}

// PermissionModes are the values for --permission-mode that ADE passes on.
var PermissionModes = []string{"default", "acceptEdits", "auto", "bypassPermissions", "dontAsk", "plan"}

// Adapter is the Claude Code adapter.
type Adapter struct {
	// Binary overrides the program name (tests).
	Binary string
}

// New creates the adapter.
func New() *Adapter { return &Adapter{Binary: Binary} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "claude" }

// Capabilities implements adapter.Adapter.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Input: true, Resume: true, Approvals: true}
}

// Command implements adapter.Adapter.
func (a *Adapter) Command(spec adapter.Spec) (*adapter.Cmd, error) {
	bin := a.Binary
	if bin == "" {
		bin = Binary
	}
	if _, err := exec.LookPath(bin); err != nil && !spec.Sandbox {
		return nil, fmt.Errorf("%s not found – install Claude Code and log in (https://docs.claude.com/claude-code)", bin)
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if spec.Model != "" {
		if strings.HasPrefix(spec.Model, "-") {
			return nil, fmt.Errorf("invalid model %q", spec.Model)
		}
		args = append(args, "--model", spec.Model)
	}
	if spec.PermissionMode != "" {
		if !slices.Contains(PermissionModes, spec.PermissionMode) {
			return nil, fmt.Errorf("unknown permission mode %q (allowed: %s)", spec.PermissionMode, strings.Join(PermissionModes, ", "))
		}
		args = append(args, "--permission-mode", spec.PermissionMode)
	}
	if spec.ReadOnly {
		args = append(args, "--disallowedTools="+strings.Join(ReadOnlyDenied, ","))
	}
	if spec.Hook != nil && spec.Hook.Command != "" {
		// PreToolUse hook for all tools: ADE decides (rules or approval in the dashboard);
		// allow skips the CLI's question, deny blocks even with bypassPermissions.
		args = append(args, "--settings", approval.HookSettings(spec.Hook.Command, spec.Hook.Timeout))
	}
	if spec.ResumeID != "" {
		if strings.HasPrefix(spec.ResumeID, "-") {
			return nil, fmt.Errorf("invalid resume ID %q", spec.ResumeID)
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

// EncodeInput implements adapter.Adapter: a user message in stream-json format.
func (a *Adapter) EncodeInput(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty instruction")
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

// line is a line of the stream-json output (only the fields ADE uses).
type line struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	// Message is an object with content for assistant/user, a string for some system lines
	// (e.g. permission_denied) – therefore read it raw.
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

// maxLine limits an output line (large file contents in tool results).
const maxLine = 64 << 20

// Parse implements adapter.Adapter.
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
	lastTotal float64 // total_cost_usd is cumulative per process
	initDone  bool
}

func (p *parser) line(raw []byte) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.Type == "" {
		// Not a JSON line (e.g. a warning on stdout): make it visible as an error.
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
		// Further subtypes (thinking_tokens, permission_denied, …) bring no new information:
		// denied tools appear as tool_result with is_error.
		if l.Subtype == "init" && !p.initDone {
			p.initDone = true
			p.sink.Emit(adapter.TypeStatus, adapter.Status{Status: "running", Model: l.Model, Reason: "Claude Code started"})
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
				// Without approval the API only delivers the thinking as a signature – leave out empty blocks.
				if strings.TrimSpace(b.Thinking) != "" {
					text, _ := adapter.Truncate(b.Thinking, adapter.MaxText)
					p.sink.Emit(adapter.TypeThinking, adapter.Thinking{Text: text})
				}
			case "tool_use", "server_tool_use":
				p.sink.Emit(adapter.TypeToolCall, adapter.ToolCall{ID: b.ID, Tool: b.Name, Input: b.Input, ParentID: l.ParentToolUseID})
			}
		}
	case "user":
		// User messages in the stream are tool results; the manager reports own instructions.
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
			msg = "turn ended with an error (" + l.Subtype + ")"
		}
		text, _ := adapter.Truncate(msg, adapter.MaxText)
		p.sink.Emit(adapter.TypeError, adapter.Error{Message: text})
	}
	p.sink.TurnDone()
}

// messageContent returns message.content if message is an object.
func messageContent(raw json.RawMessage) json.RawMessage {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Content
}

// blocks reads message.content: either a string or a list of blocks.
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

// contentText turns the content of a tool_result into text (string or blocks).
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
