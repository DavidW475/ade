// Package git calls the git binary. It only wraps the commands ADE needs.
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

// Error is a failed git call with its error output.
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

// Cmd describes a git call.
type Cmd struct {
	Dir  string
	Args []string
	// Env is appended to the environment (KEY=VALUE).
	Env []string
	// Runner runs the call elsewhere (e.g. in the sandbox container); Dir does not apply then.
	Runner Runner
}

// Runner builds the process for argv (argv[0] is the program, e.g. git) with the additional
// variables env in another place, for example via docker exec in a workspace's container.
// The process runs there in the workspace directory.
type Runner func(ctx context.Context, env, argv []string) *exec.Cmd

// Worktree is a workspace in which the daemon runs git.
type Worktree struct {
	// Path is the directory on the host.
	Path string
	// Runner is set if git must not run on the host (sandbox, issue #41): the repository
	// configuration belongs to the agent; filters or fsmonitor from it should only take effect
	// in the container.
	Runner Runner
}

// Local returns a worktree in which git runs on the host.
func Local(path string) Worktree { return Worktree{Path: path} }

func (w Worktree) cmd(env []string, args ...string) Cmd {
	return Cmd{Dir: w.Path, Env: env, Args: args, Runner: w.Runner}
}

// baseEnv makes output reproducible and non-interactive.
var baseEnv = []string{
	"LC_ALL=C",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
	"GIT_CONFIG_NOSYSTEM=1",
}

// Run runs git and returns stdout.
func Run(ctx context.Context, c Cmd) ([]byte, error) {
	var cmd *exec.Cmd
	if c.Runner != nil {
		cmd = c.Runner(ctx, append(append([]string{}, baseEnv...), c.Env...), append([]string{"git"}, c.Args...))
	} else {
		cmd = exec.CommandContext(ctx, "git", c.Args...)
		cmd.Dir = c.Dir
		cmd.Env = append(append(os.Environ(), baseEnv...), c.Env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &Error{Args: c.Args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Output runs git in the directory dir and returns stdout without trailing whitespace.
func Output(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Run(ctx, Cmd{Dir: dir, Args: args})
	return strings.TrimSpace(string(out)), err
}

// ExitCode returns the exit code of a failed call or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Available reports whether git is installed.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// RepoInfo describes a repository.
type RepoInfo struct {
	// TopLevel is the root directory of the working tree.
	TopLevel string
	// Bare is true for repositories without a working tree.
	Bare bool
}

// Inspect checks whether dir is in a Git repository.
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

// DefaultBranch determines the base branch: origin/HEAD, otherwise the current branch,
// otherwise "main" or "master" if present.
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
	return "", errors.New("no base branch found (HEAD is detached and neither main nor master exists)")
}

// BranchExists reports whether the local branch exists and points to a commit.
func BranchExists(ctx context.Context, repo, branch string) bool {
	_, err := Output(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	return err == nil
}

// ResolveCommit returns the commit ID of rev.
func ResolveCommit(ctx context.Context, repo, rev string) (string, error) {
	return Output(ctx, repo, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// ValidBranchName checks a branch name with git check-ref-format.
func ValidBranchName(ctx context.Context, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	_, err := Output(ctx, "", "check-ref-format", "--branch", name)
	return err == nil
}
