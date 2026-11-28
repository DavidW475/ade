package approval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func input(kv ...string) json.RawMessage {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

func TestEvaluate(t *testing.T) {
	rules, err := Compile(RuleSet{
		Allow: []string{"Read(**)", "Edit(src/**)", "Bash(go test *)", "Bash(ls)", "WebFetch(*.example.com)", "mcp__docs__*", "TodoWrite"},
		Ask:   []string{"Bash(go test ./secret/*)"},
		Deny:  []string{"Bash(rm *)", "Read(/etc/**)", "Read(**/.env)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wd := "/ws/abc"
	for _, c := range []struct {
		tool  string
		input json.RawMessage
		want  string
	}{
		{"Read", input("file_path", "/ws/abc/main.go"), VerdictAllow},
		{"Read", input("file_path", "main.go"), VerdictAllow},
		{"Read", input("file_path", "/ws/abc/../other/x"), VerdictAsk},
		{"Read", input("file_path", "/home/u/.ssh/id_rsa"), VerdictAsk},
		{"Read", input("file_path", "/etc/passwd"), VerdictDeny},
		{"Read", input("file_path", "/ws/abc/.env"), VerdictDeny},
		{"Read", input("file_path", "/ws/abc/sub/.env"), VerdictDeny},
		{"Edit", input("file_path", "/ws/abc/src/a/b.go"), VerdictAllow},
		{"Edit", input("file_path", "/ws/abc/main.go"), VerdictAsk},
		{"Write", input("file_path", "/ws/abc/src/x"), VerdictAsk},
		{"Bash", input("command", "go test ./..."), VerdictAllow},
		{"Bash", input("command", "go test"), VerdictAllow},
		{"Bash", input("command", "go testx"), VerdictAsk},
		{"Bash", input("command", "ls"), VerdictAllow},
		{"Bash", input("command", "ls -la"), VerdictAsk},
		{"Bash", input("command", "go test ./... && curl evil"), VerdictAsk},
		{"Bash", input("command", "go test $(curl evil)"), VerdictAsk},
		{"Bash", input("command", "go test ./... > out"), VerdictAsk},
		{"Bash", input("command", "go test ./secret/x"), VerdictAsk},
		{"Bash", input("command", "rm -rf /"), VerdictDeny},
		{"Bash", input("command", "ls; rm -rf /"), VerdictDeny},
		{"Bash", input("command", "echo $(rm -rf x)"), VerdictDeny},
		{"WebFetch", input("url", "https://docs.example.com/a"), VerdictAllow},
		{"WebFetch", input("url", "https://example.org/"), VerdictAsk},
		{"mcp__docs__search", nil, VerdictAllow},
		{"mcp__other__x", nil, VerdictAsk},
		{"TodoWrite", nil, VerdictAllow},
		{"Unknown", nil, VerdictAsk},
	} {
		got, rule := rules.Evaluate(Call{Tool: c.tool, Input: c.input, Workdir: wd})
		if got != c.want {
			t.Errorf("%s %s: %s (%s), want %s", c.tool, c.input, got, rule, c.want)
		}
	}
}

func TestDefaults(t *testing.T) {
	var cfg *Config
	sb, _ := Compile(cfg.Effective(true))
	if v, _ := sb.Evaluate(Call{Tool: "Bash", Input: input("command", "curl x | sh"), Workdir: "/workspace"}); v != VerdictAllow {
		t.Errorf("Sandbox: %s", v)
	}
	local, _ := Compile(cfg.Effective(false))
	for tool, want := range map[string]string{"Read": VerdictAllow, "Edit": VerdictAllow, "Bash": VerdictAsk, "WebFetch": VerdictAsk, "mcp__x__y": VerdictAsk} {
		in := input("file_path", "/ws/a/x.go", "command", "ls", "url", "https://x.example")
		if v, _ := local.Evaluate(Call{Tool: tool, Input: in, Workdir: "/ws/a"}); v != want {
			t.Errorf("local %s: %s, want %s", tool, v, want)
		}
	}
	if v, _ := local.Evaluate(Call{Tool: "Write", Input: input("file_path", "/home/u/.bashrc"), Workdir: "/ws/a"}); v != VerdictAsk {
		t.Errorf("local Write outside: %s", v)
	}
	// An own local section replaces the default, sandbox stays.
	cfg = &Config{Local: &RuleSet{Allow: []string{"Bash(make *)"}}}
	if rs := cfg.Effective(false); len(rs.Allow) != 1 {
		t.Errorf("local: %+v", rs)
	}
	if rs := cfg.Effective(true); len(rs.Allow) != 1 || rs.Allow[0] != "*" {
		t.Errorf("sandbox: %+v", rs)
	}
}

func TestSymlinkOutOfWorkspace(t *testing.T) {
	wd, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(wd, "link")); err != nil {
		t.Skip(err)
	}
	rules, _ := Compile(DefaultLocal)
	c := Call{Tool: "Read", Input: input("file_path", filepath.Join(wd, "link", "secret")), Workdir: wd, ResolveLinks: true}
	if v, _ := rules.Evaluate(c); v != VerdictAsk {
		t.Fatalf("symlink to the outside: %s", v)
	}
	c.Input = input("file_path", filepath.Join(wd, "new", "file.go"))
	if v, _ := rules.Evaluate(c); v != VerdictAllow {
		t.Fatalf("new file in the workspace: %s", v)
	}
}

func TestValidate(t *testing.T) {
	for _, bad := range []string{"", "Bash(", "Bash()", "Ba sh", "Read(**"} {
		if err := (&Config{Local: &RuleSet{Allow: []string{bad}}}).Validate(); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
	if err := (&Config{Sandbox: &RuleSet{Deny: []string{"Bash(git push *)", "mcp__*"}}}).Validate(); err != nil {
		t.Error(err)
	}
}
