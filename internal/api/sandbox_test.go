package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// fakeDocker ist ein Skript statt der Docker-CLI: kein Docker-Daemon (version scheitert),
// keine Container.
const fakeDocker = `#!/bin/sh
case "$1" in
  version) echo "Cannot connect to the Docker daemon" >&2; exit 1 ;;
  container) case "$2" in ls) exit 0 ;; inspect) echo "Error: No such container" >&2; exit 1 ;; esac ;;
esac
exit 1
`

func TestSandboxRoutes(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o700); err != nil { //nolint:gosec // Testskript
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	ws := workspace.NewManager(st, ps, filepath.Join(dir, "ws"), log)
	t.Cleanup(ws.Close)
	sb := sandbox.New(sandbox.Options{Docker: docker, DataDir: dir, Image: "img:1", Logger: log})
	ws.SetSandbox(sb, true)
	s := New(Options{Version: "test", Port: "7000", Store: st, Auth: NewAuth(testSecret), Logger: log, Projects: ps, Workspaces: ws, Sandbox: sb, Egress: st})

	for _, rt := range [][2]string{{"GET", "/api/sandbox"}, {"POST", "/api/sandbox/gc"}, {"GET", "/api/workspaces/x/sandbox"}, {"GET", "/api/workspaces/x/egress"}} {
		r := httptest.NewRequest(rt[0], rt[1], nil)
		r.Host = "localhost:7000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s ohne Auth: %d", rt[0], rt[1], w.Code)
		}
	}

	var info SandboxInfo
	api(t, s, "GET", "/api/sandbox", "", http.StatusOK, &info)
	if info.Available || info.Error == "" || info.Image != "img:1" || !info.Default || info.Network != NetworkProxy ||
		len(info.Presets["npm"]) == 0 || len(info.DefaultPresets) == 0 {
		t.Errorf("Info = %+v", info)
	}
	var gc GCResponse
	api(t, s, "POST", "/api/sandbox/gc?dry_run=1", "", http.StatusOK, &gc)
	if !gc.DryRun || len(gc.Removed) != 0 {
		t.Errorf("GC = %+v", gc)
	}

	// Ohne Docker scheitert ein Sandbox-Workspace mit klarer Meldung; ohne Sandbox klappt es.
	repo := t.TempDir()
	gitInit(t, repo)
	var p project.View
	api(t, s, "POST", "/api/projects", `{"path":"`+repo+`"}`, http.StatusCreated, &p)
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", "", http.StatusBadRequest, nil)
	var w store.Workspace
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", `{"sandbox":false}`, http.StatusCreated, &w)
	if w.Sandbox {
		t.Error("Workspace in der Sandbox angelegt")
	}
	var wsb WorkspaceSandbox
	api(t, s, "GET", "/api/workspaces/"+w.ID+"/sandbox", "", http.StatusOK, &wsb)
	if wsb.Sandbox || wsb.Container != nil {
		t.Errorf("Workspace-Sandbox = %+v", wsb)
	}

	// Netzwerk-Ansicht: Events und Zusammenfassung je Host.
	now := time.Now()
	if err := st.AppendEgress(context.Background(), w.ID, []store.EgressEvent{
		{TS: now, Kind: "connect", Host: "registry.npmjs.org", Port: 443, Allowed: true, Reason: "allowlist", BytesIn: 100},
		{TS: now, Kind: "connect", Host: "example.com", Port: 443, Allowed: false, Reason: "not_allowed"},
		{TS: now, Kind: "connect", Host: "example.com", Port: 443, Allowed: false, Reason: "not_allowed"},
	}); err != nil {
		t.Fatal(err)
	}
	var eg WorkspaceEgress
	api(t, s, "GET", "/api/workspaces/"+w.ID+"/egress", "", http.StatusOK, &eg)
	if len(eg.Events) != 3 || len(eg.Hosts) != 2 || eg.Hosts[0].Host != "example.com" || eg.Hosts[0].Blocked != 2 || eg.Hosts[0].Reason != "not_allowed" ||
		eg.Hosts[1].Allowed != 1 || eg.Hosts[1].BytesIn != 100 {
		t.Errorf("Egress = %+v", eg)
	}
	api(t, s, "GET", "/api/workspaces/"+w.ID+"/egress?blocked=1", "", http.StatusOK, &eg)
	if len(eg.Events) != 2 || eg.Events[0].Allowed {
		t.Errorf("nur geblockt: %+v", eg.Events)
	}
	api(t, s, "GET", "/api/workspaces/unbekannt/egress", "", http.StatusNotFound, nil)
}

func gitInit(t *testing.T, repo string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
