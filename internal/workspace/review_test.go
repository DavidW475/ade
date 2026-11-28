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

	// Without changes: the branch points to the base commit, no commit.
	got, err := e.m.Collect(ctx, w.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit != w.BaseCommit || got.CollectedAt == nil {
		t.Fatalf("HeadCommit = %s, want %s", got.HeadCommit, w.BaseCommit)
	}

	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")
	writeFile(t, filepath.Join(w.Path, "README.md"), "changed\n")
	got, err = e.m.Collect(ctx, w.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadCommit == w.BaseCommit {
		t.Fatal("no WIP commit")
	}
	if msg := gitRun(t, e.repo, "log", "-1", "--format=%s", w.Branch); msg != "ade: wip s1" {
		t.Errorf("commit message = %q", msg)
	}
	if head := gitRun(t, e.repo, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("branch in the repository = %s, want %s", head, got.HeadCommit)
	}
	if files := gitRun(t, e.repo, "diff", "--name-only", w.BaseCommit, w.Branch); files != "README.md\nnew.txt" {
		t.Errorf("files in the branch = %q", files)
	}
	if st := gitRun(t, w.Path, "status", "--porcelain"); st != "" {
		t.Errorf("workspace not clean: %q", st)
	}
	// The main repository only gets the branch, nothing else.
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("working tree of the repository changed: %q", st)
	}
	if r := gitRun(t, w.Path, "remote"); r != "" {
		t.Errorf("workspace has remotes: %q", r)
	}
}

func TestCollectIgnoresWorkspaceHooks(t *testing.T) {
	e := setup(t, "")
	w := e.create()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(w.Path, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\ntouch "+marker+"\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec // G302: the hook must be executable
		t.Fatal(err)
	}
	gitRun(t, w.Path, "config", "core.hooksPath", filepath.Join(w.Path, ".git", "hooks"))
	writeFile(t, filepath.Join(w.Path, "x.txt"), "x\n")
	if _, err := e.m.Collect(context.Background(), w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the workspace's hook was run")
	}
}

func TestMergeIntoCheckedOutBase(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")

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
	// main is checked out: the working tree follows.
	if data, err := os.ReadFile(filepath.Join(e.repo, "new.txt")); err != nil || string(data) != "new\n" {
		t.Errorf("new.txt in the repository: %q, %v", data, err)
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("repository not clean: %q", st)
	}
	// A second time there is nothing to merge.
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("second merge: %v", err)
	}
}

func TestMergeWithoutWorktree(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	gitRun(t, e.repo, "switch", "-q", "-c", "other")
	writeFile(t, filepath.Join(e.repo, "local.txt"), "unsaved\n")
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")

	got, err := e.m.Merge(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if head := gitRun(t, e.repo, "rev-parse", "main"); head != got.MergeCommit {
		t.Errorf("main = %s, want %s", head, got.MergeCommit)
	}
	if files := gitRun(t, e.repo, "ls-tree", "--name-only", "main"); files != "README.md\nnew.txt" {
		t.Errorf("files in main = %q", files)
	}
	// The checked-out branch and unsaved changes stay untouched.
	if b := gitRun(t, e.repo, "branch", "--show-current"); b != "other" {
		t.Errorf("ausgecheckt = %s", b)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt in the working tree: %v", err)
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "?? local.txt" {
		t.Errorf("status = %q", st)
	}
}

func TestMergeConflict(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "README.md"), "from the workspace\n")
	writeFile(t, filepath.Join(e.repo, "README.md"), "in the repo\n")
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
		t.Error("main was changed despite the conflict")
	}
	if st := gitRun(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("repository not clean: %q", st)
	}
}

func TestMergeRequiresChangesAndIdleAgent(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("merge without changes: %v", err)
	}
	e.m.SetBusyCheck(func(context.Context, string) error { return apperr.Conflict("busy") })
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")
	if _, err := e.m.Merge(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("merge while an agent is running: %v", err)
	}
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("PR while an agent is running: %v", err)
	}
}

// fakeGH writes a gh script that logs its arguments and stdin.
func fakeGH(t *testing.T, script string) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "gh")
	writeFile(t, path, "#!/bin/sh\necho \"$@\" >> "+log+"\n"+script)
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // G302: the script must be executable
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
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")
	d, err := e.m.PullRequestDraft(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Base != "main" || d.Head != w.Branch || d.RemoteURL != remote || d.Title == "" {
		t.Errorf("Entwurf = %#v", d)
	}

	got, err := e.m.PullRequest(ctx, w.ID, PRRequest{Title: "New file", Body: "Description"})
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != "https://github.com/o/r/pull/7" {
		t.Errorf("PRURL = %q", got.PRURL)
	}
	if head := gitRun(t, remote, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("remote branch = %s, want %s", head, got.HeadCommit)
	}
	log, _ := os.ReadFile(calls)
	if want := "pr create --head " + w.Branch + " --base main --title New file --body-file -\n"; string(log) != want {
		t.Errorf("gh call = %q, want %q", log, want)
	}
	if b, _ := os.ReadFile(body); string(b) != "Description" {
		t.Errorf("Body = %q", b)
	}

	// More changes: the push updates the PR, gh is not called again.
	writeFile(t, filepath.Join(w.Path, "two.txt"), "two\n")
	got, err = e.m.PullRequest(ctx, w.ID, PRRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if head := gitRun(t, remote, "rev-parse", w.Branch); head != got.HeadCommit {
		t.Errorf("remote branch after update = %s, want %s", head, got.HeadCommit)
	}
	if log2, _ := os.ReadFile(calls); string(log2) != string(log) {
		t.Errorf("gh called again: %q", log2)
	}
}

func TestPullRequestErrors(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrInvalid) || !strings.Contains(err.Error(), "remote") {
		t.Errorf("without remote: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, e.repo, "init", "-q", "--bare", remote)
	gitRun(t, e.repo, "remote", "add", "origin", remote)
	gh, _ := fakeGH(t, "echo 'GraphQL: broken' >&2\nexit 1\n")
	e.m.SetGitHubCLI(gh)
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{}); !errors.Is(err, apperr.ErrConflict) || !strings.Contains(err.Error(), "broken") {
		t.Errorf("gh error: %v", err)
	}
	if _, err := e.m.PullRequest(ctx, w.ID, PRRequest{Title: "a\nb"}); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("title with a line break: %v", err)
	}
}

func TestDeleteRemovesBranch(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	before := repoState(t, e.repo)
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "new.txt"), "new\n")
	if _, err := e.m.Collect(ctx, w.ID, ""); err != nil {
		t.Fatal(err)
	}
	if !git.BranchExists(ctx, e.repo, w.Branch) {
		t.Fatal("branch missing in the repository")
	}

	// Checked out in the repository: do not delete.
	gitRun(t, e.repo, "switch", "-q", w.Branch)
	if err := e.m.Delete(ctx, w.ID); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("Delete with a checked-out branch: %v", err)
	}
	gitRun(t, e.repo, "switch", "-q", "main")

	if err := e.m.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if git.BranchExists(ctx, e.repo, w.Branch) {
		t.Error("branch still exists")
	}
	if after := repoState(t, e.repo); after != before {
		t.Errorf("repository not free of leftovers:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) != 0 {
		t.Errorf("workspace directory not empty: %v", entries)
	}
}

func TestFileContents(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := e.create()
	writeFile(t, filepath.Join(w.Path, "README.md"), "hello world\n")
	writeFile(t, filepath.Join(w.Path, "bin.dat"), "a\x00b")
	gitRun(t, w.Path, "mv", "README.md", "LIESMICH.md")
	if err := os.Symlink("/etc/passwd", filepath.Join(w.Path, "link")); err != nil {
		t.Fatal(err)
	}

	fc, err := e.m.File(ctx, w.ID, "LIESMICH.md", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Original.Text != "hello\n" || fc.Modified.Text != "hello world\n" || fc.OldPath != "README.md" {
		t.Errorf("umbenannt = %#v", fc)
	}
	fc, err = e.m.File(ctx, w.ID, "bin.dat", "")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Original.Exists || !fc.Modified.Exists || !fc.Modified.Binary {
		t.Errorf("binary = %#v", fc)
	}
	// Symlinks are not followed: the content is the link target.
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
		t.Errorf("deleted = %#v", fc)
	}
	// The workspace's index stays unchanged.
	if st := gitRun(t, w.Path, "status", "--porcelain"); !strings.Contains(st, "?? bin.dat") {
		t.Errorf("status = %q", st)
	}
	for _, p := range []string{"", "../x", "/etc/passwd", "a/../../b", "a//b"} {
		if _, err := e.m.File(ctx, w.ID, p, ""); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("path %q: %v", p, err)
		}
	}
}

func TestCloneAcrossFilesystems(t *testing.T) {
	old := git.SameDevice
	git.SameDevice = func(string, string) bool { return false }
	t.Cleanup(func() { git.SameDevice = old })

	e := setup(t, "")
	w := e.create()
	// Without hardlinks every object file has exactly one link.
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
			t.Errorf("%s has %d links", p, links)
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
		t.Fatalf("Sensitive = %v, want %v", got, want)
	}
}
