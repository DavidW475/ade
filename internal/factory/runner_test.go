package factory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testFactory: Planer (lesend) → Gate → Entwickler (schreibend) → Reviewer (lesend) → Gate → PR.
const testFactory = `name: t
inputs:
  task: { required: true }
roles:
  planner: { permissions: read, outputs: [plan.md], prompt: prompts/planner.md }
  implementer: { inputs: [plan.md], outputs: [summary.md] }
  reviewer: { permissions: read, inputs: [plan.md, summary.md], outputs: [review.md] }
flow:
  - { step: plan, role: planner }
  - { step: approve-plan, gate: human }
  - { step: implement, role: implementer }
  - { step: review, role: reviewer }
  - { step: final, gate: human }
  - { step: deliver, action: pull_request }
`

// structFactory: reviewer with a structured artifact → implementer.
const structFactory = `name: s
inputs:
  task: { required: true }
roles:
  reviewer: { permissions: read, outputs: [{ name: review.json, schema: schemas/review.json }] }
  implementer: { inputs: [review.json], outputs: [summary.md] }
flow:
  - { step: review, role: reviewer }
  - { step: fix, role: implementer }
`

// inputs returns all instructions to the fake agent in step step (JSON-encoded).
func (e *env) inputs(step string) string {
	b, _ := os.ReadFile(filepath.Join(e.resp, step+".log"))
	return string(b)
}

// loopRunFactory: implementation → review → jump back on change request → gate with jump back → PR.
const loopRunFactory = `name: l
inputs:
  task: { required: true }
roles:
  implementer: { inputs: [review.json], outputs: [summary.md] }
  reviewer: { permissions: read, inputs: [summary.md], outputs: [{ name: review.json, schema: schemas/review.json }] }
  writer: {}
flow:
  - { step: implement, role: implementer }
  - { step: review, role: reviewer }
  - { step: decide, when: "review.verdict == 'changes'", goto: implement }
  - { step: docs, role: writer, when: "len(review.comments) > 5" }
  - { step: final, gate: human, on_deny: { goto: implement } }
  - { step: deliver, action: pull_request }
`

// parallelRunFactory: implementation → review (read-only) and test (writing) in parallel → jump back
// as long as tests fail.
const parallelRunFactory = `name: p
inputs:
  task: { required: true }
roles:
  implementer: { inputs: [review.json, test.json], outputs: [summary.md] }
  reviewer: { permissions: read, inputs: [summary.md], outputs: [{ name: review.json, schema: schemas/review.json }] }
  tester: { inputs: [summary.md], outputs: [test.json] }
flow:
  - { step: implement, role: implementer }
  - parallel:
      - { step: review, role: reviewer }
      - { step: test, role: tester }
  - { step: decide, when: "review.verdict == 'changes' || test.failed > 0", goto: implement }
`

// unattendedFactory: implementer → pull_request without a gate (#103).
const unattendedFactory = `name: u
inputs:
  task: { required: true }
roles:
  dev: {}
flow:
  - { step: implement, role: dev }
  - { step: deliver, action: pull_request, unattended: true }
`

// answer sets the next answers of the fake agent in step step.
func (e *env) answer(step string, texts ...string) {
	e.t.Helper()
	var b strings.Builder
	for _, t := range texts {
		line, err := json.Marshal(map[string]any{"type": "message", "payload": map[string]string{"role": "assistant", "text": t}})
		if err != nil {
			e.t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeFile(e.t, filepath.Join(e.resp, step), b.String())
}

type env struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	ps      *project.Service
	ws      *workspace.Manager
	project *project.View
	dataDir string
	flag    string // as long as the file is missing, the fake agent does not finish its turn
	resp    string // answers of the fake agent per step (one line per turn, otherwise echo)
	gh      string
	log     *slog.Logger

	agents    *agent.Manager
	approvals *approval.Service
	runner    *Runner
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	for p, c := range map[string]string{
		"README.md":                          "hello\n",
		".ade/factories/t.yaml":              testFactory,
		".ade/factories/prompts/planner.md":  "You plan.",
		".ade/factories/s.yaml":              structFactory,
		".ade/factories/p.yaml":              parallelRunFactory,
		".ade/factories/l.yaml":              loopRunFactory,
		".ade/factories/u.yaml":              unattendedFactory,
		".ade/factories/schemas/review.json": reviewSchema,
	} {
		writeFile(t, filepath.Join(repo, p), c)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	remote := t.TempDir()
	gitRun(t, remote, "init", "-q", "--bare")
	gitRun(t, repo, "remote", "add", "origin", remote)

	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	wm := workspace.NewManager(st, ps, t.TempDir(), log)
	t.Cleanup(wm.Close)
	gh := filepath.Join(t.TempDir(), "gh")
	writeFile(t, gh, "#!/bin/sh\ncat > /dev/null\necho https://github.com/o/r/pull/9\n")
	if err := os.Chmod(gh, 0o700); err != nil { //nolint:gosec // G302: Testskript
		t.Fatal(err)
	}
	wm.SetGitHubCLI(gh)
	e := &env{t: t, ctx: ctx, st: st, ps: ps, ws: wm, project: p, dataDir: dataDir, flag: filepath.Join(t.TempDir(), "go"), resp: t.TempDir(), gh: gh, log: log}
	writeFile(t, e.flag, "")
	e.boot()
	t.Cleanup(e.shutdown)
	return e
}

// script is the fake agent: reports a resume ID, writes a file into the working directory,
// waits until the flag file exists (and no hold file <step>.hold), and then answers with the
// next prepared answer of its step or mirrors the instruction. It remembers the step per
// session (for follow-up instructions and resume).
func (e *env) script() []string {
	return []string{"sh", "-c", `echo '{"type":"resume_id","payload":{"id":"r1"}}'
mem="$2/session-$ADE_SESSION_ID"
step=$(cat "$mem" 2>/dev/null)
while read -r l; do
  s=$(printf '%s' "$l" | sed -n 's/.*You are one step (\([a-z0-9_-]*\)).*/\1/p')
  [ -n "$s" ] && step=$s && printf '%s' "$step" > "$mem"
  f="$2/$step"
  [ -n "$step" ] && printf '%s\n' "$l" >> "$f.log"
  echo "$ADE_SESSION_ID" > "touched-$ADE_SESSION_ID"
  while [ ! -e "$1" ] || [ -e "$2/$step.hold" ]; do sleep 0.05; done
  if [ -n "$step" ] && [ -s "$f" ]; then head -n 1 "$f"; sed -i 1d "$f"; else printf '%s\n' "$l"; fi
  echo '{"type":"turn_done"}'
done`, "sh", e.flag, e.resp}
}

// boot starts agent manager, approvals and runner (like the daemon, also after a restart).
func (e *env) boot() {
	e.t.Helper()
	fa := &fake.Adapter{AdapterName: "claude", Argv: e.script()}
	e.agents = agent.NewManager(agent.Options{
		Store: e.st, Projects: e.ps, Workspaces: e.ws, Adapters: adapter.NewRegistry(fa), DataDir: e.dataDir,
		Logger: e.log, DefaultAdapter: "claude", FlushInterval: 10 * time.Millisecond, StopTimeout: 2 * time.Second,
	})
	e.ws.SetBusyCheck(e.agents.WorkspaceBusy)
	e.ws.SetBeforeDelete(e.agents.BeforeWorkspaceDelete)
	e.approvals = approval.New(approval.Options{Store: e.st, Logger: e.log})
	if err := e.approvals.Recover(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.agents.Recover(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.runner = New(Options{Store: e.st, Projects: e.ps, Workspaces: e.ws, Agents: e.agents, Gates: e.approvals,
		DataDir: e.dataDir, Logger: e.log, Tick: 200 * time.Millisecond})
	if err := e.runner.Recover(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) shutdown() {
	if e.runner == nil {
		return
	}
	e.runner.Close()
	e.approvals.Close()
	e.agents.Close()
	e.runner = nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) run(id string) *store.Run {
	e.t.Helper()
	r, err := e.st.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// waitStep waits until the run is at step name with status status.
func (e *env) waitStep(id, name, status string) store.RunStep {
	e.t.Helper()
	var st store.RunStep
	waitFor(e.t, name+" "+status, func() bool {
		r := e.run(id)
		if !store.RunActive(r.Status) && r.Status != store.RunDone {
			e.t.Fatalf("run ended with %s: %s", r.Status, r.Error)
		}
		for _, s := range r.Steps {
			if s.Name == name && s.Status == status {
				st = s
				return true
			}
		}
		return false
	})
	return st
}

func (e *env) decide(approvalID, decision, reason string) {
	e.t.Helper()
	if _, err := e.approvals.Decide(e.ctx, approvalID, decision, reason); err != nil {
		e.t.Fatal(err)
	}
}

func TestRunThroughWithRestart(t *testing.T) {
	e := setup(t)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "t", Task: "Build X"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != SourceProject || len(v.Steps) != 6 || v.WorkspaceID == "" {
		t.Fatalf("Run %+v", v.Run)
	}
	ws, err := e.ws.Get(e.ctx, v.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}

	// Planner: the answer becomes plan.md, the prompt contains role prompt and task; read access
	// leaves the workspace unchanged.
	gate := e.waitStep(v.ID, "approve-plan", store.StepWaiting)
	plan, err := e.runner.ReadArtifact(e.ctx, v.ID, "plan.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"echo: You plan.", "## Task", "Build X", "only have read access", "`plan.md`"} {
		if !strings.Contains(string(plan), want) {
			t.Errorf("plan.md without %q:\n%s", want, plan)
		}
	}
	planner := e.run(v.ID).Steps[0]
	if _, err := os.Stat(filepath.Join(ws.Path, "touched-"+planner.SessionID)); err == nil {
		t.Fatal("the read-only role created a file in the workspace")
	}
	if s, _ := e.st.GetSession(e.ctx, planner.SessionID); !s.ReadOnly || s.Role != "planner" || s.Status != store.SessionDone {
		t.Fatalf("Planer-Session %+v", s)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "snapshots", planner.SessionID)); err == nil {
		t.Fatal("the planner's snapshot was left behind")
	}

	// Restart at the gate: the old approval expires, the runner creates a new one.
	e.shutdown()
	e.boot()
	e.waitStep(v.ID, "approve-plan", store.StepWaiting)
	waitFor(t, "new approval", func() bool { return e.run(v.ID).Steps[1].ApprovalID != gate.ApprovalID })
	gate2 := e.run(v.ID).Steps[1]
	if a, _ := e.st.GetApproval(e.ctx, gate2.ApprovalID); a.Status != store.ApprovalPending || a.Kind != store.ApprovalGate {
		t.Fatalf("approval after restart %+v", a)
	}

	// Approval with a note → the implementer gets plan and note. It gets stuck in the middle of
	// the turn (flag missing), the daemon restarts, the session is resumed.
	if err := os.Remove(e.flag); err != nil {
		t.Fatal(err)
	}
	e.decide(gate2.ApprovalID, store.DecisionAllow, "Please with tests")
	impl := e.waitStep(v.ID, "implement", store.StepRunning)
	waitFor(t, "Entwickler arbeitet", func() bool {
		_, err := os.Stat(filepath.Join(ws.Path, "touched-"+impl.SessionID))
		return err == nil
	})
	e.shutdown()
	if s, _ := e.st.GetSession(e.ctx, impl.SessionID); s.Status != store.SessionInterrupted {
		t.Fatalf("implementer after shutdown: %s", s.Status)
	}
	writeFile(t, e.flag, "")
	e.boot()
	final := e.waitStep(v.ID, "final", store.StepWaiting)
	summary, err := e.runner.ReadArtifact(e.ctx, v.ID, "summary.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), agent.ResumePrompt) {
		t.Errorf("summary.md does not come from the resumed turn:\n%s", summary)
	}
	// The writing role changed the workspace, ADE saved the state.
	if out := gitRun(t, ws.Path, "log", "--name-only", "--format=", ws.BaseCommit+"..HEAD"); !strings.Contains(out, "touched-"+impl.SessionID) {
		t.Fatalf("change of the implementer not committed: %q", out)
	}
	review, _ := e.runner.ReadArtifact(e.ctx, v.ID, "review.md")
	for _, want := range []string{"Artifact plan.md", "Artifact summary.md"} {
		if !strings.Contains(string(review), want) {
			t.Errorf("reviewer prompt without %q", want)
		}
	}

	// Letztes Gate → PR.
	e.decide(final.ApprovalID, store.DecisionAllow, "")
	waitFor(t, "run done", func() bool { return e.run(v.ID).Status == store.RunDone })
	got, err := e.runner.Get(e.ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRURL != "https://github.com/o/r/pull/9" || len(got.Artifacts) != 3 || got.EndedAt == nil {
		t.Fatalf("Ende %+v", got)
	}
	for _, st := range got.Steps {
		if st.Status != store.StepDone {
			t.Errorf("step %s: %s", st.Name, st.Status)
		}
	}
}

func TestRunGateDenyAndCancel(t *testing.T) {
	e := setup(t)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "t", Task: "Y"})
	if err != nil {
		t.Fatal(err)
	}
	gate := e.waitStep(v.ID, "approve-plan", store.StepWaiting)
	e.decide(gate.ApprovalID, store.DecisionDeny, "falscher Ansatz")
	waitFor(t, "run canceled", func() bool { return e.run(v.ID).Status == store.RunCanceled })
	if r := e.run(v.ID); !strings.Contains(r.Error, "falscher Ansatz") || r.Steps[1].Status != store.StepCanceled {
		t.Fatalf("%+v", r)
	}

	// Cancel while a role is working: the session is stopped.
	if err := os.Remove(e.flag); err != nil {
		t.Fatal(err)
	}
	v2, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "t", Task: "Z"})
	if err != nil {
		t.Fatal(err)
	}
	st := e.waitStep(v2.ID, "plan", store.StepRunning)
	got, err := e.runner.Cancel(e.ctx, v2.ID)
	if err != nil || got.Status != store.RunCanceled {
		t.Fatalf("Cancel: %+v %v", got, err)
	}
	waitFor(t, "session stopped", func() bool {
		s, _ := e.st.GetSession(e.ctx, st.SessionID)
		return s.Status == store.SessionStopped
	})
	if _, err := e.runner.Cancel(e.ctx, v2.ID); err == nil {
		t.Fatal("second cancel without an error")
	}
}

// TestRunUnattendedSensitive: pull_request without a gate waits for an approval if the agent
// changes a workflow; without such a change the PR is created directly (#103).
func TestRunUnattendedSensitive(t *testing.T) {
	e := setup(t)
	hold := filepath.Join(e.resp, "implement.hold")
	writeFile(t, hold, "")
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "u", Task: "CI"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStep(v.ID, "implement", store.StepRunning)
	ws, err := e.ws.Get(e.ctx, v.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ws.Path, ".github", "workflows", "ci.yml"), "on: pull_request\n")
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	st := e.waitStep(v.ID, "deliver", store.StepWaiting)
	a, err := e.st.GetApproval(e.ctx, st.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	var d gateDetail
	if err := json.Unmarshal(a.Detail, &d); err != nil {
		t.Fatal(err)
	}
	if a.Kind != store.ApprovalGate || !d.Unattended || len(d.Sensitive) != 1 || d.Sensitive[0] != ".github/workflows/ci.yml" || d.OnDeny != "" {
		t.Fatalf("approval %+v %+v", a, d)
	}
	if e.run(v.ID).Status != store.RunWaiting {
		t.Fatal("run does not wait")
	}
	if got, _ := e.ws.Get(e.ctx, v.WorkspaceID); got.PRURL != "" {
		t.Fatal("PR before the approval")
	}
	e.decide(st.ApprovalID, store.DecisionAllow, "")
	waitFor(t, "run done", func() bool { return e.run(v.ID).Status == store.RunDone })
	if got, _ := e.runner.Get(e.ctx, v.ID); got.PRURL == "" {
		t.Fatalf("no PR: %+v", got.Run)
	}

	// Without a sensitive change: PR without approval.
	v2, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "u", Task: "Doku"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run 2 done", func() bool { return e.run(v2.ID).Status == store.RunDone })
	if r := e.run(v2.ID); r.Steps[1].ApprovalID != "" {
		t.Fatalf("approval without a sensitive change: %+v", r.Steps[1])
	}

	// Denying cancels the run, no PR is created.
	writeFile(t, hold, "")
	v3, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "u", Task: "ADE"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStep(v3.ID, "implement", store.StepRunning)
	ws3, _ := e.ws.Get(e.ctx, v3.WorkspaceID)
	writeFile(t, filepath.Join(ws3.Path, ".ade", "project.yaml"), "setup: [curl evil]\n")
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	st3 := e.waitStep(v3.ID, "deliver", store.StepWaiting)
	e.decide(st3.ApprovalID, store.DecisionDeny, "nein")
	waitFor(t, "run 3 canceled", func() bool { return e.run(v3.ID).Status == store.RunCanceled })
	if got, _ := e.ws.Get(e.ctx, v3.WorkspaceID); got.PRURL != "" {
		t.Fatal("PR trotz Ablehnung")
	}
}

// TestGateShowsSensitive: the gate before the PR names changed workflows.
func TestGateShowsSensitive(t *testing.T) {
	e := setup(t)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "t", Task: "X"})
	if err != nil {
		t.Fatal(err)
	}
	g := e.waitStep(v.ID, "approve-plan", store.StepWaiting)
	ws, _ := e.ws.Get(e.ctx, v.WorkspaceID)
	writeFile(t, filepath.Join(ws.Path, ".github", "actions", "a", "action.yml"), "runs: {}\n")
	e.decide(g.ApprovalID, store.DecisionAllow, "")
	final := e.waitStep(v.ID, "final", store.StepWaiting)
	a, err := e.st.GetApproval(e.ctx, final.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	var d gateDetail
	if err := json.Unmarshal(a.Detail, &d); err != nil {
		t.Fatal(err)
	}
	if d.Unattended || len(d.Sensitive) != 1 || d.Sensitive[0] != ".github/actions/a/action.yml" {
		t.Fatalf("Gate %+v", d)
	}
}

func TestRunStartErrors(t *testing.T) {
	e := setup(t)
	for name, req := range map[string]StartRequest{
		"factory missing": {ProjectID: e.project.ID, Factory: "doesnotexist", Task: "x"},
		"task missing":    {ProjectID: e.project.ID, Factory: "t"},
		"project missing": {Factory: "t", Task: "x"},
		"foreign input":   {ProjectID: e.project.ID, Factory: "t", Task: "x", Inputs: map[string]string{"foo": "1"}},
		// Template: model opus is not an alias here and goes to the CLI unchanged.
		"Vorlage feature": {ProjectID: e.project.ID, Factory: "feature", Task: "x"},
	} {
		_, err := e.runner.Start(e.ctx, req)
		if name == "Vorlage feature" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	list, err := e.runner.Factories(e.ctx, e.project.ID)
	if err != nil || len(list) != 9 {
		t.Fatalf("Fabriken %+v %v", list, err)
	}
}

func TestRunStructuredArtifact(t *testing.T) {
	e := setup(t)
	e.answer("review", `{"verdict": "maybe"}`, "Here:\n```json\n{\"verdict\": \"changes\", \"comments\": [\"test missing\"]}\n```")
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "s", Task: "Check"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run done", func() bool { return e.run(v.ID).Status == store.RunDone })
	r := e.run(v.ID)
	if r.Steps[0].Fixes != 1 || r.Steps[0].Error != "" {
		t.Fatalf("reviewer step %+v", r.Steps[0])
	}
	review, err := e.runner.ReadArtifact(e.ctx, v.ID, "review.json")
	if err != nil || string(review) != "{\n  \"verdict\": \"changes\",\n  \"comments\": [\n    \"test missing\"\n  ]\n}" {
		t.Fatalf("review.json %q %v", review, err)
	}
	// The role knew the schema and got the check errors.
	for _, want := range []string{"JSON Schema for `review.json`", `$.verdict: only`, "field comments missing"} {
		if !strings.Contains(e.inputs("review"), want) {
			t.Errorf("instructions to the reviewer without %q:\n%s", want, e.inputs("review"))
		}
	}
	summary, _ := e.runner.ReadArtifact(e.ctx, v.ID, "summary.md")
	if !strings.Contains(string(summary), "```json") || !strings.Contains(string(summary), "test missing") {
		t.Fatalf("implementer did not get review.json as JSON:\n%s", summary)
	}

	// If the answer stays invalid, the run fails after MaxFixes corrections.
	e.answer("review", "no", "still not", `{"verdict": "approve"}`)
	v2, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "s", Task: "Check"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Run gescheitert", func() bool { return e.run(v2.ID).Status == store.RunFailed })
	if r := e.run(v2.ID); !strings.Contains(r.Error, "still invalid after 2 corrections") || r.Steps[0].Fixes != 2 {
		t.Fatalf("Run %+v", r)
	}
}

func TestRunLoop(t *testing.T) {
	e := setup(t)
	e.answer("review",
		`{"verdict": "changes", "comments": ["test missing"]}`,
		`{"verdict": "approve", "comments": []}`,
		`{"verdict": "approve", "comments": []}`)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "l", Task: "Baue Y"})
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer asks for changes → second pass → approval at the gate waits.
	final := e.waitStep(v.ID, "final", store.StepWaiting)
	impl := strings.Split(strings.TrimSpace(e.inputs("implement")), "\n")
	if len(impl) != 2 {
		t.Fatalf("implementer got %d instructions, want 2", len(impl))
	}
	if strings.Contains(impl[0], "Artifact review.json") || strings.Contains(impl[0], "## Pass") {
		t.Errorf("first pass with review feedback: %s", impl[0])
	}
	for _, want := range []string{"Pass 2", "test missing", "Artifact review.json (step review, role reviewer)", "back to implement"} {
		if !strings.Contains(impl[1], want) {
			t.Errorf("second pass without %q:\n%s", want, impl[1])
		}
	}
	r := e.run(v.ID)
	if r.Steps[3].Status != store.StepSkipped || r.Steps[2].Status != store.StepDone || r.Steps[0].Iteration != 2 {
		t.Fatalf("Schritte %+v", r.Steps)
	}
	if len(r.History) != 3 || r.History[2].Name != "decide" || r.History[2].Status != store.StepLooped {
		t.Fatalf("History %+v", r.History)
	}

	// Restart in the middle of the loop, then deny at the gate with a reason → third pass.
	e.shutdown()
	e.boot()
	waitFor(t, "new approval", func() bool { return e.run(v.ID).Steps[4].ApprovalID != final.ApprovalID })
	gateID := e.run(v.ID).Steps[4].ApprovalID
	ga, err := e.st.GetApproval(e.ctx, gateID)
	if err != nil {
		t.Fatal(err)
	}
	var gd gateDetail
	if err := json.Unmarshal(ga.Detail, &gd); err != nil || gd.OnDeny != "implement" || gd.Iteration != 1 || gd.MaxIterations != DefaultMaxIterations {
		t.Fatalf("Gate-Detail %s %v", ga.Detail, err)
	}
	if _, err := e.approvals.Decide(e.ctx, gateID, store.DecisionDeny, " "); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("deny without a reason despite on_deny: %v", err)
	}
	pv, err := e.runner.Get(e.ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Plan) != len(pv.Steps) || pv.Plan[4].Goto != "implement" || pv.Plan[2].Goto != "implement" || pv.Plan[2].When == "" || pv.Plan[0].Goto != "" {
		t.Fatalf("Plan %+v", pv.Plan)
	}
	e.decide(gateID, store.DecisionDeny, "Please add docs")
	waitFor(t, "third pass at the gate", func() bool {
		r := e.run(v.ID)
		return r.Steps[4].Iteration == 2 && r.Steps[4].Status == store.StepWaiting
	})
	impl = strings.Split(strings.TrimSpace(e.inputs("implement")), "\n")
	if len(impl) != 3 || !strings.Contains(impl[2], "Please add docs") || !strings.Contains(impl[2], "Approval final (denied") {
		t.Fatalf("third pass without the reason for the denial:\n%s", impl[len(impl)-1])
	}
	e.decide(e.run(v.ID).Steps[4].ApprovalID, store.DecisionAllow, "")
	waitFor(t, "run done", func() bool { return e.run(v.ID).Status == store.RunDone })

	got, err := e.runner.Get(e.ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	var review Artifact
	for _, a := range got.Artifacts {
		if a.Name == "review.json" {
			review = a
		}
	}
	if len(review.Iterations) != 3 {
		t.Fatalf("Artefakt review.json %+v", review)
	}
	first, err := e.runner.ReadArtifactIteration(e.ctx, v.ID, "review.json", 1)
	if err != nil || !strings.Contains(string(first), "test missing") {
		t.Fatalf("review.json from pass 1: %q %v", first, err)
	}
	if latest, _ := e.runner.ReadArtifact(e.ctx, v.ID, "review.json"); !strings.Contains(string(latest), "approve") {
		t.Fatalf("latest review.json: %s", latest)
	}
}

func TestRunLoopExhausted(t *testing.T) {
	e := setup(t)
	changes := `{"verdict": "changes", "comments": ["not yet"]}`
	e.answer("review", changes, changes, changes)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "l", Task: "Z"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Run gescheitert", func() bool { return e.run(v.ID).Status == store.RunFailed })
	r := e.run(v.ID)
	if !strings.Contains(r.Error, "still true after 3 passes") || r.Steps[0].Iteration != 3 {
		t.Fatalf("Run %+v", r)
	}
}

func TestRunParallel(t *testing.T) {
	e := setup(t)
	approve := `{"verdict": "approve", "comments": []}`
	e.answer("review", approve, approve)
	e.answer("test", `{"failed": 1, "name": "TestX"}`, `{"failed": 0}`)
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "p", Task: "Baue P"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Steps[2].Group == nil || *v.Steps[2].Group != 1 || v.Steps[0].Group != nil {
		t.Fatalf("Schritte %+v", v.Steps)
	}
	waitFor(t, "run done", func() bool {
		r := e.run(v.ID)
		if r.Status == store.RunFailed {
			t.Fatalf("Run gescheitert: %s", r.Error)
		}
		return r.Status == store.RunDone
	})
	r := e.run(v.ID)
	for _, st := range r.Steps[:3] {
		if st.Iteration != 2 || st.Status != store.StepDone {
			t.Errorf("step %s: pass %d, %s", st.Name, st.Iteration, st.Status)
		}
	}
	// Review and test ran at the same time: both started before either was done.
	rev, tst := r.Steps[1], r.Steps[2]
	if rev.StartedAt.After(*tst.EndedAt) || tst.StartedAt.After(*rev.EndedAt) {
		t.Fatalf("not parallel: review %v–%v, test %v–%v", rev.StartedAt, rev.EndedAt, tst.StartedAt, tst.EndedAt)
	}
	impl := strings.Split(strings.TrimSpace(e.inputs("implement")), "\n")
	if len(impl) != 2 || !strings.Contains(impl[1], "TestX") || !strings.Contains(impl[1], "Artifact review.json") {
		t.Fatalf("second pass without test result:\n%s", impl[len(impl)-1])
	}
}

func TestRunParallelRestart(t *testing.T) {
	e := setup(t)
	approve := `{"verdict": "approve", "comments": []}`
	e.answer("review", approve)
	e.answer("test", `{"failed": 0}`)
	for _, step := range []string{"review", "test"} {
		writeFile(t, filepath.Join(e.resp, step+".hold"), "")
	}
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "p", Task: "Baue P"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStep(v.ID, "review", store.StepRunning)
	e.waitStep(v.ID, "test", store.StepRunning)
	waitFor(t, "beide Rollen arbeiten", func() bool { return e.inputs("review") != "" && e.inputs("test") != "" })
	e.shutdown()
	for _, st := range e.run(v.ID).Steps[1:3] {
		if s, _ := e.st.GetSession(e.ctx, st.SessionID); s.Status != store.SessionInterrupted {
			t.Fatalf("%s after shutdown: %s", st.Name, s.Status)
		}
	}
	for _, step := range []string{"review", "test"} {
		if err := os.Remove(filepath.Join(e.resp, step+".hold")); err != nil {
			t.Fatal(err)
		}
	}
	e.boot()
	waitFor(t, "run done", func() bool {
		r := e.run(v.ID)
		if r.Status == store.RunFailed {
			t.Fatalf("Run gescheitert: %s", r.Error)
		}
		return r.Status == store.RunDone
	})
}

func TestRunParallelFailure(t *testing.T) {
	e := setup(t)
	e.answer("review", `{"verdict": "approve", "comments": []}`)
	e.answer("test", "kaputt", "kaputt", "kaputt")
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "p", Task: "Baue P"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Run gescheitert", func() bool { return e.run(v.ID).Status == store.RunFailed })
	r := e.run(v.ID)
	if !strings.HasPrefix(r.Error, "step test: ") || r.Steps[2].Status != store.StepFailed {
		t.Fatalf("Run %q, Schritte %+v", r.Error, r.Steps)
	}
	if s := r.Steps[1].Status; s != store.StepDone && s != store.StepCanceled {
		t.Fatalf("Review: %s", s)
	}
}

func TestRunBranchInput(t *testing.T) {
	e := setup(t)
	gitRun(t, e.project.RepoPath, "branch", "feature-x")
	v, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "review-only", Task: "Check", Inputs: map[string]string{"branch": "feature-x"}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := e.ws.Get(e.ctx, v.WorkspaceID)
	if err != nil || ws.BaseBranch != "feature-x" {
		t.Fatalf("Workspace %+v %v", ws, err)
	}
	if _, err := e.runner.Cancel(e.ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Factory: "review-only", Task: "Check", Inputs: map[string]string{"branch": "doesnotexist"}}); err == nil {
		t.Fatal("unknown branch without an error")
	}
}
