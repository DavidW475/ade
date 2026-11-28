package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func TestReviewRoutes(t *testing.T) {
	s := newProjectServer(t)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")

	var p project.View
	api(t, s, "POST", "/api/projects", `{"path":"`+repo+`"}`, http.StatusCreated, &p)
	var ws store.Workspace
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", "", http.StatusCreated, &ws)
	base := "/api/workspaces/" + ws.ID

	// Alle Review-Routen brauchen Anmeldung.
	for _, rt := range [][2]string{
		{"GET", base + "/file?path=a.txt"}, {"POST", base + "/collect"}, {"POST", base + "/merge"},
		{"GET", base + "/pull-request"}, {"POST", base + "/pull-request"},
	} {
		r := httptest.NewRequest(rt[0], rt[1], nil)
		r.Host = "localhost:7000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: %d", rt[0], rt[1], w.Code)
		}
	}

	if err := os.WriteFile(filepath.Join(ws.Path, "a.txt"), []byte("from the workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var fc workspace.FileContents
	api(t, s, "GET", base+"/file?path=a.txt", "", http.StatusOK, &fc)
	if fc.Original.Text != "a\n" || fc.Modified.Text != "from the workspace\n" {
		t.Errorf("file = %#v", fc)
	}
	api(t, s, "GET", base+"/file?path=../x", "", http.StatusBadRequest, nil)

	var draft workspace.PRDraft
	api(t, s, "GET", base+"/pull-request", "", http.StatusOK, &draft)
	if draft.Head != ws.Branch || draft.RemoteURL != "" {
		t.Errorf("Entwurf = %#v", draft)
	}
	api(t, s, "POST", base+"/pull-request", `{}`, http.StatusBadRequest, nil)

	// Conflict: main changes the same line.
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("in the repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "parallel")
	var conflict MergeConflictResponse
	api(t, s, "POST", base+"/merge", "", http.StatusConflict, &conflict)
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0] != "a.txt" {
		t.Errorf("Konflikt = %#v", conflict)
	}

	var collected store.Workspace
	api(t, s, "POST", base+"/collect", "", http.StatusOK, &collected)
	if collected.HeadCommit == "" || collected.HeadCommit == collected.BaseCommit {
		t.Errorf("collect = %#v", collected)
	}

	api(t, s, "DELETE", base, "", http.StatusNoContent, nil)
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", ws.Branch).Output()
	if len(out) != 0 {
		t.Errorf("branch still there after discarding: %s", out)
	}
}
