package adapter

import (
	"encoding/json"
	"unicode/utf8"
)

// Event-Typen (doc/architecture.md#event-schema, JSON-Schema in event.schema.json).
// Die Liste wächst mit den Meilensteinen; die UI zeigt unbekannte Typen generisch an.
const (
	// TypeStatus meldet einen Statuswechsel der Session (Payload Status). Erzeugt der Manager.
	TypeStatus = "status"
	// TypeMessage ist eine Nachricht von Nutzer oder Agent (Payload Message).
	TypeMessage = "message"
	// TypeThinking ist sichtbares Nachdenken des Modells (Payload Thinking).
	TypeThinking = "thinking"
	// TypeToolCall ist ein Werkzeugaufruf des Agenten (Payload ToolCall).
	TypeToolCall = "tool_call"
	// TypeToolResult ist das Ergebnis eines Werkzeugaufrufs (Payload ToolResult).
	TypeToolResult = "tool_result"
	// TypeCost meldet Kosten und Tokens eines abgeschlossenen Durchgangs (Payload Cost).
	TypeCost = "cost"
	// TypeError ist ein Fehler der CLI oder des Prozesses (Payload Error).
	TypeError = "error"
)

// Status ist die Payload von TypeStatus.
type Status struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// Model und ResumeID meldet die CLI beim Start, falls bekannt.
	Model string `json:"model,omitempty"`
}

// Message ist die Payload von TypeMessage. Role ist "user" oder "assistant".
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Thinking ist die Payload von TypeThinking.
type Thinking struct {
	Text string `json:"text"`
}

// ToolCall ist die Payload von TypeToolCall.
type ToolCall struct {
	// ID verbindet Aufruf und Ergebnis.
	ID    string          `json:"id"`
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
	// ParentID ist gesetzt, wenn ein Unteragent den Aufruf macht (ID des Aufrufs, der ihn startete).
	ParentID string `json:"parent_id,omitempty"`
}

// ToolResult ist die Payload von TypeToolResult.
type ToolResult struct {
	ID        string `json:"id"`
	Output    string `json:"output"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
}

// Cost ist die Payload von TypeCost.
type Cost struct {
	// TurnUSD sind die Kosten dieses Durchgangs; die Session summiert sie zu cost_usd.
	TurnUSD      float64 `json:"turn_usd"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	// CacheReadTokens und CacheWriteTokens zählen Prompt-Cache-Tokens.
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	DurationMS       int64 `json:"duration_ms,omitempty"`
	Turns            int   `json:"turns,omitempty"`
	// TotalUSD sind die Gesamtkosten der Session; setzt der Manager.
	TotalUSD float64 `json:"total_usd"`
}

// Error ist die Payload von TypeError.
type Error struct {
	Message string `json:"message"`
}

// MaxText ist die maximale Länge von Textfeldern in Events (Werkzeug-Ausgaben, Nachrichten).
const MaxText = 32 << 10

// Truncate kürzt s auf höchstens n Bytes, ohne ein UTF-8-Zeichen zu zerschneiden.
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
