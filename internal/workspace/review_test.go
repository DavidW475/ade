package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/store"
)

func (e *env) create() *store.Workspace {
	e.t.Helper()
	w, err := e.m.Create(context.Background(), e.project.ID, CreateOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func TestCollectCommitsAndFetches(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()

	// Ohne Änderungen: Branch zeigt auf den Basis-Commit, kein Commit.
	got, err := e.m.Collect(ctx, w.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit != w.BaseCommit || got.CollectedAt == nil {
		t.Fatalf("HeadCommit = %s, want %s", got.HeadCommit, w.BaseCommit)
	}

	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	writeFile(t, filepath.Join(w.Path, "README.md"), "geändert\n")
	got, err = e.m.Collect(ctx, w.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit == w.BaseCommit {
		t.Fatal("kein WIP-Commit")
	}
	if msg := gitRun(t, e.repo, "log", "-1", "--format=%s", w.Branch); msg != "ade: wip s1" {
		t.Errorf("Commit-Nachricht = %q", msg)
	}
	if head := gitRun(t, e.repo, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("Branch im Repo = %s, want %s", head, got.HeadCommit)
	}
	if files := gitRun(t, e.repo, "diff", "--name-only", w.BaseCommit, w.Branch); files != "README.md\nneu.txt" {
		t.Errorf("Dateien im Branch = %q", files)
	}
	if st := gitRun(t, w.Path, "status", "--porcelain"); st != "" {
		t.Errorf("Workspace nicht sauber: %q", st)
	}
	// Das Haupt-Repo bekommt nur den Branch, sonst nichts.
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("Arbeitsbaum des Repos verändert: %q", st)
	}
	if r := gitRun(t, w.Path, "remote"); r != "" {
		t.Errorf("Workspace hat Remotes: %q", r)
	}
}

func TestCollectIgnoresWorkspaceHooks(t *testing.T) {
	e := setup(t, "")
	w := e.create()
	marker := filepath.Join(t.TempDir(), "hook-lief")
	hook := filepath.Join(w.Path, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec // G302: Hook muss ausführbar sein
		t.Fatal(err)
	}
	gitRun(t, w.Path, "config", "core.hooksPath", filepath.Join(w.Path, ".git", "hooks"))
	writeFile(t, filepath.Join(w.Path, "x.txt"), "x\n")
	if _, err := e.m.Collect(context.Background(), w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("Hook des Workspaces wurde ausgeführt")
	}
}

func TestMergeIntoCheckedOutBase(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")

	got, err := e.m.Merge(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MergeCommit == "" || got.MergedAt == nil {
		t.Fatalf("Workspace = %#v", got)
	}
	if head := gitRun(t, e.repo, "rev-parse", "main"); head != got.MergeCommit {
		t.Errorf("main = %s, want %s", head, got.MergeCommit)
	}
	parents := strings.Fields(gitRun(t, e.repo, "log", "-1", "--format=%P", "main"))
	if len(parents) != 2 || parents[0] != w.BaseCommit || parents[1] != got.HeadCommit {
		t.Errorf("Eltern = %v", parents)
	}
	// main ist ausgecheckt: Der Arbeitsbaum zieht mit.
	if data, err := os.ReadFile(filepath.Join(e.repo, "neu.txt")); err != nil || string(data) != "neu\n" {
		t.Errorf("neu.txt im Repo: %q, %v", data, err)
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("Repo nicht sauber: %q", st)
	}
	// Ein zweites Mal gibt es nichts zu mergen.
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("zweiter Merge: %v", err)
	}
}

func TestMergeWithoutWorktree(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	gitRun(t, e.repo, "switch", "-q", "-c", "anderer")
	writeFile(t, filepath.Join(e.repo, "lokal.txt"), "ungespeichert\n")
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")

	got, err := e.m.Merge(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if head := gitRun(t, e.repo, "rev-parse", "main"); head != got.MergeCommit {
		t.Errorf("main = %s, want %s", head, got.MergeCommit)
	}
	if files := gitRun(t, e.repo, "ls-tree", "--name-only", "main"); files != "README.md\nneu.txt" {
		t.Errorf("Dateien in main = %q", files)
	}
	// Ausgecheckter Branch und ungespeicherte Änderungen bleiben unberührt.
	if b := gitRun(t, e.repo, "branch", "--show-current"); b != "anderer" {
		t.Errorf("ausgecheckt = %s", b)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "neu.txt")); !os.IsNotExist(err) {
		t.Errorf("neu.txt im Arbeitsbaum: %v", err)
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "?? lokal.txt" {
		t.Errorf("status = %q", st)
	}
}

func TestMergeConflict(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "README.md"), "aus dem Workspace\n")
	writeFile(t, filepath.Join(e.repo, "README.md"), "im Repo\n")
	gitRun(t, e.repo, "commit", "-q", "-am", "parallel")
	before := gitRun(t, e.repo, "rev-parse", "main")

	_, err := e.m.Merge(ctx, w.ID)
	var mc *MergeConflictError
	if !errors.As(err, &mc) || !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if len(mc.Files) != 1 || mc.Files[0] != "README.md" {
		t.Errorf("Konflikte = %v", mc.Files)
	}
	if after := gitRun(t, e.repo, "rev-parse", "main"); after != before {
		t.Error("main wurde trotz Konflikt verändert")
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("Repo nicht sauber: %q", st)
	}
}

func TestMergeRequiresChangesAndIdleAgent(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("Merge ohne Änderungen: %v", err)
	}
	e.m.SetBusyCheck(func(context.Context, string) error { return apperr.Conflict("beschäftigt") })
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Merge bei laufendem Agenten: %v", err)
	}
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("PR bei laufendem Agenten: %v", err)
	}
}

// fakeGH schreibt ein gh-Skript, das seine Argumente und stdin protokolliert.
func fakeGH(t *testing.T, script string) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "gh")
	writeFile(t, path, "#!/bin/sh\necho \"$@\" >> "+log+"\n"+script)
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // G302: Skript muss ausführbar sein
		t.Fatal(err)
	}
	return path, log
}

func TestPullRequest(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, e.repo, "init", "-q", "--bare", remote)
	gitRun(t, e.repo, "remote", "add", "origin", remote)
	body := filepath.Join(t.TempDir(), "body")
	gh, calls := fakeGH(t, "cat > "+body+"\necho 'Creating pull request…'\necho https://github.com/o/r/pull/7\n")
	e.m.SetGitHubCLI(gh)

	w := e.create()
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	d, err := e.m.PullRequestDraft(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Base != "main" || d.Head != w.Branch || d.RemoteURL != remote || d.Title == "" {
		t.Errorf("Entwurf = %#v", d)
	}

	got, err := e.m.PullRequest(ctx, w.ID, PRRequest{Title: "Neue Datei", Body: "Beschreibung"})
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != "https://github.com/o/r/pull/7" {
		t.Errorf("PRURL = %q", got.PRURL)
	}
	if head := gitRun(t, remote, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("Remote-Branch = %s, want %s", head, got.HeadCommit)
	}
	log, _ := os.ReadFile(calls)
	if want := "pr create --head " + w.Branch + " --base main --title Neue Datei --body-file -\n"; string(log) != want {
		t.Errorf("gh-Aufruf = %q, want %q", log, want)
	}
	if b, _ := os.ReadFile(body); string(b) != "Beschreibung" {
		t.Errorf("Body = %q", b)
	}

	// Weitere Änderungen: Push aktualisiert den PR, gh wird nicht erneut aufgerufen.
	writeFile(t, filepath.Join(w.Path, "zwei.txt"), "zwei\n")
	got, err = e.m.PullRequest(ctx, w.ID, PRRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if head := gitRun(t, remote, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("Remote-Branch nach Update = %s, want %s", head, got.HeadCommit)
	}
	if log2, _ := os.ReadFile(calls); string(log2) != string(log) {
		t.Errorf("gh erneut aufgerufen: %q", log2)
	}
}

func TestPullRequestErrors(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrInvalid) || !strings.Contains(err.Error(), "Remote") {
		t.Errorf("ohne Remote: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, e.repo, "init", "-q", "--bare", remote)
	gitRun(t, e.repo, "remote", "add", "origin", remote)
	gh, _ := fakeGH(t, "echo 'GraphQL: kaputt' >&2\nexit 1\n")
	e.m.SetGitHubCLI(gh)
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrConflict) || !strings.Contains(err.Error(), "kaputt") {
		t.Errorf("gh-Fehler: %v", err)
	}
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{Title: "a\nb"}); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("Titel mit Umbruch: %v", err)
	}
}

func TestDeleteRemovesBranch(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	before := repoState(t, e.repo)
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	if _, err := e.m.Collect(ctx, w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if !git.BranchExists(ctx, e.repo, w.Branch) {
		t.Fatal("Branch fehlt im Repo")
	}

	// Ausgecheckt im Repo: nicht löschen.
	gitRun(t, e.repo, "switch", "-q", w.Branch)
	if err := e.m.Delete(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("Delete bei ausgechecktem Branch: %v", err)
	}
	gitRun(t, e.repo, "switch", "-q", "main")

	if err := e.m.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if git.BranchExists(ctx, e.repo, w.Branch) {
		t.Error("Branch existiert noch")
	}
	if after := repoState(t, e.repo); after != before {
		t.Errorf("Repo nicht rückstandsfrei:\nvorher:\n%s\nnachher:\n%s", before, after)
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
		t.Errorf("Workspace-Verzeichnis nicht leer: %v", entries)
	}
}

func TestFileContents(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "README.md"), "hallo welt\n")
	writeFile(t, filepath.Join(w.Path, "bin.dat"), "a\x00b")
	gitRun(t, w.Path, "mv", "README.md", "LIESMICH.md")
	if err := os.Symlink("/etc/passwd", filepath.Join(w.Path, "link")); err != nil {
		t.Fatal(err)
	}

	fc, err := e.m.File(ctx, w.ID, "LIESMICH.md", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Original.Text != "hallo\n" || fc.Modified.Text != "hallo welt\n" || fc.OldPath != "README.md" {
		t.Errorf("umbenannt = %#v", fc)
	}
	fc, err = e.m.File(ctx, w.ID, "bin.dat", "")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Original.Exists || !fc.Modified.Exists || !fc.Modified.Binary {
		t.Errorf("binär = %#v", fc)
	}
	// Symlinks werden nicht verfolgt: Inhalt ist das Linkziel.
	fc, err = e.m.File(ctx, w.ID, "link", "")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Modified.Text != "/etc/passwd" {
		t.Errorf("Symlink = %#v", fc.Modified)
	}
	fc, err = e.m.File(ctx, w.ID, "README.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if !fc.Original.Exists || fc.Modified.Exists {
		t.Errorf("gelöscht = %#v", fc)
	}
	// Index des Workspaces bleibt unverändert.
	if st := gitRun(t, w.Path, "status", "--porcelain"); !strings.Contains(st, "?? bin.dat") {
		t.Errorf("status = %q", st)
	}
	for _, p := range []string{"", "../x", "/etc/passwd", "a/../../b", "a//b"} {
		if _, err := e.m.File(ctx, w.ID, p, ""); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("Pfad %q: %v", p, err)
		}
	}
}

func TestCloneAcrossFilesystems(t *testing.T) {
	old := git.SameDevice
	git.SameDevice = func(string, string) bool { return false }
	t.Cleanup(func() { git.SameDevice = old })

	e := setup(t, "")
	w := e.create()
	// Ohne Hardlinks hat jede Objektdatei genau einen Link.
	objects := filepath.Join(w.Path, ".git", "objects")
	n := 0
	err := filepath.WalkDir(objects, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(filepath.Dir(p)) == "info" {
			return err
		}
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if links := st.Sys().(*syscall.Stat_t).Nlink; links != 1 {
			t.Errorf("%s hat %d Links", p, links)
		}
		n++
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("Objekte: %d, %v", n, err)
	}
}

func TestSensitive(t *testing.T) {
	d := &git.Diff{Files: []git.FileChange{
		{Path: "README.md"},
		{Path: ".github/workflows/ci.yml"},
		{Path: "docs/x.md", OldPath: ".ade/project.yaml", Status: "renamed"},
		{Path: ".github/actions/a/action.yml"},
		{Path: ".github/CODEOWNERS"},
		{Path: ".adex/foo"},
		{Path: ".ade"},
	}}
	got := Sensitive(d)
	want := []string{".ade", ".ade/project.yaml", ".github/actions/a/action.yml", ".github/workflows/ci.yml"}
	if !slices.Equal(got, want) {
		t.Fatalf("Sensitive = %v, erwartet %v", got, want)
	}
}
