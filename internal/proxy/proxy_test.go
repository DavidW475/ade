package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseRule(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		err  bool
	}{
		{"Example.COM", "example.com", false},
		{"*.githubusercontent.com", "*.githubusercontent.com", false},
		{"api.example.com:8443", "api.example.com:8443", false},
		{"*.example.com:8443", "*.example.com:8443", false},
		{"example.com.", "example.com", false},
		{"1.2.3.4", "1.2.3.4", false},
		{"[2001:db8::1]:443", "[2001:db8::1]:443", false},
		{"*.com", "", true},
		{"*", "", true},
		{"exa mple.com", "", true},
		{"example.com:0", "", true},
		{"example.com:99999", "", true},
		{"-bad.com", "", true},
		{"*.1.2.3.4", "", true},
		{"http://example.com", "", true},
		{"", "", true},
	} {
		r, err := ParseRule(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseRule(%q) = %v, expected an error", tc.in, r)
			}
			continue
		}
		if err != nil || r.String() != tc.want {
			t.Errorf("ParseRule(%q) = %q, %v; want %q", tc.in, r.String(), err, tc.want)
		}
	}
}

func TestDecide(t *testing.T) {
	rs, err := CompileRules(
		[]string{"registry.npmjs.org", "*.githubusercontent.com", "api.example.com:8443", "*.pastebin.com"},
		[]string{"*.pastebin.com", "evil.githubusercontent.com"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host   string
		port   int
		ok     bool
		reason string
	}{
		{"registry.npmjs.org", 443, true, ReasonAllowed},
		{"REGISTRY.npmjs.org.", 80, true, ReasonAllowed},
		{"registry.npmjs.org", 22, false, ReasonNotAllowed},
		{"npmjs.org", 443, false, ReasonNotAllowed},
		{"evilregistry.npmjs.org", 443, false, ReasonNotAllowed},
		{"raw.githubusercontent.com", 443, true, ReasonAllowed},
		{"githubusercontent.com", 443, false, ReasonNotAllowed},
		{"xgithubusercontent.com", 443, false, ReasonNotAllowed},
		{"evil.githubusercontent.com", 443, false, ReasonDenied},
		{"api.example.com", 8443, true, ReasonAllowed},
		{"api.example.com", 443, false, ReasonNotAllowed},
		{"a.pastebin.com", 443, false, ReasonDenied},
		{"a.pastebin.com", 8080, false, ReasonDenied},
		{"example.com", 443, false, ReasonNotAllowed},
	} {
		ok, reason := rs.Decide(tc.host, tc.port)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("Decide(%s:%d) = %v %s; want %v %s", tc.host, tc.port, ok, reason, tc.ok, tc.reason)
		}
	}
}

func TestInternal(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.17.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::", "224.0.0.1", "64:ff9b::a00:1",
		"::7f00:1", "::a00:1", "fec0::1", "2002:7f00:1::1", "2001::1"} {
		if !Internal(netip.MustParseAddr(s)) {
			t.Errorf("%s does not count as internal", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "172.32.0.1", "2606:4700::1111", "::ffff:1.1.1.1"} {
		if Internal(netip.MustParseAddr(s)) {
			t.Errorf("%s counts as internal", s)
		}
	}
}

// testProxy starts a proxy whose connections to all targets end up at backend.
func testProxy(t *testing.T, backend string, opts Options) (*Server, *httptest.Server) {
	t.Helper()
	opts.Logger = slog.New(slog.DiscardHandler)
	if opts.Dial == nil {
		opts.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, backend)
		}
	}
	s := New(opts)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func proxyClient(ts *httptest.Server) *http.Client {
	u, _ := url.Parse(ts.URL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 5 * time.Second}
}

// lastEvent waits briefly for the last event: the proxy records it only after it has copied
// the response; the client may already have read the body by then.
func lastEvent(t *testing.T, s *Server) Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs, _, _ := s.events.since(0, 10000)
		if len(evs) > 0 {
			return evs[len(evs)-1]
		}
		if time.Now().After(deadline) {
			t.Fatal("no event")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoPolicyBlocksEverything(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer backend.Close()
	s, ts := testProxy(t, backend.Listener.Addr().String(), Options{})
	resp, err := proxyClient(ts).Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	if ev := lastEvent(t, s); ev.Allowed || ev.Reason != ReasonNoPolicy {
		t.Fatalf("Event %+v", ev)
	}
}

func TestForwardHTTP(t *testing.T) {
	var gotHost, gotProxyAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotProxyAuth = r.Host, r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "hello")
	}))
	defer backend.Close()
	s, ts := testProxy(t, backend.Listener.Addr().String(), Options{})
	if err := s.SetPolicy(Policy{Allow: []string{"allowed.example"}}); err != nil {
		t.Fatal(err)
	}
	c := proxyClient(ts)

	req, _ := http.NewRequest(http.MethodGet, "http://allowed.example/path", nil)
	req.Header.Set("Proxy-Authorization", "Basic eDp5")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello" || gotHost != "allowed.example" || gotProxyAuth != "" {
		t.Fatalf("Status %d, Body %q, Host %q, Proxy-Authorization %q", resp.StatusCode, body, gotHost, gotProxyAuth)
	}
	ev := lastEvent(t, s)
	if !ev.Allowed || ev.Kind != KindHTTP || ev.Host != "allowed.example" || ev.Port != 80 || ev.BytesIn != 5 || ev.Status != 200 {
		t.Fatalf("Event %+v", ev)
	}

	resp, err = c.Get("http://blocked.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not allowed") {
		t.Fatalf("Status %d, Body %q", resp.StatusCode, body)
	}
	if ev := lastEvent(t, s); ev.Allowed || ev.Reason != ReasonNotAllowed || ev.Host != "blocked.example" {
		t.Fatalf("Event %+v", ev)
	}
}

func TestConnect(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tls-ok") }))
	defer backend.Close()
	s, ts := testProxy(t, backend.Listener.Addr().String(), Options{})
	if err := s.SetPolicy(Policy{Allow: []string{"secure.example"}, Deny: []string{"*.bad.example"}}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(ts.URL)
	tr := backend.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(u)
	tr.TLSClientConfig.ServerName = "example.com" // certificate of httptest
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := c.Get("https://secure.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	tr.CloseIdleConnections()
	if string(body) != "tls-ok" {
		t.Fatalf("Body %q", body)
	}
	// The tunnel's event is created when it closes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ev Event
		if evs, _, _ := s.events.since(0, 100); len(evs) > 0 {
			ev = evs[len(evs)-1]
		}
		if ev.Kind == KindConnect && ev.Allowed && ev.BytesIn > 0 && ev.BytesOut > 0 && ev.Port == 443 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Event %+v", ev)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for host, reason := range map[string]string{"other.example": ReasonNotAllowed, "x.bad.example": ReasonDenied} {
		resp, err := c.Get("https://" + host + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Fatalf("%s: error %v, want Forbidden", host, err)
		}
		if ev := lastEvent(t, s); ev.Allowed || ev.Reason != reason || ev.Host != host {
			t.Fatalf("Event %+v", ev)
		}
	}

	// Port that is not allowed.
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT secure.example:22 HTTP/1.1\r\nHost: secure.example:22\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		_ = r.Body.Close()
	}
	if err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT :22 → %v %v", r, err)
	}
}

func TestInternalTargetsAreBlocked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "intern") }))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	// Real dialer with protection; the allowlist explicitly allows the target.
	s := New(Options{Logger: slog.New(slog.DiscardHandler)})
	ts := httptest.NewServer(s)
	defer ts.Close()
	if err := s.SetPolicy(Policy{Allow: []string{"127.0.0.1:" + port, "localhost:" + port}}); err != nil {
		t.Fatal(err)
	}
	c := proxyClient(ts)
	for _, target := range []string{"http://127.0.0.1:" + port + "/", "http://localhost:" + port + "/"} {
		resp, err := c.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), "internal\n") && !strings.Contains(string(body), "internal address") {
			t.Fatalf("%s: Status %d, Body %q", target, resp.StatusCode, body)
		}
		if ev := lastEvent(t, s); ev.Allowed || ev.Reason != ReasonInternal {
			t.Fatalf("%s: Event %+v", target, ev)
		}
	}
	// CONNECT to the proxy's own control API.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT 127.0.0.1:%s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\n\r\n", port, port)
	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		_ = r.Body.Close()
	}
	if err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT internal → %v %v", r, err)
	}
}

func TestProviderInjectsKey(t *testing.T) {
	type seen struct{ path, query, key, auth, host string }
	got := make(chan seen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.Host}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	s, ts := testProxy(t, up.Host, Options{Providers: map[string]Provider{"anthropic": {Upstream: up, Header: "X-Api-Key"}}})

	// Without a key: blocked.
	if err := s.SetPolicy(Policy{}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/p/anthropic/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without key: status %d", resp.StatusCode)
	}

	if err := s.SetPolicy(Policy{Secrets: map[string]string{"anthropic": "sk-real"}}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/p/anthropic/v1/messages?beta=true", strings.NewReader(`{"x":1}`))
	req.Header.Set("X-Api-Key", "placeholder")
	req.Header.Set("Authorization", "Bearer placeholder")
	start := time.Now()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	line, _ := br.ReadString('\n')
	if line != "data: one\n" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("first line %q after %s – streaming buffered?", line, time.Since(start))
	}
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("rest %q", rest)
	}
	g := <-got
	if g.path != "/v1/messages" || g.query != "beta=true" || g.key != "sk-real" || g.auth != "" || g.host != up.Host {
		t.Fatalf("upstream saw %+v", g)
	}
	ev := lastEvent(t, s)
	if !ev.Allowed || ev.Kind != KindAPI || ev.Provider != "anthropic" || ev.Path != "/v1/messages" || ev.BytesOut != 7 {
		t.Fatalf("Event %+v", ev)
	}

	// Clients with HTTP_PROXY send the request to the base URL as a proxy request.
	viaProxy := proxyClient(ts)
	req, _ = http.NewRequest(http.MethodPost, "http://proxy:8080/p/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", "placeholder")
	resp, err = viaProxy.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if g := <-got; resp.StatusCode != 200 || g.key != "sk-real" || g.path != "/v1/messages" {
		t.Fatalf("via HTTP_PROXY: status %d, upstream %+v", resp.StatusCode, g)
	}
	// Another host with /p/ is a normal proxy request instead (not allowed).
	resp, err = viaProxy.Get("http://evil.example/p/anthropic/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host with /p/: %d", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/p/unknown/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", resp2.StatusCode)
	}
}

func TestSetPolicyRejectsInvalid(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.DiscardHandler)})
	if err := s.SetPolicy(Policy{Allow: []string{"*"}}); err == nil {
		t.Fatal("invalid rule accepted")
	}
	if err := s.SetPolicy(Policy{Secrets: map[string]string{"nope": "x"}}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestControlAPI(t *testing.T) {
	s := New(Options{Token: "geheim", Logger: slog.New(slog.DiscardHandler)})
	ts := httptest.NewServer(s.ControlHandler())
	defer ts.Close()

	for _, tok := range []string{"", "falsch"} {
		c := &Client{Base: ts.URL, Token: tok}
		if _, err := c.Health(context.Background()); !IsUnauthorized(err) {
			t.Fatalf("token %q: %v, want 401", tok, err)
		}
		if err := c.SetPolicy(context.Background(), Policy{Allow: []string{"example.com"}}); !IsUnauthorized(err) {
			t.Fatalf("token %q: %v, want 401", tok, err)
		}
	}
	if s.policy.Load() != nil {
		t.Fatal("policy set without a token")
	}

	c := &Client{Base: ts.URL, Token: "geheim"}
	h, err := c.Health(context.Background())
	if err != nil || h.Policy || h.Boot == "" {
		t.Fatalf("Health %+v %v", h, err)
	}
	if err := c.SetPolicy(context.Background(), Policy{Allow: []string{"*"}}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if err := c.SetPolicy(context.Background(), Policy{Allow: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := s.decide(context.Background(), "example.com", 443); !ok {
		t.Fatal("policy not effective")
	}

	// Long polling: waits for the next event.
	done := make(chan EventsResponse, 1)
	go func() {
		r, err := c.Events(context.Background(), h.Boot, 0, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	time.Sleep(100 * time.Millisecond)
	s.record(Event{Kind: KindConnect, Host: "x.example", Port: 443, Reason: ReasonNotAllowed}, time.Now())
	select {
	case r := <-done:
		if len(r.Events) != 1 || r.Next != 1 || r.Events[0].Host != "x.example" {
			t.Fatalf("Events %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long polling does not return")
	}
	r, err := c.Events(context.Background(), h.Boot, 1, 0)
	if err != nil || len(r.Events) != 0 || r.Next != 1 {
		t.Fatalf("after after=1: %+v %v", r, err)
	}
	// Another boot: from the start.
	r, err = c.Events(context.Background(), "old", 1, 0)
	if err != nil || len(r.Events) != 1 {
		t.Fatalf("other boot: %+v %v", r, err)
	}
}

func TestControlWithoutTokenIsClosed(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.DiscardHandler)})
	ts := httptest.NewServer(s.ControlHandler())
	defer ts.Close()
	if _, err := (&Client{Base: ts.URL}).Health(context.Background()); !IsUnauthorized(err) {
		t.Fatalf("%v, want 401", err)
	}
}

func TestEventLogRing(t *testing.T) {
	var l eventLog
	l.init(3)
	for i := range 5 {
		l.add(Event{Host: strconv.Itoa(i)})
	}
	evs, next, _ := l.since(0, 10)
	if len(evs) != 3 || evs[0].Seq != 3 || next != 5 {
		t.Fatalf("%+v next=%d", evs, next)
	}
	evs, next, _ = l.since(99, 10)
	if len(evs) != 0 || next != 5 {
		t.Fatalf("%+v next=%d", evs, next)
	}
}

func TestRunAndIDEForward(t *testing.T) {
	ide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ide") }))
	defer ide.Close()
	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		return ln.Addr().String()
	}
	listen, control, ideAddr := free(), free(), free()
	s := New(Options{Listen: listen, Control: control, IDE: ideAddr, IDETarget: ide.Listener.Addr().String(), Token: "t", Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	c := &Client{Base: "http://" + control, Token: "t"}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := c.Health(context.Background()); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + ideAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ide" {
		t.Fatalf("IDE forwarding: %q", body)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run does not end")
	}
}
