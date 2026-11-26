package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type fakeStore struct{ err error }

func (f fakeStore) Ping(context.Context) error                 { return f.err }
func (f fakeStore) SchemaVersion(context.Context) (int, error) { return 1, nil }

const testSecret = "geheim"

func newTestServer(t *testing.T, st Store) (*Server, *Auth) {
	t.Helper()
	auth := NewAuth(testSecret)
	return New(Options{Version: "test", Port: "7000", Store: st, Auth: auth}), auth
}

func do(s http.Handler, method, target string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "localhost:7000"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestHealth(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	w := do(s, "GET", "/api/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Code = %d", w.Code)
	}
	var h HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Service != "ade" || h.Status != "ok" || h.Version != "test" || h.Schema != 1 {
		t.Fatalf("Health = %#v", h)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control fehlt")
	}
}

func TestHealthDegraded(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{err: errors.New("kaputt")})
	if w := do(s, "GET", "/api/health", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Code = %d", w.Code)
	}
}

func TestHostGuard(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	cases := map[string]int{
		"localhost:7000":         http.StatusOK,
		"LOCALHOST:7000":         http.StatusOK,
		"127.0.0.1:7000":         http.StatusOK,
		"[::1]:7000":             http.StatusOK,
		"ws-42.localhost:7000":   http.StatusOK,
		"localhost":              http.StatusForbidden, // Port fehlt
		"localhost:8080":         http.StatusForbidden,
		"evil.example:7000":      http.StatusForbidden,
		"localhost.evil.example": http.StatusForbidden,
		"evillocalhost:7000":     http.StatusForbidden,
		"192.168.1.10:7000":      http.StatusForbidden,
		"":                       http.StatusForbidden,
	}
	for host, want := range cases {
		w := do(s, "GET", "/api/health", func(r *http.Request) { r.Host = host })
		if w.Code != want {
			t.Errorf("Host %q: Code = %d, want %d", host, w.Code, want)
		}
	}
}

func TestOriginGuard(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	cases := map[string]int{
		"":                            http.StatusCreated, // CLI ohne Origin
		"http://localhost:7000":       http.StatusCreated,
		"http://127.0.0.1:7000":       http.StatusCreated,
		"http://localhost:8080":       http.StatusForbidden,
		"https://localhost:7000":      http.StatusForbidden,
		"http://evil.example":         http.StatusForbidden,
		"http://ws-42.localhost:7000": http.StatusForbidden, // IDE-Origin darf die API nicht ändern
		"null":                        http.StatusForbidden,
	}
	for origin, want := range cases {
		w := do(s, "POST", "/api/auth/tickets", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testSecret)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		})
		if w.Code != want {
			t.Errorf("Origin %q: Code = %d, want %d", origin, w.Code, want)
		}
	}

	w := do(s, "POST", "/api/auth/tickets", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testSecret)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site: Code = %d", w.Code)
	}

	w = do(s, "GET", "/api/session", func(r *http.Request) {
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Origin", "http://evil.example")
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("WebSocket mit fremdem Origin: Code = %d", w.Code)
	}
}

func TestTicketRequiresSecret(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	for _, auth := range []string{"", "Bearer falsch", testSecret} {
		w := do(s, "POST", "/api/auth/tickets", func(r *http.Request) {
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
		})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: Code = %d", auth, w.Code)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})

	if w := do(s, "GET", "/api/session", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("ohne Login: Code = %d", w.Code)
	}

	w := do(s, "POST", "/api/auth/tickets", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testSecret)
	})
	var tr TicketResponse
	if err := json.NewDecoder(w.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tr.LoginPath, "/auth/login?ticket=") {
		t.Fatalf("LoginPath = %q", tr.LoginPath)
	}

	// Anmeldung über eine IDE-Subdomain ist nicht erlaubt.
	w = do(s, "GET", tr.LoginPath, func(r *http.Request) { r.Host = "ws-1.localhost:7000" })
	if w.Code != http.StatusForbidden {
		t.Fatalf("Login über Subdomain: Code = %d", w.Code)
	}

	w = do(s, "GET", tr.LoginPath, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("Login: Code = %d, Location = %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookie || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("Cookie = %#v", cookies)
	}
	session := cookies[0]

	if w := do(s, "GET", "/api/session", func(r *http.Request) { r.AddCookie(session) }); w.Code != http.StatusOK {
		t.Fatalf("mit Cookie: Code = %d", w.Code)
	}

	// Tickets gelten nur einmal.
	if w := do(s, "GET", tr.LoginPath, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("zweites Einlösen: Code = %d", w.Code)
	}

	w = do(s, "POST", "/api/auth/logout", func(r *http.Request) {
		r.AddCookie(session)
		r.Header.Set("Origin", "http://localhost:7000")
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("Logout: Code = %d", w.Code)
	}
	if w := do(s, "GET", "/api/session", func(r *http.Request) { r.AddCookie(session) }); w.Code != http.StatusUnauthorized {
		t.Fatalf("nach Logout: Code = %d", w.Code)
	}
}

func TestSecretAuthorizesAPI(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	w := do(s, "GET", "/api/session", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testSecret) })
	if w.Code != http.StatusOK {
		t.Fatalf("Code = %d", w.Code)
	}
}

func TestTicketExpires(t *testing.T) {
	a := NewAuth(testSecret)
	now := time.Now()
	a.now = func() time.Time { return now }
	ticket := a.NewTicket()
	now = now.Add(TicketTTL)
	if _, ok := a.Redeem(ticket); ok {
		t.Fatal("abgelaufenes Ticket wurde akzeptiert")
	}

	ticket = a.NewTicket()
	id, ok := a.Redeem(ticket)
	if !ok {
		t.Fatal("gültiges Ticket abgelehnt")
	}
	now = now.Add(SessionTTL)
	if a.ValidSession(id) {
		t.Fatal("abgelaufene Session akzeptiert")
	}
}

func TestUnknownAPIPath(t *testing.T) {
	s, _ := newTestServer(t, fakeStore{})
	w := do(s, "GET", "/api/gibt-es-nicht", nil)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("Code = %d, Content-Type = %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":      {Data: []byte("<html>index</html>")},
		"assets/app-1.js": {Data: []byte("console.log(1)")},
		"favicon.svg":     {Data: []byte("<svg/>")},
	}
	h := spaHandler(fsys)

	cases := []struct {
		path, body string
		code       int
	}{
		{"/", "index", http.StatusOK},
		{"/projekte/42", "index", http.StatusOK},
		{"/assets/app-1.js", "console", http.StatusOK},
		{"/favicon.svg", "svg", http.StatusOK},
		{"/assets/fehlt.js", "", http.StatusNotFound},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: Code = %d, Body = %q", c.path, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app-1.js", nil))
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Assets ohne Cache-Control immutable")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("index Content-Type = %q", w.Header().Get("Content-Type"))
	}
}

func TestSPAHandlerWithoutFrontend(t *testing.T) {
	w := httptest.NewRecorder()
	spaHandler(fstest.MapFS{}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ADE läuft") {
		t.Fatalf("Code = %d, Body = %q", w.Code, w.Body.String())
	}
}
