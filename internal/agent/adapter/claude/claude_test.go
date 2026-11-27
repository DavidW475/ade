package claude

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent/adapter"
)

type rec struct {
	typ     string
	payload any
}

type recSink struct {
	events []rec
	resume []string
	turns  int
}

func (s *recSink) Emit(typ string, payload any) { s.events = append(s.events, rec{typ, payload}) }
func (s *recSink) SetResumeID(id string)        { s.resume = append(s.resume, id) }
func (s *recSink) TurnDone()                    { s.turns++ }
func (s *recSink) Baseline() adapter.Tokens     { return adapter.Tokens{} }

func (s *recSink) types() []string {
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.typ
	}
	return out
}

func parseFile(t *testing.T, name string) *recSink {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	s := &recSink{}
	if err := New().Parse(f, s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Aufgezeichnet mit Claude Code 2.1: Datei lesen (Read), Zeile anhängen und zählen (Bash).
func TestParseRecordedTask(t *testing.T) {
	s := parseFile(t, "task.jsonl")

	want := []string{"status", "tool_call", "tool_result", "tool_call", "tool_result", "message", "cost"}
	if got := s.types(); !slices.Equal(got, want) {
		t.Fatalf("Events = %v\nwant     %v", got, want)
	}
	if st := s.events[0].payload.(adapter.Status); st.Model != "claude-haiku-5-5" {
		t.Errorf("Status = %+v", st)
	}

	read := s.events[1].payload.(adapter.ToolCall)
	if read.Tool != "Read" || read.ID == "" || !strings.Contains(string(read.Input), "/workspace/a.txt") {
		t.Errorf("Read-Aufruf = %+v", read)
	}
	readRes := s.events[2].payload.(adapter.ToolResult)
	if readRes.ID != read.ID || !strings.Contains(readRes.Output, "hello") || readRes.IsError {
		t.Errorf("Read-Ergebnis = %+v", readRes)
	}
	bash := s.events[3].payload.(adapter.ToolCall)
	var in struct{ Command string }
	if err := json.Unmarshal(bash.Input, &in); err != nil || bash.Tool != "Bash" || !strings.Contains(in.Command, "wc -l a.txt") {
		t.Errorf("Bash-Aufruf = %+v (%v)", bash, err)
	}
	if res := s.events[4].payload.(adapter.ToolResult); res.ID != bash.ID || res.Output != "2 a.txt" {
		t.Errorf("Bash-Ergebnis = %+v", res)
	}
	if msg := s.events[5].payload.(adapter.Message); msg.Role != "assistant" || !strings.Contains(msg.Text, "world") {
		t.Errorf("Nachricht = %+v", msg)
	}
	c := s.events[6].payload.(adapter.Cost)
	if math.Abs(c.TurnUSD-0.00398777) > 1e-9 || c.OutputTokens != 683 || c.Turns != 3 || c.CacheReadTokens != 56267 {
		t.Errorf("Kosten = %+v", c)
	}

	if s.turns != 1 {
		t.Errorf("TurnDone = %d, want 1", s.turns)
	}
	if !slices.Equal(s.resume, []string{"5c272151-5536-458f-ad5b-bdfd71e8a920"}) {
		t.Errorf("Resume-IDs = %v", s.resume)
	}
}

// Fortsetzen mit --resume und zwei Anweisungen in einem Prozess: zwei Durchgänge, Kosten
// als Differenz der kumulierten total_cost_usd.
func TestParseRecordedResumeWithFollowUp(t *testing.T) {
	s := parseFile(t, "resume.jsonl")
	want := []string{"status", "message", "cost", "message", "cost"}
	if got := s.types(); !slices.Equal(got, want) {
		t.Fatalf("Events = %v\nwant     %v", got, want)
	}
	if s.turns != 2 {
		t.Errorf("TurnDone = %d, want 2", s.turns)
	}
	c1, c2 := s.events[2].payload.(adapter.Cost), s.events[4].payload.(adapter.Cost)
	if math.Abs(c1.TurnUSD-0.00444588) > 1e-9 || math.Abs(c2.TurnUSD-(0.00511358-0.00444588)) > 1e-9 {
		t.Errorf("Kosten = %v / %v", c1.TurnUSD, c2.TurnUSD)
	}
	if m := s.events[3].payload.(adapter.Message); m.Text != "3" {
		t.Errorf("zweite Antwort = %+v", m)
	}
	if len(s.resume) != 1 {
		t.Errorf("Resume-ID mehrfach gemeldet: %v", s.resume)
	}
}

func TestParseErrorsAndOddLines(t *testing.T) {
	in := strings.Join([]string{
		`kein json`,
		``,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"überlege"},{"type":"text","text":"  "}]},"parent_tool_use_id":"toolu_parent"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Task","input":{}}]},"parent_tool_use_id":"toolu_parent"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"image"}],"is_error":true}]}}`,
		`{"type":"user","message":{"content":"normale Nutzernachricht"}}`,
		// Claude Code 2.1: message ist hier ein String.
		`{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"t2","decision_reason_type":"subcommandResults","message":"This Bash command contains multiple operations.","session_id":"s"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["zu viele Durchgänge"]}`,
	}, "\n")
	s := &recSink{}
	if err := New().Parse(strings.NewReader(in), s); err != nil {
		t.Fatal(err)
	}
	want := []string{"error", "thinking", "tool_call", "tool_result", "cost", "error"}
	if got := s.types(); !slices.Equal(got, want) {
		t.Fatalf("Events = %v, want %v", got, want)
	}
	if th := s.events[1].payload.(adapter.Thinking); th.Text != "überlege" {
		t.Errorf("Thinking = %+v", th)
	}
	if tc := s.events[2].payload.(adapter.ToolCall); tc.ParentID != "toolu_parent" {
		t.Errorf("ParentID fehlt: %+v", tc)
	}
	if tr := s.events[3].payload.(adapter.ToolResult); tr.Output != "a\n[Bild]" || !tr.IsError {
		t.Errorf("ToolResult = %+v", tr)
	}
	if e := s.events[5].payload.(adapter.Error); e.Message != "zu viele Durchgänge" {
		t.Errorf("Fehler = %+v", e)
	}
	if s.turns != 1 {
		t.Errorf("TurnDone = %d", s.turns)
	}
}

func TestParseTruncatesLargeToolResult(t *testing.T) {
	big := strings.Repeat("ä", adapter.MaxText) // 2 Bytes je Zeichen
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": big}},
	}})
	s := &recSink{}
	if err := New().Parse(strings.NewReader(string(line)), s); err != nil {
		t.Fatal(err)
	}
	r := s.events[0].payload.(adapter.ToolResult)
	if !r.Truncated || len(r.Output) > adapter.MaxText || !strings.HasSuffix(r.Output, "ä") {
		t.Errorf("Kürzung: truncated=%v len=%d", r.Truncated, len(r.Output))
	}
}

func TestCommand(t *testing.T) {
	a := &Adapter{Binary: "sh"} // sh gibt es überall; der Aufruf wird nur gebaut
	c, err := a.Command(adapter.Spec{Task: "Hallo", Model: "haiku", PermissionMode: "acceptEdits", ResumeID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--verbose", "--model haiku", "--permission-mode acceptEdits", "--resume abc"} {
		if !strings.Contains(args, want) {
			t.Errorf("Argumente %q enthalten nicht %q", args, want)
		}
	}
	var msg struct {
		Type    string
		Message struct{ Role, Content string }
	}
	if err := json.Unmarshal(c.Input, &msg); err != nil || msg.Type != "user" || msg.Message.Content != "Hallo" || c.Input[len(c.Input)-1] != '\n' {
		t.Errorf("Input = %q (%v)", c.Input, err)
	}

	for _, spec := range []adapter.Spec{
		{Task: "x", PermissionMode: "alles"},
		{Task: "x", Model: "--dangerously-skip-permissions"},
		{Task: "x", ResumeID: "-x"},
	} {
		if _, err := a.Command(spec); err == nil {
			t.Errorf("Command(%+v) ohne Fehler", spec)
		}
	}
	if _, err := (&Adapter{Binary: "gibt-es-nicht-ade"}).Command(adapter.Spec{Task: "x"}); err == nil {
		t.Error("fehlendes Programm ohne Fehler")
	}
	if _, err := a.EncodeInput("  "); err == nil {
		t.Error("leere Anweisung ohne Fehler")
	}
}

func TestCommandWithHook(t *testing.T) {
	a := &Adapter{Binary: "sh"}
	c, err := a.Command(adapter.Spec{Task: "x", Hook: &adapter.Hook{Command: "/opt/ade/ade hook pretooluse", Timeout: 10 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(c.Args, "--settings")
	if i < 0 || i+1 >= len(c.Args) {
		t.Fatalf("kein --settings in %q", c.Args)
	}
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct {
					Type, Command string
					Timeout       int
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(c.Args[i+1]), &s); err != nil {
		t.Fatal(err)
	}
	pt := s.Hooks.PreToolUse
	if len(pt) != 1 || pt[0].Matcher != "*" || len(pt[0].Hooks) != 1 || pt[0].Hooks[0].Command != "/opt/ade/ade hook pretooluse" ||
		pt[0].Hooks[0].Type != "command" || pt[0].Hooks[0].Timeout != 630 {
		t.Fatalf("Settings %s", c.Args[i+1])
	}
	if c, _ := a.Command(adapter.Spec{Task: "x"}); slices.Contains(c.Args, "--settings") {
		t.Error("--settings ohne Hook")
	}
}
