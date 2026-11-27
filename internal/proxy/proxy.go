// Package proxy ist der Egress-Proxy der Sandbox (`ade proxy`, doc/sandbox.md#3-egress-proxy-ade-proxy).
//
// Er läuft im Proxy-Container eines Workspaces, der als einziger Container sowohl im internen
// Netz des Agenten als auch im Egress-Netz hängt. Der Agent erreicht das Internet nur über ihn:
//
//   - CONNECT host:port und Klartext-HTTP mit absoluter URI prüft er gegen die Allowlist.
//   - /p/<anbieter>/… leitet er an die API des Anbieters weiter und fügt den API-Key selbst
//     ein; der Agent sieht nur einen Platzhalter (Secret-Injection, Abschnitt 4).
//   - Interne Adressen (Loopback, private Netze, Link-Local) sind immer gesperrt.
//   - Ein weiterer Port leitet TCP an die IDE im Agent-Container weiter, weil Docker in
//     internen Netzen keine Ports veröffentlicht.
//
// Der Daemon steuert den Proxy über eine eigene Steuer-API (Token aus ADE_PROXY_TOKEN):
// Policy und Secrets setzen, Egress-Events abholen. Ohne Policy ist alles gesperrt.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Ports des Proxys im Container.
const (
	Port        = 8080
	ControlPort = 8081
	IDEPort     = 3000
)

// EnvToken ist die Umgebungsvariable mit dem Token der Steuer-API.
const EnvToken = "ADE_PROXY_TOKEN" //nolint:gosec // G101: Name, kein Secret

// Policy ist die Netzwerk-Policy eines Workspaces, wie der Daemon sie schickt.
type Policy struct {
	// Allow und Deny sind Einträge im Format von ParseRule.
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	// Secrets sind API-Keys je Anbieter (siehe Providers).
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Provider ist eine Anbieter-API, an die /p/<name>/… weitergeleitet wird.
type Provider struct {
	// Upstream ist die Basis-URL der API.
	Upstream *url.URL
	// Header nimmt den API-Key auf.
	Header string
}

// Providers sind die Anbieter mit Secret-Injection.
var Providers = map[string]Provider{
	"anthropic": {Upstream: mustURL("https://api.anthropic.com"), Header: "X-Api-Key"},
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Arten von Events.
const (
	KindConnect = "connect" // CONNECT-Tunnel (meist HTTPS)
	KindHTTP    = "http"    // Klartext-HTTP über den Proxy
	KindAPI     = "api"     // Anbieter-API über /p/<anbieter>/ mit Secret-Injection
)

// Event beschreibt eine Anfrage an den Proxy.
type Event struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Method   string    `json:"method,omitempty"`
	Host     string    `json:"host"`
	Port     int       `json:"port"`
	Path     string    `json:"path,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Allowed  bool      `json:"allowed"`
	Reason   string    `json:"reason"`
	Status   int       `json:"status,omitempty"`
	// BytesOut: vom Agenten gesendet, BytesIn: an den Agenten geliefert.
	BytesOut   int64  `json:"bytes_out"`
	BytesIn    int64  `json:"bytes_in"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Options konfiguriert den Proxy.
type Options struct {
	// Listen ist die Adresse für den Agenten (Standard :8080).
	Listen string
	// Control ist die Adresse der Steuer-API (Standard :8081).
	Control string
	// IDE ist die Adresse der IDE-Weiterleitung (leer: keine).
	IDE string
	// IDETarget ist das Ziel der IDE-Weiterleitung (z. B. agent:3000).
	IDETarget string
	// Token schützt die Steuer-API; leer sperrt sie ganz.
	Token  string
	Logger *slog.Logger
	// Dial verbindet zu Zielen im Internet (Standard: nur öffentliche Adressen). Für Tests.
	Dial dialFunc
	// Providers ersetzt die Anbieter-Liste (für Tests).
	Providers map[string]Provider
	// Self sind Authorities (host:port), unter denen der Agent den Proxy selbst anspricht
	// (Standard: proxy:8080). Clients mit HTTP_PROXY schicken auch Anfragen an die Basis-URL
	// /p/<anbieter>/ als Proxy-Anfrage; die gehören nicht zur Allowlist-Prüfung.
	Self []string
}

// Server ist der Egress-Proxy.
type Server struct {
	opts      Options
	log       *slog.Logger
	dial      dialFunc
	providers map[string]Provider
	transport *http.Transport
	boot      string

	policy atomic.Pointer[compiled]
	events eventLog
}

type compiled struct {
	rules   *Rules
	secrets map[string]string
}

// New erzeugt einen Proxy.
func New(opts Options) *Server {
	if opts.Listen == "" {
		opts.Listen = ":" + strconv.Itoa(Port)
	}
	if opts.Control == "" {
		opts.Control = ":" + strconv.Itoa(ControlPort)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts, log: opts.Logger, dial: opts.Dial, providers: opts.Providers, boot: randomID()}
	if s.dial == nil {
		s.dial = guardedDialer().DialContext
	}
	if s.providers == nil {
		s.providers = Providers
	}
	s.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           s.dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s.events.init(5000)
	return s
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetPolicy setzt die Policy (prüft die Einträge vorher).
func (s *Server) SetPolicy(p Policy) error {
	rules, err := CompileRules(p.Allow, p.Deny)
	if err != nil {
		return err
	}
	secrets := map[string]string{}
	for k, v := range p.Secrets {
		if _, ok := s.providers[k]; !ok {
			return fmt.Errorf("secrets: unbekannter Anbieter %q", k)
		}
		if v != "" {
			secrets[k] = v
		}
	}
	s.policy.Store(&compiled{rules: rules, secrets: secrets})
	s.log.Info("Policy gesetzt", "allow", len(p.Allow), "deny", len(p.Deny), "secrets", len(secrets))
	return nil
}

// Run startet alle Listener und blockiert, bis ctx endet oder einer scheitert.
func (s *Server) Run(ctx context.Context) error {
	type srv struct {
		ln  net.Listener
		run func(net.Listener) error
	}
	var srvs []srv
	listen := func(addr string) (net.Listener, error) {
		var lc net.ListenConfig
		return lc.Listen(ctx, "tcp", addr)
	}
	closeAll := func() {
		for _, x := range srvs {
			_ = x.ln.Close()
		}
	}
	main := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second, ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)}
	ctl := &http.Server{Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second}
	for _, x := range []struct {
		addr string
		run  func(net.Listener) error
	}{
		{s.opts.Listen, main.Serve},
		{s.opts.Control, ctl.Serve},
	} {
		ln, err := listen(x.addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("auf %s lauschen: %w", x.addr, err)
		}
		srvs = append(srvs, srv{ln, x.run})
	}
	if s.opts.IDE != "" && s.opts.IDETarget != "" {
		ln, err := listen(s.opts.IDE)
		if err != nil {
			closeAll()
			return fmt.Errorf("auf %s lauschen: %w", s.opts.IDE, err)
		}
		srvs = append(srvs, srv{ln, s.forwardTCP})
	}
	errc := make(chan error, len(srvs))
	for _, x := range srvs {
		go func() { errc <- x.run(x.ln) }()
	}
	s.log.Info("Egress-Proxy läuft", "listen", s.opts.Listen, "control", s.opts.Control, "ide", s.opts.IDE)
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = main.Shutdown(sctx)
	_ = ctl.Shutdown(sctx)
	closeAll()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

// forwardTCP leitet Verbindungen an die IDE im Agent-Container weiter.
func (s *Server) forwardTCP(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = c.Close() }()
			d := net.Dialer{Timeout: 10 * time.Second}
			up, err := d.Dial("tcp", s.opts.IDETarget)
			if err != nil {
				s.log.Debug("IDE nicht erreichbar", "ziel", s.opts.IDETarget, "err", err)
				return
			}
			defer func() { _ = up.Close() }()
			pipe(c, up)
		}()
	}
}

// pipe kopiert in beide Richtungen und liefert die Bytes a→b und b→a.
func pipe(a, b net.Conn) (int64, int64) {
	var ab, ba int64
	var wg sync.WaitGroup
	wg.Go(func() {
		ab, _ = io.Copy(b, a)
		closeWrite(b)
	})
	ba, _ = io.Copy(a, b)
	closeWrite(a)
	wg.Wait()
	return ab, ba
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// ServeHTTP bedient den Agenten: CONNECT, Klartext-HTTP und /p/<anbieter>/….
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		s.connect(w, r)
	case r.URL.IsAbs() && r.URL.Host != "" && s.isSelf(r.URL.Host) && strings.HasPrefix(r.URL.Path, "/p/"):
		s.provider(w, r)
	case r.URL.IsAbs() && r.URL.Host != "":
		s.forward(w, r)
	case strings.HasPrefix(r.URL.Path, "/p/"):
		s.provider(w, r)
	default:
		http.Error(w, "ADE-Egress-Proxy: als HTTP(S)-Proxy verwenden (HTTPS_PROXY) oder über /p/<anbieter>/", http.StatusBadRequest)
	}
}

func (s *Server) isSelf(authority string) bool {
	host, port, err := splitTarget(authority, 80)
	if err != nil {
		return false
	}
	self := s.opts.Self
	if self == nil {
		self = []string{"proxy:" + strconv.Itoa(Port)}
	}
	for _, a := range self {
		if h, p, err := splitTarget(a, 80); err == nil && h == host && p == port {
			return true
		}
	}
	return false
}

// decide prüft host:port gegen die aktuelle Policy.
func (s *Server) decide(host string, port int) (bool, string) {
	p := s.policy.Load()
	if p == nil {
		return false, ReasonNoPolicy
	}
	return p.rules.Decide(host, port)
}

func blockedMessage(host string, port int, reason string) string {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	switch reason {
	case ReasonDenied:
		return "ADE: " + target + " ist gesperrt (egress.deny in .ade/sandbox.yaml)\n"
	case ReasonInternal:
		return "ADE: " + target + " ist eine interne Adresse und aus der Sandbox nicht erreichbar\n"
	case ReasonNoPolicy:
		return "ADE: Netzwerk-Policy noch nicht geladen\n"
	}
	return "ADE: " + target + " ist nicht freigegeben (egress in .ade/sandbox.yaml)\n"
}

// splitTarget liest host und port aus einer Authority; def ist der Standard-Port.
func splitTarget(authority string, def int) (string, int, error) {
	host, ps, err := net.SplitHostPort(authority)
	if err != nil {
		var ae *net.AddrError
		if !errors.As(err, &ae) || !strings.Contains(ae.Err, "missing port") || def == 0 {
			return "", 0, err
		}
		host, ps = strings.Trim(authority, "[]"), strconv.Itoa(def)
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return "", 0, fmt.Errorf("ungültiges Ziel %q", authority)
	}
	return normalizeHost(host), port, nil
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ev := Event{Kind: KindConnect, Method: r.Method}
	host, port, err := splitTarget(r.Host, 0)
	if err != nil {
		ev.Host, ev.Reason, ev.Status, ev.Error = r.Host, ReasonBadRequest, http.StatusBadRequest, err.Error()
		s.record(ev, start)
		http.Error(w, "ADE: "+err.Error(), http.StatusBadRequest)
		return
	}
	ev.Host, ev.Port = host, port
	ok, reason := s.decide(host, port)
	ev.Reason = reason
	if !ok {
		ev.Status = http.StatusForbidden
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, reason), http.StatusForbidden)
		return
	}
	up, err := s.dial(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		ev.Allowed, ev.Reason, ev.Status, ev.Error = false, ReasonUpstream, http.StatusBadGateway, err.Error()
		if errors.Is(err, ErrInternal) {
			ev.Reason, ev.Status = ReasonInternal, http.StatusForbidden
		}
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, ev.Reason), ev.Status)
		return
	}
	defer func() { _ = up.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking nicht unterstützt", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	ev.Allowed, ev.Status = true, http.StatusOK
	// Bereits gepufferte Bytes des Clients (z. B. ein früh gesendetes TLS-ClientHello) zuerst.
	var pre int64
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Peek(n)
		w, err := up.Write(b)
		pre = int64(w)
		if err != nil {
			return
		}
	}
	out, in := pipe(client, up)
	ev.BytesOut, ev.BytesIn = out+pre, in
	s.record(ev, start)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ev := Event{Kind: KindHTTP, Method: r.Method, Path: r.URL.Path}
	def := 80
	if r.URL.Scheme == "https" {
		def = 443
	}
	host, port, err := splitTarget(r.URL.Host, def)
	if err != nil || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		ev.Host, ev.Reason, ev.Status = r.URL.Host, ReasonBadRequest, http.StatusBadRequest
		s.record(ev, start)
		http.Error(w, "ADE: ungültige Anfrage", http.StatusBadRequest)
		return
	}
	ev.Host, ev.Port = host, port
	ok, reason := s.decide(host, port)
	ev.Reason = reason
	if !ok {
		ev.Status = http.StatusForbidden
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, reason), http.StatusForbidden)
		return
	}
	ev.Allowed = true
	target := *r.URL
	s.reverse(w, r, &ev, start, func(out *httputil.ProxyRequest) {
		out.Out.URL = &target
		out.Out.Host = target.Host
		out.Out.Header.Del("Proxy-Authorization")
		out.Out.Header.Del("Proxy-Connection")
	})
}

func (s *Server) provider(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	name, path, _ := strings.Cut(rest, "/")
	path = "/" + path
	ev := Event{Kind: KindAPI, Method: r.Method, Provider: name, Path: path}
	prov, ok := s.providers[name]
	if !ok {
		ev.Reason, ev.Status = ReasonBadRequest, http.StatusNotFound
		s.record(ev, start)
		http.Error(w, fmt.Sprintf("ADE: unbekannter Anbieter %q", name), http.StatusNotFound)
		return
	}
	ev.Host, ev.Port, _ = splitTarget(prov.Upstream.Host, 443)
	p := s.policy.Load()
	key := ""
	if p != nil {
		key = p.secrets[name]
	}
	if key == "" {
		ev.Reason, ev.Status = ReasonProvider, http.StatusForbidden
		ev.Error = "kein API-Key hinterlegt"
		s.record(ev, start)
		http.Error(w, fmt.Sprintf("ADE: für %s ist kein API-Key konfiguriert (providers.%s.api_key)", name, name), http.StatusForbidden)
		return
	}
	ev.Allowed, ev.Reason = true, ReasonProvider
	s.reverse(w, r, &ev, start, func(out *httputil.ProxyRequest) {
		u := *prov.Upstream
		u.Path = strings.TrimSuffix(u.Path, "/") + path
		u.RawPath = ""
		u.RawQuery = r.URL.RawQuery
		out.Out.URL = &u
		out.Out.Host = u.Host
		// Was der Agent als Zugang schickt, ist ein Platzhalter – ersetzen, nie weiterreichen.
		out.Out.Header.Del("Authorization")
		out.Out.Header.Del("X-Api-Key")
		out.Out.Header.Set(prov.Header, key)
		out.Out.Header.Del("Proxy-Authorization")
	})
}

// reverse leitet eine Anfrage weiter, zählt Bytes und meldet das Event am Ende.
func (s *Server) reverse(w http.ResponseWriter, r *http.Request, ev *Event, start time.Time, rewrite func(*httputil.ProxyRequest)) {
	body := &countingReader{r: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = body
	}
	rec := &recorder{ResponseWriter: w}
	rp := &httputil.ReverseProxy{
		Rewrite:       rewrite,
		Transport:     s.transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			ev.Error = err.Error()
			ev.Reason = ReasonUpstream
			status := http.StatusBadGateway
			if errors.Is(err, ErrInternal) {
				ev.Allowed, ev.Reason, status = false, ReasonInternal, http.StatusForbidden
			}
			http.Error(w, blockedMessage(ev.Host, ev.Port, ev.Reason), status)
		},
	}
	rp.ServeHTTP(rec, r)
	ev.Status = rec.status
	if ev.Status == 0 {
		ev.Status = http.StatusOK
	}
	ev.BytesOut, ev.BytesIn = body.n.Load(), rec.n
	s.record(*ev, start)
}

type countingReader struct {
	r io.ReadCloser
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

type recorder struct {
	http.ResponseWriter
	status int
	n      int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.n += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) record(ev Event, start time.Time) {
	ev.Time = start.UTC()
	ev.DurationMS = time.Since(start).Milliseconds()
	ev = s.events.add(ev)
	if !ev.Allowed {
		s.log.Info("Anfrage geblockt", "host", ev.Host, "port", ev.Port, "grund", ev.Reason)
	} else {
		s.log.Debug("Anfrage", "kind", ev.Kind, "host", ev.Host, "port", ev.Port, "status", ev.Status)
	}
}

// eventLog ist ein Ringpuffer der letzten Events mit Benachrichtigung für Long-Polling.
type eventLog struct {
	mu     sync.Mutex
	buf    []Event
	cap    int
	seq    int64
	notify chan struct{}
}

func (l *eventLog) init(capacity int) {
	l.cap = capacity
	l.notify = make(chan struct{})
}

func (l *eventLog) add(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev.Seq = l.seq
	l.buf = append(l.buf, ev)
	if len(l.buf) > l.cap {
		l.buf = append([]Event(nil), l.buf[len(l.buf)-l.cap:]...)
	}
	close(l.notify)
	l.notify = make(chan struct{})
	return ev
}

// since liefert Events nach after (höchstens limit), die nächste Position und einen Kanal,
// der sich bei neuen Events schließt.
func (l *eventLog) since(after int64, limit int) ([]Event, int64, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, ev := range l.buf {
		if ev.Seq > after {
			out = append(out, ev)
			if len(out) == limit {
				break
			}
		}
	}
	next := after
	if len(out) > 0 {
		next = out[len(out)-1].Seq
	} else if after > l.seq {
		next = l.seq // Zähler eines früheren Proxy-Laufs
	}
	return out, next, l.notify
}
