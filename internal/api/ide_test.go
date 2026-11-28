package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/gateway"
	"github.com/DavidW475/ade/internal/ide"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

func TestIDETickets(t *testing.T) {
	a := NewAuth(testSecret)
	now := time.Now()
	a.now = func() time.Time { return now }
	parent, _ := a.Redeem(a.NewTicket())

	// A ticket only applies to its workspace and only once.
	t1 := a.NewIDETicket("ws1", parent)
	if _, ok := a.RedeemIDE(t1, "ws2"); ok {
		t.Error("ticket redeemed for another workspace")
	}
	if _, ok := a.RedeemIDE(t1, "ws1"); ok {
		t.Error("discarded ticket redeemed again")
	}
	sess, ok := a.RedeemIDE(a.NewIDETicket("ws1", parent), "ws1")
	if !ok {
		t.Fatal("ticket not redeemed")
	}
	if !a.ValidIDESession(sess, "ws1") || a.ValidIDESession(sess, "ws2") || a.ValidIDESession("", "ws1") {
		t.Error("IDE session checked wrongly")
	}
	// An IDE session is not a dashboard session.
	if a.ValidSession(sess) {
		t.Error("IDE session applies to the dashboard")
	}

	// Abgelaufenes Ticket.
	t3 := a.NewIDETicket("ws1", parent)
	now = now.Add(TicketTTL)
	if _, ok := a.RedeemIDE(t3, "ws1"); ok {
		t.Error("expired ticket redeemed")
	}

	// Logging out of the dashboard session also ends the IDE session.
	a.Logout(parent)
	if a.ValidIDESession(sess, "ws1") {
		t.Error("IDE session valid after logout")
	}
	if _, ok := a.RedeemIDE(a.NewIDETicket("ws1", parent), "ws1"); ok {
		t.Error("ticket of an ended session redeemed")
	}

	// Without a dashboard session (CLI with secret) its own lifetime applies.
	cli, ok := a.RedeemIDE(a.NewIDETicket("ws1", ""), "ws1")
	if !ok || !a.ValidIDESession(cli, "ws1") {
		t.Fatal("CLI-Ticket")
	}
	now = now.Add(SessionTTL)
	if a.ValidIDESession(cli, "ws1") {
		t.Error("expired IDE session valid")
	}
}

type ideServer struct {
	s    *Server
	auth *Auth
	ws   *store.Workspace
}

// newIDEServer builds a server with project, workspace and an IDE whose binary is missing.
// The tests in internal/gateway and internal/ide check the proxying itself.
func newIDEServer(t *testing.T) *ideServer {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	wm := workspace.NewManager(st, ps, filepath.Join(t.TempDir(), "ws"), log)
	t.Cleanup(wm.Close)
	ides := ide.NewManager(ide.Options{
		Dir:    t.TempDir(),
		Binary: "/does/not/exist/openvscode-server",
		Logger: log,
		Workspaces: func(ctx context.Context, id string) (string, error) {
			w, err := wm.Get(ctx, id)
			if err != nil {
				return "", err
			}
			return w.Path, nil
		},
	})
	t.Cleanup(ides.Close)
	auth := NewAuth(testSecret)
	s := New(Options{Version: "test", Port: "7000", Store: st, Auth: auth, Logger: log, Projects: ps, Workspaces: wm, IDE: ides})

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	var p project.View
	api(t, s, "POST", "/api/projects", `{"path":"`+repo+`"}`, http.StatusCreated, &p)
	var w store.Workspace
	api(t, s, "POST", "/api/projects/"+p.ID+"/workspaces", "", http.StatusCreated, &w)
	return &ideServer{s: s, auth: auth, ws: &w}
}

func TestIDERoutes(t *testing.T) {
	x := newIDEServer(t)
	base := "/api/workspaces/" + x.ws.ID + "/ide"

	var st IDEStatus
	api(t, x.s, "GET", base, "", http.StatusOK, &st)
	if st.State != ide.StateStopped || st.URL != "http://ws-"+x.ws.ID+".localhost:7000/" {
		t.Fatalf("Status = %#v", st)
	}
	api(t, x.s, "GET", "/api/workspaces/missing/ide", "", http.StatusNotFound, nil)
	api(t, x.s, "POST", "/api/workspaces/missing/ide", "", http.StatusNotFound, nil)

	api(t, x.s, "POST", base, "", http.StatusAccepted, &st)
	deadline := time.Now().Add(5 * time.Second)
	for st.State != ide.StateError {
		if time.Now().After(deadline) {
			t.Fatalf("Status = %#v, want error", st)
		}
		time.Sleep(20 * time.Millisecond)
		api(t, x.s, "GET", base, "", http.StatusOK, &st)
	}
	if !strings.Contains(st.Error, "not found") {
		t.Errorf("error = %q", st.Error)
	}
	api(t, x.s, "DELETE", base, "", http.StatusNoContent, nil)
	api(t, x.s, "GET", base, "", http.StatusOK, &st)
	if st.State != ide.StateStopped {
		t.Errorf("status after Stop = %#v", st)
	}

	// Without login there is neither status nor ticket.
	if w := do(x.s, "GET", base, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("status without auth: %d", w.Code)
	}
	if w := do(x.s, "POST", base+"/ticket", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("ticket without auth: %d", w.Code)
	}
}

// TestIDEOnlyWithSession checks the path dashboard → ticket → subdomain through the whole
// server (host routing, origin check, gateway).
func TestIDEOnlyWithSession(t *testing.T) {
	x := newIDEServer(t)
	host := "ws-" + x.ws.ID + ".localhost:7000"
	onHost := func(h string, mod func(*http.Request)) func(*http.Request) {
		return func(r *http.Request) {
			r.Host = h
			if mod != nil {
				mod(r)
			}
		}
	}

	// Without IDE cookie: locked – even with a dashboard cookie or the daemon secret.
	dash, _ := x.auth.Redeem(x.auth.NewTicket())
	for name, mod := range map[string]func(*http.Request){
		"ohne":             nil,
		"dashboard cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash}) },
		"Daemon-Secret":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testSecret) },
	} {
		if w := do(x.s, "GET", "/", onHost(host, mod)); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: Code = %d, want 401", name, w.Code)
		}
	}

	// Fetch a ticket through the dashboard (with dashboard session).
	w := do(x.s, "POST", "/api/workspaces/"+x.ws.ID+"/ide/ticket", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash})
		r.Header.Set("Origin", "http://localhost:7000")
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("Ticket: %d %s", w.Code, w.Body.String())
	}
	var tr IDETicketResponse
	_ = json.NewDecoder(w.Body).Decode(&tr)
	u, err := url.Parse(tr.URL)
	if err != nil || u.Host != host || u.Path != gateway.LoginPath {
		t.Fatalf("Ticket-URL = %q", tr.URL)
	}

	// The ticket does not apply on the dashboard and not for another IDE.
	if w := do(x.s, "GET", u.RequestURI(), onHost("ws-anders.localhost:7000", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("ticket on another IDE: %d", w.Code)
	}
	w = do(x.s, "POST", "/api/workspaces/"+x.ws.ID+"/ide/ticket", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash})
	})
	_ = json.NewDecoder(w.Body).Decode(&tr)
	u, _ = url.Parse(tr.URL)

	w = do(x.s, "GET", u.RequestURI(), onHost(host, nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("Login: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	// The dashboard CSP does not apply to the IDE (VS Code brings its own).
	if w.Header().Get("X-Frame-Options") != "" {
		t.Error("dashboard header on the IDE subdomain")
	}

	// With cookie the request goes to the IDE (here: binary missing → 502).
	w = do(x.s, "GET", "/", onHost(host, func(r *http.Request) { r.AddCookie(cookie) }))
	if w.Code != http.StatusBadGateway {
		t.Errorf("with IDE cookie: %d %s", w.Code, w.Body.String())
	}

	// The IDE cookie does not open the dashboard.
	if w := do(x.s, "GET", "/api/session", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie.Value})
	}); w.Code != http.StatusUnauthorized {
		t.Errorf("IDE-Session am Dashboard: %d", w.Code)
	}

	// WebSockets and write access to the IDE only from its own origin.
	for origin, want := range map[string]int{
		"http://" + host:                  http.StatusBadGateway,
		"http://ws-anders.localhost:7000": http.StatusForbidden,
		"http://localhost:7000":           http.StatusForbidden,
		"http://evil.example":             http.StatusForbidden,
	} {
		w := do(x.s, "GET", "/socket", onHost(host, func(r *http.Request) {
			r.AddCookie(cookie)
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Origin", origin)
		}))
		if w.Code != want {
			t.Errorf("WebSocket von %s: %d, want %d", origin, w.Code, want)
		}
	}

	// Logging out in the dashboard also locks the IDE.
	do(x.s, "POST", "/api/auth/logout", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash}) })
	if w := do(x.s, "GET", "/", onHost(host, func(r *http.Request) { r.AddCookie(cookie) })); w.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", w.Code)
	}
}
