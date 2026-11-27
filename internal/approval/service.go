// Package approval ist der Freigabe-Service (doc/architecture.md#43-freigaben): Agent-Hooks
// und Egress-Proxys fragen an, ein Mensch entscheidet im Dashboard, die Anfrage wartet so
// lange (höchstens bis zum Timeout, dann gilt sie als abgelehnt).
//
// Werkzeug-Aufrufe prüft der Service zuerst gegen die Regeln des Projekts (approvals in
// .ade/project.yaml, rules.go); nur was keine Regel erlaubt oder sperrt, landet in der
// Warteschlange.
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

// DefaultTimeout ist die Wartezeit auf eine Entscheidung ohne approvals.timeout.
const DefaultTimeout = 10 * time.Minute

// Options konfiguriert den Service.
type Options struct {
	Store *store.Store
	// Timeout ist die Wartezeit auf eine Entscheidung (Standard DefaultTimeout).
	Timeout time.Duration
	Logger  *slog.Logger
	// Rules liefert den Abschnitt approvals der project.yaml eines Projekts. Ein Fehler
	// (ungültige Datei) heißt: keine Regel erlaubt etwas, alles braucht eine Freigabe.
	Rules func(ctx context.Context, projectID string) (*Config, error)
	// OnGrant läuft nach einer neuen Egress-Ausnahme (Policies an die Proxys schicken).
	OnGrant func(ctx context.Context)
	// Workdir liefert das Arbeitsverzeichnis einer lokalen Session, wenn es nicht der
	// Workspace ist (Lese-Rollen arbeiten in einem Schnappschuss; leer: Workspace).
	Workdir func(sess *store.Session) string
}

// Service verwaltet Freigaben.
type Service struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	waiters map[string]*waiter
	egress  map[string]string // workspace|host:port → offene Freigabe
	tokens  map[string]string // Hook-Token → Session
}

type waiter struct {
	done   chan struct{}
	result *store.Approval
	timer  *time.Timer
}

// New erzeugt den Service.
func New(opts Options) *Service {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts, log: opts.Logger, waiters: map[string]*waiter{}, egress: map[string]string{}, tokens: map[string]string{}}
}

// Timeout liefert die Wartezeit auf eine Entscheidung.
func (s *Service) Timeout() time.Duration { return s.opts.Timeout }

// Recover schließt Freigaben eines früheren Daemon-Laufs: Auf sie wartet niemand mehr.
func (s *Service) Recover(ctx context.Context) error {
	n, err := s.opts.Store.ExpirePendingApprovals(ctx, "Daemon wurde neu gestartet")
	if err != nil {
		return fmt.Errorf("offene Freigaben schließen: %w", err)
	}
	if n > 0 {
		s.log.Info("Offene Freigaben eines früheren Laufs als abgelaufen markiert", "anzahl", n)
	}
	return nil
}

// Close beendet alle Wartenden (Daemon wird beendet).
func (s *Service) Close() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.waiters))
	for id := range s.waiters {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_, _ = s.finish(context.Background(), id, store.ApprovalCanceled, "", "Daemon wird beendet")
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BudgetTimeout ist die Wartezeit auf eine Budget-Freigabe. Der Agent ist währenddessen
// beendet; es wartet also kein Prozess.
const BudgetTimeout = 24 * time.Hour

// create legt eine offene Freigabe an und startet ihren Timeout.
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
			"keine Entscheidung innerhalb von "+timeout.String())
	})
	s.mu.Lock()
	s.waiters[id] = w
	s.mu.Unlock()
	s.log.Info("Freigabe angefragt", "id", id, "art", a.Kind, "workspace", a.WorkspaceID, "titel", a.Title)
	return nil
}

// finish schließt eine offene Freigabe und weckt die Wartenden.
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
	s.log.Info("Freigabe entschieden", "id", id, "status", status, "entscheidung", decision)
	return a, nil
}

// wait wartet auf die Entscheidung. Endet ctx vorher, liefert es ctx.Err().
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

// Get liefert eine Freigabe.
func (s *Service) Get(ctx context.Context, id string) (*store.Approval, error) {
	return s.opts.Store.GetApproval(ctx, id)
}

// List liefert Freigaben.
func (s *Service) List(ctx context.Context, f store.ApprovalFilter) ([]*store.Approval, error) {
	return s.opts.Store.ListApprovals(ctx, f)
}

// Decide entscheidet eine offene Freigabe. Bei egress legen allow_workspace und
// allow_project zuerst die Ausnahme an, damit die Policy gilt, bevor die Anfrage weiterläuft.
func (s *Service) Decide(ctx context.Context, id, decision, reason string) (*store.Approval, error) {
	a, err := s.opts.Store.GetApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != store.ApprovalPending {
		return a, apperr.Conflict("Freigabe %s ist bereits entschieden (%s)", id, a.Status)
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
		return nil, apperr.Invalid("decision: %q ist ungültig (erlaubt: %s)", decision, strings.Join(valid, ", "))
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 2000 {
		return nil, apperr.Invalid("reason: höchstens 2000 Zeichen")
	}
	if decision == store.DecisionAllowWorkspace || decision == store.DecisionAllowProject {
		var d egressDetail
		if err := json.Unmarshal(a.Detail, &d); err != nil || d.Host == "" {
			return nil, fmt.Errorf("Freigabe %s: Ziel nicht lesbar", id)
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

// GrantRule ist der Allowlist-Eintrag für ein Ziel: Ports 80 und 443 ohne Port (gilt für
// beide), sonst host:port.
func GrantRule(host string, port int) string {
	if port == 80 || port == 443 || port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// AddGrant speichert eine Egress-Ausnahme (workspaceID leer: für das ganze Projekt) und
// schickt die Policies neu an die Proxys.
func (s *Service) AddGrant(ctx context.Context, projectID, workspaceID, rule, approvalID string) (*store.EgressGrant, error) {
	r, err := proxy.ParseRule(rule)
	if err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	if r.Wildcard {
		return nil, apperr.Invalid("%q: Ausnahmen ohne Platzhalter (Platzhalter in .ade/sandbox.yaml eintragen)", rule)
	}
	g := &store.EgressGrant{ProjectID: projectID, WorkspaceID: workspaceID, Rule: r.String(), ApprovalID: approvalID}
	if err := s.opts.Store.AddEgressGrant(ctx, g); err != nil {
		return nil, err
	}
	s.log.Info("Egress-Ausnahme angelegt", "projekt", projectID, "workspace", workspaceID, "regel", g.Rule)
	if s.opts.OnGrant != nil {
		s.opts.OnGrant(context.WithoutCancel(ctx))
	}
	return g, nil
}

// DeleteGrant entfernt eine Egress-Ausnahme.
func (s *Service) DeleteGrant(ctx context.Context, id int64) error {
	if err := s.opts.Store.DeleteEgressGrant(ctx, id); err != nil {
		return err
	}
	if s.opts.OnGrant != nil {
		s.opts.OnGrant(context.WithoutCancel(ctx))
	}
	return nil
}

// CancelSession bricht die offenen Freigaben einer beendeten Session ab.
func (s *Service) CancelSession(ctx context.Context, sessionID string) {
	as, err := s.opts.Store.ListApprovals(ctx, store.ApprovalFilter{Status: store.ApprovalPending, SessionID: sessionID})
	if err != nil {
		return
	}
	for _, a := range as {
		_, _ = s.finish(ctx, a.ID, store.ApprovalCanceled, "", "Session beendet")
	}
}

// --- Hook-Tokens (lokale Agenten) ------------------------------------------------------

// IssueToken erzeugt ein Token, mit dem der Hook einer lokalen Session fragen darf.
func (s *Service) IssueToken(sessionID string) string {
	t := randomHex(24)
	s.mu.Lock()
	s.tokens[t] = sessionID
	s.mu.Unlock()
	return t
}

// RevokeTokens macht die Tokens einer Session ungültig.
func (s *Service) RevokeTokens(sessionID string) {
	s.mu.Lock()
	for t, id := range s.tokens {
		if id == sessionID {
			delete(s.tokens, t)
		}
	}
	s.mu.Unlock()
}

// TokenSession liefert die Session zu einem Token.
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

// --- Werkzeug-Aufrufe (PreToolUse) ------------------------------------------------------

// HookRequest ist eine Frage von ade hook pretooluse.
type HookRequest struct {
	Call proxy.HookCall
	// Sandbox: über den Egress-Proxy des Workspaces WorkspaceID.
	Sandbox     bool
	WorkspaceID string
	// TokenSession ist die Session des Hook-Tokens (lokal).
	TokenSession string
}

// hookInput sind die genutzten Felder der Hook-Eingabe von Claude Code.
type hookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
}

// toolDetail ist der Inhalt einer Werkzeug-Freigabe.
type toolDetail struct {
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Workdir   string          `json:"workdir"`
}

func deny(format string, args ...any) proxy.Answer {
	return proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: " + fmt.Sprintf(format, args...)}
}

// PreToolUse entscheidet über einen Werkzeug-Aufruf: per Regel sofort, sonst über eine
// Freigabe im Dashboard (blockiert bis zur Entscheidung). Endet ctx vorher (der Agent wartet
// nicht mehr), wird die Freigabe abgebrochen.
func (s *Service) PreToolUse(ctx context.Context, req HookRequest) proxy.Answer {
	sessionID := req.Call.Session
	if !req.Sandbox {
		if req.TokenSession == "" || (sessionID != "" && sessionID != req.TokenSession) {
			return deny("Hook-Token passt nicht zur Session")
		}
		sessionID = req.TokenSession
	}
	sess, err := s.opts.Store.GetSession(ctx, sessionID)
	if err != nil {
		return deny("Session %q unbekannt", sessionID)
	}
	if !store.SessionActive(sess.Status) {
		return deny("Session %s läuft nicht mehr", sess.ID)
	}
	ws, err := s.opts.Store.GetWorkspace(ctx, sess.WorkspaceID)
	if err != nil {
		return deny("Workspace der Session nicht gefunden")
	}
	if ws.Sandbox != req.Sandbox || (req.Sandbox && ws.ID != req.WorkspaceID) {
		return deny("Anfrage kommt nicht aus dem Workspace der Session")
	}
	var in hookInput
	if err := json.Unmarshal(req.Call.Input, &in); err != nil || in.ToolName == "" {
		return deny("Hook-Eingabe ohne tool_name")
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
	rules := &Rules{} // ungültige Konfiguration: alles fragen
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
		return proxy.Answer{Decision: proxy.AnswerAllow, Reason: "ADE: erlaubt durch " + rule}
	case VerdictDeny:
		return deny("gesperrt durch %s (approvals.%s.deny in .ade/project.yaml)", rule, env)
	}

	detail, _ := json.Marshal(toolDetail{Tool: in.ToolName, Input: in.ToolInput, ToolUseID: in.ToolUseID, Workdir: workdir})
	a := &store.Approval{
		Kind: store.ApprovalTool, ProjectID: ws.ProjectID, WorkspaceID: ws.ID, SessionID: sess.ID,
		Title: toolTitle(in.ToolName, in.ToolInput), Detail: detail, Sandbox: ws.Sandbox,
	}
	if err := s.create(ctx, a); err != nil {
		s.log.Error("Freigabe anlegen", "err", err)
		return deny("Freigabe konnte nicht angelegt werden")
	}
	res, err := s.wait(ctx, a.ID)
	if err != nil {
		_, _ = s.finish(context.Background(), a.ID, store.ApprovalCanceled, "", "Agent wartet nicht mehr")
		return deny("abgebrochen")
	}
	return answerFor(res)
}

func answerFor(a *store.Approval) proxy.Answer {
	switch {
	case a.Status == store.ApprovalApproved:
		r := "ADE: im Dashboard freigegeben"
		if a.Reason != "" {
			r += ": " + a.Reason
		}
		return proxy.Answer{Decision: proxy.AnswerAllow, Reason: r}
	case a.Status == store.ApprovalDenied && a.Reason != "":
		return deny("im Dashboard abgelehnt: %s", a.Reason)
	case a.Status == store.ApprovalDenied:
		return deny("im Dashboard abgelehnt")
	case a.Status == store.ApprovalExpired:
		return deny("abgelehnt, %s", a.Reason)
	}
	return deny("Freigabe %s", a.Status)
}

// toolTitle ist eine Kurzbeschreibung eines Aufrufs.
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

// RequestBudget legt eine Budget-Freigabe an (agent.Budgets). decided läuft in einer eigenen
// Goroutine, sobald entschieden, abgelaufen oder abgebrochen ist.
func (s *Service) RequestBudget(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error {
	a.Kind = store.ApprovalBudget
	if err := s.createFor(ctx, a, BudgetTimeout); err != nil {
		return err
	}
	go func() { //nolint:gosec // G118: wartet über die Anfrage hinaus bis zur Entscheidung
		res, err := s.wait(context.Background(), a.ID)
		if err == nil && res != nil {
			decided(res)
		}
	}()
	return nil
}

// --- Gates ------------------------------------------------------------------------------

// GateTimeout ist die Wartezeit an einem Gate eines Fabrik-Runs. Es wartet kein Prozess.
const GateTimeout = 7 * 24 * time.Hour

// RequestGate legt die Freigabe eines Gates an (Art gate). decided läuft in einer eigenen
// Goroutine, sobald entschieden, abgelaufen oder abgebrochen ist.
func (s *Service) RequestGate(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error {
	a.Kind = store.ApprovalGate
	if err := s.createFor(ctx, a, GateTimeout); err != nil {
		return err
	}
	go func() { //nolint:gosec // G118: wartet über die Anfrage hinaus bis zur Entscheidung
		res, err := s.wait(context.Background(), a.ID)
		if err == nil && res != nil {
			decided(res)
		}
	}()
	return nil
}

// Cancel bricht eine offene Freigabe ab (z. B. weil der Run abgebrochen wurde).
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

// Egress entscheidet über ein nicht freigegebenes Ziel eines Sandbox-Workspaces. Gleiche
// Ziele teilen sich eine offene Freigabe. Endet ctx (die Anfrage im Container hat
// aufgegeben), bleibt die Freigabe offen – eine Ausnahme ist auch danach noch nützlich.
func (s *Service) Egress(ctx context.Context, workspaceID, host string, port int) proxy.Answer {
	ws, err := s.opts.Store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return deny("Workspace %s unbekannt", workspaceID)
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
			s.log.Error("Freigabe anlegen", "err", err)
			return deny("Freigabe konnte nicht angelegt werden")
		}
		s.mu.Lock()
		if other := s.egress[key]; other != "" {
			id = other // gleichzeitig angelegt: die andere gilt, diese schließen
		} else {
			s.egress[key], id = a.ID, a.ID
		}
		s.mu.Unlock()
		if id != a.ID {
			_, _ = s.finish(context.Background(), a.ID, store.ApprovalCanceled, "", "doppelte Anfrage")
		}
	}
	res, err := s.wait(ctx, id)
	if err != nil {
		return deny("keine Entscheidung")
	}
	if res.Status == store.ApprovalApproved {
		return proxy.Answer{Decision: proxy.AnswerAllow}
	}
	return answerFor(res)
}

// Question beantwortet eine Frage eines Egress-Proxys (sandbox.QuestionHandler).
func (s *Service) Question(ctx context.Context, workspace string, q proxy.Question) proxy.Answer {
	switch {
	case q.Kind == proxy.QuestionTool && q.Hook != nil:
		return s.PreToolUse(ctx, HookRequest{Call: *q.Hook, Sandbox: true, WorkspaceID: workspace})
	case q.Kind == proxy.QuestionEgress && q.Host != "":
		return s.Egress(ctx, workspace, q.Host, q.Port)
	}
	return deny("unbekannte Frage")
}
