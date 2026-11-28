package trigger

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// fakeGH is a gh script: issue list returns the contents of issues.json, issue comment
// appends the comment to comments.log, pr create returns a PR URL and stores the text.
type fakeGH struct {
	dir, path string
}

func newFakeGH(t *testing.T) *fakeGH {
	dir := t.TempDir()
	g := &fakeGH{dir: dir, path: filepath.Join(dir, "gh")}
	writeFile(t, g.path, `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/calls.log"
case "$1 $2" in
  "issue list") cat "$d/issues.json" ;;
  "issue comment") printf '#%s: ' "$3" >> "$d/comments.log"; cat >> "$d/comments.log"; echo >> "$d/comments.log" ;;
  "pr create") cat > "$d/pr.md"; echo https://github.com/o/r/pull/42 ;;
  *) echo "unknown: $*" >&2; exit 1 ;;
esac
`, 0o700)
	g.issues(t)
	return g
}

func (g *fakeGH) issues(t *testing.T, list ...map[string]any) {
	t.Helper()
	b, _ := json.Marshal(list)
	if len(list) == 0 {
		b = []byte("[]")
	}
	writeFile(t, filepath.Join(g.dir, "issues.json"), string(b), 0o600)
}

func (g *fakeGH) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(g.dir, name))
	return string(b)
}

func issueJSON(n int, title, author string) map[string]any {
	return map[string]any{"number": n, "title": title, "body": "Please " + title, "url": "https://github.com/o/r/issues/" + itoa(n),
		"author": map[string]string{"login": author}}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// fakeRuns remembers started runs; the status can be set.
type fakeRuns struct {
	mu     sync.Mutex
	reqs   []factory.StartRequest
	status map[string]string
	err    error
}

func (f *fakeRuns) Start(_ context.Context, req factory.StartRequest) (*factory.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.reqs = append(f.reqs, req)
	id := "run" + itoa(len(f.reqs))
	f.status[id] = store.RunRunning
	return &factory.View{Run: &store.Run{ID: id, Factory: req.Factory, Status: store.RunRunning}}, nil
}

func (f *fakeRuns) Get(_ context.Context, id string) (*factory.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &factory.View{Run: &store.Run{ID: id, Factory: "feature", Status: f.status[id]}}
	if v.Status == store.RunDone {
		v.PRURL = "https://github.com/o/r/pull/1"
	}
	if v.Status == store.RunFailed {
		v.Error = "step x: broken"
	}
	return v, nil
}

func (f *fakeRuns) set(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = status
}

type env struct {
	ctx  context.Context
	st   *store.Store
	ps   *project.Service
	p    *project.View
	gh   *fakeGH
	runs *fakeRuns
	now  time.Time
	svc  *Service
}

func setup(t *testing.T, projectYAML string) *env {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, ".ade", "project.yaml"), projectYAML, 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps := project.NewService(st)
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigError != "" {
		t.Fatal(p.ConfigError)
	}
	e := &env{ctx: ctx, st: st, ps: ps, p: p, gh: newFakeGH(t), runs: &fakeRuns{status: map[string]string{}},
		now: time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)}
	e.svc = New(Options{Store: st, Projects: ps, Runs: e.runs, GH: e.gh.path, Now: func() time.Time { return e.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return e
}

const issuesYAML = `triggers:
  - name: features
    factory: feature
    inputs: { area: backend }
    github_issues: { label: "ade:feature", authors: [alice, bob], interval: 5m }
`

func TestGitHubIssues(t *testing.T) {
	e := setup(t, issuesYAML)
	e.gh.issues(t, issueJSON(7, "Login page", "alice"), issueJSON(3, "Export", "bob"), issueJSON(9, "Foreign", "mallory"))
	e.svc.Check(e.ctx)

	if len(e.runs.reqs) != 2 {
		t.Fatalf("Runs %+v", e.runs.reqs)
	}
	r := e.runs.reqs[0] // smallest number first
	if r.Closes != 3 || r.Factory != "feature" || r.Trigger != "features" || !r.RequireBudget || r.Sandbox == nil || !*r.Sandbox {
		t.Fatalf("Start %+v", r)
	}
	if !strings.Contains(r.Task, "GitHub issue #3: Export") || !strings.Contains(r.Task, "Please Export") || r.Inputs["area"] != "backend" {
		t.Fatalf("task %q, inputs %v", r.Task, r.Inputs)
	}
	if r.TriggerRef != "https://github.com/o/r/issues/3" {
		t.Fatalf("Verweis %q", r.TriggerRef)
	}
	if c := e.gh.read("comments.log"); !strings.Contains(c, "#3: ADE: run `run1`") || !strings.Contains(c, "#7: ADE: run `run2`") || strings.Contains(c, "#9") {
		t.Fatalf("Kommentare %q", c)
	}
	if !strings.Contains(e.gh.read("calls.log"), "issue list --state open --label ade:feature") {
		t.Fatalf("calls %q", e.gh.read("calls.log"))
	}

	// No new poll before the interval expires; afterwards no duplicate runs.
	calls := strings.Count(e.gh.read("calls.log"), "issue list")
	e.now = e.now.Add(time.Minute)
	e.svc.Check(e.ctx)
	if strings.Count(e.gh.read("calls.log"), "issue list") != calls {
		t.Fatal("poll before the interval expired")
	}
	e.now = e.now.Add(5 * time.Minute)
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 2 || strings.Count(e.gh.read("calls.log"), "issue list") != calls+1 {
		t.Fatalf("started twice: %d runs", len(e.runs.reqs))
	}

	// Report the end: done with PR, failed; each only once.
	e.runs.set("run1", store.RunDone)
	e.runs.set("run2", store.RunFailed)
	e.svc.Check(e.ctx)
	e.svc.Check(e.ctx)
	c := e.gh.read("comments.log")
	if strings.Count(c, "pull/1") != 1 || strings.Count(c, "failed: step x: broken") != 1 {
		t.Fatalf("reports %q", c)
	}

	// View and pause.
	list, err := e.svc.List(e.ctx, e.p.ID)
	if err != nil || len(list) != 1 || len(list[0].Runs) != 2 || list[0].Next == nil || list[0].State.LastCheck == nil {
		t.Fatalf("Liste %+v %v", list, err)
	}
	if _, err := e.svc.SetPaused(e.ctx, e.p.ID, "features", true); err != nil {
		t.Fatal(err)
	}
	e.gh.issues(t, issueJSON(11, "New", "alice"))
	e.now = e.now.Add(time.Hour)
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 2 {
		t.Fatal("paused trigger started a run")
	}
	if _, err := e.svc.SetPaused(e.ctx, e.p.ID, "nope", true); err == nil {
		t.Fatal("unknown trigger without an error")
	}
}

func TestGitHubIssuesErrors(t *testing.T) {
	e := setup(t, issuesYAML)
	e.gh.issues(t, issueJSON(1, "A", "alice"))
	e.runs.err = errString("factory feature has no budget.max_cost_usd")
	e.svc.Check(e.ctx)
	st, _ := e.st.GetTriggerState(e.ctx, e.p.ID, "features")
	if !strings.Contains(st.LastError, "issue #1: factory feature has no budget") {
		t.Fatalf("error %q", st.LastError)
	}
	// A failed start is retried next time.
	e.runs.err = nil
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Check(e.ctx)
	st, _ = e.st.GetTriggerState(e.ctx, e.p.ID, "features")
	if len(e.runs.reqs) != 1 || st.LastError != "" {
		t.Fatalf("second attempt: %d runs, error %q", len(e.runs.reqs), st.LastError)
	}
	// gh fails.
	writeFile(t, filepath.Join(e.gh.dir, "issues.json"), "no json", 0o600)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Check(e.ctx)
	st, _ = e.st.GetTriggerState(e.ctx, e.p.ID, "features")
	if !strings.Contains(st.LastError, "not readable") {
		t.Fatalf("error %q", st.LastError)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSchedule(t *testing.T) {
	e := setup(t, `triggers:
  - name: nightly
    factory: deps
    sandbox: false
    allow_unbudgeted: true
    inputs: { task: "Update dependencies" }
    schedule: { cron: "0 3 * * *" }
`)
	// Seen for the first time: catch up on nothing.
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 0 {
		t.Fatal("run when first seen")
	}
	list, _ := e.svc.List(e.ctx, e.p.ID)
	if list[0].Next == nil || list[0].Next.Format("2006-01-02 15:04") != "2026-11-28 03:00" {
		t.Fatalf("next date %v", list[0].Next)
	}
	// At the date: one run.
	e.now = time.Date(2026, 11, 28, 3, 0, 20, 0, time.UTC)
	e.svc.Check(e.ctx)
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 1 {
		t.Fatalf("%d Runs", len(e.runs.reqs))
	}
	r := e.runs.reqs[0]
	if r.Inputs["task"] != "Update dependencies" || r.RequireBudget || *r.Sandbox || r.Closes != 0 || !strings.Contains(r.TriggerRef, "2026-11-28 03:00") {
		t.Fatalf("Start %+v", r)
	}
	// Next date while the previous run is still running: skipped.
	e.now = time.Date(2026, 11, 29, 3, 0, 5, 0, time.UTC)
	e.svc.Check(e.ctx)
	st, _ := e.st.GetTriggerState(e.ctx, e.p.ID, "nightly")
	if len(e.runs.reqs) != 1 || !strings.Contains(st.LastError, "still running") {
		t.Fatalf("%d runs, error %q", len(e.runs.reqs), st.LastError)
	}
	// The daemon was idle for three days: only the last missed date is caught up.
	e.runs.set("run1", store.RunDone)
	e.now = time.Date(2026, 12, 2, 12, 0, 0, 0, time.UTC)
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 2 || !strings.Contains(e.runs.reqs[1].TriggerRef, "2026-12-02 03:00") {
		t.Fatalf("Nachholen: %+v", e.runs.reqs)
	}
	// Schedule runs get no comment.
	e.runs.set("run2", store.RunDone)
	e.svc.Check(e.ctx)
	if e.gh.read("comments.log") != "" {
		t.Fatal("comment for a schedule run")
	}
}

func TestScheduleNoCatchUp(t *testing.T) {
	e := setup(t, `triggers:
  - name: hourly
    factory: deps
    allow_unbudgeted: true
    schedule: { cron: "@hourly", catch_up: false }
`)
	e.svc.Check(e.ctx)
	e.now = e.now.Add(3*time.Hour + 30*time.Minute)
	e.svc.Check(e.ctx)
	st, _ := e.st.GetTriggerState(e.ctx, e.p.ID, "hourly")
	if len(e.runs.reqs) != 0 || !strings.Contains(st.LastError, "missed") {
		t.Fatalf("%d runs, error %q", len(e.runs.reqs), st.LastError)
	}
	e.now = time.Date(2026, 11, 27, 18, 0, 30, 0, time.UTC)
	e.svc.Check(e.ctx)
	if len(e.runs.reqs) != 1 {
		t.Fatalf("punctual date: %d runs", len(e.runs.reqs))
	}
}

// TestIssueToPullRequest: a labeled issue starts a run with the real runner that ends as a
// PR (acceptance criterion M10); the PR closes the issue, the issue has the link.
func TestIssueToPullRequest(t *testing.T) {
	e := setup(t, `triggers:
  - name: issues
    factory: quick
    sandbox: false
    github_issues: { label: ade }
`)
	repo := e.p.RepoPath
	writeFile(t, filepath.Join(repo, ".ade", "factories", "quick.yaml"), `name: quick
inputs:
  task: { required: true }
roles:
  dev: {}
flow:
  - { step: implement, role: dev }
  - { step: deliver, action: pull_request, unattended: true }
budget: { max_cost_usd: 1 }
`, 0o600)
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "fabrik")
	remote := t.TempDir()
	gitRun(t, remote, "init", "-q", "--bare")
	gitRun(t, repo, "remote", "add", "origin", remote)

	dataDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wm := workspace.NewManager(e.st, e.ps, t.TempDir(), log)
	t.Cleanup(wm.Close)
	wm.SetGitHubCLI(e.gh.path)
	fa := &fake.Adapter{AdapterName: "claude", Argv: []string{"sh", "-c", `while read -r l; do
  echo changed > "file-$ADE_SESSION_ID"
  printf '%s\n' "$l"
  echo '{"type":"turn_done"}'
done`}}
	agents := agent.NewManager(agent.Options{Store: e.st, Projects: e.ps, Workspaces: wm, Adapters: adapter.NewRegistry(fa),
		DataDir: dataDir, Logger: log, DefaultAdapter: "claude", FlushInterval: 10 * time.Millisecond, StopTimeout: 2 * time.Second})
	wm.SetBusyCheck(agents.WorkspaceBusy)
	approvals := approval.New(approval.Options{Store: e.st, Logger: log})
	runner := factory.New(factory.Options{Store: e.st, Projects: e.ps, Workspaces: wm, Agents: agents, Gates: approvals,
		DataDir: dataDir, Logger: log, Tick: 100 * time.Millisecond})
	if err := runner.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runner.Close(); approvals.Close(); agents.Close() })
	svc := New(Options{Store: e.st, Projects: e.ps, Runs: runner, GH: e.gh.path, Logger: log})

	e.gh.issues(t, issueJSON(5, "Create file", "alice"))
	svc.Check(e.ctx)
	trs, _ := e.st.ListTriggerRuns(e.ctx, store.TriggerRunFilter{ProjectID: e.p.ID})
	if len(trs) != 1 {
		t.Fatalf("triggered runs %+v (error: %v)", trs, func() string { s, _ := e.st.GetTriggerState(e.ctx, e.p.ID, "issues"); return s.LastError }())
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, err := runner.Get(e.ctx, trs[0].RunID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status == store.RunDone {
			if v.Trigger != "issues" || v.Closes != 5 || v.PRURL != "https://github.com/o/r/pull/42" {
				t.Fatalf("Run %+v", v.Run)
			}
			break
		}
		if !store.RunActive(v.Status) {
			t.Fatalf("run ended with %s: %s", v.Status, v.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pr := e.gh.read("pr.md"); !strings.HasPrefix(pr, "Closes #5\n") || !strings.Contains(pr, "trigger `issues`") {
		t.Fatalf("PR-Text:\n%s", pr)
	}
	svc.Check(e.ctx)
	if c := e.gh.read("comments.log"); !strings.Contains(c, "#5: ADE: run") || !strings.Contains(c, "pull/42") {
		t.Fatalf("Kommentare %q", c)
	}
}
