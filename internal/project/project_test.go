package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

	// Unterverzeichnis registriert das ganze Repo.
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
		t.Errorf("doppelt: err = %v", err)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if _, err := s.Find(ctx, v.Name); err != nil {
		t.Errorf("Find per Name: %v", err)
	}
	if err := s.Remove(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAddRejectsNonGit(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	for _, p := range []string{t.TempDir(), "relativ/pfad", "/gibt/es/nicht", ""} {
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

	// Ungültige Datei: Projekt bleibt lesbar, Fehler wird gemeldet.
	writeConfig(t, repo, "adapter: gibtsnicht\n")
	v, err = s.Get(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.ConfigError, "adapter") {
		t.Errorf("ConfigError = %q", v.ConfigError)
	}

	// base_branch aus der Datei gewinnt.
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
		"unbekannt: 1\n":          "unbekannt",
		"base_branch: \"a b\"\n":  "base_branch",
		"adapter: vim\n":          "adapter",
		"setup: [\"\"]\n":         "setup[0]",
		"setup: \"kein array\"\n": "",
		"name: [1, 2\n":           "",
	}
	for c, want := range invalid {
		_, err := ParseConfig(ctx, []byte(c))
		var ce *ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want ConfigError mit %q", c, err, want)
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
