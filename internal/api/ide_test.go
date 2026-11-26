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

	// Ticket gilt nur für seinen Workspace und nur einmal.
	t1 := a.NewIDETicket("ws1", parent)
	if _, ok := a.RedeemIDE(t1, "ws2"); ok {
		t.Error("Ticket für anderen Workspace eingelöst")
	}
	if _, ok := a.RedeemIDE(t1, "ws1"); ok {
		t.Error("verworfenes Ticket erneut eingelöst")
	}
	sess, ok := a.RedeemIDE(a.NewIDETicket("ws1", parent), "ws1")
	if !ok {
		t.Fatal("Ticket nicht eingelöst")
	}
	if !a.ValidIDESession(sess, "ws1") || a.ValidIDESession(sess, "ws2") || a.ValidIDESession("", "ws1") {
		t.Error("IDE-Session falsch geprüft")
	}
	// Eine IDE-Session ist keine Dashboard-Session.
	if a.ValidSession(sess) {
		t.Error("IDE-Session gilt fürs Dashboard")
	}

	// Abgelaufenes Ticket.
	t3 := a.NewIDETicket("ws1", parent)
	now = now.Add(TicketTTL)
	if _, ok := a.RedeemIDE(t3, "ws1"); ok {
		t.Error("abgelaufenes Ticket eingelöst")
	}

	// Logout der Dashboard-Session beendet auch die IDE-Session.
	a.Logout(parent)
	if a.ValidIDESession(sess, "ws1") {
		t.Error("IDE-Session nach Logout gültig")
	}
	if _, ok := a.RedeemIDE(a.NewIDETicket("ws1", parent), "ws1"); ok {
		t.Error("Ticket einer beendeten Session eingelöst")
	}

	// Ohne Dashboard-Session (CLI mit Secret) gilt die eigene Laufzeit.
	cli, ok := a.RedeemIDE(a.NewIDETicket("ws1", ""), "ws1")
	if !ok || !a.ValidIDESession(cli, "ws1") {
		t.Fatal("CLI-Ticket")
	}
	now = now.Add(SessionTTL)
	if a.ValidIDESession(cli, "ws1") {
		t.Error("abgelaufene IDE-Session gültig")
	}
}

type ideServer struct {
	s    *Server
	auth *Auth
	ws   *store.Workspace
}

// newIDEServer baut einen Server mit Projekt, Workspace und einer IDE, deren Binary fehlt.
// Das Proxying selbst prüfen die Tests in internal/gateway und internal/ide.
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
		Binary: "/gibt/es/nicht/openvscode-server",
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
	api(t, x.s, "GET", "/api/workspaces/fehlt/ide", "", http.StatusNotFound, nil)
	api(t, x.s, "POST", "/api/workspaces/fehlt/ide", "", http.StatusNotFound, nil)

	api(t, x.s, "POST", base, "", http.StatusAccepted, &st)
	deadline := time.Now().Add(5 * time.Second)
	for st.State != ide.StateError {
		if time.Now().After(deadline) {
			t.Fatalf("Status = %#v, want error", st)
		}
		time.Sleep(20 * time.Millisecond)
		api(t, x.s, "GET", base, "", http.StatusOK, &st)
	}
	if !strings.Contains(st.Error, "nicht gefunden") {
		t.Errorf("Fehler = %q", st.Error)
	}
	api(t, x.s, "DELETE", base, "", http.StatusNoContent, nil)
	api(t, x.s, "GET", base, "", http.StatusOK, &st)
	if st.State != ide.StateStopped {
		t.Errorf("Status nach Stop = %#v", st)
	}

	// Ohne Anmeldung gibt es weder Status noch Ticket.
	if w := do(x.s, "GET", base, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("Status ohne Auth: %d", w.Code)
	}
	if w := do(x.s, "POST", base+"/ticket", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("Ticket ohne Auth: %d", w.Code)
	}
}

// TestIDEOnlyWithSession prüft den Weg Dashboard → Ticket → Subdomain durch den ganzen
// Server (Host-Routing, Origin-Prüfung, Gateway).
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

	// Ohne IDE-Cookie: gesperrt – auch mit Dashboard-Cookie oder Daemon-Secret.
	dash, _ := x.auth.Redeem(x.auth.NewTicket())
	for name, mod := range map[string]func(*http.Request){
		"ohne":             nil,
		"Dashboard-Cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash}) },
		"Daemon-Secret":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testSecret) },
	} {
		if w := do(x.s, "GET", "/", onHost(host, mod)); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: Code = %d, want 401", name, w.Code)
		}
	}

	// Ticket über das Dashboard holen (mit Dashboard-Session).
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

	// Das Ticket gilt nicht auf dem Dashboard und nicht für eine andere IDE.
	if w := do(x.s, "GET", u.RequestURI(), onHost("ws-anders.localhost:7000", nil)); w.Code != http.StatusUnauthorized {
		t.Errorf("Ticket auf anderer IDE: %d", w.Code)
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
	// Die Dashboard-CSP gilt nicht für die IDE (VS Code bringt eine eigene mit).
	if w.Header().Get("X-Frame-Options") != "" {
		t.Error("Dashboard-Header auf der IDE-Subdomain")
	}

	// Mit Cookie geht die Anfrage an die IDE (hier: Binary fehlt → 502).
	w = do(x.s, "GET", "/", onHost(host, func(r *http.Request) { r.AddCookie(cookie) }))
	if w.Code != http.StatusBadGateway {
		t.Errorf("mit IDE-Cookie: %d %s", w.Code, w.Body.String())
	}

	// Das IDE-Cookie öffnet nicht das Dashboard.
	if w := do(x.s, "GET", "/api/session", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie.Value})
	}); w.Code != http.StatusUnauthorized {
		t.Errorf("IDE-Session am Dashboard: %d", w.Code)
	}

	// WebSockets und Schreibzugriffe auf die IDE nur vom eigenen Origin.
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

	// Logout im Dashboard sperrt auch die IDE.
	do(x.s, "POST", "/api/auth/logout", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookie, Value: dash}) })
	if w := do(x.s, "GET", "/", onHost(host, func(r *http.Request) { r.AddCookie(cookie) })); w.Code != http.StatusUnauthorized {
		t.Errorf("nach Logout: %d", w.Code)
	}
}
