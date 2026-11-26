package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/DavidW475/ade/internal/ide"
)

type fakeAuth struct {
	mu       sync.Mutex
	tickets  map[string]string // Ticket → Workspace
	sessions map[string]string // Session → Workspace
}

func (a *fakeAuth) RedeemIDE(ticket, ws string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.tickets[ticket]
	delete(a.tickets, ticket)
	if !ok || w != ws {
		return "", false
	}
	id := "sess-" + ticket
	a.sessions[id] = ws
	return id, true
}

func (a *fakeAuth) ValidIDESession(id, ws string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id] == ws && id != ""
}

type fakeIDEs struct {
	url      *url.URL
	err      error
	acquired atomic.Int32
	released atomic.Int32
}

func (f *fakeIDEs) Acquire(_ context.Context, ws string) (*ide.Upstream, error) {
	if f.err != nil {
		return nil, f.err
	}
	if ws != "abc" {
		return nil, errors.New("falscher Workspace " + ws)
	}
	f.acquired.Add(1)
	return &ide.Upstream{URL: f.url, Token: "tkn-geheim", Release: func() { f.released.Add(1) }}, nil
}

type seen struct {
	mu     sync.Mutex
	host   string
	cookie string
}

// upstream spielt openvscode-server: prüft den Token und bietet ein WebSocket-Echo.
func upstream(t *testing.T, s *seen) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.host, s.cookie = r.Host, r.Header.Get("Cookie")
		s.mu.Unlock()
		if c, err := r.Cookie("vscode-tkn"); err != nil || c.Value != "tkn-geheim" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			for {
				typ, data, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if err := c.Write(r.Context(), typ, data); err != nil {
					return
				}
			}
		}
		_, _ = io.WriteString(w, "IDE "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func setup(t *testing.T) (*Gateway, *fakeAuth, *fakeIDEs, *seen) {
	t.Helper()
	s := &seen{}
	auth := &fakeAuth{tickets: map[string]string{"t1": "abc", "t2": "andere"}, sessions: map[string]string{}}
	ides := &fakeIDEs{url: upstream(t, s)}
	g := New(Options{Auth: auth, IDEs: ides, DashboardURL: "http://localhost:7000/", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return g, auth, ides, s
}

func req(g http.Handler, method, host, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestWorkspaceFromHost(t *testing.T) {
	cases := map[string]string{
		"ws-abc.localhost":   "abc",
		"WS-ABC.localhost":   "abc",
		"ws-abc.localhost.":  "abc",
		"ws-.localhost":      "",
		"ws-a-b.localhost":   "",
		"x.ws-abc.localhost": "",
		"abc.localhost":      "",
		"ws-abc.example":     "",
	}
	for host, want := range cases {
		got, ok := WorkspaceFromHost(host)
		if got != want || ok != (want != "") {
			t.Errorf("%q → %q, %v; want %q", host, got, ok, want)
		}
	}
}

func TestRequiresSession(t *testing.T) {
	g, auth, ides, _ := setup(t)
	auth.sessions["fremd"] = "andere"

	for name, cookies := range map[string][]*http.Cookie{
		"ohne Cookie":       nil,
		"falsches Cookie":   {{Name: Cookie, Value: "erfunden"}},
		"anderer Workspace": {{Name: Cookie, Value: "fremd"}},
		"nur IDE-Token":     {{Name: "vscode-tkn", Value: "tkn-geheim"}},
	} {
		w := req(g, "GET", "ws-abc.localhost:7000", "/", cookies...)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: Code = %d, want 401", name, w.Code)
		}
	}
	if ides.acquired.Load() != 0 {
		t.Error("IDE ohne Session angefordert")
	}
	if w := req(g, "GET", "foo.localhost:7000", "/"); w.Code != http.StatusNotFound {
		t.Errorf("fremde Subdomain: Code = %d", w.Code)
	}
}

func TestLoginAndProxy(t *testing.T) {
	g, _, ides, s := setup(t)

	// Ticket für einen anderen Workspace gilt hier nicht.
	if w := req(g, "GET", "ws-abc.localhost:7000", LoginPath+"?ticket=t2"); w.Code != http.StatusUnauthorized {
		t.Errorf("fremdes Ticket: Code = %d", w.Code)
	}

	w := req(g, "GET", "ws-abc.localhost:7000", LoginPath+"?ticket=t1")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("Login: Code = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}
	resp := w.Result()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != Cookie || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" {
		t.Errorf("Cookie = %#v", c)
	}
	// Ticket gilt nur einmal.
	if w := req(g, "GET", "ws-abc.localhost:7000", LoginPath+"?ticket=t1"); w.Code != http.StatusUnauthorized {
		t.Errorf("zweites Einlösen: Code = %d", w.Code)
	}

	w = req(g, "GET", "ws-abc.localhost:7000", "/static/x.js", c,
		&http.Cookie{Name: "vscode-tkn", Value: "vom-browser"}, &http.Cookie{Name: "andere", Value: "1"})
	if w.Code != http.StatusOK || w.Body.String() != "IDE /static/x.js" {
		t.Fatalf("Proxy: Code = %d, Body = %q", w.Code, w.Body.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host != "ws-abc.localhost:7000" {
		t.Errorf("Host beim Upstream = %q", s.host)
	}
	if strings.Contains(s.cookie, Cookie+"=") || strings.Contains(s.cookie, "vom-browser") ||
		!strings.Contains(s.cookie, "vscode-tkn=tkn-geheim") || !strings.Contains(s.cookie, "andere=1") {
		t.Errorf("Cookie beim Upstream = %q", s.cookie)
	}
	if ides.acquired.Load() != 1 || ides.released.Load() != 1 {
		t.Errorf("acquired = %d, released = %d", ides.acquired.Load(), ides.released.Load())
	}
}

func TestWebSocketThroughGateway(t *testing.T) {
	g, auth, ides, _ := setup(t)
	auth.sessions["s"] = "abc"
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "ws-abc.localhost:7000"
		g.ServeHTTP(w, r)
	}))
	defer front.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Cookie", Cookie+"=s")
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("hallo")); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil || string(data) != "hallo" {
		t.Fatalf("Echo = %q, %v", data, err)
	}
	if ides.released.Load() != 0 {
		t.Error("Verbindung freigegeben, obwohl der WebSocket offen ist")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for ides.released.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("Verbindung nach Schließen nicht freigegeben")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAcquireError(t *testing.T) {
	g, auth, ides, _ := setup(t)
	auth.sessions["s"] = "abc"
	ides.err = errors.New("openvscode-server unerwartet beendet")
	w := req(g, "GET", "ws-abc.localhost:7000", "/", &http.Cookie{Name: Cookie, Value: "s"})
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "unerwartet beendet") {
		t.Errorf("Code = %d, Body = %s", w.Code, w.Body.String())
	}
}
