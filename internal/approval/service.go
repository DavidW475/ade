// Package approval is the approval service (doc/architecture.md#43-approvals): agent hooks
// and egress proxies ask, a human decides in the dashboard, the request waits that long
// (at most until the timeout, then it counts as denied).
//
// The service first checks tool calls against the project's rules (approvals in
// .ade/project.yaml, rules.go); only what no rule allows or denies ends up in the
// queue.
package approval

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// DefaultTimeout is the waiting time for a decision without approvals.timeout.
const DefaultTimeout = 10 * time.Minute

// Options configures the service.
type Options struct {
	Store *store.Store
	// Timeout is the waiting time for a decision (default DefaultTimeout).
	Timeout time.Duration
	Logger  *slog.Logger
	// Rules returns the approvals section of a project's project.yaml. An error (invalid
	// file) means: no rule allows anything, everything needs an approval.
	Rules func(ctx context.Context, projectID string) (*Config, error)
	// OnGrant runs after a new egress exception (send the policies to the proxies).
	OnGrant func(ctx context.Context)
	// Workdir returns the working directory of a local session if it is not the workspace
	// (read-only roles work in a snapshot; empty: workspace).
	Workdir func(sess *store.Session) string
}

// Service manages approvals.
type Service struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	waiters map[string]*waiter
	egress  map[string]string // workspace|host:port → pending approval
	tokens  map[string]string // hook token → session
}

type waiter struct {
	done   chan struct{}
	result *store.Approval
	timer  *time.Timer
}

// New creates the service.
func New(opts Options) *Service {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts, log: opts.Logger, waiters: map[string]*waiter{}, egress: map[string]string{}, tokens: map[string]string{}}
}

// Timeout returns the waiting time for a decision.
func (s *Service) Timeout() time.Duration { return s.opts.Timeout }

// Recover closes approvals of an earlier daemon run: nobody waits for them any more.
func (s *Service) Recover(ctx context.Context) error {
	n, err := s.opts.Store.ExpirePendingApprovals(ctx, "daemon was restarted")
	if err != nil {
		return fmt.Errorf("close pending approvals: %w", err)
	}
	if n > 0 {
		s.log.Info("pending approvals of an earlier run marked as expired", "count", n)
	}
	return nil
}

// Close ends all waiting requests (daemon is stopping).
func (s *Service) Close() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.waiters))
	for id := range s.waiters {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_, _ = s.finish(context.Background(), id, store.ApprovalCanceled, "", "daemon is stopping")
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BudgetTimeout is the waiting time for a budget approval. The agent has ended in the
// meantime, so no process waits.
const BudgetTimeout = 24 * time.Hour

// create creates a pending approval and starts its timeout.
func (s *Service) create(ctx context.Context, a *store.Approval) error {
	return s.createFor(ctx, a, s.opts.Timeout)
}

func (s *Service) createFor(ctx context.Context, a *store.Approval, timeout time.Duration) error {
	a.ID = randomHex(6)
	a.ExpiresAt = time.Now().Add(timeout)
	if err := s.opts.Store.CreateApproval(ctx, a); err != nil {
		return err
	}
	w := &waiter{done: make(chan struct{})}
	id := a.ID
	w.timer = time.AfterFunc(timeout, func() {
		_, _ = s.finish(context.Background(), id, store.ApprovalExpired, store.DecisionDeny,
			"no decision within "+timeout.String())
	})
	s.mu.Lock()
	s.waiters[id] = w
	s.mu.Unlock()
	s.log.Info("approval requested", "id", id, "kind", a.Kind, "workspace", a.WorkspaceID, "title", a.Title)
	return nil
}

// finish closes a pending approval and wakes the waiting requests.
func (s *Service) finish(ctx context.Context, id, status, decision, reason string) (*store.Approval, error) {
	a, err := s.opts.Store.ResolveApproval(ctx, id, status, decision, reason)
	if err != nil {
		return a, err
	}
	s.mu.Lock()
	w := s.waiters[id]
	delete(s.waiters, id)
	for k, v := range s.egress {
		if v == id {
			delete(s.egress, k)
		}
	}
	s.mu.Unlock()
	if w != nil {
		w.timer.Stop()
		w.result = a
		close(w.done)
	}
	s.log.Info("approval decided", "id", id, "status", status, "decision", decision)
	return a, nil
}

// wait waits for the decision. If ctx ends first, it returns ctx.Err().
func (s *Service) wait(ctx context.Context, id string) (*store.Approval, error) {
	s.mu.Lock()
	w := s.waiters[id]
	s.mu.Unlock()
	if w == nil {
		return s.opts.Store.GetApproval(ctx, id)
	}
	select {
	case <-w.done:
		return w.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Get returns an approval.
func (s *Service) Get(ctx context.Context, id string) (*store.Approval, error) {
	return s.opts.Store.GetApproval(ctx, id)
}

// List returns approvals.
func (s *Service) List(ctx context.Context, f store.ApprovalFilter) ([]*store.Approval, error) {
	return s.opts.Store.ListApprovals(ctx, f)
}

// Decide decides a pending approval. For egress, allow_workspace and allow_project first
// create the exception, so the policy applies before the request continues.
func (s *Service) Decide(ctx context.Context, id, decision, reason string) (*store.Approval, error) {
	a, err := s.opts.Store.GetApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != store.ApprovalPending {
		return a, apperr.Conflict("approval %s has already been decided (%s)", id, a.Status)
	}
	valid := []string{store.DecisionAllow, store.DecisionDeny}
	if a.Kind == store.ApprovalEgress {
		valid = append(valid, store.DecisionAllowWorkspace, store.DecisionAllowProject)
	}
	ok := false
	for _, v := range valid {
		ok = ok || v == decision
	}
	if !ok {
		return nil, apperr.Invalid("decision: %q is invalid (allowed: %s)", decision, strings.Join(valid, ", "))
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 2000 {
		return nil, apperr.Invalid("reason: at most 2000 characters")
	}
	if a.Kind == store.ApprovalGate && decision == store.DecisionDeny && reason == "" {
		// If the gate jumps back on denial, the reason is the feedback to the role.
		var d struct {
			OnDeny string `json:"on_deny"`
		}
		if json.Unmarshal(a.Detail, &d) == nil && d.OnDeny != "" {
			return nil, apperr.Invalid("reason: denying jumps back to %s and needs a reason (it goes to the role as feedback)", d.OnDeny)
		}
	}
	if decision == store.DecisionAllowWorkspace || decision == store.DecisionAllowProject {
		var d egressDetail
		if err := json.Unmarshal(a.Detail, &d); err != nil || d.Host == "" {
			return nil, fmt.Errorf("approval %s: target not readable", id)
		}
		ws := a.WorkspaceID
		if decision == store.DecisionAllowProject {
			ws = ""
		}
		if _, err := s.AddGrant(ctx, a.ProjectID, ws, GrantRule(d.Host, d.Port), id); err != nil {
			return nil, err
		}
	}
	status := store.ApprovalApproved
	if decision == store.DecisionDeny {
		status = store.ApprovalDenied
	}
	return s.finish(ctx, id, status, decision, reason)
}

// GrantRule is the allowlist entry for a target: without a port for ports 80 and 443
// (applies to both), otherwise host:port.
func GrantRule(host string, port int) string {
	if port == 80 || port == 443 || port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// AddGrant stores an egress exception (workspaceID empty: for the whole project) and sends
// the policies to the proxies again.
func (s *Service) AddGrant(ctx context.Context, projectID, workspaceID, rule, approvalID string) (*store.EgressGrant, error) {
	r, err := proxy.ParseRule(rule)
	if err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	if r.Wildcard {
		return nil, apperr.Invalid("%q: exceptions without wildcards (put wildcards into .ade/sandbox.yaml)", rule)
	}
	g := &store.EgressGrant{ProjectID: projectID, WorkspaceID: workspaceID, Rule: r.String(), ApprovalID: approvalID}
	if err := s.opts.Store.AddEgressGrant(ctx, g); err != nil {
		return nil, err
	}
	s.log.Info("egress exception created", "project", projectID, "workspace", workspaceID, "rule", g.Rule)
	if s.opts.OnGrant != nil {
		s.opts.OnGrant(context.WithoutCancel(ctx))
	}
	return g, nil
}

// DeleteGrant removes an egress exception.
func (s *Service) DeleteGrant(ctx context.Context, id int64) error {
	if err := s.opts.Store.DeleteEgressGrant(ctx, id); err != nil {
		return err
	}
	if s.opts.OnGrant != nil {
		s.opts.OnGrant(context.WithoutCancel(ctx))
	}
	return nil
}

// CancelSession cancels the pending approvals of an ended session.
func (s *Service) CancelSession(ctx context.Context, sessionID string) {
	as, err := s.opts.Store.ListApprovals(ctx, store.ApprovalFilter{Status: store.ApprovalPending, SessionID: sessionID})
	if err != nil {
		return
	}
	for _, a := range as {
		_, _ = s.finish(ctx, a.ID, store.ApprovalCanceled, "", "session ended")
	}
}

// --- Hook tokens (local agents) ---------------------------------------------------------

// IssueToken creates a token with which the hook of a local session may ask.
func (s *Service) IssueToken(sessionID string) string {
	t := randomHex(24)
	s.mu.Lock()
	s.tokens[t] = sessionID
	s.mu.Unlock()
	return t
}

// RevokeTokens invalidates the tokens of a session.
func (s *Service) RevokeTokens(sessionID string) {
	s.mu.Lock()
	for t, id := range s.tokens {
		if id == sessionID {
			delete(s.tokens, t)
		}
	}
	s.mu.Unlock()
}

// TokenSession returns the session of a token.
func (s *Service) TokenSession(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, id := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return id, true
		}
	}
	return "", false
}

// --- Tool calls (PreToolUse) -----------------------------------------------------------

// HookRequest is a question from ade hook pretooluse.
type HookRequest struct {
	Call proxy.HookCall
	// Sandbox: through the egress proxy of the workspace WorkspaceID.
	Sandbox     bool
	WorkspaceID string
	// TokenSession is the session of the hook token (local).
	TokenSession string
}

// hookInput are the used fields of Claude Code's hook input.
type hookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
}

// toolDetail is the content of a tool approval.
type toolDetail struct {
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Workdir   string          `json:"workdir"`
}

func deny(format string, args ...any) proxy.Answer {
	return proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: " + fmt.Sprintf(format, args...)}
}

// PreToolUse decides about a tool call: by rule right away, otherwise through an approval in
// the dashboard (blocks until the decision). If ctx ends first (the agent no longer waits),
// the approval is canceled.
func (s *Service) PreToolUse(ctx context.Context, req HookRequest) proxy.Answer {
	sessionID := req.Call.Session
	if !req.Sandbox {
		if req.TokenSession == "" || (sessionID != "" && sessionID != req.TokenSession) {
			return deny("hook token does not match the session")
		}
		sessionID = req.TokenSession
	}
	sess, err := s.opts.Store.GetSession(ctx, sessionID)
	if err != nil {
		return deny("session %q unknown", sessionID)
	}
	if !store.SessionActive(sess.Status) {
		return deny("session %s is no longer running", sess.ID)
	}
	ws, err := s.opts.Store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		return deny("workspace of the session not found")
	}
	if ws.Sandbox != req.Sandbox || (req.Sandbox && ws.ID != req.WorkspaceID) {
		return deny("request does not come from the session's workspace")
	}
	var in hookInput
	if err := json.Unmarshal(req.Call.Input, &in); err != nil || in.ToolName == "" {
		return deny("hook input without tool_name")
	}
	workdir := ws.Path
	if ws.Sandbox {
		workdir = sandbox.Workdir
	} else if s.opts.Workdir != nil {
		if wd := s.opts.Workdir(sess); wd != "" {
			workdir = wd
		}
	}
	env := "local"
	if ws.Sandbox {
		env = "sandbox"
	}
	rules := &Rules{} // invalid configuration: ask for everything
	if s.opts.Rules != nil {
		cfg, err := s.opts.Rules(ctx, ws.ProjectID)
		if err == nil {
			if rules, err = Compile(cfg.Effective(ws.Sandbox)); err != nil {
				rules = &Rules{}
			}
		}
	}
	verdict, rule := rules.Evaluate(Call{Tool: in.ToolName, Input: in.ToolInput, Workdir: workdir, ResolveLinks: !ws.Sandbox})
	switch verdict {
	case VerdictAllow:
		return proxy.Answer{Decision: proxy.AnswerAllow, Reason: "ADE: allowed by " + rule}
	case VerdictDeny:
		return deny("denied by %s (approvals.%s.deny in .ade/project.yaml)", rule, env)
	}

	detail, _ := json.Marshal(toolDetail{Tool: in.ToolName, Input: in.ToolInput, ToolUseID: in.ToolUseID, Workdir: workdir})
	a := &store.Approval{
		Kind: store.ApprovalTool, ProjectID: ws.ProjectID, WorkspaceID: ws.ID, SessionID: sess.ID,
		Title: toolTitle(in.ToolName, in.ToolInput), Detail: detail, Sandbox: ws.Sandbox,
	}
	if err := s.create(ctx, a); err != nil {
		s.log.Error("create approval", "err", err)
		return deny("approval could not be created")
	}
	res, err := s.wait(ctx, a.ID)
	if err != nil {
		_, _ = s.finish(context.Background(), a.ID, store.ApprovalCanceled, "", "agent no longer waits")
		return deny("abgebrochen")
	}
	return answerFor(res)
}

func answerFor(a *store.Approval) proxy.Answer {
	switch {
	case a.Status == store.ApprovalApproved:
		r := "ADE: approved in the dashboard"
		if a.Reason != "" {
			r += ": " + a.Reason
		}
		return proxy.Answer{Decision: proxy.AnswerAllow, Reason: r}
	case a.Status == store.ApprovalDenied && a.Reason != "":
		return deny("denied in the dashboard: %s", a.Reason)
	case a.Status == store.ApprovalDenied:
		return deny("denied in the dashboard")
	case a.Status == store.ApprovalExpired:
		return deny("abgelehnt, %s", a.Reason)
	}
	return deny("Freigabe %s", a.Status)
}

// toolTitle is a short description of a call.
func toolTitle(tool string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	arg := ""
	switch {
	case tool == "Bash":
		arg = str("command")
	case pathFields[tool] != "":
		arg = str(pathFields[tool])
	case tool == "WebFetch":
		arg = str("url")
	case tool == "WebSearch":
		arg = str("query")
	}
	arg = strings.Join(strings.Fields(arg), " ")
	if r := []rune(arg); len(r) > 160 {
		arg = string(r[:160]) + "…"
	}
	if arg == "" {
		return tool
	}
	return tool + ": " + arg
}

// --- Budgets ----------------------------------------------------------------------------

// RequestBudget creates a budget approval (agent.Budgets). decided runs in its own goroutine
// as soon as it is decided, expired or canceled.
func (s *Service) RequestBudget(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error {
	a.Kind = store.ApprovalBudget
	if err := s.createFor(ctx, a, BudgetTimeout); err != nil {
		return err
	}
	go func() { //nolint:gosec // G118: waits beyond the request until the decision
		res, err := s.wait(context.Background(), a.ID)
		if err == nil && res != nil {
			decided(res)
		}
	}()
	return nil
}

// --- Gates ------------------------------------------------------------------------------

// GateTimeout is the waiting time at a gate of a factory run. No process waits.
const GateTimeout = 7 * 24 * time.Hour

// RequestGate creates the approval of a gate (kind gate). decided runs in its own goroutine
// as soon as it is decided, expired or canceled.
func (s *Service) RequestGate(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error {
	a.Kind = store.ApprovalGate
	if err := s.createFor(ctx, a, GateTimeout); err != nil {
		return err
	}
	go func() { //nolint:gosec // G118: waits beyond the request until the decision
		res, err := s.wait(context.Background(), a.ID)
		if err == nil && res != nil {
			decided(res)
		}
	}()
	return nil
}

// Cancel cancels a pending approval (e.g. because the run was canceled).
func (s *Service) Cancel(ctx context.Context, id, reason string) error {
	a, err := s.opts.Store.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	if a.Status != store.ApprovalPending {
		return nil
	}
	_, err = s.finish(ctx, id, store.ApprovalCanceled, "", reason)
	return err
}

// --- Egress -----------------------------------------------------------------------------

type egressDetail struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Egress decides about a target of a sandbox workspace that is not allowed. Equal targets
// share one pending approval. If ctx ends (the request in the container gave up), the
// approval stays pending – an exception is still useful afterwards.
func (s *Service) Egress(ctx context.Context, workspaceID, host string, port int) proxy.Answer {
	ws, err := s.opts.Store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return deny("workspace %s unknown", workspaceID)
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	key := ws.ID + "|" + target
	s.mu.Lock()
	id := s.egress[key]
	s.mu.Unlock()
	if id == "" {
		detail, _ := json.Marshal(egressDetail{Host: host, Port: port})
		a := &store.Approval{
			Kind: store.ApprovalEgress, ProjectID: ws.ProjectID, WorkspaceID: ws.ID,
			Title: target, Detail: detail, Sandbox: true,
		}
		if err := s.create(context.WithoutCancel(ctx), a); err != nil {
			s.log.Error("create approval", "err", err)
			return deny("approval could not be created")
		}
		s.mu.Lock()
		if other := s.egress[key]; other != "" {
			id = other // created at the same time: the other one applies, close this one
		} else {
			s.egress[key], id = a.ID, a.ID
		}
		s.mu.Unlock()
		if id != a.ID {
			_, _ = s.finish(context.Background(), a.ID, store.ApprovalCanceled, "", "duplicate request")
		}
	}
	res, err := s.wait(ctx, id)
	if err != nil {
		return deny("no decision")
	}
	if res.Status == store.ApprovalApproved {
		return proxy.Answer{Decision: proxy.AnswerAllow}
	}
	return answerFor(res)
}

// Question answers a question of an egress proxy (sandbox.QuestionHandler).
func (s *Service) Question(ctx context.Context, workspace string, q proxy.Question) proxy.Answer {
	switch {
	case q.Kind == proxy.QuestionTool && q.Hook != nil:
		return s.PreToolUse(ctx, HookRequest{Call: *q.Hook, Sandbox: true, WorkspaceID: workspace})
	case q.Kind == proxy.QuestionEgress && q.Host != "":
		return s.Egress(ctx, workspace, q.Host, q.Port)
	}
	return deny("unknown question")
}
