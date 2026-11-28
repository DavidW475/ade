package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/store"
)

func newRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func newService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewService(st)
}

func writeConfig(t *testing.T, repo, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".ade"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ConfigFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAddAndList(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	repo := newRepo(t, "trunk")
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A subdirectory registers the whole repository.
	v, err := s.Add(ctx, filepath.Join(repo, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(repo)
	if v.RepoPath != resolved || v.Name != filepath.Base(repo) || v.DefaultBranch != "trunk" || v.BaseBranch != "trunk" {
		t.Errorf("View = %#v", v)
	}
	if len(v.ID) != 8 {
		t.Errorf("ID = %q", v.ID)
	}

	if _, err := s.Add(ctx, repo); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("duplicate: err = %v", err)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if _, err := s.Find(ctx, v.Name); err != nil {
		t.Errorf("Find by name: %v", err)
	}
	if err := s.Remove(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAddRejectsNonGit(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	for _, p := range []string{t.TempDir(), "relative/path", "/does/not/exist", ""} {
		if _, err := s.Add(ctx, p); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", p, err)
		}
	}
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := s.Add(ctx, bare); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("bare: err = %v", err)
	}
}

func TestProjectConfig(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	repo := newRepo(t, "main")
	writeConfig(t, repo, "name: Demo\nadapter: pty\nsetup:\n  - npm ci\n")

	v, err := s.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "Demo" || v.Config.Adapter != "pty" || len(v.Config.Setup) != 1 || v.ConfigError != "" {
		t.Errorf("View = %#v, Config = %#v", v, v.Config)
	}

	// Invalid file: the project stays readable, the error is reported.
	writeConfig(t, repo, "adapter: doesnotexist\n")
	v, err = s.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.ConfigError, "adapter") {
		t.Errorf("ConfigError = %q", v.ConfigError)
	}

	// base_branch from the file wins.
	writeConfig(t, repo, "base_branch: release\n")
	v, _ = s.Get(ctx, v.ID)
	if v.BaseBranch != "release" || v.DefaultBranch != "main" {
		t.Errorf("BaseBranch = %q, DefaultBranch = %q", v.BaseBranch, v.DefaultBranch)
	}
}

func TestParseConfig(t *testing.T) {
	ctx := context.Background()
	valid := []string{"", "name: x\n", "base_branch: feature/a\nsetup: [\"make\"]\n"}
	for _, c := range valid {
		if _, err := ParseConfig(ctx, []byte(c)); err != nil {
			t.Errorf("%q: %v", c, err)
		}
	}
	invalid := map[string]string{
		"unknown: 1\n":              "unknown",
		"base_branch: \"a b\"\n":    "base_branch",
		"adapter: vim\n":            "adapter",
		"setup: [\"\"]\n":           "setup[0]",
		"setup: \"not an array\"\n": "",
		"name: [1, 2\n":             "",
	}
	for c, want := range invalid {
		_, err := ParseConfig(ctx, []byte(c))
		var ce *ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want ConfigError with %q", c, err, want)
		}
	}
}

func TestMntWarning(t *testing.T) {
	v := (&Service{}).view(&store.Project{RepoPath: "/mnt/c/Users/x/repo"}, &Config{}, nil)
	if len(v.Warnings) != 1 {
		t.Errorf("Warnings = %v", v.Warnings)
	}
}

func TestExampleProjectConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", ".ade", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(context.Background(), data)
	if err != nil || len(cfg.Setup) == 0 {
		t.Fatalf("examples/.ade/project.yaml: %v, %#v", err, cfg)
	}
}

func TestBudgetConfig(t *testing.T) {
	cfg, err := ParseConfig(context.Background(), []byte("budget:\n  session_usd: 2.5\n  monthly_usd: 50\n"))
	if err != nil || cfg.SessionBudget() != 2.5 || cfg.MonthlyBudget() != 50 {
		t.Fatalf("Budget = %+v, %v", cfg.Budget, err)
	}
	var none *Config
	if none.SessionBudget() != 0 || (&Config{}).MonthlyBudget() != 0 {
		t.Error("without budget")
	}
	for _, bad := range []string{"budget:\n  session_usd: -1\n", "budget:\n  monthly_usd: 1e9\n", "budget:\n  daily_usd: 1\n"} {
		if _, err := ParseConfig(context.Background(), []byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestApprovalsConfig(t *testing.T) {
	cfg, err := ParseConfig(context.Background(), []byte("approvals:\n  local:\n    allow: [\"Bash(make *)\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rs := cfg.Approvals.Effective(false); len(rs.Allow) != 1 || rs.Allow[0] != "Bash(make *)" {
		t.Fatalf("local: %+v", rs)
	}
	if rs := cfg.Approvals.Effective(true); len(rs.Allow) != 1 || rs.Allow[0] != "*" {
		t.Fatalf("sandbox: %+v", rs)
	}
	for _, bad := range []string{"approvals:\n  local:\n    allow: [\"Bash(\"]\n", "approvals:\n  remote: {}\n"} {
		if _, err := ParseConfig(context.Background(), []byte(bad)); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
}

func TestTriggersConfig(t *testing.T) {
	ctx := context.Background()
	cfg, err := ParseConfig(ctx, []byte(`triggers:
  - name: features
    factory: feature
    github_issues: { label: "ade:feature", authors: [octo-cat, "dependabot[bot]"], interval: 10m, comment: false }
  - name: nightly
    factory: deps
    sandbox: false
    inputs: { task: "Update dependencies" }
    schedule: { cron: "0 3 * * 1-5" }
`))
	if err != nil {
		t.Fatal(err)
	}
	g, s := cfg.Triggers[0], cfg.Triggers[1]
	if g.Kind() != "github_issues" || g.GitHubIssues.IssueInterval() != 10*time.Minute || g.GitHubIssues.Comments() || !g.SandboxOn() {
		t.Fatalf("issue trigger %+v", g)
	}
	if s.Kind() != "schedule" || s.SandboxOn() || !s.Schedule.CatchUpOn() || s.Inputs["task"] == "" {
		t.Fatalf("Zeitplan %+v", s)
	}
	for bad, want := range map[string]string{
		"triggers:\n  - { name: X, factory: f, schedule: { cron: '@daily' } }\n":                                                            "name",
		"triggers:\n  - { name: a, factory: f }\n":                                                                                          "exactly one",
		"triggers:\n  - { name: a, factory: f, schedule: { cron: '1 2 3' } }\n":                                                             "5 fields",
		"triggers:\n  - { name: a, factory: f, github_issues: { label: 'a,b' } }\n":                                                         "without a comma",
		"triggers:\n  - { name: a, factory: f, github_issues: { label: x, interval: 10s } }\n":                                              "interval",
		"triggers:\n  - { name: a, factory: f, github_issues: { label: x }, inputs: { task: y } }\n":                                        "from the issue",
		"triggers:\n  - { name: a, factory: f, github_issues: { label: x, authors: ['a b'] } }\n":                                           "GitHub name",
		"triggers:\n  - { name: a, factory: f, schedule: { cron: '@daily' } }\n  - { name: a, factory: g, schedule: { cron: '@daily' } }\n": "already exists",
	} {
		if _, err := ParseConfig(ctx, []byte(bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", bad, err, want)
		}
	}
}
