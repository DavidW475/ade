package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/agent/adapter/pty"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

type agentServer struct {
	srv     *httptest.Server
	handler *Server
	auth    *Auth
	base    string // http://localhost:<port>
	project string
}

// newAgentServer startet einen echten HTTP-Server (für WebSockets) mit Fake- und pty-Adapter
// und einem registrierten Projekt.
func newAgentServer(t *testing.T) *agentServer {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	ws := workspace.NewManager(st, ps, filepath.Join(t.TempDir(), "ws"), log)
	t.Cleanup(ws.Close)
	am := agent.NewManager(agent.Options{
		Store: st, Projects: ps, Workspaces: ws, Adapters: adapter.NewRegistry(&fake.Adapter{}, pty.New()),
		DataDir: dataDir, Logger: log, DefaultAdapter: "fake", FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(am.Close)
	ws.SetBeforeDelete(am.BeforeWorkspaceDelete)

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	auth := NewAuth(testSecret)
	h := New(Options{Version: "test", Port: port, Store: st, Auth: auth, Logger: log, Projects: ps, Workspaces: ws, Agents: am, DefaultAdapter: "fake"})
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)
	return &agentServer{srv: srv, handler: h, auth: auth, base: "http://localhost:" + port, project: p.ID}
}

func (a *agentServer) call(t *testing.T, method, path, body string, wantCode int, out any) {
	t.Helper()
	req, err := http.NewRequest(method, a.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != wantCode {
		t.Fatalf("%s %s: Code = %d, want %d, Body = %s", method, path, res.StatusCode, wantCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v (%s)", method, path, err, data)
		}
	}
}

// dial verbindet einen WebSocket und liefert den HTTP-Status der Upgrade-Antwort.
func (a *agentServer) dial(t *testing.T, path string, header http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if header == nil {
		header = http.Header{"Authorization": {"Bearer " + testSecret}}
	}
	c, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.base, "http")+path, &websocket.DialOptions{HTTPHeader: header})
	code := 0
	if res != nil {
		code = res.StatusCode
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}
	return c, code, err
}

func readEvent(t *testing.T, c *websocket.Conn) store.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Event lesen: %v", err)
	}
	var e store.Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// readUntil liest Events, bis eines pred erfüllt; liefert alle gelesenen.
func readUntil(t *testing.T, c *websocket.Conn, pred func(store.Event) bool) []store.Event {
	t.Helper()
	var out []store.Event
	for {
		e := readEvent(t, c)
		out = append(out, e)
		if pred(e) {
			return out
		}
	}
}

func isEcho(text string) func(store.Event) bool {
	return func(e store.Event) bool {
		return e.Type == adapter.TypeMessage && strings.Contains(string(e.Payload), "echo: "+text)
	}
}

func TestAgentFlowWithEventStream(t *testing.T) {
	a := newAgentServer(t)

	var adapters AdaptersResponse
	a.call(t, "GET", "/api/adapters", "", http.StatusOK, &adapters)
	if adapters.Default != "fake" || len(adapters.Adapters) != 2 || len(adapters.PermissionModes) == 0 {
		t.Fatalf("Adapter = %+v", adapters)
	}

	var v agent.View
	a.call(t, "POST", "/api/agents", fmt.Sprintf(`{"project_id":%q,"task":"hallo"}`, a.project), http.StatusCreated, &v)
	if v.ID == "" || v.WorkspaceID == "" || !v.Capabilities.Input {
		t.Fatalf("Session = %+v", v)
	}

	// Erster Client: komplette Historie, dann live.
	c1, _, err := a.dial(t, "/ws/sessions/"+v.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c1.CloseNow() }()
	first := readUntil(t, c1, isEcho("hallo"))
	if first[0].Seq != 1 || first[0].Type != adapter.TypeStatus {
		t.Fatalf("erstes Event = %+v", first[0])
	}

	a.call(t, "POST", "/api/sessions/"+v.ID+"/input", `{"text":"weiter"}`, http.StatusOK, nil)
	live := readUntil(t, c1, isEcho("weiter"))
	for i, e := range append(first, live...) {
		if e.Seq != int64(i+1) {
			t.Fatalf("Event %d hat seq %d", i, e.Seq)
		}
	}
	last := live[len(live)-1].Seq

	// Zweiter Client mit since: nur die Events danach, dann live (Fan-out).
	c2, _, err := a.dial(t, fmt.Sprintf("/ws/sessions/%s/events?since=%d", v.ID, last-1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.CloseNow() }()
	if e := readEvent(t, c2); e.Seq != last {
		t.Fatalf("Replay ab since: seq %d, want %d", e.Seq, last)
	}

	a.call(t, "POST", "/api/sessions/"+v.ID+"/stop", "", http.StatusOK, &v)
	if v.Status != store.SessionStopped {
		t.Fatalf("nach Stop: %+v", v)
	}
	isStopped := func(e store.Event) bool {
		return e.Type == adapter.TypeStatus && strings.Contains(string(e.Payload), `"stopped"`)
	}
	readUntil(t, c1, isStopped)
	readUntil(t, c2, isStopped)

	var evs []store.Event
	a.call(t, "GET", "/api/sessions/"+v.ID+"/events?since=0", "", http.StatusOK, &evs)
	if len(evs) == 0 || evs[len(evs)-1].Seq < last {
		t.Fatalf("REST-Events: %d", len(evs))
	}
	var list []agent.View
	a.call(t, "GET", "/api/sessions?project_id="+a.project, "", http.StatusOK, &list)
	if len(list) != 1 || list[0].ID != v.ID {
		t.Fatalf("Liste = %+v", list)
	}
	a.call(t, "GET", "/api/sessions?active=true", "", http.StatusOK, &list)
	if len(list) != 0 {
		t.Fatalf("aktive Sessions = %+v", list)
	}

	a.call(t, "POST", "/api/agents", fmt.Sprintf(`{"project_id":%q}`, a.project), http.StatusBadRequest, nil)
	a.call(t, "GET", "/api/sessions/gibtsnicht", "", http.StatusNotFound, nil)
	a.call(t, "GET", "/api/sessions/"+v.ID+"/events?since=-1", "", http.StatusBadRequest, nil)
}

func TestWebSocketGuards(t *testing.T) {
	a := newAgentServer(t)
	var v agent.View
	a.call(t, "POST", "/api/agents", fmt.Sprintf(`{"project_id":%q,"task":"x"}`, a.project), http.StatusCreated, &v)

	for _, path := range []string{"/ws/sessions/" + v.ID + "/events", "/ws/sessions/" + v.ID + "/pty"} {
		// Ohne Anmeldung.
		if _, code, err := a.dial(t, path, http.Header{}); err == nil || code != http.StatusUnauthorized {
			t.Errorf("%s ohne Auth: %d %v", path, code, err)
		}
		// Fremde Webseite (Cross-Site-WebSocket-Hijacking) – auch mit gültigem Cookie.
		h := http.Header{"Origin": {"http://evil.example"}, "Cookie": {SessionCookie + "=" + a.loginCookie(t)}}
		if _, code, err := a.dial(t, path, h); err == nil || code != http.StatusForbidden {
			t.Errorf("%s mit fremdem Origin: %d %v", path, code, err)
		}
	}
	// Browser-Session mit eigenem Origin funktioniert.
	h := http.Header{"Origin": {a.base}, "Cookie": {SessionCookie + "=" + a.loginCookie(t)}}
	c, _, err := a.dial(t, "/ws/sessions/"+v.ID+"/events", h)
	if err != nil {
		t.Fatalf("eigener Origin: %v", err)
	}
	_ = c.CloseNow()

	// Agent-Session hat kein Terminal; unbekannte Session.
	if _, code, err := a.dial(t, "/ws/sessions/"+v.ID+"/pty", nil); err == nil || code != http.StatusBadRequest {
		t.Errorf("pty einer Agent-Session: %d %v", code, err)
	}
	if _, code, err := a.dial(t, "/ws/sessions/gibtsnicht/events", nil); err == nil || code != http.StatusNotFound {
		t.Errorf("unbekannte Session: %d %v", code, err)
	}
	a.call(t, "GET", "/ws/unbekannt", "", http.StatusNotFound, nil)
}

// loginCookie meldet einen Browser an und liefert den Wert des Session-Cookies.
func (a *agentServer) loginCookie(t *testing.T) string {
	t.Helper()
	id, ok := a.auth.Redeem(a.auth.NewTicket())
	if !ok {
		t.Fatal("Ticket nicht einlösbar")
	}
	return id
}

func TestPTYWebSocket(t *testing.T) {
	a := newAgentServer(t)
	var v agent.View
	a.call(t, "POST", "/api/agents", fmt.Sprintf(`{"project_id":%q,"adapter":"pty","command":["sh"]}`, a.project), http.StatusCreated, &v)
	waitRunning := time.Now().Add(5 * time.Second)
	for v.Status != store.SessionRunning {
		if time.Now().After(waitRunning) {
			t.Fatalf("Terminal startet nicht: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
		a.call(t, "GET", "/api/sessions/"+v.ID, "", http.StatusOK, &v)
	}

	c, _, err := a.dial(t, "/ws/sessions/"+v.ID+"/pty", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":20}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("stty size; echo summe-$((2+3))\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), "summe-5") {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("lesen: %v (bisher %q)", err, out.String())
		}
		out.Write(data)
	}
	if !strings.Contains(out.String(), "20 90") {
		t.Errorf("Größenänderung nicht angekommen: %q", out.String())
	}

	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"exit\n"}`)); err != nil {
		t.Fatal(err)
	}
	var exit bool
	for !exit {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("Ende nicht gemeldet: %v", err)
		}
		exit = typ == websocket.MessageText && strings.Contains(string(data), `"exit"`)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("Close = %v", err)
	}

	// Nach dem Ende: Ausgabe aus dem Log, dann exit.
	c2, _, err := a.dial(t, "/ws/sessions/"+v.ID+"/pty", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.CloseNow() }()
	_, data, err := c2.Read(ctx)
	if err != nil || !strings.Contains(string(data), "summe-5") {
		t.Fatalf("Replay nach Ende: %q %v", data, err)
	}
}

func TestDeleteWorkspaceStopsAgent(t *testing.T) {
	a := newAgentServer(t)
	var v agent.View
	a.call(t, "POST", "/api/agents", fmt.Sprintf(`{"project_id":%q,"task":"x"}`, a.project), http.StatusCreated, &v)
	a.call(t, "DELETE", "/api/workspaces/"+v.WorkspaceID, "", http.StatusNoContent, nil)
	a.call(t, "GET", "/api/sessions/"+v.ID, "", http.StatusNotFound, nil)
}
