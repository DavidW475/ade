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

// Schnappschüsse für Lese-Rollen ohne Sandbox (doc/factory.md#3-ausführung): Die Rolle
// arbeitet in einem eigenen Klon des Workspace-Stands. Was sie dort ändert, erreicht den
// Workspace nicht und wird mit dem Schnappschuss gelöscht. Echten Schutz vor einem Agenten,
// der es darauf anlegt, bietet nur die Sandbox (Lese-Container); lokal sperren die Adapter
// zusätzlich ihre Schreibwerkzeuge (adapter.Spec.ReadOnly).

// SnapshotDir ist das Verzeichnis des Schnappschusses einer Session unter dataDir.
func SnapshotDir(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "snapshots", sessionID)
}

func (m *Manager) snapshotDir(sessionID string) string { return SnapshotDir(m.opts.DataDir, sessionID) }

// prepareSnapshot legt den Schnappschuss einer Session an (bzw. nimmt den vorhandenen beim
// Fortsetzen) und liefert sein Verzeichnis.
func (m *Manager) prepareSnapshot(ctx context.Context, ws *store.Workspace, sessionID string) (string, error) {
	dir := m.snapshotDir(sessionID)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("Schnappschuss anlegen: %w", err)
	}
	_ = os.RemoveAll(dir)
	// --shared: Objekte über alternates lesen statt kopieren; HEAD ist der Branch des
	// Workspaces mit dem zuletzt gesicherten Stand (WIP-Commit nach jeder Runde).
	steps := []git.Cmd{
		{Args: []string{"clone", "--shared", "--quiet", "--", ws.Path, dir}},
	}
	for _, c := range steps {
		if _, err := git.Run(ctx, c); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("Schnappschuss des Workspaces anlegen: %w", err)
		}
	}
	// Den Basis-Branch als lokalen Branch übernehmen (ein Reviewer vergleicht mit ihm), dann
	// die Verbindung zum Workspace kappen.
	if ws.BaseBranch != "" && ws.BaseBranch != ws.Branch {
		_, _ = git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"branch", "--quiet", "--no-track", "--", ws.BaseBranch, "refs/remotes/origin/" + ws.BaseBranch}})
	}
	if _, err := git.Run(ctx, git.Cmd{Dir: dir, Args: []string{"remote", "remove", "origin"}}); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("Schnappschuss des Workspaces anlegen: %w", err)
	}
	return dir, nil
}

func (m *Manager) removeSnapshot(sessionID string) {
	dir := m.snapshotDir(sessionID)
	// Agenten können Dateien schreibgeschützt anlegen; vor dem Löschen freigeben.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: Verzeichnis wird gleich gelöscht
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("Schnappschuss löschen", "session", sessionID, "err", err)
	}
}
