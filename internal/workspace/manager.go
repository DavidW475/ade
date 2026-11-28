// Package workspace creates workspaces as local Git clones (ADR-005), runs their setup
// commands, computes diffs and cleans them up again.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// Limits for setup commands.
const (
	SetupTimeout = 15 * time.Minute
	MaxSetupLog  = 64 << 10
)

// BranchPrefix is the prefix of workspace branches.
const BranchPrefix = "ade/"

// Manager manages the workspaces of all projects.
type Manager struct {
	store    *store.Store
	projects *project.Service
	dir      string
	log      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	setup map[string]*setupRun // running setups per workspace

	// beforeDelete runs before a workspace is deleted (e.g. end agent sessions).
	beforeDelete func(ctx context.Context, id string) error
	// busyCheck runs before merge and PR (review.go).
	busyCheck func(ctx context.Context, id string) error
	gh        string
	locks     map[string]*wsLock

	// sandbox is nil if the daemon runs without the Docker sandbox.
	sandbox        *sandbox.Manager
	sandboxDefault bool
}

type setupRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager creates a manager that creates workspaces under dir.
func NewManager(st *store.Store, projects *project.Service, dir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: st, projects: projects, dir: dir, log: log, ctx: ctx, cancel: cancel,
		setup: map[string]*setupRun{}, gh: "gh", locks: map[string]*wsLock{}}
}

// Recover marks workspaces whose creation or setup was interrupted during the last run.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.store.MarkInterruptedWorkspaces(ctx)
	if n > 0 {
		m.log.Warn("interrupted workspaces marked", "count", n)
	}
	return err
}

// SetBeforeDelete registers a function that runs before a workspace is deleted.
// If it returns an error, nothing is deleted.
func (m *Manager) SetBeforeDelete(fn func(ctx context.Context, id string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeDelete = fn
}

// WaitSetup waits until a running setup of the workspace is done (right away if none runs).
func (m *Manager) WaitSetup(ctx context.Context, id string) error {
	m.mu.Lock()
	run := m.setup[id]
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close aborts running setups and waits for them.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// SetSandbox enables the Docker sandbox; def is the default for new workspaces
// (defaults.sandbox).
func (m *Manager) SetSandbox(sb *sandbox.Manager, def bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandbox, m.sandboxDefault = sb, def
}

// Sandbox returns the sandbox manager (nil without a sandbox).
func (m *Manager) Sandbox() *sandbox.Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandbox
}

// SandboxDefault reports the default for new workspaces.
func (m *Manager) SandboxDefault() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxDefault
}

// CreateOptions are options for a new workspace.
type CreateOptions struct {
	// BaseBranch overrides the project's base branch.
	BaseBranch string `json:"base_branch,omitempty"`
	// Sandbox: agent, setup and IDE in the Docker container (nil: defaults.sandbox).
	Sandbox *bool `json:"sandbox,omitempty"`
}

// Create creates a workspace for the project: a local clone, its own branch ade/<id> from
// the base branch. Setup commands then run in the background.
func (m *Manager) Create(ctx context.Context, projectID string, opts CreateOptions) (*store.Workspace, error) {
	p, err := m.projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p.ConfigError != "" {
		return nil, apperr.Invalid("project configuration invalid: %s", p.ConfigError)
	}
	base := opts.BaseBranch
	if base == "" {
		base = p.BaseBranch
	}
	if !git.ValidBranchName(ctx, base) {
		return nil, apperr.Invalid("%q is not a valid branch name", base)
	}
	commit, err := git.ResolveCommit(ctx, p.RepoPath, "refs/heads/"+base)
	if err != nil {
		return nil, apperr.Invalid("base branch %q does not exist in the repository", base)
	}

	useSandbox := m.SandboxDefault()
	if opts.Sandbox != nil {
		useSandbox = *opts.Sandbox
	}
	image := ""
	if useSandbox {
		sb := m.Sandbox()
		if sb == nil {
			return nil, apperr.Invalid("the Docker sandbox is not enabled")
		}
		if p.SandboxError != "" {
			return nil, apperr.Invalid("sandbox configuration invalid: %s", p.SandboxError)
		}
		if _, err := sb.Available(ctx); err != nil {
			return nil, apperr.Invalid("%v – start without a sandbox or set up Docker", err)
		}
		image = sb.ImageFor(*p.Sandbox)
	}

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace directory: %w", err)
	}
	id := project.NewID()
	w := &store.Workspace{
		ID:         id,
		ProjectID:  p.ID,
		Path:       filepath.Join(m.dir, id),
		Branch:     BranchPrefix + id,
		BaseBranch: base,
		BaseCommit: commit,
		Status:     store.WorkspaceCreating,

		Sandbox:      useSandbox,
		SandboxImage: image,
	}
	if err := m.store.CreateWorkspace(ctx, w); err != nil {
		return nil, err
	}

	if err := clone(ctx, p.RepoPath, w); err != nil {
		_ = removeAll(w.Path)
		_ = m.store.DeleteWorkspace(context.WithoutCancel(ctx), id)
		return nil, err
	}

	status := store.WorkspaceReady
	if len(p.Config.Setup) > 0 || w.Sandbox {
		// With the sandbox, creating the container (including an image download) is part of the setup.
		status = store.WorkspaceSetup
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{Status: &status}); err != nil {
		return nil, err
	}
	if status == store.WorkspaceSetup {
		m.startSetup(w, p.Config.Setup)
	}

	m.log.Info("workspace created", "id", id, "project", p.Name, "branch", w.Branch, "base", base)
	return m.store.GetWorkspace(ctx, id)
}

// clone creates the clone and then removes the connection to the main repository, so an
// agent cannot push there. The daemon fetches results later (ADR-005).
//
// If the repository and the workspace directory are on different file systems, the hardlinks
// of --local fail (EXDEV); git then copies the objects (--no-hardlinks). Sandbox workspaces
// always clone without hardlinks: the container could otherwise overwrite object files that
// workspace and main repository share.
func clone(ctx context.Context, repo string, w *store.Workspace) error {
	cloneArgs := func(hardlinks bool) []string {
		args := []string{"clone", "--local", "--quiet"}
		if !hardlinks {
			args = append(args, "--no-hardlinks")
		}
		return append(args, "--branch", w.BaseBranch, "--", repo, w.Path)
	}
	hardlinks := !w.Sandbox && git.SameDevice(repo, filepath.Dir(w.Path))
	_, err := git.Run(ctx, git.Cmd{Args: cloneArgs(hardlinks)})
	if err != nil && hardlinks && isCrossDevice(err) {
		_ = removeAll(w.Path)
		_, err = git.Run(ctx, git.Cmd{Args: cloneArgs(false)})
	}
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	steps := []git.Cmd{
		{Dir: w.Path, Args: []string{"remote", "remove", "origin"}},
		{Dir: w.Path, Args: []string{"switch", "--quiet", "--create", w.Branch, w.BaseCommit}},
	}
	for _, c := range steps {
		if _, err := git.Run(ctx, c); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
	}
	return nil
}

func isCrossDevice(err error) bool {
	var ge *git.Error
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "cross-device")
}

func (m *Manager) startSetup(w *store.Workspace, cmds []string) {
	ctx, cancel := context.WithCancel(m.ctx)
	run := &setupRun{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.setup[w.ID] = run
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(run.done)
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.setup, w.ID)
			m.mu.Unlock()
		}()

		var log string
		var err error
		if w.Sandbox {
			if err = m.EnsureSandbox(ctx, w.ID); err != nil {
				err = fmt.Errorf("sandbox container: %w", err)
				log = err.Error() + "\n"
			}
		}
		if err == nil {
			log, err = m.runSetup(ctx, w, cmds)
		}
		u := store.WorkspaceUpdate{SetupLog: &log, Status: store.Ptr(store.WorkspaceReady), Error: store.Ptr("")}
		if err != nil {
			u.Status, u.Error = store.Ptr(store.WorkspaceSetupFailed), store.Ptr(err.Error())
			m.log.Warn("workspace setup failed", "id", w.ID, "err", err)
		}
		// Save the state also on abort (daemon stopping).
		if err := m.store.UpdateWorkspace(context.WithoutCancel(ctx), w.ID, u); err != nil && !errors.Is(err, apperr.ErrNotFound) {
			m.log.Error("store workspace status", "id", w.ID, "err", err)
		}
	}()
}

// envSetup marks setup processes in the container (to end them on abort).
const envSetup = "ADE_SETUP"

func (m *Manager) runSetup(ctx context.Context, w *store.Workspace, cmds []string) (string, error) {
	var out tailBuffer
	sb := m.Sandbox()
	for i, c := range cmds {
		_, _ = fmt.Fprintf(&out, "$ %s\n", c)
		cctx, cancel := context.WithTimeout(ctx, SetupTimeout)
		var cmd *exec.Cmd
		stop := func() bool { return false }
		if w.Sandbox {
			marker := envSetup + "=" + w.ID
			// In the container: setup commands come from the repository and are considered untrusted.
			cmd = sb.Command(cctx, w.ID, sandbox.Exec{
				Argv: []string{"sh", "-c", c},
				Env:  []string{"ADE_WORKSPACE_ID=" + w.ID, "ADE_WORKSPACE_PATH=" + sandbox.Workdir, "CI=1", marker},
			})
			stop = context.AfterFunc(cctx, func() {
				_ = sb.Signal(context.Background(), w.ID, marker, syscall.SIGKILL)
			})
		} else {
			cmd = exec.CommandContext(cctx, "sh", "-c", c)
			cmd.Dir = w.Path
			cmd.Env = append(os.Environ(), "ADE_WORKSPACE_ID="+w.ID, "ADE_WORKSPACE_PATH="+w.Path, "CI=1")
		}
		cmd.Stdout, cmd.Stderr = &out, &out
		// Own process group: on abort, child processes are ended as well.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		stop()
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case ctx.Err() != nil:
			return out.String(), fmt.Errorf("setup aborted at command %d (%s)", i+1, c)
		case timedOut:
			return out.String(), fmt.Errorf("setup command %d (%s) aborted after %s", i+1, c, SetupTimeout)
		case err != nil:
			if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 127 {
				hint := "program not found"
				if w.Sandbox {
					hint += " – is the tool missing in the sandbox image? (image in .ade/sandbox.yaml)"
				}
				return out.String(), fmt.Errorf("setup command %d (%s) failed: %w (%s)", i+1, c, err, hint)
			}
			return out.String(), fmt.Errorf("setup command %d (%s) failed: %w", i+1, c, err)
		}
	}
	return out.String(), nil
}

// Get returns a workspace.
func (m *Manager) Get(ctx context.Context, id string) (*store.Workspace, error) {
	return m.store.GetWorkspace(ctx, id)
}

// Summary summarizes the changes of a workspace.
type Summary struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// View is a workspace with a change overview.
type View struct {
	*store.Workspace
	Changes   *Summary `json:"changes,omitempty"`
	DiffError string   `json:"diff_error,omitempty"`
}

// List returns the workspaces of a project including the number of changed files.
func (m *Manager) List(ctx context.Context, projectID string) ([]*View, error) {
	if _, err := m.store.GetProject(ctx, projectID); err != nil {
		return nil, err
	}
	ws, err := m.store.ListWorkspaces(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]*View, len(ws))
	for i, w := range ws {
		out[i] = m.view(ctx, w)
	}
	return out, nil
}

// GetView returns a workspace including the change overview.
func (m *Manager) GetView(ctx context.Context, id string) (*View, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.view(ctx, w), nil
}

func (m *Manager) view(ctx context.Context, w *store.Workspace) *View {
	v := &View{Workspace: w}
	if !hasWorktree(w) {
		return v
	}
	wt, err := m.worktree(ctx, w, false)
	if err != nil {
		v.DiffError = err.Error()
		return v
	}
	if wt == nil {
		return v // sandbox stopped: do not start it just for the overview
	}
	d, err := git.WorktreeDiff(ctx, *wt, w.BaseCommit, false)
	if err != nil {
		v.DiffError = err.Error()
		return v
	}
	v.Changes = &Summary{Files: len(d.Files), Additions: d.Additions, Deletions: d.Deletions}
	return v
}

func hasWorktree(w *store.Workspace) bool {
	switch w.Status {
	case store.WorkspaceReady, store.WorkspaceSetup, store.WorkspaceSetupFailed:
		_, err := os.Stat(filepath.Join(w.Path, ".git"))
		return err == nil
	}
	return false
}

// Diff compares the workspace's working tree with its base commit.
func (m *Manager) Diff(ctx context.Context, id string, withPatch bool) (*git.Diff, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("workspace %s has no working tree (status %s)", id, w.Status)
	}
	wt, err := m.worktree(ctx, w, true)
	if err != nil {
		return nil, err
	}
	return git.WorktreeDiff(ctx, *wt, w.BaseCommit, withPatch)
}

// sandboxSpec builds the container description of a workspace from the current project
// configuration (sandbox.yaml in the main repository, never from the workspace).
func (m *Manager) sandboxSpec(ctx context.Context, w *store.Workspace) (sandbox.Spec, error) {
	p, err := m.projects.Get(ctx, w.ProjectID)
	if err != nil {
		return sandbox.Spec{}, err
	}
	if p.SandboxError != "" {
		// Do not silently fall back to the defaults: they could allow more than the intended
		// policy. Running proxies keep the last valid one.
		return sandbox.Spec{}, apperr.Invalid("sandbox configuration invalid: %s", p.SandboxError)
	}
	cfg := *p.Sandbox
	if w.SandboxImage != "" {
		cfg.Image = w.SandboxImage
	}
	var env []string
	// The user's Git identity, so the agent's commits in the container have an author.
	for _, kv := range [][2]string{{"user.name", "NAME"}, {"user.email", "EMAIL"}} {
		if v, _ := git.Output(ctx, p.RepoPath, "config", kv[0]); v != "" && !strings.ContainsAny(v, "\n\x00") {
			env = append(env, "GIT_AUTHOR_"+kv[1]+"="+v, "GIT_COMMITTER_"+kv[1]+"="+v)
		}
	}
	grants, err := m.store.ListEgressGrants(ctx, w.ProjectID, w.ID)
	if err != nil {
		return sandbox.Spec{}, err
	}
	spec := sandbox.Spec{Workspace: w.ID, Path: w.Path, Config: cfg, Env: env}
	for _, g := range grants {
		spec.Grants = append(spec.Grants, g.Rule)
	}
	return spec, nil
}

// SandboxSpec returns the current container description of a sandbox workspace (source for
// the hot reload of the network policy).
func (m *Manager) SandboxSpec(ctx context.Context, id string) (sandbox.Spec, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return sandbox.Spec{}, err
	}
	if !w.Sandbox {
		return sandbox.Spec{}, apperr.Invalid("workspace %s runs without a sandbox", id)
	}
	return m.sandboxSpec(ctx, w)
}

// EnsureSandbox creates or starts the container of a sandbox workspace.
func (m *Manager) EnsureSandbox(ctx context.Context, id string) error {
	return m.ensureSandbox(ctx, id, false)
}

// EnsureSandboxReader additionally creates or starts the reader container (read-only roles
// of a factory, /workspace read-only).
func (m *Manager) EnsureSandboxReader(ctx context.Context, id string) error {
	return m.ensureSandbox(ctx, id, true)
}

func (m *Manager) ensureSandbox(ctx context.Context, id string, reader bool) error {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	if !w.Sandbox {
		return nil
	}
	sb := m.Sandbox()
	if sb == nil {
		return apperr.Conflict("workspace %s runs in the sandbox, but Docker is not enabled", id)
	}
	spec, err := m.sandboxSpec(ctx, w)
	if err != nil {
		return err
	}
	if reader {
		return sb.EnsureReader(ctx, spec)
	}
	return sb.Ensure(ctx, spec)
}

// worktree returns the place where the daemon runs git in the workspace: for sandbox
// workspaces in the container (issue #41). With start=false it returns nil if the container
// is not running.
func (m *Manager) worktree(ctx context.Context, w *store.Workspace, start bool) (*git.Worktree, error) {
	if !w.Sandbox {
		wt := git.Local(w.Path)
		return &wt, nil
	}
	sb := m.Sandbox()
	if sb == nil {
		return nil, apperr.Conflict("workspace %s runs in the sandbox, but Docker is not enabled", w.ID)
	}
	if start {
		if err := m.EnsureSandbox(ctx, w.ID); err != nil {
			return nil, err
		}
	} else if c, err := sb.Get(ctx, w.ID); err != nil || c == nil || !c.Running {
		return nil, err
	}
	return &git.Worktree{Path: w.Path, Runner: func(ctx context.Context, env, argv []string) *exec.Cmd {
		return sb.Command(ctx, w.ID, sandbox.Exec{Argv: argv, Env: env})
	}}, nil
}

// Delete (“Discard”) aborts a running setup, deletes the branch ade/<id> in the main
// repository (if it was fetched), the directory and the record. A branch that was already
// pushed and a pull request stay on the remote.
func (m *Manager) Delete(ctx context.Context, id string) error {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	run, before := m.setup[id], m.beforeDelete
	m.mu.Unlock()
	if before != nil {
		if err := before(ctx, id); err != nil {
			return err
		}
	}
	if run != nil {
		run.cancel()
		<-run.done
	}

	if err := m.checkPath(w); err != nil {
		return err
	}
	// After beforeDelete: a WIP commit of ended sessions can still create the branch.
	unlock := m.lockWorkspace(id)
	defer unlock()
	if p, err := m.store.GetProject(ctx, w.ProjectID); err == nil {
		if wt, _ := git.CheckedOutIn(ctx, p.RepoPath, w.Branch); wt != "" {
			return apperr.Conflict("branch %s is checked out in %s – check out another branch there first", w.Branch, wt)
		}
		if err := git.DeleteBranch(ctx, p.RepoPath, w.Branch); err != nil {
			if _, statErr := os.Stat(p.RepoPath); statErr == nil {
				return fmt.Errorf("delete branch %s in the project repository: %w", w.Branch, err)
			}
			m.log.Warn("project repository not reachable, branch stays", "id", id, "repo", p.RepoPath, "err", err)
		}
	}
	if w.Sandbox {
		if sb := m.Sandbox(); sb != nil {
			if err := sb.Remove(ctx, id); err != nil {
				return fmt.Errorf("remove sandbox containers: %w", err)
			}
		}
	}
	if err := removeAll(w.Path); err != nil {
		return fmt.Errorf("delete workspace directory: %w", err)
	}
	if err := m.store.DeleteWorkspace(ctx, id); err != nil {
		return err
	}
	m.log.Info("workspace deleted", "id", id)
	return nil
}

// DeleteAll deletes all workspaces of a project.
func (m *Manager) DeleteAll(ctx context.Context, projectID string) error {
	ws, err := m.store.ListWorkspaces(ctx, projectID)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if err := m.Delete(ctx, w.ID); err != nil {
			return err
		}
	}
	return nil
}

// checkPath makes sure that only directories directly below the workspaces directory are
// deleted – never anything else, not even with manipulated records.
func (m *Manager) checkPath(w *store.Workspace) error {
	rel, err := filepath.Rel(m.dir, w.Path)
	if err != nil || rel != w.ID || w.ID == "" || strings.ContainsAny(w.ID, `/\.`) {
		return fmt.Errorf("workspace %s: path %s is not in the workspaces directory %s", w.ID, w.Path, m.dir)
	}
	return nil
}

// removeAll deletes a directory; write-protected contents (e.g. the Go module cache) are
// made writable first.
func removeAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: directories need the x bit
		}
		return nil
	})
	return os.RemoveAll(path)
}

// tailBuffer keeps the last MaxSetupLog bytes.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - MaxSetupLog; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cut {
		return "… (truncated)\n" + string(t.buf)
	}
	return string(t.buf)
}
