package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestEgressRetentionAndCascade(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.CreateProject(ctx, &Project{ID: "p1", Name: "demo", RepoPath: "/repo/demo", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkspace(ctx, &Workspace{ID: "w1", ProjectID: "p1", Path: "/ws/w1", Branch: "ade/w1", BaseBranch: "main", Status: WorkspaceReady}); err != nil {
		t.Fatal(err)
	}
	evs := make([]EgressEvent, EgressKeep+10)
	for i := range evs {
		evs[i] = EgressEvent{TS: time.Now(), Kind: "connect", Host: "h" + strconv.Itoa(i%3) + ".example", Port: 443, Allowed: i%2 == 0}
	}
	if err := s.AppendEgress(ctx, "w1", evs); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListEgress(ctx, "w1", EgressFilter{Limit: EgressKeep * 2})
	if err != nil || len(all) != EgressKeep {
		t.Fatalf("%d Events, %v; erwartet %d", len(all), err, EgressKeep)
	}
	if all[0].ID < all[1].ID {
		t.Error("nicht neueste zuerst")
	}
	hosts, err := s.EgressHosts(ctx, "w1")
	if err != nil || len(hosts) != 3 {
		t.Fatalf("Hosts %+v %v", hosts, err)
	}
	// Events eines unbekannten Workspaces werden verworfen.
	if err := s.AppendEgress(ctx, "fehlt", evs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListEgress(ctx, "w1", EgressFilter{}); len(left) != 0 {
		t.Fatalf("%d Events nach dem Löschen", len(left))
	}
}
