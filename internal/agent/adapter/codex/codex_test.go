package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	base   adapter.Tokens
}

func (s *recSink) Emit(typ string, payload any) { s.events = append(s.events, rec{typ, payload}) }
func (s *recSink) SetResumeID(id string)        { s.resume = append(s.resume, id) }
func (s *recSink) TurnDone()                    { s.turns++ }
func (s *recSink) Baseline() adapter.Tokens     { return s.base }

func (s *recSink) types() []string {
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.typ
	}
	return out
}

func (s *recSink) cost(t *testing.T) adapter.Cost {
	t.Helper()
	for _, e := range s.events {
		if c, ok := e.payload.(adapter.Cost); ok {
			return c
		}
	}
	t.Fatal("kein cost-Event")
	return adapter.Cost{}
}

func parseFile(t *testing.T, name string, base adapter.Tokens) *recSink {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	s := &recSink{base: base}
	if err := New().Parse(f, s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Aufgezeichnet mit Codex CLI 0.172 gegen einen nachgebauten Responses-Endpunkt: ein
// Befehl (echo hallo), dann eine Antwort; je Anfrage 1000 Eingabe-Tokens (200 aus dem Cache)
// und 50 Ausgabe-Tokens.
func TestParseRecordedTask(t *testing.T) {
	s := parseFile(t, "task.jsonl", adapter.Tokens{})
	want := []string{"status", "error", "tool_call", "tool_result", "message", "cost"}
	if got := s.types(); !slices.Equal(got, want) {
		t.Fatalf("Events = %v\nwant     %v", got, want)
	}
	if len(s.resume) != 1 || s.resume[0] == "" {
		t.Errorf("Resume-IDs = %v", s.resume)
	}
	if s.turns != 1 {
		t.Errorf("turns = %d", s.turns)
	}
	call := s.events[2].payload.(adapter.ToolCall)
	if call.Tool != "Bash" || !strings.Contains(string(call.Input), "echo hallo") {
		t.Errorf("tool_call = %+v", call)
	}
	res := s.events[3].payload.(adapter.ToolResult)
	if res.ID != call.ID || res.Output != "hallo\n" || res.IsError {
		t.Errorf("tool_result = %+v", res)
	}
	if m := s.events[4].payload.(adapter.Message); m.Text != "Fertig." || m.Role != "assistant" {
		t.Errorf("message = %+v", m)
	}
	c := s.cost(t)
	if !c.Estimated || c.InputTokens != 1600 || c.CacheReadTokens != 400 || c.OutputTokens != 100 {
		t.Errorf("cost = %+v", c)
	}
}

// Beim Fortsetzen sind die Zahlen kumuliert; die Basis aus dem ersten Lauf wird abgezogen.
func TestParseResumeSubtractsBaseline(t *testing.T) {
	s := parseFile(t, "resume.jsonl", adapter.Tokens{Input: 1600, CacheRead: 400, Output: 100})
	c := s.cost(t)
	if c.InputTokens != 800 || c.CacheReadTokens != 200 || c.OutputTokens != 50 {
		t.Errorf("cost = %+v", c)
	}
}

func TestParseFailedTurnAndNoise(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"error","message":"Reconnecting... 1/5 (stream disconnected)"}`,
		`kein json`,
		`{"type":"item.completed","item":{"id":"i1","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"i2","type":"command_execution","command":"false","aggregated_output":"","exit_code":1,"status":"failed"}}`,
		`{"type":"turn.failed","error":{"message":"401 Unauthorized"}}`,
	}, "\n")
	s := &recSink{}
	if err := New().Parse(strings.NewReader(in), s); err != nil {
		t.Fatal(err)
	}
	want := []string{"status", "error", "tool_call", "tool_result", "tool_call", "tool_result", "cost", "error"}
	if got := s.types(); !slices.Equal(got, want) {
		t.Fatalf("Events = %v\nwant     %v", got, want)
	}
	if c := s.events[2].payload.(adapter.ToolCall); c.Tool != "Edit" || !strings.Contains(string(c.Input), `"path":"a.go"`) {
		t.Errorf("file_change = %+v", c)
	}
	if r := s.events[5].payload.(adapter.ToolResult); !r.IsError || !strings.Contains(r.Output, "Exit-Code 1") {
		t.Errorf("Befehl mit Fehler = %+v", r)
	}
	if e := s.events[7].payload.(adapter.Error); e.Message != "401 Unauthorized" {
		t.Errorf("error = %+v", e)
	}
	if s.turns != 1 {
		t.Errorf("turns = %d", s.turns)
	}
}

func TestCommand(t *testing.T) {
	a := &Adapter{Binary: "sh"} // sh gibt es sicher
	cases := []struct {
		name string
		spec adapter.Spec
		want []string
		not  []string
	}{
		{"lokal", adapter.Spec{Task: "x", Model: "gpt-5"}, []string{"exec", "--json", "--model gpt-5", "--sandbox workspace-write", " -"}, []string{"bypass", "resume"}},
		{"modus", adapter.Spec{Task: "x", PermissionMode: "read-only"}, []string{"--sandbox read-only"}, nil},
		{"sandbox", adapter.Spec{Task: "x", Sandbox: true, Provider: &adapter.ProviderAccess{BaseURL: "http://proxy:8080/p/openai/v1", KeyEnv: "OPENAI_API_KEY"}},
			[]string{"--dangerously-bypass-approvals-and-sandbox", "model_provider=ade", `base_url="http://proxy:8080/p/openai/v1"`, `env_key="OPENAI_API_KEY"`}, []string{"--sandbox "}},
		{"resume", adapter.Spec{Task: "weiter", ResumeID: "t-1"}, []string{"resume t-1 -"}, nil},
		{"lese-rolle", adapter.Spec{Task: "x", ReadOnly: true, PermissionMode: "workspace-write"}, []string{"--sandbox read-only"}, []string{"workspace-write"}},
		{"lese-rolle sandbox", adapter.Spec{Task: "x", ReadOnly: true, Sandbox: true}, []string{"--dangerously-bypass-approvals-and-sandbox"}, []string{"--sandbox "}},
	}
	for _, tc := range cases {
		c, err := a.Command(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		joined := strings.Join(c.Args, " ")
		for _, w := range tc.want {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: %q fehlt in %q", tc.name, w, joined)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(joined, n) {
				t.Errorf("%s: %q unerwartet in %q", tc.name, n, joined)
			}
		}
		if !c.CloseStdin || string(c.Input) != tc.spec.Task {
			t.Errorf("%s: stdin = %q, close %v", tc.name, c.Input, c.CloseStdin)
		}
	}
	for _, bad := range []adapter.Spec{
		{Task: "x", Model: "-x"},
		{Task: "x", PermissionMode: "bypassPermissions"},
		{Task: "x", ResumeID: "--last"},
		{Task: "x", Provider: &adapter.ProviderAccess{BaseURL: `http://a"b`, KeyEnv: "K"}},
		{Task: "x", Provider: &adapter.ProviderAccess{BaseURL: "file:///etc", KeyEnv: "K"}},
		{Task: " "},
	} {
		if _, err := a.Command(bad); err == nil {
			t.Errorf("Command(%+v): kein Fehler", bad)
		}
	}
}
