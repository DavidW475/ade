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

// TypeUsage reports a usage measured by the egress proxy (payload UsageEvent). The timeline
// does not show it; the UI uses it to update costs and budget live.
const TypeUsage = "usage"

// UsageEvent is the payload of TypeUsage.
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

// Scopes of a budget.
const (
	BudgetSession = "session"
	BudgetMonthly = "monthly"
)

// BudgetDetail is the content of a budget approval.
type BudgetDetail struct {
	Scope    string  `json:"scope"`
	LimitUSD float64 `json:"limit_usd"`
	SpentUSD float64 `json:"spent_usd"`
	// StepUSD is the amount by which an approval raises the budget.
	StepUSD float64 `json:"step_usd"`
}

type budgetPause BudgetDetail

func (p *budgetPause) reason() string {
	if p.Scope == BudgetMonthly {
		return fmt.Sprintf("monthly budget of the project exceeded (%s of %s)", usd(p.SpentUSD), usd(p.LimitUSD))
	}
	return fmt.Sprintf("budget of the session exceeded (%s of %s)", usd(p.SpentUSD), usd(p.LimitUSD))
}

func usd(v float64) string { return fmt.Sprintf("%.2f $", v) }

func tokensOf(c adapter.Cost) provider.Usage {
	return provider.Usage{Input: c.InputTokens, Output: c.OutputTokens, CacheRead: c.CacheReadTokens, CacheWrite: c.CacheWriteTokens}
}

// turnCost books the cost report of a turn and completes the payload. If the proxy
// measures, its sum since the last turn counts, not the CLI's report.
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
		m.log.Error("store costs", "session", r.id, "err", err)
	}
	r.mu.Lock()
	r.sessionUSD += c.TurnUSD
	r.turnBaseUSD = r.sessionUSD
	c.TotalUSD, c.BudgetUSD = r.sessionUSD, r.budgetUSD
	r.mu.Unlock()
	return c
}

// ProxyUsage is a usage measured by the egress proxy.
type ProxyUsage struct {
	Provider string
	Model    string
	Tokens   provider.Usage
	Time     time.Time
}

// RecordProxyUsage books usages that a workspace's egress proxy measured. They count for
// the agent in the workspace if its usage is measured through the proxy, or for a terminal;
// otherwise only for the workspace (no double counting with the CLI).
func (m *Manager) RecordProxyUsage(ctx context.Context, workspaceID string, us []ProxyUsage) {
	if len(us) == 0 {
		return
	}
	ws, err := m.opts.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		m.log.Warn("usage for an unknown workspace", "workspace", workspaceID, "err", err)
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
			m.log.Error("store usage", "workspace", ws.ID, "err", err)
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

// usageRun returns the run a proxy usage in the workspace belongs to (nil: none).
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
			return nil // the CLI reports itself; do not count twice
		}
		term = r
	}
	return term
}

// checkBudget checks the budgets. If one is exceeded, it remembers the pause and ends the
// process (stop) or, before the start, returns the reason. Empty: everything within limits.
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
			r.m.log.Error("read monthly costs", "project", r.projectID, "err", err)
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
	r.m.log.Warn("budget exceeded – pausing session", "session", r.id, "scope", p.Scope, "costs", p.SpentUSD, "limit", p.LimitUSD)
	if stop {
		r.stop(store.SessionPaused)
	}
	return p.reason()
}

// requestBudget creates an approval after the pause. Allowing raises the budget by one step
// and resumes the session; denying ends it.
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
		m.log.Error("create budget approval", "session", r.id, "err", err)
	}
}

func (m *Manager) budgetDecided(sess *store.Session, p budgetPause, a *store.Approval) {
	ctx := context.Background()
	cur, err := m.opts.Store.GetSession(ctx, sess.ID)
	if err != nil || cur.Status != store.SessionPaused {
		return // deleted, resumed or ended in the meantime
	}
	if a.Status == store.ApprovalCanceled {
		return // e.g. resumed by hand; a new pause asks anew
	}
	if a.Status != store.ApprovalApproved {
		m.finish(sess.ID, store.SessionStopped, p.reason()+" – not approved", nil)
		return
	}
	u := store.SessionUpdate{}
	if p.Scope == BudgetMonthly {
		u.MonthlyAllowUSD = store.Ptr(p.SpentUSD + p.StepUSD)
	} else {
		u.BudgetUSD = store.Ptr(p.SpentUSD + p.StepUSD)
	}
	if err := m.opts.Store.UpdateSession(ctx, sess.ID, u); err != nil {
		m.log.Error("store budget", "session", sess.ID, "err", err)
		return
	}
	m.log.Info("budget approved", "session", sess.ID, "scope", p.Scope)
	if v, err := m.Get(ctx, sess.ID); err == nil && v.Resumable {
		if _, err := m.Input(ctx, sess.ID, ""); err != nil {
			m.log.Error("resume session after budget approval", "session", sess.ID, "err", err)
			m.emit(sess.ID, adapter.TypeError, adapter.Error{Message: "resume after budget approval: " + err.Error()})
		}
		return
	}
	m.finish(sess.ID, store.SessionStopped, "budget approved – the session cannot be resumed, please start a new one", nil)
}
