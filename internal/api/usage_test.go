package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/store"
)

func TestUsageAPI(t *testing.T) {
	a := newAgentServer(t)
	ctx := context.Background()
	var ws struct {
		ID string `json:"id"`
	}
	a.call(t, http.MethodPost, "/api/projects/"+a.project+"/workspaces", `{"sandbox":false}`, http.StatusCreated, &ws)
	for _, u := range []store.Usage{
		{TS: store.MonthStart(time.Now()).Add(-time.Hour), CostUSD: 10, InputTokens: 5},
		{CostUSD: 1.5, OutputTokens: 7},
		{CostUSD: 0, OutputTokens: 1, Priced: false},
	} {
		u.ProjectID, u.WorkspaceID, u.Source = a.project, ws.ID, store.UsageProxy
		if u.CostUSD > 0 {
			u.Priced = true
		}
		if err := a.st.AddUsage(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}

	var all UsageResponse
	a.call(t, http.MethodGet, "/api/usage", "", http.StatusOK, &all)
	if all.Total.CostUSD != 11.5 || all.Month.CostUSD != 1.5 || !all.Month.Unpriced || all.Projects[a.project].CostUSD != 1.5 || all.PricesAsOf == "" {
		t.Fatalf("GET /api/usage = %+v", all)
	}
	var one UsageResponse
	a.call(t, http.MethodGet, "/api/usage?project_id="+a.project, "", http.StatusOK, &one)
	if one.Total.CostUSD != 11.5 || one.Workspaces[ws.ID].Count != 3 || one.Total.InputTokens != 5 || one.Total.OutputTokens != 8 {
		t.Fatalf("GET /api/usage?project_id = %+v", one)
	}
	a.call(t, http.MethodGet, "/api/usage?project_id=gibtsnicht", "", http.StatusNotFound, nil)

	// No access without login.
	res, err := http.Get(a.base + "/api/usage")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without login: %d", res.StatusCode)
	}
}
