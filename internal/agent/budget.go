package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/store"
)

// TypeUsage meldet eine vom Egress-Proxy gemessene Nutzung (Payload UsageEvent). Die
// Timeline zeigt es nicht an; die UI aktualisiert damit Kosten und Budget live.
const TypeUsage = "usage"

// UsageEvent ist die Payload von TypeUsage.
type UsageEvent struct {
	Provider         string  `json:"provider,omitempty"`
	Model            string  `json:"model,omitempty"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_write_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
	Priced           bool    `json:"priced"`
	TotalUSD         float64 `json:"total_usd"`
	BudgetUSD        float64 `json:"budget_usd,omitempty"`
}

// Bereiche eines Budgets.
const (
	BudgetSession = "session"
	BudgetMonthly = "monthly"
)

// BudgetDetail ist der Inhalt einer Budget-Freigabe.
type BudgetDetail struct {
	Scope    string  `json:"scope"`
	LimitUSD float64 `json:"limit_usd"`
	SpentUSD float64 `json:"spent_usd"`
	// StepUSD ist der Betrag, um den eine Freigabe das Budget erhöht.
	StepUSD float64 `json:"step_usd"`
}

type budgetPause BudgetDetail

func (p *budgetPause) reason() string {
	if p.Scope == BudgetMonthly {
		return fmt.Sprintf("Monatsbudget des Projekts überschritten (%s von %s)", usd(p.SpentUSD), usd(p.LimitUSD))
	}
	return fmt.Sprintf("Budget der Session überschritten (%s von %s)", usd(p.SpentUSD), usd(p.LimitUSD))
}

func usd(v float64) string { return fmt.Sprintf("%.2f $", v) }

func tokensOf(c adapter.Cost) provider.Usage {
	return provider.Usage{Input: c.InputTokens, Output: c.OutputTokens, CacheRead: c.CacheReadTokens, CacheWrite: c.CacheWriteTokens}
}

// turnCost verbucht die Kostenmeldung eines Durchgangs und ergänzt die Payload. Misst der
// Proxy, zählt seine Summe seit dem letzten Durchgang, nicht die Meldung der CLI.
func (r *run) turnCost(c adapter.Cost) adapter.Cost {
	m := r.m
	if c.Model == "" {
		c.Model = r.model
	}
	r.mu.Lock()
	metered := r.metered
	r.mu.Unlock()
	if metered {
		r.mu.Lock()
		c.Metered = true
		c.Estimated = false
		c.TurnUSD = max(r.sessionUSD-r.turnBaseUSD, 0)
		r.turnBaseUSD = r.sessionUSD
		c.TotalUSD, c.BudgetUSD = r.sessionUSD, r.budgetUSD
		r.mu.Unlock()
		return c
	}
	priced := true
	if c.Estimated {
		c.TurnUSD, priced = m.opts.Catalog.Cost(c.Model, tokensOf(c))
	}
	u := &store.Usage{
		ProjectID: r.projectID, WorkspaceID: r.workspaceID, SessionID: r.id, Source: store.UsageCLI,
		Provider: provider.ForAdapter(r.adapter), Model: c.Model,
		InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, CacheReadTokens: c.CacheReadTokens, CacheWriteTokens: c.CacheWriteTokens,
		CostUSD: c.TurnUSD, Priced: priced,
	}
	if err := m.opts.Store.AddUsage(context.Background(), u); err != nil {
		m.log.Error("Kosten speichern", "session", r.id, "err", err)
	}
	r.mu.Lock()
	r.sessionUSD += c.TurnUSD
	r.turnBaseUSD = r.sessionUSD
	c.TotalUSD, c.BudgetUSD = r.sessionUSD, r.budgetUSD
	r.mu.Unlock()
	return c
}

// ProxyUsage ist eine vom Egress-Proxy gemessene Nutzung.
type ProxyUsage struct {
	Provider string
	Model    string
	Tokens   provider.Usage
	Time     time.Time
}

// RecordProxyUsage verbucht Nutzungen, die der Egress-Proxy eines Workspaces gemessen hat.
// Sie zählen für den Agenten im Workspace, wenn seine Nutzung über den Proxy gemessen wird,
// oder für ein Terminal; sonst nur für den Workspace (keine Doppelzählung mit der CLI).
func (m *Manager) RecordProxyUsage(ctx context.Context, workspaceID string, us []ProxyUsage) {
	if len(us) == 0 {
		return
	}
	ws, err := m.opts.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		m.log.Warn("Nutzung für unbekannten Workspace", "workspace", workspaceID, "err", err)
		return
	}
	r := m.usageRun(workspaceID)
	for _, pu := range us {
		cost, priced := m.opts.Catalog.Cost(pu.Model, pu.Tokens)
		u := &store.Usage{
			TS: pu.Time, ProjectID: ws.ProjectID, WorkspaceID: ws.ID, Source: store.UsageProxy, Provider: pu.Provider, Model: pu.Model,
			InputTokens: pu.Tokens.Input, OutputTokens: pu.Tokens.Output, CacheReadTokens: pu.Tokens.CacheRead, CacheWriteTokens: pu.Tokens.CacheWrite,
			CostUSD: cost, Priced: priced,
		}
		if r != nil {
			u.SessionID = r.id
		}
		if err := m.opts.Store.AddUsage(ctx, u); err != nil {
			m.log.Error("Nutzung speichern", "workspace", ws.ID, "err", err)
			continue
		}
		if r == nil {
			continue
		}
		r.mu.Lock()
		r.sessionUSD += cost
		ev := UsageEvent{
			Provider: pu.Provider, Model: pu.Model, InputTokens: pu.Tokens.Input, OutputTokens: pu.Tokens.Output,
			CacheReadTokens: pu.Tokens.CacheRead, CacheWriteTokens: pu.Tokens.CacheWrite, CostUSD: cost, Priced: priced,
			TotalUSD: r.sessionUSD, BudgetUSD: r.budgetUSD,
		}
		r.mu.Unlock()
		m.emit(r.id, TypeUsage, ev)
	}
	if r != nil {
		r.checkBudget(ctx, true)
	}
}

// usageRun liefert den Lauf, dem eine Proxy-Nutzung im Workspace zusteht (nil: keiner).
func (m *Manager) usageRun(workspaceID string) *run {
	m.mu.Lock()
	defer m.mu.Unlock()
	var term *run
	for _, r := range m.runs {
		if r.workspaceID != workspaceID {
			continue
		}
		if !r.caps.Terminal {
			r.mu.Lock()
			metered := r.metered
			r.mu.Unlock()
			if metered {
				return r
			}
			return nil // die CLI meldet selbst; nicht doppelt zählen
		}
		term = r
	}
	return term
}

// checkBudget prüft die Budgets. Ist eins überschritten, merkt es sich die Pause und beendet
// den Prozess (stop) bzw. liefert vor dem Start den Grund. Leer: alles im Rahmen.
func (r *run) checkBudget(ctx context.Context, stop bool) string {
	r.mu.Lock()
	if r.stopAs != "" && r.stopAs != store.SessionPaused {
		r.mu.Unlock()
		return ""
	}
	if r.pause != nil {
		reason := r.pause.reason()
		r.mu.Unlock()
		return reason
	}
	spent, budget, monthly, allow := r.sessionUSD, r.budgetUSD, r.monthlyUSD, r.monthlyAllow
	r.mu.Unlock()

	var p *budgetPause
	if budget > 0 && spent >= budget {
		p = &budgetPause{Scope: BudgetSession, LimitUSD: budget, SpentUSD: spent, StepUSD: budget}
	} else if monthly > 0 {
		t, err := r.m.opts.Store.UsageTotal(ctx, store.UsageFilter{ProjectID: r.projectID, Since: store.MonthStart(time.Now())})
		if err != nil {
			r.m.log.Error("Monatskosten lesen", "projekt", r.projectID, "err", err)
		} else if limit := math.Max(monthly, allow); t.CostUSD >= limit {
			p = &budgetPause{Scope: BudgetMonthly, LimitUSD: limit, SpentUSD: t.CostUSD, StepUSD: math.Max(monthly/10, 1)}
		}
	}
	if p == nil {
		return ""
	}
	r.mu.Lock()
	if r.pause == nil {
		r.pause = p
	}
	p = r.pause
	r.mu.Unlock()
	r.m.log.Warn("Budget überschritten – Session wird pausiert", "session", r.id, "bereich", p.Scope, "kosten", p.SpentUSD, "grenze", p.LimitUSD)
	if stop {
		r.stop(store.SessionPaused)
	}
	return p.reason()
}

// requestBudget legt nach der Pause eine Freigabe an. Erlauben erhöht das Budget um einen
// Schritt und setzt die Session fort; Ablehnen beendet sie.
func (m *Manager) requestBudget(r *run) {
	r.mu.Lock()
	p := r.pause
	r.mu.Unlock()
	if p == nil || m.opts.Budgets == nil {
		return
	}
	detail, _ := json.Marshal(BudgetDetail(*p))
	sess, err := m.opts.Store.GetSession(context.Background(), r.id)
	if err != nil {
		return
	}
	a := &store.Approval{
		Kind: store.ApprovalBudget, ProjectID: r.projectID, WorkspaceID: r.workspaceID, SessionID: r.id,
		Title: p.reason(), Detail: detail, Sandbox: r.sandbox,
	}
	err = m.opts.Budgets.RequestBudget(context.Background(), a, func(res *store.Approval) {
		m.budgetDecided(sess, *p, res)
	})
	if err != nil {
		m.log.Error("Budget-Freigabe anlegen", "session", r.id, "err", err)
	}
}

func (m *Manager) budgetDecided(sess *store.Session, p budgetPause, a *store.Approval) {
	ctx := context.Background()
	cur, err := m.opts.Store.GetSession(ctx, sess.ID)
	if err != nil || cur.Status != store.SessionPaused {
		return // inzwischen gelöscht, fortgesetzt oder beendet
	}
	if a.Status == store.ApprovalCanceled {
		return // z. B. von Hand fortgesetzt; eine neue Pause fragt neu
	}
	if a.Status != store.ApprovalApproved {
		m.finish(sess.ID, store.SessionStopped, p.reason()+" – nicht freigegeben", nil)
		return
	}
	u := store.SessionUpdate{}
	if p.Scope == BudgetMonthly {
		u.MonthlyAllowUSD = store.Ptr(p.SpentUSD + p.StepUSD)
	} else {
		u.BudgetUSD = store.Ptr(p.SpentUSD + p.StepUSD)
	}
	if err := m.opts.Store.UpdateSession(ctx, sess.ID, u); err != nil {
		m.log.Error("Budget speichern", "session", sess.ID, "err", err)
		return
	}
	m.log.Info("Budget freigegeben", "session", sess.ID, "bereich", p.Scope)
	if v, err := m.Get(ctx, sess.ID); err == nil && v.Resumable {
		if _, err := m.Input(ctx, sess.ID, ""); err != nil {
			m.log.Error("Session nach Budget-Freigabe fortsetzen", "session", sess.ID, "err", err)
			m.emit(sess.ID, adapter.TypeError, adapter.Error{Message: "Fortsetzen nach Budget-Freigabe: " + err.Error()})
		}
		return
	}
	m.finish(sess.ID, store.SessionStopped, "Budget freigegeben – die Session lässt sich nicht fortsetzen, bitte neu starten", nil)
}
