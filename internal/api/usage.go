package api

import (
	"net/http"
	"time"

	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/store"
)

func (s *Server) routesUsage(mux *http.ServeMux) {
	if s.opts.Usage == nil {
		return
	}
	mux.HandleFunc("GET /api/usage", s.requireAuth(s.handleUsage))
}

// UsageResponse is the answer of GET /api/usage.
type UsageResponse struct {
	// Total are all costs (of the project, if project_id is set).
	Total store.UsageTotal `json:"total"`
	// Month are the costs in the current calendar month (UTC).
	Month      store.UsageTotal `json:"month"`
	MonthStart time.Time        `json:"month_start"`
	// MonthlyBudgetUSD is the project's budget.monthly_usd (0: none).
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd,omitempty"`
	// Projects: monthly sums per project (without project_id).
	Projects map[string]store.UsageTotal `json:"projects,omitempty"`
	// Workspaces: sums per workspace (with project_id).
	Workspaces map[string]store.UsageTotal `json:"workspaces,omitempty"`
	// PricesAsOf is the date of the built-in price table.
	PricesAsOf string `json:"prices_as_of"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID := r.URL.Query().Get("project_id")
	month := store.MonthStart(time.Now())
	resp := UsageResponse{MonthStart: month, PricesAsOf: provider.PricesAsOf}
	var err error
	if projectID != "" && s.opts.Projects != nil {
		p, perr := s.opts.Projects.Get(ctx, projectID)
		if perr != nil {
			s.writeAppError(w, perr)
			return
		}
		resp.MonthlyBudgetUSD = p.Config.MonthlyBudget()
	}
	if resp.Total, err = s.opts.Usage.UsageTotal(ctx, store.UsageFilter{ProjectID: projectID}); err != nil {
		s.writeAppError(w, err)
		return
	}
	if resp.Month, err = s.opts.Usage.UsageTotal(ctx, store.UsageFilter{ProjectID: projectID, Since: month}); err != nil {
		s.writeAppError(w, err)
		return
	}
	if projectID == "" {
		resp.Projects, err = s.opts.Usage.UsageByProject(ctx, month)
	} else {
		resp.Workspaces, err = s.opts.Usage.UsageByWorkspace(ctx, projectID)
	}
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
