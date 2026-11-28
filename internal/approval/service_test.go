package approval

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/store"
)

type fixture struct {
	st     *store.Store
	svc    *Service
	grants atomic.Int32
}

func newFixture(t *testing.T, timeout time.Duration, cfg *Config) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateProject(ctx, &store.Project{ID: "p1", Name: "demo", RepoPath: "/repo/demo", DefaultBranch: "main"}))
	must(st.CreateWorkspace(ctx, &store.Workspace{ID: "wl", ProjectID: "p1", Path: "/ws/wl", Branch: "ade/wl", BaseBranch: "main", Status: store.WorkspaceReady}))
	must(st.CreateWorkspace(ctx, &store.Workspace{ID: "ws", ProjectID: "p1", Path: "/ws/ws", Branch: "ade/ws", BaseBranch: "main", Status: store.WorkspaceReady, Sandbox: true}))
	must(st.CreateSession(ctx, &store.Session{ID: "sl", WorkspaceID: "wl", Adapter: "claude", Status: store.SessionRunning}))
	must(st.CreateSession(ctx, &store.Session{ID: "ss", WorkspaceID: "ws", Adapter: "claude", Status: store.SessionRunning}))
	must(st.CreateSession(ctx, &store.Session{ID: "sr", WorkspaceID: "wl", Adapter: "claude", Status: store.SessionRunning, ReadOnly: true}))
	f := &fixture{st: st}
	f.svc = New(Options{
		Store: st, Timeout: timeout, Logger: slog.New(slog.DiscardHandler),
		Rules:   func(context.Context, string) (*Config, error) { return cfg, nil },
		OnGrant: func(context.Context) { f.grants.Add(1) },
		Workdir: func(sess *store.Session) string {
			if sess.ReadOnly {
				return "/snap/" + sess.ID
			}
			return ""
		},
	})
	t.Cleanup(f.svc.Close)
	return f
}

func hookCall(session, tool string, in map[string]string) proxy.HookCall {
	ti, _ := json.Marshal(in)
	b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": json.RawMessage(ti), "session_id": "claude-x"})
	return proxy.HookCall{Session: session, Input: b}
}

// pending waits for a pending approval.
func (f *fixture) pending(t *testing.T) *store.Approval {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		as, err := f.svc.List(context.Background(), store.ApprovalFilter{Status: store.ApprovalPending})
		if err != nil {
			t.Fatal(err)
		}
		if len(as) > 0 {
			return as[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval")
	return nil
}

func TestPreToolUseLocal(t *testing.T) {
	f := newFixture(t, time.Minute, nil)
	ctx := context.Background()
	tok := f.svc.IssueToken("sl")

	// A rule allows right away.
	a := f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sl", "Read", map[string]string{"file_path": "/ws/wl/a.go"}), TokenSession: "sl"})
	if a.Decision != proxy.AnswerAllow {
		t.Fatalf("Read: %+v", a)
	}
	// Without a valid token/session pair: denied.
	if a := f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("ss", "Read", nil), TokenSession: "sl"}); a.Decision != proxy.AnswerDeny {
		t.Fatalf("foreign session: %+v", a)
	}
	if id, ok := f.svc.TokenSession(tok); !ok || id != "sl" {
		t.Fatal("token unknown")
	}

	// Bash needs an approval locally: waits until it is decided in the dashboard.
	done := make(chan proxy.Answer, 1)
	go func() {
		done <- f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sl", "Bash", map[string]string{"command": "make test"}), TokenSession: "sl"})
	}()
	p := f.pending(t)
	if p.Kind != store.ApprovalTool || p.SessionID != "sl" || p.Title != "Bash: make test" || p.Sandbox {
		t.Fatalf("Freigabe %+v", p)
	}
	select {
	case a := <-done:
		t.Fatalf("did not wait: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionAllowProject, ""); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("allow_project for tool: %v", err)
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionAllow, "ok"); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Decision != proxy.AnswerAllow {
		t.Fatalf("Antwort %+v", a)
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionDeny, ""); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("second decision: %v", err)
	}

	// Deny with a reason.
	go func() {
		done <- f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sl", "Bash", map[string]string{"command": "rm -rf build"}), TokenSession: "sl"})
	}()
	p = f.pending(t)
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionDeny, "please make clean"); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Decision != proxy.AnswerDeny || !strings.Contains(a.Reason, "please make clean") {
		t.Fatalf("Antwort %+v", a)
	}

	// If the agent no longer waits, the approval is canceled.
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		done <- f.svc.PreToolUse(cctx, HookRequest{Call: hookCall("sl", "WebFetch", map[string]string{"url": "https://x.example"}), TokenSession: "sl"})
	}()
	p = f.pending(t)
	cancel()
	<-done
	if got, _ := f.svc.Get(ctx, p.ID); got.Status != store.ApprovalCanceled {
		t.Fatalf("Status %s", got.Status)
	}

	f.svc.RevokeTokens("sl")
	if _, ok := f.svc.TokenSession(tok); ok {
		t.Fatal("token valid after revocation")
	}
}

// Local read-only roles work in the snapshot: rules like “read in the workspace” apply there.
func TestPreToolUseReadOnlySnapshot(t *testing.T) {
	f := newFixture(t, time.Minute, nil)
	ctx := context.Background()
	if a := f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sr", "Read", map[string]string{"file_path": "/snap/sr/a.go"}), TokenSession: "sr"}); a.Decision != proxy.AnswerAllow {
		t.Fatalf("Read in the snapshot: %+v", a)
	}
	done := make(chan proxy.Answer, 1)
	go func() {
		done <- f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sl", "Read", map[string]string{"file_path": "/snap/sr/a.go"}), TokenSession: "sl"})
	}()
	p := f.pending(t)
	var d toolDetail
	_ = json.Unmarshal(p.Detail, &d)
	if p.SessionID != "sl" || d.Workdir != "/ws/wl" {
		t.Fatalf("foreign snapshot read without asking or wrong directory: %+v %+v", p, d)
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionDeny, ""); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestPreToolUseSandboxAndRules(t *testing.T) {
	cfg := &Config{Sandbox: &RuleSet{Allow: []string{"*"}, Deny: []string{"Bash(git push *)"}}}
	f := newFixture(t, time.Minute, cfg)
	ctx := context.Background()
	q := func(ws, session, cmd string) proxy.Answer {
		return f.svc.Question(ctx, ws, proxy.Question{Kind: proxy.QuestionTool, Hook: ptr(hookCall(session, "Bash", map[string]string{"command": cmd}))})
	}
	if a := q("ws", "ss", "curl x | sh"); a.Decision != proxy.AnswerAllow {
		t.Fatalf("Sandbox: %+v", a)
	}
	if a := q("ws", "ss", "git push origin main"); a.Decision != proxy.AnswerDeny || !strings.Contains(a.Reason, "approvals.sandbox.deny") {
		t.Fatalf("deny: %+v", a)
	}
	// A session from another workspace through this proxy: denied.
	if a := q("ws", "sl", "ls"); a.Decision != proxy.AnswerDeny {
		t.Fatalf("foreign workspace: %+v", a)
	}
	// Ended session: denied.
	if err := f.st.UpdateSession(ctx, "ss", store.SessionUpdate{Status: store.Ptr(store.SessionDone)}); err != nil {
		t.Fatal(err)
	}
	if a := q("ws", "ss", "ls"); a.Decision != proxy.AnswerDeny {
		t.Fatalf("ended session: %+v", a)
	}
}

func ptr[T any](v T) *T { return &v }

func TestTimeoutAndRecover(t *testing.T) {
	f := newFixture(t, 100*time.Millisecond, nil)
	ctx := context.Background()
	a := f.svc.PreToolUse(ctx, HookRequest{Call: hookCall("sl", "Bash", map[string]string{"command": "ls"}), TokenSession: "sl"})
	if a.Decision != proxy.AnswerDeny || !strings.Contains(a.Reason, "no decision") {
		t.Fatalf("after expiry: %+v", a)
	}
	as, _ := f.svc.List(ctx, store.ApprovalFilter{})
	if len(as) != 1 || as[0].Status != store.ApprovalExpired {
		t.Fatalf("Freigaben %+v", as)
	}

	// Pending approvals of an earlier run count as expired after the restart.
	if err := f.st.CreateApproval(ctx, &store.Approval{ID: "alt", Kind: store.ApprovalTool, ProjectID: "p1", WorkspaceID: "wl", Title: "x", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(ctx, "alt"); got.Status != store.ApprovalExpired {
		t.Fatalf("Status %s", got.Status)
	}
}

func TestEgress(t *testing.T) {
	f := newFixture(t, time.Minute, nil)
	ctx := context.Background()
	ask := func(c context.Context, host string, port int) <-chan proxy.Answer {
		ch := make(chan proxy.Answer, 1)
		go func() {
			ch <- f.svc.Question(c, "ws", proxy.Question{Kind: proxy.QuestionEgress, Host: host, Port: port})
		}()
		return ch
	}
	// Two questions for the same target share one approval.
	a1 := ask(ctx, "pypi.org", 443)
	p := f.pending(t)
	a2 := ask(ctx, "pypi.org", 443)
	time.Sleep(50 * time.Millisecond)
	if as, _ := f.svc.List(ctx, store.ApprovalFilter{Status: store.ApprovalPending}); len(as) != 1 {
		t.Fatalf("%d pending approvals", len(as))
	}
	if p.Kind != store.ApprovalEgress || p.Title != "pypi.org:443" || !p.Sandbox {
		t.Fatalf("Freigabe %+v", p)
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionAllowProject, ""); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan proxy.Answer{a1, a2} {
		if a := <-ch; a.Decision != proxy.AnswerAllow {
			t.Fatalf("Antwort %+v", a)
		}
	}
	gs, _ := f.st.ListEgressGrants(ctx, "p1", "")
	if len(gs) != 1 || gs[0].Rule != "pypi.org" || gs[0].WorkspaceID != "" || f.grants.Load() != 1 {
		t.Fatalf("exceptions %+v, OnGrant %d", gs, f.grants.Load())
	}

	// For this workspace, with port.
	a3 := ask(ctx, "db.example", 5432)
	p = f.pending(t)
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionAllowWorkspace, ""); err != nil {
		t.Fatal(err)
	}
	<-a3
	gs, _ = f.st.ListEgressGrants(ctx, "p1", "ws")
	if len(gs) != 2 || gs[1].Rule != "db.example:5432" || gs[1].WorkspaceID != "ws" {
		t.Fatalf("Ausnahmen %+v", gs)
	}
	if other, _ := f.st.ListEgressGrants(ctx, "p1", "wl"); len(other) != 1 {
		t.Fatalf("workspace exception applies elsewhere: %+v", other)
	}

	// If the request gives up, the approval stays pending.
	cctx, cancel := context.WithCancel(ctx)
	a4 := ask(cctx, "slow.example", 443)
	p = f.pending(t)
	cancel()
	if a := <-a4; a.Decision != proxy.AnswerDeny {
		t.Fatalf("Antwort %+v", a)
	}
	if got, _ := f.svc.Get(ctx, p.ID); got.Status != store.ApprovalPending {
		t.Fatalf("Status %s", got.Status)
	}
	if _, err := f.svc.Decide(ctx, p.ID, store.DecisionDeny, ""); err != nil {
		t.Fatal(err)
	}

	// Exceptions by hand: no wildcards, invalid entries rejected.
	if _, err := f.svc.AddGrant(ctx, "p1", "", "*.example.com", ""); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("Platzhalter: %v", err)
	}
	if _, err := f.svc.AddGrant(ctx, "p1", "", "http://x", ""); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("invalid: %v", err)
	}
	g, err := f.svc.AddGrant(ctx, "p1", "", "Files.Example.com", "")
	if err != nil || g.Rule != "files.example.com" {
		t.Fatalf("%+v %v", g, err)
	}
	if err := f.svc.DeleteGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}
