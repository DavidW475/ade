package adapter_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/claude"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/agent/adapter/pty"
)

func TestRegistry(t *testing.T) {
	r := adapter.NewRegistry(claude.New(), pty.New())
	r.Register(&fake.Adapter{})
	if got := r.Names(); !slices.Equal(got, []string{"claude", "fake", "pty"}) {
		t.Fatalf("Names = %v", got)
	}
	a, err := r.Get("pty")
	if err != nil || !a.Capabilities().Terminal {
		t.Fatalf("pty = %v, %v", a, err)
	}
	if _, err := r.Get("gibtsnicht"); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("error = %v", err)
	}
	infos := r.Infos()
	if len(infos) != 3 || infos[0].Name != "claude" || !infos[0].Capabilities.Resume {
		t.Fatalf("Infos = %+v", infos)
	}
}

// The JSON schema must be valid JSON and describe every event type that adapters
// erzeugen.
func TestEventSchemaCoversTypes(t *testing.T) {
	data, err := os.ReadFile("event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required []string `json:"required"`
		AllOf    []struct {
			If struct {
				Properties struct {
					Type struct {
						Const string `json:"const"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"if"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	var covered []string
	for _, c := range schema.AllOf {
		covered = append(covered, c.If.Properties.Type.Const)
	}
	for _, typ := range []string{adapter.TypeStatus, adapter.TypeMessage, adapter.TypeThinking, adapter.TypeToolCall,
		adapter.TypeToolResult, adapter.TypeCost, adapter.TypeError} {
		if !slices.Contains(covered, typ) {
			t.Errorf("schema does not describe type %q", typ)
		}
	}
	if !slices.Equal(schema.Required, []string{"seq", "session_id", "ts", "type", "payload"}) {
		t.Errorf("required = %v", schema.Required)
	}
}

func TestTruncate(t *testing.T) {
	if s, cut := adapter.Truncate("abc", 5); s != "abc" || cut {
		t.Error("short text truncated")
	}
	if s, cut := adapter.Truncate("aäb", 2); s != "a" || !cut {
		t.Errorf("Truncate = %q, %v", s, cut)
	}
}

func TestPTYCommand(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	c, err := pty.New().Command(adapter.Spec{})
	if err != nil || c.Path != "/bin/zsh" {
		t.Fatalf("Command = %+v, %v", c, err)
	}
	c, err = pty.New().Command(adapter.Spec{Command: []string{"htop", "-d", "5"}})
	if err != nil || c.Path != "htop" || !slices.Equal(c.Args, []string{"-d", "5"}) {
		t.Fatalf("Command = %+v, %v", c, err)
	}
	if _, err := pty.New().EncodeInput("x"); err == nil {
		t.Error("pty accepts input through EncodeInput")
	}
}
