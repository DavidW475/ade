package agent

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func (e *env) gitOut(dir string, args ...string) string {
	e.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commits returns the subject lines of base..branch in the project repository (empty: branch missing).
func (e *env) commits(w *store.Workspace) []string {
	if exec.Command("git", "-C", e.project.RepoPath, "rev-parse", "--verify", "--quiet", w.Branch).Run() != nil {
		return nil
	}
	out := e.gitOut(e.project.RepoPath, "log", "--format=%s", w.BaseCommit+".."+w.Branch)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestSessionEndCommitsAndFeedbackAddsCommit(t *testing.T) {
	e := setup(t, "")
	// The agent appends a line on every run and exits.
	e.fake.Argv = []string{"sh", "-c", `echo "$ADE_SESSION_ID" >> work.txt`}
	s1 := e.start(StartRequest{Task: "create file"})
	w, err := e.ws.Get(e.ctx, s1.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "WIP commit of the first session", func() bool { return len(e.commits(w)) == 1 })
	if c := e.commits(w); c[0] != "ade: wip "+s1.ID {
		t.Errorf("Commit = %q", c[0])
	}

	fb, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "Please add one more line."})
	if err != nil {
		t.Fatal(err)
	}
	if fb.WorkspaceID != w.ID || fb.Role != workspace.RoleFeedback || fb.Task != "Please add one more line." || fb.Adapter != "fake" {
		t.Errorf("Feedback-Session = %#v", fb)
	}
	waitFor(t, "zweiter Commit", func() bool { return len(e.commits(w)) == 2 })
	if c := e.commits(w); c[0] != "ade: wip "+fb.ID {
		t.Errorf("Commits = %v", c)
	}
	// The prompt contains the comment and the current diff.
	specs := e.fake.Specs()
	prompt := specs[len(specs)-1].Task
	for _, want := range []string{"Please add one more line.", "work.txt", "+" + s1.ID} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, prompt)
		}
	}
	// The history only shows the comment.
	if msgs := messages(e.events(fb.ID)); len(msgs) == 0 || msgs[0] != "user: Please add one more line." {
		t.Errorf("Nachrichten = %v", msgs)
	}

	if _, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "  "}); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("leeres Feedback: %v", err)
	}
}

func TestIdleCommitsAndBusyCheck(t *testing.T) {
	e := setup(t, "")
	// Reads the task, works after the second input, reports turn_done and waits.
	e.fake.Argv = []string{"sh", "-c", `read a; read b; echo new > new.txt; echo '{"type":"turn_done"}'; exec cat`}
	s := e.start(StartRequest{Task: "warte"})
	e.waitStatus(s.ID, store.SessionRunning)
	if err := e.m.WorkspaceBusy(e.ctx, s.WorkspaceID); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("WorkspaceBusy with a running agent: %v", err)
	}
	if _, err := e.m.Feedback(e.ctx, s.WorkspaceID, FeedbackRequest{Comment: "x"}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("feedback with a running agent: %v", err)
	}
	w, _ := e.ws.Get(e.ctx, s.WorkspaceID)
	if _, err := e.m.Input(e.ctx, s.ID, "los"); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(s.ID, store.SessionIdle)
	if err := e.m.WorkspaceBusy(e.ctx, s.WorkspaceID); err != nil {
		t.Errorf("WorkspaceBusy with a waiting agent: %v", err)
	}
	waitFor(t, "WIP commit after the turn", func() bool { return len(e.commits(w)) == 1 })

	// Feedback ends the waiting agent.
	fb, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if st := e.status(s.ID); st != store.SessionDone {
		t.Errorf("alte Session: %s", st)
	}
	e.waitStatus(fb.ID, store.SessionRunning)
}
