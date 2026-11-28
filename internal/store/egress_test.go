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
		t.Fatalf("%d events, %v; want %d", len(all), err, EgressKeep)
	}
	if all[0].ID < all[1].ID {
		t.Error("not newest first")
	}
	hosts, err := s.EgressHosts(ctx, "w1")
	if err != nil || len(hosts) != 3 {
		t.Fatalf("Hosts %+v %v", hosts, err)
	}
	// Events of an unknown workspace are discarded.
	if err := s.AppendEgress(ctx, "missing", evs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListEgress(ctx, "w1", EgressFilter{}); len(left) != 0 {
		t.Fatalf("%d events after deleting", len(left))
	}
}

func TestEgressGrantsSurviveUpdateAndGoWithDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.CreateProject(ctx, &Project{ID: "p1", Name: "demo", RepoPath: "/repo/demo", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w1", "w2"} {
		if err := s.CreateWorkspace(ctx, &Workspace{ID: id, ProjectID: "p1", Path: "/ws/" + id, Branch: "ade/" + id, BaseBranch: "main", Status: WorkspaceReady}); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []EgressGrant{
		{ProjectID: "p1", WorkspaceID: "w1", Rule: "w1.example.com"},
		{ProjectID: "p1", WorkspaceID: "w2", Rule: "w2.example.com"},
		{ProjectID: "p1", Rule: "all.example.com"},
	} {
		if err := s.AddEgressGrant(ctx, &g); err != nil {
			t.Fatal(err)
		}
	}
	rules := func(ws string) []string {
		t.Helper()
		gs, err := s.ListEgressGrants(ctx, "p1", ws)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, g := range gs {
			out = append(out, g.Rule)
		}
		return out
	}

	// A WIP commit updates the workspace; its exception must stay (#79).
	if err := s.UpdateWorkspace(ctx, "w1", WorkspaceUpdate{HeadCommit: Ptr("abc123")}); err != nil {
		t.Fatal(err)
	}
	if got := rules("w1"); len(got) != 2 || got[0] != "w1.example.com" || got[1] != "all.example.com" {
		t.Fatalf("after update: %v", got)
	}

	if err := s.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if got := rules("w1"); len(got) != 1 || got[0] != "all.example.com" {
		t.Fatalf("after delete: %v", got)
	}
	if got := rules("w2"); len(got) != 2 {
		t.Fatalf("other workspace lost exceptions: %v", got)
	}
}
