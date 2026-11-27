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

// Lese-Rollen ohne Sandbox arbeiten in einem Schnappschuss: Der Workspace bleibt unverändert,
// der Basis-Branch ist da (Reviewer vergleichen mit ihm), danach wird er gelöscht.
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
		t.Fatalf("Branches im Schnappschuss: %q", out)
	}
	if out, _ := exec.Command("git", "-C", snap, "remote").Output(); len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("Schnappschuss hat noch ein Remote: %q", out)
	}
	// Was die Rolle im Schnappschuss ändert, erreicht den Workspace nicht.
	if err := os.WriteFile(filepath.Join(snap, "README.md"), []byte("überschrieben\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Dismiss(e.ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.status(s.ID); got != store.SessionDone {
		t.Fatalf("Status %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "README.md")); string(data) != "hallo\n" {
		t.Fatalf("Workspace geändert: %q", data)
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatalf("Schnappschuss nicht gelöscht: %v", err)
	}
	if out, _ := exec.Command("git", "-C", ws.Path, "status", "--porcelain").Output(); len(out) > 0 {
		t.Fatalf("Workspace nicht sauber: %s", out)
	}
}
