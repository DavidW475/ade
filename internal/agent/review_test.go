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

// commits liefert die Betreffzeilen von base..branch im Projekt-Repo (leer: Branch fehlt).
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
	// Der Agent hängt bei jedem Lauf eine Zeile an und beendet sich.
	e.fake.Argv = []string{"sh", "-c", `echo "$ADE_SESSION_ID" >> arbeit.txt`}
	s1 := e.start(StartRequest{Task: "Datei anlegen"})
	w, err := e.ws.Get(e.ctx, s1.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "WIP-Commit der ersten Session", func() bool { return len(e.commits(w)) == 1 })
	if c := e.commits(w); c[0] != "ade: wip "+s1.ID {
		t.Errorf("Commit = %q", c[0])
	}

	fb, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "Bitte noch eine Zeile."})
	if err != nil {
		t.Fatal(err)
	}
	if fb.WorkspaceID != w.ID || fb.Role != workspace.RoleFeedback || fb.Task != "Bitte noch eine Zeile." || fb.Adapter != "fake" {
		t.Errorf("Feedback-Session = %#v", fb)
	}
	waitFor(t, "zweiter Commit", func() bool { return len(e.commits(w)) == 2 })
	if c := e.commits(w); c[0] != "ade: wip "+fb.ID {
		t.Errorf("Commits = %v", c)
	}
	// Der Prompt enthält Kommentar und aktuellen Diff.
	specs := e.fake.Specs()
	prompt := specs[len(specs)-1].Task
	for _, want := range []string{"Bitte noch eine Zeile.", "arbeit.txt", "+" + s1.ID} {
		if !strings.Contains(prompt, want) {
			t.Errorf("Prompt enthält %q nicht:\n%s", want, prompt)
		}
	}
	// Im Verlauf steht nur der Kommentar.
	if msgs := messages(e.events(fb.ID)); len(msgs) == 0 || msgs[0] != "user: Bitte noch eine Zeile." {
		t.Errorf("Nachrichten = %v", msgs)
	}

	if _, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "  "}); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("leeres Feedback: %v", err)
	}
}

func TestIdleCommitsAndBusyCheck(t *testing.T) {
	e := setup(t, "")
	// Liest die Aufgabe, arbeitet nach der zweiten Eingabe, meldet turn_done und wartet.
	e.fake.Argv = []string{"sh", "-c", `read a; read b; echo neu > neu.txt; echo '{"type":"turn_done"}'; exec cat`}
	s := e.start(StartRequest{Task: "warte"})
	e.waitStatus(s.ID, store.SessionRunning)
	if err := e.m.WorkspaceBusy(e.ctx, s.WorkspaceID); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("WorkspaceBusy bei laufendem Agenten: %v", err)
	}
	if _, err := e.m.Feedback(e.ctx, s.WorkspaceID, FeedbackRequest{Comment: "x"}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Feedback bei laufendem Agenten: %v", err)
	}
	w, _ := e.ws.Get(e.ctx, s.WorkspaceID)
	if _, err := e.m.Input(e.ctx, s.ID, "los"); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(s.ID, store.SessionIdle)
	if err := e.m.WorkspaceBusy(e.ctx, s.WorkspaceID); err != nil {
		t.Errorf("WorkspaceBusy bei wartendem Agenten: %v", err)
	}
	waitFor(t, "WIP-Commit nach der Runde", func() bool { return len(e.commits(w)) == 1 })

	// Feedback beendet den wartenden Agenten.
	fb, err := e.m.Feedback(e.ctx, w.ID, FeedbackRequest{Comment: "weiter"})
	if err != nil {
		t.Fatal(err)
	}
	if st := e.status(s.ID); st != store.SessionDone {
		t.Errorf("alte Session: %s", st)
	}
	e.waitStatus(fb.ID, store.SessionRunning)
}
