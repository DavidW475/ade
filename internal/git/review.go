package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WorkspaceArgs stehen vor git-Befehlen, die der Daemon in einem Workspace ausführt. Ein
// Agent kann dort die Repo-Konfiguration ändern; Hooks und fsmonitor würden sonst Befehle
// außerhalb der Sandbox ausführen.
var WorkspaceArgs = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// Fallback-Identität für Commits, wenn git keine konfigurierte kennt.
const (
	FallbackName  = "ADE"
	FallbackEmail = "ade@localhost"
)

func workspaceCmd(w Worktree, env []string, args ...string) Cmd {
	return w.cmd(env, append(append([]string{}, WorkspaceArgs...), args...)...)
}

// IdentityEnv liefert Umgebungsvariablen für Autor und Committer, falls in dir kein
// user.name bzw. user.email konfiguriert ist.
func IdentityEnv(ctx context.Context, dir string) []string { return identityEnv(ctx, Local(dir)) }

func identityEnv(ctx context.Context, w Worktree) []string {
	get := func(key string) string {
		out, _ := Run(ctx, w.cmd(nil, "config", key))
		return strings.TrimSpace(string(out))
	}
	var env []string
	if get("user.name") == "" {
		env = append(env, "GIT_AUTHOR_NAME="+FallbackName, "GIT_COMMITTER_NAME="+FallbackName)
	}
	if get("user.email") == "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+FallbackEmail, "GIT_COMMITTER_EMAIL="+FallbackEmail)
	}
	return env
}

// CommitAll committet alle Änderungen im Arbeitsbaum von w (auch neue, nicht ignorierte
// Dateien) ohne Hooks und Signatur. Liefert false, wenn es nichts zu committen gab.
func CommitAll(ctx context.Context, w Worktree, message string) (bool, error) {
	out, err := Run(ctx, workspaceCmd(w, nil, "status", "--porcelain", "-z", "--untracked-files=all"))
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return false, nil
	}
	if _, err := Run(ctx, workspaceCmd(w, nil, "add", "--all", "--", ".")); err != nil {
		return false, err
	}
	env := identityEnv(ctx, w)
	if _, err := Run(ctx, workspaceCmd(w, env, "-c", "commit.gpgSign=false",
		"commit", "--quiet", "--no-verify", "--allow-empty-message", "-m", message)); err != nil {
		return false, err
	}
	return true, nil
}

// FetchBranch holt den Branch aus dem Repository src nach repo und überschreibt ihn dort.
func FetchBranch(ctx context.Context, repo, src, branch string) error {
	ref := "refs/heads/" + branch
	_, err := Run(ctx, Cmd{Dir: repo, Args: []string{
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		"--", src, "+" + ref + ":" + ref,
	}})
	return err
}

// CheckedOutIn liefert den Arbeitsbaum, in dem branch ausgecheckt ist (leer: nirgends).
func CheckedOutIn(ctx context.Context, repo, branch string) (string, error) {
	out, err := Run(ctx, Cmd{Dir: repo, Args: []string{"worktree", "list", "--porcelain", "-z"}})
	if err != nil {
		return "", err
	}
	var path string
	for _, f := range splitZ(out) {
		switch {
		case f == "":
			path = ""
		case strings.HasPrefix(f, "worktree "):
			path = strings.TrimPrefix(f, "worktree ")
		case f == "branch refs/heads/"+branch:
			return path, nil
		}
	}
	return "", nil
}

// IsAncestor meldet, ob a ein Vorfahre von b ist (oder gleich).
func IsAncestor(ctx context.Context, repo, a, b string) (bool, error) {
	_, err := Run(ctx, Cmd{Dir: repo, Args: []string{"merge-base", "--is-ancestor", a, b}})
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// MergeTree berechnet den Merge von base und head, ohne Arbeitsbaum oder Refs zu ändern.
// Liefert den Tree oder bei Konflikten die betroffenen Dateien.
func MergeTree(ctx context.Context, repo, base, head string) (tree string, conflicts []string, err error) {
	out, err := Run(ctx, Cmd{Dir: repo, Args: []string{
		"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", base, head,
	}})
	fields := splitZ(out)
	if err != nil && ExitCode(err) != 1 {
		return "", nil, err
	}
	if len(fields) == 0 {
		return "", nil, fmt.Errorf("merge-tree: keine Ausgabe")
	}
	if err != nil {
		for _, f := range fields[1:] {
			if f != "" && !contains(conflicts, f) {
				conflicts = append(conflicts, f)
			}
		}
		return "", conflicts, nil
	}
	return fields[0], nil, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MergeCommit legt einen Merge-Commit mit den Eltern base und head an und setzt branch
// darauf – nur, wenn branch noch auf base zeigt. Für nicht ausgecheckte Branches.
func MergeCommit(ctx context.Context, repo, branch, base, head, tree, message string) (string, error) {
	env := IdentityEnv(ctx, repo)
	out, err := Run(ctx, Cmd{Dir: repo, Env: env, Args: []string{
		"commit-tree", "--no-gpg-sign", "-p", base, "-p", head, "-m", message, tree,
	}})
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if _, err := Run(ctx, Cmd{Dir: repo, Args: []string{
		"update-ref", "-m", "ade: " + firstLine(message), "refs/heads/" + branch, commit, base,
	}}); err != nil {
		return "", err
	}
	return commit, nil
}

// MergeInWorktree führt git merge --no-ff im Arbeitsbaum aus, in dem der Basis-Branch
// ausgecheckt ist. Schlägt der Merge fehl, wird er abgebrochen.
func MergeInWorktree(ctx context.Context, worktree, head, message string) (string, error) {
	env := IdentityEnv(ctx, worktree)
	if _, err := Run(ctx, Cmd{Dir: worktree, Env: env, Args: []string{
		"merge", "--quiet", "--no-ff", "--no-edit", "-m", message, head,
	}}); err != nil {
		if _, verr := Output(ctx, worktree, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); verr == nil {
			_, _ = Run(context.WithoutCancel(ctx), Cmd{Dir: worktree, Args: []string{"merge", "--abort"}})
		}
		return "", err
	}
	return Output(ctx, worktree, "rev-parse", "HEAD")
}

// DeleteBranch löscht einen lokalen Branch, falls er existiert.
func DeleteBranch(ctx context.Context, repo, branch string) error {
	if !BranchExists(ctx, repo, branch) {
		return nil
	}
	_, err := Run(ctx, Cmd{Dir: repo, Args: []string{"branch", "--quiet", "-D", "--", branch}})
	return err
}

// RemoteURL liefert die URL eines Remotes oder einen Fehler, wenn es fehlt.
func RemoteURL(ctx context.Context, repo, remote string) (string, error) {
	return Output(ctx, repo, "remote", "get-url", "--", remote)
}

// Push schiebt branch unter gleichem Namen zum Remote.
func Push(ctx context.Context, repo, remote, branch string) error {
	ref := "refs/heads/" + branch
	_, err := Run(ctx, Cmd{Dir: repo, Args: []string{"push", "--quiet", "--", remote, ref + ":" + ref}})
	return err
}

// MaxBlob ist die größte Datei, die Blob liefert.
const MaxBlob = 4 << 20

// ErrTooLarge meldet eine Datei über MaxBlob.
var ErrTooLarge = errors.New("Datei zu groß")

// Blob liefert den Inhalt von path im Commit rev; ok ist false, wenn die Datei dort fehlt.
func Blob(ctx context.Context, w Worktree, rev, path string) (data []byte, ok bool, err error) {
	return blob(ctx, w.cmd(nil), rev+":"+path)
}

// WorktreeBlob liefert den Inhalt von path im Arbeitsbaum, so wie git ihn sieht
// (Symlinks als Linkziel, Filter angewendet). Der Index des Workspaces bleibt unverändert.
func WorktreeBlob(ctx context.Context, w Worktree, path string) (data []byte, ok bool, err error) {
	// Gelöschte Dateien: git add würde am Pfad scheitern. Lstat folgt keinem Link am Ende;
	// Pfade hinter Symlinks weist git add selbst zurück.
	if _, err := os.Lstat(filepath.Join(w.Path, path)); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	idx, cleanup, err := tempIndex(ctx, w)
	if err != nil {
		return nil, false, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx, "GIT_LITERAL_PATHSPECS=1"}
	if _, err := Run(ctx, workspaceCmd(w, env, "add", "--all", "--", path)); err != nil {
		return nil, false, err
	}
	return blob(ctx, workspaceCmd(w, env), ":0:"+path)
}

func blob(ctx context.Context, c Cmd, object string) ([]byte, bool, error) {
	size, err := Run(ctx, Cmd{Dir: c.Dir, Env: c.Env, Runner: c.Runner, Args: append(append([]string{}, c.Args...), "cat-file", "-s", object)})
	if err != nil {
		// Fehlende Datei (oder fehlender Pfad im Commit) ist kein Fehler.
		return nil, false, nil
	}
	var n int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(size)), &n); err != nil {
		return nil, false, err
	}
	if n > MaxBlob {
		return nil, true, ErrTooLarge
	}
	out, err := Run(ctx, Cmd{Dir: c.Dir, Env: c.Env, Runner: c.Runner, Args: append(append([]string{}, c.Args...), "cat-file", "blob", object)})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// IsBinary schätzt wie git, ob Daten binär sind (NUL-Byte in den ersten 8000 Bytes).
func IsBinary(data []byte) bool {
	if len(data) > 8000 {
		data = data[:8000]
	}
	return bytes.IndexByte(data, 0) >= 0
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// SameDevice meldet, ob zwei Pfade auf demselben Dateisystem liegen. Im Zweifel true.
var SameDevice = func(a, b string) bool {
	da, errA := deviceOf(a)
	db, errB := deviceOf(b)
	if errA != nil || errB != nil {
		return true
	}
	return da == db
}

func deviceOf(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("keine Geräte-ID")
	}
	return uint64(sys.Dev), nil //nolint:unconvert // Dev ist nicht auf allen Plattformen uint64
}
