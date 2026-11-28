package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func newProjectServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	ws := workspace.NewManager(st, ps, filepath.Join(t.TempDir(), "ws"), log)
	t.Cleanup(ws.Close)
	return New(Options{Version: "test", Port: "7000", Store: st, Auth: NewAuth(testSecret), Logger: log, Projects: ps, Workspaces: ws})
}

func api(t *testing.T, s http.Handler, method, path, body string, wantCode int, out any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "localhost:7000"
	r.Header.Set("Authorization", "Bearer "+testSecret)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != wantCode {
		t.Fatalf("%s %s: Code = %d, want %d, Body = %s", method, path, w.Code, wantCode, w.Body.String())
	}
	if out != nil {
		if err := json.NewDecoder(w.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

func TestProjectWorkspaceFlow(t *testing.T) {
	s := newProjectServer(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}

	// Locked without auth.
	r := httptest.NewRequest("GET", "/api/projects", nil)
	r.Host = "localhost:7000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without auth: code = %d", w.Code)
	}

	api(t, s, "POST", "/api/projects", `{"path":"`+t.TempDir()+`"}`, http.StatusBadRequest, nil)
	api(t, s, "POST", "/api/projects", `{"pathh":"x"}`, http.StatusBadRequest, nil)

	var p project.View
	api(t, s, "POST", "/api/projects", `{"path":"`+repo+`"}`, http.StatusCreated, &p)
	if p.ID == "" || p.BaseBranch != "main" {
		t.Fatalf("project = %#v", p)
	}
	api(t, s, "POST", "/api/projects", `{"path":"`+repo+`"}`, http.StatusConflict, nil)

	var list []project.View
	api(t, s, "GET", "/api/projects", "", http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("Projekte = %v", list)
	}
	api(t, s, "GET", "/api/projects/"+p.Name, "", http.StatusOK, nil)
	api(t, s, "GET", "/api/projects/gibtsnicht", "", http.StatusNotFound, nil)

	var ws store.Workspace
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", "", http.StatusCreated, &ws)
	if ws.Status != store.WorkspaceReady {
		t.Fatalf("Workspace = %#v", ws)
	}
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", `{"base_branch":"missing"}`, http.StatusBadRequest, nil)

	if err := os.WriteFile(filepath.Join(ws.Path, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var d git.Diff
	api(t, s, "GET", "/api/workspaces/"+ws.ID+"/diff", "", http.StatusOK, &d)
	if len(d.Files) != 1 || d.Files[0].Path != "new.txt" || d.Files[0].Patch == "" {
		t.Fatalf("Diff = %#v", d)
	}
	var noPatch git.Diff
	api(t, s, "GET", "/api/workspaces/"+ws.ID+"/diff?patch=false", "", http.StatusOK, &noPatch)
	if noPatch.Files[0].Patch != "" {
		t.Error("Patch trotz patch=false")
	}

	var views []workspace.View
	api(t, s, "GET", "/api/projects/"+p.ID+"/workspaces", "", http.StatusOK, &views)
	if len(views) != 1 || views[0].Changes == nil || views[0].Changes.Files != 1 {
		t.Fatalf("Workspaces = %#v", views)
	}

	api(t, s, "DELETE", "/api/projects/"+p.ID, "", http.StatusConflict, nil)
	api(t, s, "DELETE", "/api/projects/"+p.ID+"?force=true", "", http.StatusNoContent, nil)
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Error("workspace still exists after force delete")
	}
	api(t, s, "GET", "/api/workspaces/"+ws.ID, "", http.StatusNotFound, nil)
}
