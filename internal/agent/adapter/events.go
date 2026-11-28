package adapter

import (
	"encoding/json"
	"unicode/utf8"
)

// Event types (doc/architecture.md#event-schema, JSON schema in event.schema.json).
// The list grows with the milestones; the UI shows unknown types generically.
const (
	// TypeStatus reports a status change of the session (payload Status). The manager creates it.
	TypeStatus = "status"
	// TypeMessage is a message from the user or the agent (payload Message).
	TypeMessage = "message"
	// TypeThinking is the model's visible thinking (payload Thinking).
	TypeThinking = "thinking"
	// TypeToolCall is a tool call of the agent (payload ToolCall).
	TypeToolCall = "tool_call"
	// TypeToolResult is the result of a tool call (payload ToolResult).
	TypeToolResult = "tool_result"
	// TypeCost reports costs and tokens of a finished turn (payload Cost).
	TypeCost = "cost"
	// TypeError is an error of the CLI or the process (payload Error).
	TypeError = "error"
)

// Status is the payload of TypeStatus.
type Status struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// The CLI reports Model and ResumeID at start if known.
	Model string `json:"model,omitempty"`
}

// Message is the payload of TypeMessage. Role is "user" or "assistant".
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Thinking is the payload of TypeThinking.
type Thinking struct {
	Text string `json:"text"`
}

// ToolCall is the payload of TypeToolCall.
type ToolCall struct {
	// ID links call and result.
	ID    string          `json:"id"`
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
	// ParentID is set if a sub-agent makes the call (ID of the call that started it).
	ParentID string `json:"parent_id,omitempty"`
}

// ToolResult is the payload of TypeToolResult.
type ToolResult struct {
	ID        string `json:"id"`
	Output    string `json:"output"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
}

// Cost is the payload of TypeCost.
type Cost struct {
	// TurnUSD are the costs of this turn; the session sums them up to cost_usd.
	TurnUSD      float64 `json:"turn_usd"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	// CacheReadTokens and CacheWriteTokens count prompt cache tokens.
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	DurationMS       int64 `json:"duration_ms,omitempty"`
	Turns            int   `json:"turns,omitempty"`
	// Model is the turn's model, if the CLI reports it.
	Model string `json:"model,omitempty"`
	// Estimated: the CLI reports no USD; ADE computes TurnUSD from tokens and the price table.
	// Adapters set it, the manager fills in TurnUSD.
	Estimated bool `json:"estimated,omitempty"`
	// Metered: the egress proxy measured the usage; TurnUSD is its sum since the last turn,
	// not the CLI's report. The manager sets it.
	Metered bool `json:"metered,omitempty"`
	// TotalUSD are the session's total costs; the manager sets them.
	TotalUSD float64 `json:"total_usd"`
	// BudgetUSD is the session's budget (0: none); the manager sets it.
	BudgetUSD float64 `json:"budget_usd,omitempty"`
}

// Error is the payload of TypeError.
type Error struct {
	Message string `json:"message"`
}

// MaxText is the maximum length of text fields in events (tool output, messages).
const MaxText = 32 << 10

// Truncate shortens s to at most n bytes without cutting a UTF-8 character.
func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
