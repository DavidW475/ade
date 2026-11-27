package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// fakeBudgets merkt sich Budget-Freigaben; Tests entscheiden sie von Hand.
type fakeBudgets struct {
	mu      sync.Mutex
	reqs    []*store.Approval
	decided []func(*store.Approval)
}

func (b *fakeBudgets) RequestBudget(_ context.Context, a *store.Approval, decided func(*store.Approval)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, a)
	b.decided = append(b.decided, decided)
	return nil
}

func (b *fakeBudgets) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reqs)
}

func (b *fakeBudgets) decide(i int, status string) {
	b.mu.Lock()
	a, f := *b.reqs[i], b.decided[i]
	b.mu.Unlock()
	a.Status = status
	f(&a)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func costLine(c adapter.Cost) string {
	b, _ := json.Marshal(map[string]any{"type": "cost", "payload": c})
	return string(b)
}

func TestEstimatedCostIsStoredAsUsage(t *testing.T) {
	e := setup(t, "")
	e.fake.Argv = []string{"sh", "-c", `echo '` + costLine(adapter.Cost{Estimated: true, Model: "gpt-5", InputTokens: 1_000_000, OutputTokens: 100_000}) + `'; echo '{"type":"turn_done"}'; exec cat`}
	s := e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionIdle)
	got, _ := e.st.GetSession(e.ctx, s.ID)
	// gpt-5: 1,25 $ je Million Eingabe + 10 $ je Million Ausgabe → 1,25 + 1,00
	if !near(got.CostUSD, 2.25) || got.InputTokens != 1_000_000 || got.OutputTokens != 100_000 || got.Unpriced {
		t.Fatalf("Session = cost %v, tokens %d/%d, unpriced %v", got.CostUSD, got.InputTokens, got.OutputTokens, got.Unpriced)
	}
	tot, err := e.st.UsageTotal(e.ctx, store.UsageFilter{ProjectID: e.project.ID})
	if err != nil || !near(tot.CostUSD, 2.25) || tot.Count != 1 {
		t.Fatalf("UsageTotal = %+v, %v", tot, err)
	}
	var c adapter.Cost
	for _, ev := range e.events(s.ID) {
		if ev.Type == adapter.TypeCost {
			_ = json.Unmarshal(ev.Payload, &c)
		}
	}
	if !near(c.TurnUSD, 2.25) || !near(c.TotalUSD, 2.25) || !c.Estimated {
		t.Errorf("cost-Event = %+v", c)
	}
}

func TestSessionBudgetPausesAndApprovalResumes(t *testing.T) {
	e := setup(t, "budget:\n  session_usd: 1\n")
	b := &fakeBudgets{}
	e.m.opts.Budgets = b
	e.fake.Argv = []string{"sh", "-c", `echo '{"type":"resume_id","payload":{"id":"cli-1"}}'; echo '` + costLine(adapter.Cost{TurnUSD: 1.5}) + `'; echo '{"type":"turn_done"}'; exec cat`}
	s := e.start(StartRequest{Task: "x"})
	if s.BudgetUSD != 1 {
		t.Fatalf("budget_usd = %v", s.BudgetUSD)
	}
	e.waitStatus(s.ID, store.SessionPaused)
	waitFor(t, "Budget-Freigabe", func() bool { return b.count() == 1 })
	a := b.reqs[0]
	var d BudgetDetail
	_ = json.Unmarshal(a.Detail, &d)
	if a.Kind != store.ApprovalBudget || a.SessionID != s.ID || d.Scope != BudgetSession || !near(d.SpentUSD, 1.5) || d.StepUSD != 1 {
		t.Fatalf("Freigabe = %+v, %+v", a, d)
	}
	got, _ := e.st.GetSession(e.ctx, s.ID)
	if got.Error == "" {
		t.Errorf("Grund fehlt: %+v", got)
	}

	// Von Hand fortsetzen, ohne Freigabe: pausiert sofort wieder, ohne Prozess.
	specs := len(e.fake.Specs())
	if _, err := e.m.Input(e.ctx, s.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "zweite Freigabe", func() bool { return b.count() == 2 })
	e.waitStatus(s.ID, store.SessionPaused)
	if len(e.fake.Specs()) != specs {
		t.Errorf("Prozess trotz überschrittenem Budget gestartet")
	}

	// Abgebrochene Freigabe ändert nichts, Erlauben erhöht das Budget und setzt fort.
	b.decide(0, store.ApprovalCanceled)
	if e.status(s.ID) != store.SessionPaused {
		t.Fatalf("Status nach Abbruch = %s", e.status(s.ID))
	}
	b.decide(1, store.ApprovalApproved)
	waitFor(t, "Fortsetzen", func() bool { return len(e.fake.Specs()) > specs })
	if spec := e.fake.Specs()[len(e.fake.Specs())-1]; spec.ResumeID != "cli-1" {
		t.Errorf("Resume-Spec = %+v", spec)
	}
	got, _ = e.st.GetSession(e.ctx, s.ID)
	if !near(got.BudgetUSD, 2.5) {
		t.Errorf("budget_usd nach Freigabe = %v", got.BudgetUSD)
	}
	// Der neue Lauf kostet wieder 1,5 $ → 3,0 ≥ 2,5: wieder pausiert, Ablehnen beendet.
	waitFor(t, "dritte Freigabe", func() bool { return b.count() == 3 })
	e.waitStatus(s.ID, store.SessionPaused)
	b.decide(2, store.ApprovalDenied)
	e.waitStatus(s.ID, store.SessionStopped)
}

func TestMonthlyBudgetBlocksStart(t *testing.T) {
	e := setup(t, "budget:\n  monthly_usd: 2\n")
	b := &fakeBudgets{}
	e.m.opts.Budgets = b
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Alte Nutzung im Vormonat zählt nicht, die aus diesem Monat schon.
	for _, u := range []store.Usage{
		{TS: store.MonthStart(time.Now()).Add(-time.Hour), CostUSD: 50},
		{CostUSD: 2.5},
	} {
		u.ProjectID, u.WorkspaceID, u.Source, u.Priced = e.project.ID, ws.ID, store.UsageCLI, true
		if err := e.st.AddUsage(e.ctx, &u); err != nil {
			t.Fatal(err)
		}
	}
	s := e.start(StartRequest{WorkspaceID: ws.ID, Task: "x"})
	e.waitStatus(s.ID, store.SessionPaused)
	waitFor(t, "Freigabe", func() bool { return b.count() == 1 })
	var d BudgetDetail
	_ = json.Unmarshal(b.reqs[0].Detail, &d)
	if d.Scope != BudgetMonthly || !near(d.SpentUSD, 2.5) || d.LimitUSD != 2 || d.StepUSD != 1 {
		t.Fatalf("Detail = %+v", d)
	}
	if n := len(e.fake.Specs()); n != 1 { // nur die Vorabprüfung in Start
		t.Errorf("Command-Aufrufe = %d", n)
	}
	b.decide(0, store.ApprovalApproved)
	got, _ := e.st.GetSession(e.ctx, s.ID)
	if !near(got.MonthlyAllowUSD, 3.5) {
		t.Errorf("monthly_allow_usd = %v", got.MonthlyAllowUSD)
	}
}

func TestProxyUsageCountsForMeteredSession(t *testing.T) {
	e := setup(t, "")
	e.m.opts.Access = func(string, bool) (*adapter.ProviderAccess, bool) { return nil, true }
	e.fake.Argv = []string{"sh", "-c", `read x; read y; echo '` + costLine(adapter.Cost{TurnUSD: 9, InputTokens: 1}) + `'; echo '{"type":"turn_done"}'; exec cat`}
	s := e.start(StartRequest{Task: "x", BudgetUSD: new(float64)})
	waitFor(t, "läuft", func() bool { v, _ := e.m.Get(e.ctx, s.ID); return v.Live && e.status(s.ID) == store.SessionRunning })

	e.m.RecordProxyUsage(e.ctx, s.WorkspaceID, []ProxyUsage{{Provider: "anthropic", Model: "claude-sonnet-4-5", Tokens: provider.Usage{Input: 1_000_000}}})
	got, _ := e.st.GetSession(e.ctx, s.ID)
	if !got.Metered || !near(got.CostUSD, 3) || got.InputTokens != 1_000_000 {
		t.Fatalf("Session = metered %v cost %v tokens %d", got.Metered, got.CostUSD, got.InputTokens)
	}
	// Die Meldung der CLI (9 $) zählt nicht; das cost-Event zeigt die Summe des Proxys.
	if _, err := e.m.Input(e.ctx, s.ID, "los"); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(s.ID, store.SessionIdle)
	got, _ = e.st.GetSession(e.ctx, s.ID)
	if !near(got.CostUSD, 3) {
		t.Fatalf("cost_usd = %v (CLI-Meldung doppelt gezählt?)", got.CostUSD)
	}
	var c adapter.Cost
	var usage int
	for _, ev := range e.events(s.ID) {
		switch ev.Type {
		case adapter.TypeCost:
			_ = json.Unmarshal(ev.Payload, &c)
		case TypeUsage:
			usage++
		}
	}
	if !c.Metered || !near(c.TurnUSD, 3) || !near(c.TotalUSD, 3) || usage != 1 {
		t.Errorf("cost-Event = %+v, usage-Events %d", c, usage)
	}

	// Ohne Messung (CLI meldet selbst) bekommt die Session nichts vom Proxy.
	e2 := setup(t, "")
	s2 := e2.start(StartRequest{Task: "x"})
	waitFor(t, "läuft", func() bool { v, _ := e2.m.Get(e2.ctx, s2.ID); return v.Live })
	e2.m.RecordProxyUsage(e2.ctx, s2.WorkspaceID, []ProxyUsage{{Provider: "anthropic", Model: "claude-sonnet-4-5", Tokens: provider.Usage{Input: 1_000_000}}})
	got, _ = e2.st.GetSession(e2.ctx, s2.ID)
	tot, _ := e2.st.UsageTotal(e2.ctx, store.UsageFilter{WorkspaceID: s2.WorkspaceID})
	if got.CostUSD != 0 || !near(tot.CostUSD, 3) {
		t.Errorf("nicht gemessen: Session %v, Workspace %v", got.CostUSD, tot.CostUSD)
	}
}

func TestModelAlias(t *testing.T) {
	e := setup(t, "")
	e.m.opts.Catalog = provider.NewCatalog(&config.Config{Models: map[string]config.Model{
		"opus": {Provider: "anthropic", ID: "claude-opus-5-5"},
		"gpt":  {Provider: "openai", ID: "gpt-5.3-codex"},
	}})
	codex := &fake.Adapter{AdapterName: "codex"}
	e.reg.Register(codex)

	s := e.start(StartRequest{Task: "x", Model: "opus"})
	if s.Model != "opus" || s.ModelID != "claude-opus-5-5" {
		t.Fatalf("Session = %+v", s)
	}
	waitFor(t, "Start", func() bool { return len(e.fake.Specs()) >= 2 })
	if spec := e.fake.Specs()[1]; spec.Model != "claude-opus-5-5" {
		t.Errorf("Spec.Model = %q", spec.Model)
	}
	// Unbekannte Namen gehen unverändert an die CLI.
	s = e.start(StartRequest{Task: "x", Model: "sonnet-direkt"})
	if s.Model != "sonnet-direkt" || s.ModelID != "" {
		t.Errorf("ohne Alias: %+v", s)
	}
	// Anbieter passt nicht zum Adapter.
	_, err := e.m.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Adapter: "codex", Task: "x", Model: "opus"})
	if !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("codex mit opus: %v", err)
	}
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Adapter: "codex", Task: "x", Model: "gpt"}); err != nil {
		t.Errorf("codex mit gpt: %v", err)
	}
	bad := -1.0
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Task: "x", BudgetUSD: &bad}); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("negatives Budget: %v", err)
	}
}
