package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/trigger"
	"github.com/DavidW475/ade/internal/workspace"
)

func TestRunsAPI(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	ws := workspace.NewManager(st, ps, filepath.Join(t.TempDir(), "ws"), log)
	t.Cleanup(ws.Close)
	// Fake als „claude“: spiegelt die Anweisung und wartet (die Runde endet nie).
	am := agent.NewManager(agent.Options{
		Store: st, Projects: ps, Workspaces: ws, Adapters: adapter.NewRegistry(&fake.Adapter{AdapterName: "claude"}),
		DataDir: dataDir, Logger: log, DefaultAdapter: "claude", FlushInterval: 10 * time.Millisecond,
	})
	ws.SetBeforeDelete(am.BeforeWorkspaceDelete)
	approvals := approval.New(approval.Options{Store: st, Logger: log})
	runs := factory.New(factory.Options{Store: st, Projects: ps, Workspaces: ws, Agents: am, Gates: approvals, DataDir: dataDir, Logger: log})
	t.Cleanup(func() { runs.Close(); approvals.Close(); am.Close() })

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	h := New(Options{Version: "test", Port: port, Store: st, Auth: NewAuth(testSecret), Logger: log, Projects: ps, Workspaces: ws, Agents: am, Approvals: approvals, Runs: runs,
		Triggers: trigger.New(trigger.Options{Store: st, Projects: ps, Runs: runs, Logger: log})})
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)
	a := &agentServer{srv: srv, handler: h, base: "http://localhost:" + port, project: p.ID, st: st}

	// Ohne Anmeldung nichts.
	req, _ := http.NewRequest(http.MethodGet, a.base+"/api/runs", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ohne Auth: %d", res.StatusCode)
	}

	var list []factory.Summary
	a.call(t, "GET", "/api/factories?project_id="+p.ID, "", http.StatusOK, &list)
	if len(list) != 1 || list[0].Name != "feature" || list[0].Source != factory.SourceBuiltin || len(list[0].Steps) != 6 {
		t.Fatalf("Fabriken %+v", list)
	}
	a.call(t, "GET", "/api/factories", "", http.StatusBadRequest, nil)
	a.call(t, "POST", "/api/runs", `{"project_id":"`+p.ID+`","factory":"feature"}`, http.StatusBadRequest, nil)
	a.call(t, "POST", "/api/runs", `{"project_id":"`+p.ID+`","factory":"gibtsnicht","task":"x"}`, http.StatusNotFound, nil)

	var v factory.View
	a.call(t, "POST", "/api/runs", `{"project_id":"`+p.ID+`","factory":"feature","task":"Baue X"}`, http.StatusCreated, &v)
	if v.ID == "" || v.Status != store.RunRunning || v.Task != "Baue X" || v.WorkspaceID == "" {
		t.Fatalf("Run %+v", v.Run)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.call(t, "GET", "/api/runs/"+v.ID, "", http.StatusOK, &v)
		if v.Steps[0].Status == store.StepRunning && v.Steps[0].SessionID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Planer startet nicht: %+v", v.Steps[0])
		}
		time.Sleep(20 * time.Millisecond)
	}
	var runsList []factory.View
	a.call(t, "GET", "/api/runs?project_id="+p.ID+"&active=1", "", http.StatusOK, &runsList)
	if len(runsList) != 1 {
		t.Fatalf("Liste %+v", runsList)
	}
	a.call(t, "GET", "/api/runs/"+v.ID+"/artifacts/plan.md", "", http.StatusNotFound, nil)
	a.call(t, "GET", "/api/runs/"+v.ID+"/artifacts/..%2Fade.db", "", http.StatusNotFound, nil)
	a.call(t, "POST", "/api/runs/"+v.ID+"/cancel", "", http.StatusOK, &v)
	if v.Status != store.RunCanceled {
		t.Fatalf("nach Abbruch %s", v.Status)
	}
	a.call(t, "POST", "/api/runs/"+v.ID+"/cancel", "", http.StatusConflict, nil)
	a.call(t, "GET", "/api/runs/gibtsnicht", "", http.StatusNotFound, nil)

	// Auslöser: ohne Anmeldung nichts; ohne triggers in project.yaml eine leere Liste.
	req, _ = http.NewRequest(http.MethodGet, a.base+"/api/projects/"+p.ID+"/triggers", nil)
	if res, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Auslöser ohne Auth: %d", res.StatusCode)
	}
	var triggers []trigger.View
	a.call(t, "GET", "/api/projects/"+p.ID+"/triggers", "", http.StatusOK, &triggers)
	if triggers == nil || len(triggers) != 0 {
		t.Fatalf("Auslöser %+v", triggers)
	}
	a.call(t, "POST", "/api/projects/"+p.ID+"/triggers/x/pause", "", http.StatusNotFound, nil)
	a.call(t, "GET", "/api/projects/gibtsnicht/triggers", "", http.StatusNotFound, nil)
}
