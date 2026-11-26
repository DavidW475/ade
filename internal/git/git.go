// Package git ruft das git-Binary auf. Es kapselt nur die Befehle, die ADE braucht.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Error ist ein fehlgeschlagener git-Aufruf mit dessen Fehlerausgabe.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Cmd beschreibt einen git-Aufruf.
type Cmd struct {
	Dir  string
	Args []string
	// Env wird an die Umgebung angehängt (KEY=VALUE).
	Env []string
}

// baseEnv sorgt für reproduzierbare, nicht-interaktive Ausgaben.
var baseEnv = []string{
	"LC_ALL=C",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_CONFIG_NOSYSTEM=1",
}

// Run führt git aus und liefert stdout.
func Run(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(append(os.Environ(), baseEnv...), c.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: c.Args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Output führt git im Verzeichnis dir aus und liefert stdout ohne abschließende Leerzeichen.
func Output(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Run(ctx, Cmd{Dir: dir, Args: args})
	return strings.TrimSpace(string(out)), err
}

// ExitCode liefert den Exit-Code eines fehlgeschlagenen Aufrufs oder -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Available meldet, ob git installiert ist.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// RepoInfo beschreibt ein Repository.
type RepoInfo struct {
	// TopLevel ist das Wurzelverzeichnis des Arbeitsbaums.
	TopLevel string
	// Bare ist true für Repositories ohne Arbeitsbaum.
	Bare bool
}

// Inspect prüft, ob dir in einem Git-Repository liegt.
func Inspect(ctx context.Context, dir string) (*RepoInfo, error) {
	bare, err := Output(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	if bare == "true" {
		return &RepoInfo{Bare: true}, nil
	}
	top, err := Output(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	return &RepoInfo{TopLevel: top}, nil
}

// DefaultBranch ermittelt den Basis-Branch: origin/HEAD, sonst den aktuellen Branch,
// sonst "main" bzw. "master", falls vorhanden.
func DefaultBranch(ctx context.Context, repo string) (string, error) {
	if ref, err := Output(ctx, repo, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		b := strings.TrimPrefix(ref, "origin/")
		if BranchExists(ctx, repo, b) {
			return b, nil
		}
	}
	if b, err := Output(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && b != "" {
		return b, nil
	}
	for _, b := range []string{"main", "master"} {
		if BranchExists(ctx, repo, b) {
			return b, nil
		}
	}
	return "", errors.New("kein Basis-Branch gefunden (HEAD ist losgelöst und weder main noch master existiert)")
}

// BranchExists meldet, ob der lokale Branch existiert und auf einen Commit zeigt.
func BranchExists(ctx context.Context, repo, branch string) bool {
	_, err := Output(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	return err == nil
}

// ResolveCommit liefert die Commit-ID von rev.
func ResolveCommit(ctx context.Context, repo, rev string) (string, error) {
	return Output(ctx, repo, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// ValidBranchName prüft einen Branch-Namen mit git check-ref-format.
func ValidBranchName(ctx context.Context, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	_, err := Output(ctx, "", "check-ref-format", "--branch", name)
	return err == nil
}
