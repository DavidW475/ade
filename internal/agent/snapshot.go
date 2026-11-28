package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/store"
)

// Snapshots for read-only roles without a sandbox (doc/factory.md#3-execution): the role
// works in its own clone of the workspace state. What it changes there does not reach the
// workspace and is deleted with the snapshot. Only the sandbox (reader container) offers real
// protection against an agent that sets out to do so; locally the adapters additionally block
// their write tools (adapter.Spec.ReadOnly).

// SnapshotDir is the directory of a session's snapshot under dataDir.
func SnapshotDir(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "snapshots", sessionID)
}

func (m *Manager) snapshotDir(sessionID string) string { return SnapshotDir(m.opts.DataDir, sessionID) }

// prepareSnapshot creates a session's snapshot (or takes the existing one when resuming) and
// returns its directory.
func (m *Manager) prepareSnapshot(ctx context.Context, ws *store.Workspace, sessionID string) (string, error) {
	dir := m.snapshotDir(sessionID)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("create snapshot: %w", err)
	}
	_ = os.RemoveAll(dir)
	// --shared: read objects through alternates instead of copying them; HEAD is the workspace's
	// branch with the most recently saved state (WIP commit after every turn).
	steps := []git.Cmd{
		{Args: []string{"clone", "--shared", "--quiet", "--", ws.Path, dir}},
	}
	for _, c := range steps {
		if _, err := git.Run(ctx, c); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("create snapshot of the workspace: %w", err)
		}
	}
	// Take over the base branch as a local branch (a reviewer compares with it), then cut the
	// connection to the workspace.
	if ws.BaseBranch != "" && ws.BaseBranch != ws.Branch {
		_, _ = git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"branch", "--quiet", "--no-track", "--", ws.BaseBranch, "refs/remotes/origin/" + ws.BaseBranch}})
	}
	if _, err := git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"remote", "remove", "origin"}}); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create snapshot of the workspace: %w", err)
	}
	return dir, nil
}

func (m *Manager) removeSnapshot(sessionID string) {
	dir := m.snapshotDir(sessionID)
	// Agents can create write-protected files; make them writable before deleting.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: the directory is deleted right away
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("delete snapshot", "session", sessionID, "err", err)
	}
}
