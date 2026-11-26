package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NewTestRepo legt ein Repo mit einem Commit auf main an.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "README.md", "hallo\n")
	write(t, dir, "src/a.txt", "eins\nzwei\ndrei\n")
	write(t, dir, "alt.txt", "umbenennen\nbitte\nnicht\nverlieren\n")
	write(t, dir, "weg.txt", "löschen\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectAndDefaultBranch(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	info, err := Inspect(ctx, filepath.Join(repo, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Bare || info.TopLevel != repo {
		t.Errorf("Inspect = %#v, want TopLevel %s", info, repo)
	}
	if b, err := DefaultBranch(ctx, repo); err != nil || b != "main" {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}

	run(t, repo, "switch", "-q", "-c", "feature")
	if b, _ := DefaultBranch(ctx, repo); b != "feature" {
		t.Errorf("DefaultBranch auf feature = %q", b)
	}
	run(t, repo, "switch", "-q", "--detach", "HEAD")
	if b, _ := DefaultBranch(ctx, repo); b != "main" {
		t.Errorf("DefaultBranch losgelöst = %q, want main", b)
	}

	if _, err := Inspect(ctx, t.TempDir()); err == nil {
		t.Error("Inspect auf Nicht-Repo: Fehler erwartet")
	}

	bare := t.TempDir()
	run(t, bare, "init", "-q", "--bare")
	if info, err := Inspect(ctx, bare); err != nil || !info.Bare {
		t.Errorf("Inspect bare = %#v, %v", info, err)
	}
}

func TestValidBranchName(t *testing.T) {
	ctx := context.Background()
	for name, want := range map[string]bool{
		"main": true, "feature/x": true, "": false, "-x": false, "a..b": false, "a b": false, "a~1": false,
	} {
		if got := ValidBranchName(ctx, name); got != want {
			t.Errorf("ValidBranchName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWorktreeDiff(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	base, err := ResolveCommit(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}

	// Leer: keine Änderungen.
	d, err := WorktreeDiff(ctx, repo, base, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 0 || d.Head != base {
		t.Fatalf("leerer Diff = %#v", d)
	}

	// Committete Änderung.
	write(t, repo, "src/a.txt", "eins\nZWEI\ndrei\nvier\n")
	run(t, repo, "commit", "-q", "-am", "ändern")
	// Ungestagte Änderung, neue Datei, Löschung, Umbenennung, Binärdatei, ignorierte Datei.
	write(t, repo, "README.md", "hallo welt\n")
	write(t, repo, "neu.txt", "neu\n")
	write(t, repo, ".gitignore", "*.log\n")
	write(t, repo, "debug.log", "ignoriert\n")
	if err := os.Remove(filepath.Join(repo, "weg.txt")); err != nil {
		t.Fatal(err)
	}
	run(t, repo, "mv", "alt.txt", "neu-name.txt")
	if err := os.WriteFile(filepath.Join(repo, "bild.bin"), []byte{0, 1, 2, 0, 255}, 0o644); err != nil {
		t.Fatal(err)
	}

	indexBefore, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))

	d, err = WorktreeDiff(ctx, repo, base, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]FileChange{}
	for _, f := range d.Files {
		got[f.Path] = f
	}
	want := map[string]string{
		".gitignore":   "added",
		"README.md":    "modified",
		"bild.bin":     "added",
		"neu-name.txt": "renamed",
		"neu.txt":      "added",
		"src/a.txt":    "modified",
		"weg.txt":      "deleted",
	}
	if len(got) != len(want) {
		t.Fatalf("Dateien = %v", keys(got))
	}
	for p, s := range want {
		if got[p].Status != s {
			t.Errorf("%s: Status = %q, want %q", p, got[p].Status, s)
		}
		if got[p].Patch == "" || !strings.HasPrefix(got[p].Patch, "diff --git ") {
			t.Errorf("%s: Patch fehlt oder falsch: %q", p, got[p].Patch)
		}
	}
	if f := got["src/a.txt"]; f.Additions != 2 || f.Deletions != 1 {
		t.Errorf("src/a.txt: +%d -%d, want +2 -1", f.Additions, f.Deletions)
	}
	if f := got["neu-name.txt"]; f.OldPath != "alt.txt" {
		t.Errorf("Umbenennung: OldPath = %q", f.OldPath)
	}
	if !got["bild.bin"].Binary {
		t.Error("bild.bin nicht als binär erkannt")
	}
	if !strings.Contains(got["README.md"].Patch, "+hallo welt") {
		t.Errorf("README-Patch = %q", got["README.md"].Patch)
	}

	indexAfter, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if string(indexBefore) != string(indexAfter) {
		t.Error("WorktreeDiff hat den Index des Repos verändert")
	}

	// Ohne Patches.
	d, err = WorktreeDiff(ctx, repo, base, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.Files {
		if f.Patch != "" {
			t.Errorf("%s: Patch trotz withPatch=false", f.Path)
		}
	}
	if d.Additions == 0 {
		t.Error("Summe der Zeilen fehlt")
	}
}

func TestParseFormats(t *testing.T) {
	files, err := parseNameStatus([]byte("M\x00a.txt\x00R087\x00alt\x00neu\x00A\x00b\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[1].Status != "renamed" || files[1].OldPath != "alt" || files[1].Path != "neu" {
		t.Fatalf("files = %#v", files)
	}
	if err := applyNumStat(files, []byte("1\t2\ta.txt\x003\t0\t\x00alt\x00neu\x00-\t-\tb\x00")); err != nil {
		t.Fatal(err)
	}
	if files[0].Additions != 1 || files[1].Additions != 3 || !files[2].Binary {
		t.Fatalf("files = %#v", files)
	}
	if _, err := parseNameStatus([]byte("R100\x00nur-einer\x00")); err == nil {
		t.Error("unvollständige Umbenennung: Fehler erwartet")
	}
}

func keys(m map[string]FileChange) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
