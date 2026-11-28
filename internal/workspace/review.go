package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/store"
)

// Review and delivery (architecture.md 4.2): the daemon commits open changes in the workspace,
// fetches the branch into the main repository and merges or publishes it from there. The
// agent itself has neither access to the main repository nor push rights.

// Remote is the main repository's remote that pull requests are pushed to.
const Remote = "origin"

// Time limits for network steps.
const (
	PushTimeout = 3 * time.Minute
	GHTimeout   = 2 * time.Minute
)

// RoleFeedback marks sessions that came from “Give feedback”.
const RoleFeedback = "feedback"

// SetBusyCheck registers a check that runs before merge and PR (e.g. "is an agent working
// right now?"). If it returns an error, the action is aborted.
func (m *Manager) SetBusyCheck(fn func(ctx context.Context, id string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busyCheck = fn
}

// SetGitHubCLI sets the path to the gh binary (default: gh in PATH).
func (m *Manager) SetGitHubCLI(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gh = path
}

func (m *Manager) checkBusy(ctx context.Context, id string) error {
	m.mu.Lock()
	fn := m.busyCheck
	m.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, id)
}

// lockWorkspace serializes commit, fetch, merge and push of a workspace.
func (m *Manager) lockWorkspace(id string) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &wsLock{ch: make(chan struct{}, 1)}
		m.locks[id] = l
	}
	l.refs++
	m.mu.Unlock()
	l.ch <- struct{}{}
	return func() {
		<-l.ch
		m.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.locks, id)
		}
		m.mu.Unlock()
	}
}

type wsLock struct {
	ch   chan struct{}
	refs int
}

// Collect commits open changes in the workspace (“ade: wip <label>”) and fetches the branch
// ade/<id> into the main repository. label is e.g. the ID of the ended session.
func (m *Manager) Collect(ctx context.Context, id, label string) (*store.Workspace, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	return m.collect(ctx, id, label)
}

func (m *Manager) collect(ctx context.Context, id, label string) (*store.Workspace, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("workspace %s has no working tree (status %s)", id, w.Status)
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	if label == "" {
		label = w.ID
	}
	wt, err := m.worktree(ctx, w, true)
	if err != nil {
		return nil, err
	}
	committed, err := git.CommitAll(ctx, *wt, "ade: wip "+label)
	if err != nil {
		return nil, fmt.Errorf("WIP commit: %w", err)
	}
	if err := git.FetchBranch(ctx, p.RepoPath, w.Path, w.Branch); err != nil {
		return nil, fmt.Errorf("fetch branch into the project repository: %w", err)
	}
	head, err := git.ResolveCommit(ctx, p.RepoPath, "refs/heads/"+w.Branch)
	if err != nil {
		return nil, err
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{HeadCommit: &head}); err != nil {
		return nil, err
	}
	if committed || head != w.HeadCommit {
		m.log.Info("workspace collected", "id", id, "commit", head, "wip_commit", committed)
	}
	return m.store.GetWorkspace(ctx, id)
}

// MergeConflictError reports conflicts when merging into the base branch.
type MergeConflictError struct {
	Branch string
	Files  []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge into %s has conflicts in %d file(s): %s – give feedback or resolve them in the workspace",
		e.Branch, len(e.Files), strings.Join(e.Files, ", "))
}

func (e *MergeConflictError) Unwrap() error { return apperr.ErrConflict }

// Merge takes the workspace's branch into the main repository's base branch with a merge
// commit. If the base branch is checked out there, git merge runs in its working tree,
// otherwise the merge is computed without a working tree. Conflicts change nothing.
func (m *Manager) Merge(ctx context.Context, id string) (*store.Workspace, error) {
	if err := m.checkBusy(ctx, id); err != nil {
		return nil, err
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	w, err := m.collect(ctx, id, "")
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	repo := p.RepoPath
	if w.HeadCommit == w.BaseCommit {
		return nil, apperr.Invalid("no changes to merge")
	}
	base, err := git.ResolveCommit(ctx, repo, "refs/heads/"+w.BaseBranch)
	if err != nil {
		return nil, apperr.Conflict("base branch %s no longer exists in the project repository", w.BaseBranch)
	}
	if done, err := git.IsAncestor(ctx, repo, w.HeadCommit, base); err != nil {
		return nil, err
	} else if done {
		return nil, apperr.Conflict("%s is already fully contained in %s", w.Branch, w.BaseBranch)
	}
	tree, conflicts, err := git.MergeTree(ctx, repo, base, w.HeadCommit)
	if err != nil {
		return nil, fmt.Errorf("compute merge: %w", err)
	}
	if len(conflicts) > 0 {
		return nil, &MergeConflictError{Branch: w.BaseBranch, Files: conflicts}
	}

	msg := fmt.Sprintf("Merge %s: %s", w.Branch, m.title(ctx, w))
	wt, err := git.CheckedOutIn(ctx, repo, w.BaseBranch)
	if err != nil {
		return nil, err
	}
	var commit string
	if wt != "" {
		commit, err = git.MergeInWorktree(ctx, wt, "refs/heads/"+w.Branch, msg)
		if err != nil {
			return nil, apperr.Conflict("merge in the working tree %s failed (changes there? Then commit or stash them first): %v", wt, err)
		}
	} else {
		commit, err = git.MergeCommit(ctx, repo, w.BaseBranch, base, w.HeadCommit, tree, msg)
		if err != nil {
			return nil, fmt.Errorf("create merge commit: %w", err)
		}
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{MergeCommit: &commit}); err != nil {
		return nil, err
	}
	m.log.Info("workspace merged", "id", id, "base", w.BaseBranch, "commit", commit, "worktree", wt)
	return m.store.GetWorkspace(ctx, id)
}

// PRDraft is the suggestion for a pull request.
type PRDraft struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Remote string `json:"remote"`
	// RemoteURL is empty if the project repository has no remote.
	RemoteURL string `json:"remote_url,omitempty"`
	// URL is the existing PR (then a push only updates it).
	URL string `json:"url,omitempty"`
}

// PullRequestDraft creates title and description for a PR from task, feedback and diff.
func (m *Manager) PullRequestDraft(ctx context.Context, id string) (*PRDraft, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	d := &PRDraft{Title: m.title(ctx, w), Base: w.BaseBranch, Head: w.Branch, Remote: Remote, URL: w.PRURL}
	d.RemoteURL, _ = git.RemoteURL(ctx, p.RepoPath, Remote)

	var b strings.Builder
	sessions, _ := m.store.ListSessions(ctx, store.SessionFilter{WorkspaceID: id, Limit: 1000})
	var task string
	var feedback []string
	var cost float64
	for i := len(sessions) - 1; i >= 0; i-- { // oldest first
		s := sessions[i]
		cost += s.CostUSD
		switch {
		case s.Role == RoleFeedback && s.Task != "":
			feedback = append(feedback, s.Task)
		case task == "" && s.Task != "":
			task = s.Task
		}
	}
	if task != "" {
		b.WriteString("## Task\n\n" + task + "\n\n")
	}
	if len(feedback) > 0 {
		b.WriteString("## Feedback\n\n")
		for _, f := range feedback {
			b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(f), "\n", "\n  ") + "\n")
		}
		b.WriteString("\n")
	}
	if diff, err := m.Diff(ctx, id, false); err == nil && len(diff.Files) > 0 {
		fmt.Fprintf(&b, "## Changes\n\n%d file(s), +%d −%d\n\n", len(diff.Files), diff.Additions, diff.Deletions)
		for i, f := range diff.Files {
			if i == 50 {
				fmt.Fprintf(&b, "- … and %d more\n", len(diff.Files)-50)
				break
			}
			fmt.Fprintf(&b, "- `%s` (%s)\n", f.Path, statusLabel(f.Status))
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Created with [ADE](https://github.com/DavidW475/ade) · workspace `%s`", w.ID)
	if len(sessions) > 0 {
		fmt.Fprintf(&b, " · %d agent session(s)", len(sessions))
	}
	if cost > 0 {
		fmt.Fprintf(&b, " · costs $%.2f", cost)
	}
	b.WriteString("\n")
	d.Body = b.String()
	return d, nil
}

var statusLabels = map[string]string{
	"added": "new", "modified": "modified", "deleted": "deleted", "renamed": "renamed",
	"copied": "copied", "type_changed": "type changed", "unmerged": "conflict",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// title is the first line of the first task in the workspace (at most 72 characters).
func (m *Manager) title(ctx context.Context, w *store.Workspace) string {
	sessions, _ := m.store.ListSessions(ctx, store.SessionFilter{WorkspaceID: w.ID, Limit: 1000})
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		if s.Role == RoleFeedback || strings.TrimSpace(s.Task) == "" {
			continue
		}
		t := strings.TrimSpace(s.Task)
		if j := strings.IndexByte(t, '\n'); j >= 0 {
			t = strings.TrimSpace(t[:j])
		}
		if utf8.RuneCountInString(t) > 72 {
			r := []rune(t)
			t = strings.TrimSpace(string(r[:71])) + "…"
		}
		return t
	}
	return "Changes from " + w.Branch
}

// PRRequest is the body of POST /api/workspaces/{id}/pull-request.
type PRRequest struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// PullRequest pushes the branch from the host to the remote and creates a pull request with gh.
// If one already exists, the push only updates it.
func (m *Manager) PullRequest(ctx context.Context, id string, req PRRequest) (*store.Workspace, error) {
	if err := m.checkBusy(ctx, id); err != nil {
		return nil, err
	}
	if len(req.Title) > 256 || len(req.Body) > 64<<10 {
		return nil, apperr.Invalid("title or description too long")
	}
	if strings.ContainsRune(req.Title, '\n') {
		return nil, apperr.Invalid("the title must not contain a line break")
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	w, err := m.collect(ctx, id, "")
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	if _, err := git.RemoteURL(ctx, p.RepoPath, Remote); err != nil {
		return nil, apperr.Invalid("the project repository has no remote %q – PRs need a GitHub remote", Remote)
	}
	if w.HeadCommit == w.BaseCommit {
		return nil, apperr.Invalid("no changes for a pull request")
	}
	m.mu.Lock()
	gh := m.gh
	m.mu.Unlock()
	if w.PRURL == "" {
		if _, err := exec.LookPath(gh); err != nil {
			return nil, apperr.Invalid("GitHub CLI (gh) not found – install it and run `gh auth login`")
		}
	}

	pctx, cancel := context.WithTimeout(ctx, PushTimeout)
	err = git.Push(pctx, p.RepoPath, Remote, w.Branch)
	cancel()
	if err != nil {
		return nil, apperr.Conflict("push to %s failed: %v", Remote, err)
	}
	m.log.Info("branch pushed", "id", id, "remote", Remote, "branch", w.Branch)
	if w.PRURL != "" {
		return w, nil
	}

	if req.Title == "" || req.Body == "" {
		d, err := m.PullRequestDraft(ctx, id)
		if err != nil {
			return nil, err
		}
		if req.Title == "" {
			req.Title = d.Title
		}
		if req.Body == "" {
			req.Body = d.Body
		}
	}
	url, err := m.createPR(ctx, gh, p.RepoPath, w, req)
	if err != nil {
		return nil, err
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{PRURL: &url}); err != nil {
		return nil, err
	}
	m.log.Info("pull request created", "id", id, "url", url)
	return m.store.GetWorkspace(ctx, id)
}

func (m *Manager) createPR(ctx context.Context, gh, repo string, w *store.Workspace, req PRRequest) (string, error) {
	args := []string{"pr", "create", "--head", w.Branch, "--base", w.BaseBranch, "--title", req.Title, "--body-file", "-"}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, stderr, err := runGH(ctx, gh, repo, req.Body, args...)
	if err != nil && strings.Contains(stderr, "already exists") {
		out, stderr, err = runGH(ctx, gh, repo, "", "pr", "view", w.Branch, "--json", "url", "--jq", ".url")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", apperr.Conflict("gh pr create failed: %s", msg)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l, nil
		}
	}
	return "", fmt.Errorf("gh returned no PR URL: %q", out)
}

func runGH(ctx context.Context, gh, dir, stdin string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, GHTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, args...) //nolint:gosec // G204: fixed arguments, values from the logged-in user
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// FileSide is one side of a changed file.
type FileSide struct {
	Exists   bool   `json:"exists"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
	Text     string `json:"text,omitempty"`
}

// FileContents are the base and current version of a file for the diff editor.
type FileContents struct {
	Path     string   `json:"path"`
	OldPath  string   `json:"old_path,omitempty"`
	Original FileSide `json:"original"`
	Modified FileSide `json:"modified"`
}

// File returns a file in the base commit (under oldPath if renamed) and in the workspace's
// working tree.
func (m *Manager) File(ctx context.Context, id, file, oldPath string) (*FileContents, error) {
	if err := checkRelPath(file); err != nil {
		return nil, err
	}
	if oldPath == "" {
		oldPath = file
	} else if err := checkRelPath(oldPath); err != nil {
		return nil, err
	}
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("workspace %s has no working tree (status %s)", id, w.Status)
	}
	fc := &FileContents{Path: file}
	if oldPath != file {
		fc.OldPath = oldPath
	}
	wt, err := m.worktree(ctx, w, true)
	if err != nil {
		return nil, err
	}
	data, ok, err := git.Blob(ctx, *wt, w.BaseCommit, oldPath)
	if fc.Original, err = side(data, ok, err); err != nil {
		return nil, err
	}
	data, ok, err = git.WorktreeBlob(ctx, *wt, file)
	if fc.Modified, err = side(data, ok, err); err != nil {
		return nil, err
	}
	return fc, nil
}

func side(data []byte, ok bool, err error) (FileSide, error) {
	switch {
	case errors.Is(err, git.ErrTooLarge):
		return FileSide{Exists: true, TooLarge: true}, nil
	case err != nil:
		return FileSide{}, err
	case !ok:
		return FileSide{}, nil
	case git.IsBinary(data) || !utf8.Valid(data):
		return FileSide{Exists: true, Binary: true}, nil
	}
	return FileSide{Exists: true, Text: string(data)}, nil
}

// checkRelPath only allows relative paths within the repository.
func checkRelPath(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") || len(p) > 4096 {
		return apperr.Invalid("invalid file path %q", p)
	}
	return nil
}

// SensitivePaths are paths whose changes a human must see before a push: workflows and
// actions run in CI with the repository's secrets, .ade/ controls ADE itself (#103).
// An entry ending in / is a directory.
var SensitivePaths = []string{".github/workflows/", ".github/actions/", ".ade/"}

// Sensitive returns the changed files of the diff under SensitivePaths (also old paths of
// renamed files), sorted and without duplicates.
func Sensitive(d *git.Diff) []string {
	var out []string
	for _, f := range d.Files {
		for _, p := range []string{f.Path, f.OldPath} {
			if p != "" && IsSensitive(p) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// IsSensitive reports whether the path (relative to the repository) is under SensitivePaths.
func IsSensitive(p string) bool {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, s := range SensitivePaths {
		if strings.HasPrefix(p+"/", s) {
			return true
		}
	}
	return false
}
