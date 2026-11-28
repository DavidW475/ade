package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/store"
)

func TestApprovalRoutes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := approval.New(approval.Options{Store: st, Timeout: time.Minute, Logger: log})
	t.Cleanup(svc.Close)
	s := New(Options{Version: "test", Port: "7000", Store: st, Auth: NewAuth(testSecret), Logger: log, Approvals: svc})

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateProject(ctx, &store.Project{ID: "p1", Name: "demo", RepoPath: "/repo", DefaultBranch: "main"}))
	must(st.CreateWorkspace(ctx, &store.Workspace{ID: "w1", ProjectID: "p1", Path: "/ws/w1", Branch: "ade/w1", BaseBranch: "main", Status: store.WorkspaceReady}))
	must(st.CreateSession(ctx, &store.Session{ID: "s1", WorkspaceID: "w1", Adapter: "claude", Status: store.SessionRunning}))

	for _, rt := range [][2]string{{"GET", "/api/approvals"}, {"GET", "/api/approvals/x"}, {"POST", "/api/approvals/x"}} {
		r := httptest.NewRequest(rt[0], rt[1], nil)
		r.Host = "localhost:7000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: %d", rt[0], rt[1], w.Code)
		}
	}

	hook := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/hook/pretooluse", strings.NewReader(body))
		r.Host = "127.0.0.1:7000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	call := `{"session":"s1","input":{"tool_name":"Bash","tool_input":{"command":"make"}}}`
	// Neither the daemon secret nor a foreign token opens the hook endpoint.
	for _, tok := range []string{"", testSecret, "falsch"} {
		if w := hook(tok, call); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"deny"`) {
			t.Errorf("Token %q: %d %s", tok, w.Code, w.Body.String())
		}
	}
	tok := svc.IssueToken("s1")
	if w := hook(tok, "broken"); w.Code != http.StatusBadRequest {
		t.Errorf("kaputter Body: %d", w.Code)
	}

	done := make(chan proxy.Answer, 1)
	go func() {
		w := hook(tok, call)
		var a proxy.Answer
		_ = json.NewDecoder(w.Body).Decode(&a)
		done <- a
	}()
	var list []store.Approval
	deadline := time.Now().Add(3 * time.Second)
	for len(list) == 0 && time.Now().Before(deadline) {
		api(t, s, "GET", "/api/approvals?status=pending", "", http.StatusOK, &list)
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 || list[0].Title != "Bash: make" || list[0].SessionID != "s1" {
		t.Fatalf("Freigaben %+v", list)
	}
	id := list[0].ID
	var one store.Approval
	api(t, s, "GET", "/api/approvals/"+id, "", http.StatusOK, &one)
	api(t, s, "POST", "/api/approvals/"+id, `{"decision":"vielleicht"}`, http.StatusBadRequest, nil)
	api(t, s, "POST", "/api/approvals/"+id, `{"decision":"allow","reason":"passt"}`, http.StatusOK, &one)
	if one.Status != store.ApprovalApproved || one.Reason != "passt" {
		t.Fatalf("approval %+v", one)
	}
	if a := <-done; a.Decision != proxy.AnswerAllow {
		t.Fatalf("answer %+v", a)
	}
	api(t, s, "POST", "/api/approvals/"+id, `{"decision":"deny"}`, http.StatusConflict, nil)
	api(t, s, "GET", "/api/approvals/gibtsnicht", "", http.StatusNotFound, nil)
	api(t, s, "GET", "/api/approvals?session_id=s1", "", http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("Filter: %+v", list)
	}
}
