package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRunRepeatSteps(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.CreateProject(ctx, &Project{ID: "p", Name: "p", RepoPath: "/repo/p", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	r := &Run{ID: "r", ProjectID: "p", Factory: "f", Definition: []byte("{}"), Status: RunRunning, Steps: []RunStep{
		{Name: "impl", Kind: "role", Role: "dev", Output: "s.md"}, {Name: "review", Kind: "role", Role: "rev"},
		{Name: "decide", Kind: "decision"}, {Name: "pr", Kind: "action"},
	}}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, st := range r.Steps[:3] {
		if err := s.UpdateRunStep(ctx, "r", st.Row, StepUpdate{Status: Ptr(StepDone), SessionID: Ptr("s-" + st.Name)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateRun(ctx, "r", RunUpdate{Current: Ptr(2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.RepeatSteps(ctx, "r", 0, 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != 0 || len(got.Steps) != 4 || len(got.History) != 3 {
		t.Fatalf("Run %+v", got)
	}
	for i, st := range got.Steps {
		wantIter, wantStatus := 2, StepPending
		if i == 3 {
			wantIter = 1
		}
		if st.Index != i || st.Iteration != wantIter || st.Status != wantStatus || st.SessionID != "" {
			t.Errorf("Schritt %d: %+v", i, st)
		}
	}
	if got.Steps[0].Output != "s.md" || got.Steps[0].Row != 4 || got.History[0].Status != StepDone {
		t.Fatalf("new pass %+v, old %+v", got.Steps[0], got.History[0])
	}
	if ex := got.Executions(); len(ex) != 7 || ex[6].Row != 6 {
		t.Fatalf("executions %+v", ex)
	}
	// The session of the old pass still belongs to the run.
	if id, _ := s.RunOfSession(ctx, "s-impl"); id != "r" {
		t.Fatalf("RunOfSession %q", id)
	}
	if err := s.RepeatSteps(ctx, "r", 9, 9); err == nil {
		t.Fatal("jump back to a missing position without an error")
	}
}
