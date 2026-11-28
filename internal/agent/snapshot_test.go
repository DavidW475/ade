package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// Read-only roles without a sandbox work in a snapshot: the workspace stays unchanged, the
// base branch is there (reviewers compare with it), afterwards it is deleted.
func TestReadOnlyRoleUsesSnapshot(t *testing.T) {
	e := setup(t, "")
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ws.WaitSetup(e.ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	s, err := e.m.StartRole(e.ctx, StartRequest{WorkspaceID: ws.ID, Task: "Plane"}, RoleStart{Role: "planner", Prompt: "Plane genau", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !s.ReadOnly || s.Role != "planner" {
		t.Fatalf("Session %+v", s)
	}
	waitFor(t, "Echo", func() bool { m, _ := e.m.LastMessage(e.ctx, s.ID); return m == "echo: Plane genau" })

	snap := SnapshotDir(e.dataDir, s.ID)
	spec := e.fake.Specs()[len(e.fake.Specs())-1]
	if spec.Workdir != snap || !spec.ReadOnly {
		t.Fatalf("Spec %+v, Schnappschuss %s", spec, snap)
	}
	out, err := exec.Command("git", "-C", snap, "branch", "--format=%(refname:short)").Output()
	if err != nil {
		t.Fatal(err)
	}
	if branches := strings.Fields(string(out)); len(branches) != 2 || !strings.Contains(string(out), "main") || !strings.Contains(string(out), ws.Branch) {
		t.Fatalf("branches in the snapshot: %q", out)
	}
	if out, _ := exec.Command("git", "-C", snap, "remote").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("snapshot still has a remote: %q", out)
	}
	// What the role changes in the snapshot does not reach the workspace.
	if err := os.WriteFile(filepath.Join(snap, "README.md"), []byte("overwritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Dismiss(e.ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.status(s.ID); got != store.SessionDone {
		t.Fatalf("Status %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "README.md")); string(data) != "hello\n" {
		t.Fatalf("workspace changed: %q", data)
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatalf("snapshot not deleted: %v", err)
	}
	if out, _ := exec.Command("git", "-C", ws.Path, "status", "--porcelain").Output(); len(out) > 0 {
		t.Fatalf("workspace not clean: %s", out)
	}
}
