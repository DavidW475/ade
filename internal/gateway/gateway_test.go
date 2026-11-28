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
	tickets  map[string]string // ticket → workspace
	sessions map[string]string // session → workspace
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
		return nil, errors.New("wrong workspace " + ws)
	}
	f.acquired.Add(1)
	return &ide.Upstream{URL: f.url, Token: "tkn-secret", Release: func() { f.released.Add(1) }}, nil
}

type seen struct {
	mu     sync.Mutex
	host   string
	cookie string
}

// upstream plays openvscode-server: checks the token and offers a WebSocket echo.
func upstream(t *testing.T, s *seen) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.host, s.cookie = r.Host, r.Header.Get("Cookie")
		s.mu.Unlock()
		if c, err := r.Cookie("vscode-tkn"); err != nil || c.Value != "tkn-secret" {
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
	auth := &fakeAuth{tickets: map[string]string{"t1": "abc", "t2": "other"}, sessions: map[string]string{}}
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
	auth.sessions["foreign"] = "other"

	for name, cookies := range map[string][]*http.Cookie{
		"without cookie":  nil,
		"wrong cookie":    {{Name: Cookie, Value: "made-up"}},
		"other workspace": {{Name: Cookie, Value: "foreign"}},
		"IDE token only":  {{Name: "vscode-tkn", Value: "tkn-secret"}},
	} {
		w := req(g, "GET", "ws-abc.localhost:7000", "/", cookies...)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: Code = %d, want 401", name, w.Code)
		}
	}
	if ides.acquired.Load() != 0 {
		t.Error("IDE requested without a session")
	}
	if w := req(g, "GET", "foo.localhost:7000", "/"); w.Code != http.StatusNotFound {
		t.Errorf("foreign subdomain: code = %d", w.Code)
	}
}

func TestLoginAndProxy(t *testing.T) {
	g, _, ides, s := setup(t)

	// A ticket for another workspace does not apply here.
	if w := req(g, "GET", "ws-abc.localhost:7000", LoginPath+"?ticket=t2"); w.Code != http.StatusUnauthorized {
		t.Errorf("foreign ticket: code = %d", w.Code)
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
	// A ticket only works once.
	if w := req(g, "GET", "ws-abc.localhost:7000", LoginPath+"?ticket=t1"); w.Code != http.StatusUnauthorized {
		t.Errorf("second redemption: code = %d", w.Code)
	}

	w = req(g, "GET", "ws-abc.localhost:7000", "/static/x.js", c,
		&http.Cookie{Name: "vscode-tkn", Value: "from-browser"}, &http.Cookie{Name: "other", Value: "1"})
	if w.Code != http.StatusOK || w.Body.String() != "IDE /static/x.js" {
		t.Fatalf("Proxy: Code = %d, Body = %q", w.Code, w.Body.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host != "ws-abc.localhost:7000" {
		t.Errorf("host at the upstream = %q", s.host)
	}
	if strings.Contains(s.cookie, Cookie+"=") || strings.Contains(s.cookie, "from-browser") ||
		!strings.Contains(s.cookie, "vscode-tkn=tkn-secret") || !strings.Contains(s.cookie, "other=1") {
		t.Errorf("cookie at the upstream = %q", s.cookie)
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
	if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil || string(data) != "hello" {
		t.Fatalf("Echo = %q, %v", data, err)
	}
	if ides.released.Load() != 0 {
		t.Error("connection released although the WebSocket is open")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for ides.released.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("connection not released after closing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAcquireError(t *testing.T) {
	g, auth, ides, _ := setup(t)
	auth.sessions["s"] = "abc"
	ides.err = errors.New("openvscode-server exited unexpectedly")
	w := req(g, "GET", "ws-abc.localhost:7000", "/", &http.Cookie{Name: Cookie, Value: "s"})
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "exited unexpectedly") {
		t.Errorf("Code = %d, Body = %s", w.Code, w.Body.String())
	}
}
