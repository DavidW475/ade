// Package proxy is the sandbox's egress proxy (`ade proxy`, doc/sandbox.md#3-egress-proxy-ade-proxy).
//
// It runs in a workspace's proxy container, the only container attached both to the agent's
// internal network and to the egress network. The agent reaches the internet only through it:
//
//   - It checks CONNECT host:port and plain HTTP with an absolute URI against the allowlist.
//   - It forwards /p/<provider>/… to the provider's API and inserts the API key itself; the
//     agent only sees a placeholder (secret injection, section 4).
//   - Internal addresses (loopback, private networks, link-local) are always blocked.
//   - Another port forwards TCP to the IDE in the agent container, because Docker publishes
//     no ports in internal networks.
//   - /ade/hook/pretooluse accepts approval questions of the hook and passes them to the
//     daemon (questions.go).
//
// The daemon controls the proxy through its own control API (token from ADE_PROXY_TOKEN):
// set policy and secrets, fetch egress events and questions, answer questions. Without a
// policy everything is blocked.
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

// Ports of the proxy in the container.
const (
	Port        = 8080
	ControlPort = 8081
	IDEPort     = 3000
)

// EnvToken is the environment variable with the control API's token.
const EnvToken = "ADE_PROXY_TOKEN" //nolint:gosec // G101: name, not a secret

// Policy is a workspace's network policy as the daemon sends it.
type Policy struct {
	// Allow and Deny are entries in the format of ParseRule.
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	// Secrets are API keys per provider (see Providers).
	Secrets map[string]string `json:"secrets,omitempty"`
	// Ask: requests to targets that are not allowed wait for an approval in the dashboard
	// (egress.on_blocked: ask) instead of failing right away.
	Ask bool `json:"ask,omitempty"`
	// AskWaitSeconds limits waiting for the approval (default 120).
	AskWaitSeconds int `json:"ask_wait_seconds,omitempty"`
}

// Provider is a provider API that /p/<name>/… is forwarded to.
type Provider struct {
	// Upstream is the base URL of the API.
	Upstream *url.URL
	// Header takes the API key, Prefix comes before it (e.g. "Bearer ").
	Header string
	Prefix string
	// Dialect is how token usage is counted in the responses (DialectAnthropic, DialectOpenAI).
	Dialect string
}

// Providers are the providers with secret injection.
var Providers = map[string]Provider{
	"anthropic": {Upstream: mustURL("https://api.anthropic.com"), Header: "X-Api-Key", Dialect: DialectAnthropic},
	"openai":    {Upstream: mustURL("https://api.openai.com"), Header: "Authorization", Prefix: "Bearer ", Dialect: DialectOpenAI},
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Kinds of events.
const (
	KindConnect = "connect" // CONNECT tunnel (mostly HTTPS)
	KindHTTP    = "http"    // plain HTTP through the proxy
	KindAPI     = "api"     // provider API via /p/<provider>/ with secret injection
)

// Event describes a request to the proxy.
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
	// BytesOut: sent by the agent, BytesIn: delivered to the agent.
	BytesOut   int64  `json:"bytes_out"`
	BytesIn    int64  `json:"bytes_in"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	// Usage is the token usage from the answer of a provider API (KindAPI only).
	Usage *Usage `json:"usage,omitempty"`
}

// Options configures the proxy.
type Options struct {
	// Listen is the address for the agent (default :8080).
	Listen string
	// Control is the address of the control API (default :8081).
	Control string
	// IDE is the address of the IDE forwarding (empty: none).
	IDE string
	// IDETarget is the target of the IDE forwarding (e.g. agent:3000).
	IDETarget string
	// Token protects the control API; empty locks it completely.
	Token  string
	Logger *slog.Logger
	// Dial connects to targets on the internet (default: public addresses only). For tests.
	Dial dialFunc
	// Providers replaces the provider list (for tests).
	Providers map[string]Provider
	// Self are authorities (host:port) under which the agent addresses the proxy itself
	// (default: proxy:8080). Clients with HTTP_PROXY also send requests to the base URL
	// /p/<provider>/ as proxy requests; they are not subject to the allowlist check.
	Self []string
}

// Server is the egress proxy.
type Server struct {
	opts      Options
	log       *slog.Logger
	dial      dialFunc
	providers map[string]Provider
	transport *http.Transport
	boot      string

	policy    atomic.Pointer[compiled]
	events    eventLog
	questions questions
}

type compiled struct {
	rules   *Rules
	secrets map[string]string
	ask     time.Duration // 0: do not ask
}

// DefaultAskWait is the waiting time for an egress approval without AskWaitSeconds.
const DefaultAskWait = 2 * time.Minute

// New creates a proxy.
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
	s.questions.init()
	return s
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetPolicy sets the policy (checks the entries first).
func (s *Server) SetPolicy(p Policy) error {
	rules, err := CompileRules(p.Allow, p.Deny)
	if err != nil {
		return err
	}
	secrets := map[string]string{}
	for k, v := range p.Secrets {
		if _, ok := s.providers[k]; !ok {
			return fmt.Errorf("secrets: unknown provider %q", k)
		}
		if v != "" {
			secrets[k] = v
		}
	}
	c := &compiled{rules: rules, secrets: secrets}
	if p.Ask {
		c.ask = DefaultAskWait
		if p.AskWaitSeconds > 0 {
			c.ask = min(time.Duration(p.AskWaitSeconds)*time.Second, MaxQuestionWait)
		}
	}
	s.policy.Store(c)
	s.log.Info("policy set", "allow", len(p.Allow), "deny", len(p.Deny), "secrets", len(secrets), "ask", p.Ask)
	return nil
}

// Run starts all listeners and blocks until ctx ends or one of them fails.
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
			return fmt.Errorf("listen on %s: %w", x.addr, err)
		}
		srvs = append(srvs, srv{ln, x.run})
	}
	if s.opts.IDE != "" && s.opts.IDETarget != "" {
		ln, err := listen(s.opts.IDE)
		if err != nil {
			closeAll()
			return fmt.Errorf("listen on %s: %w", s.opts.IDE, err)
		}
		srvs = append(srvs, srv{ln, s.forwardTCP})
	}
	errc := make(chan error, len(srvs))
	for _, x := range srvs {
		go func() { errc <- x.run(x.ln) }()
	}
	s.log.Info("egress proxy running", "listen", s.opts.Listen, "control", s.opts.Control, "ide", s.opts.IDE)
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

// forwardTCP forwards connections to the IDE in the agent container.
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
				s.log.Debug("IDE not reachable", "target", s.opts.IDETarget, "err", err)
				return
			}
			defer func() { _ = up.Close() }()
			pipe(c, up)
		}()
	}
}

// pipe copies in both directions and returns the bytes a→b and b→a.
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

// ServeHTTP serves the agent: CONNECT, plain HTTP and /p/<provider>/….
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		s.connect(w, r)
	case r.URL.Path == HookPath && (!r.URL.IsAbs() || s.isSelf(r.URL.Host)):
		s.hook(w, r)
	case r.URL.IsAbs() && r.URL.Host != "" && s.isSelf(r.URL.Host) && strings.HasPrefix(r.URL.Path, "/p/"):
		s.provider(w, r)
	case r.URL.IsAbs() && r.URL.Host != "":
		s.forward(w, r)
	case strings.HasPrefix(r.URL.Path, "/p/"):
		s.provider(w, r)
	default:
		http.Error(w, "ADE egress proxy: use it as an HTTP(S) proxy (HTTPS_PROXY) or via /p/<provider>/", http.StatusBadRequest)
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

// decide checks host:port against the current policy. With Ask, a request to a target that
// is not allowed waits for the decision in the dashboard (never for the deny list or
// internal addresses). msg is an addition for the error message.
func (s *Server) decide(ctx context.Context, host string, port int) (ok bool, reason, msg string) {
	p := s.policy.Load()
	if p == nil {
		return false, ReasonNoPolicy, ""
	}
	ok, reason = p.rules.Decide(host, port)
	if ok || reason != ReasonNotAllowed || p.ask <= 0 {
		return ok, reason, ""
	}
	if ok, msg = s.askEgress(ctx, normalizeHost(host), port, p.ask); ok {
		return true, ReasonApproved, ""
	}
	return false, ReasonNotAllowed, msg
}

func blockedMessage(host string, port int, reason, extra string) string {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	if extra != "" {
		return "ADE: " + target + " is not allowed (" + extra + ")\n"
	}
	switch reason {
	case ReasonDenied:
		return "ADE: " + target + " is blocked (egress.deny in .ade/sandbox.yaml)\n"
	case ReasonInternal:
		return "ADE: " + target + " is an internal address and not reachable from the sandbox\n"
	case ReasonNoPolicy:
		return "ADE: network policy not loaded yet\n"
	}
	return "ADE: " + target + " is not allowed (egress in .ade/sandbox.yaml)\n"
}

// splitTarget reads host and port from an authority; def is the default port.
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
		return "", 0, fmt.Errorf("invalid target %q", authority)
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
	ok, reason, extra := s.decide(r.Context(), host, port)
	ev.Reason = reason
	if !ok {
		ev.Status = http.StatusForbidden
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, reason, extra), http.StatusForbidden)
		return
	}
	up, err := s.dial(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		ev.Allowed, ev.Reason, ev.Status, ev.Error = false, ReasonUpstream, http.StatusBadGateway, err.Error()
		if errors.Is(err, ErrInternal) {
			ev.Reason, ev.Status = ReasonInternal, http.StatusForbidden
		}
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, ev.Reason, ""), ev.Status)
		return
	}
	defer func() { _ = up.Close() }()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
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
	// Bytes of the client that are already buffered (e.g. an early TLS ClientHello) first.
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
		http.Error(w, "ADE: invalid request", http.StatusBadRequest)
		return
	}
	ev.Host, ev.Port = host, port
	ok, reason, extra := s.decide(r.Context(), host, port)
	ev.Reason = reason
	if !ok {
		ev.Status = http.StatusForbidden
		s.record(ev, start)
		http.Error(w, blockedMessage(host, port, reason, extra), http.StatusForbidden)
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
		http.Error(w, fmt.Sprintf("ADE: unknown provider %q", name), http.StatusNotFound)
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
		ev.Error = "no API key configured"
		s.record(ev, start)
		http.Error(w, fmt.Sprintf("ADE: no API key is configured for %s (providers.%s.api_key)", name, name), http.StatusForbidden)
		return
	}
	ev.Allowed, ev.Reason = true, ReasonProvider
	var scan *usageScanner
	s.reverseWith(w, r, &ev, start, func(resp *http.Response) {
		if resp.StatusCode < 300 {
			scan = newUsageScanner(prov.Dialect, resp.Header.Get("Content-Type"))
		}
	}, func() io.Writer {
		if scan == nil {
			return nil
		}
		return scan
	}, func() {
		if scan != nil {
			ev.Usage = scan.Result()
		}
	}, func(out *httputil.ProxyRequest) {
		u := *prov.Upstream
		u.Path = strings.TrimSuffix(u.Path, "/") + path
		u.RawPath = ""
		u.RawQuery = r.URL.RawQuery
		out.Out.URL = &u
		out.Out.Host = u.Host
		// What the agent sends as credentials is a placeholder – replace it, never pass it on.
		out.Out.Header.Del("Authorization")
		out.Out.Header.Del("X-Api-Key")
		out.Out.Header.Set(prov.Header, prov.Prefix+key)
		out.Out.Header.Del("Proxy-Authorization")
		// Request uncompressed, so the proxy can read the token usage (the transport requests
		// gzip itself and unpacks it).
		out.Out.Header.Del("Accept-Encoding")
	})
}

// reverse forwards a request, counts bytes and reports the event at the end.
func (s *Server) reverse(w http.ResponseWriter, r *http.Request, ev *Event, start time.Time, rewrite func(*httputil.ProxyRequest)) {
	s.reverseWith(w, r, ev, start, nil, nil, nil, rewrite)
}

// reverseWith is reverse with hooks: onResponse sees the target's response, tee returns a
// writer that reads along the response body (nil: none), done runs before reporting the event.
func (s *Server) reverseWith(w http.ResponseWriter, r *http.Request, ev *Event, start time.Time,
	onResponse func(*http.Response), tee func() io.Writer, done func(), rewrite func(*httputil.ProxyRequest)) {
	body := &countingReader{r: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = body
	}
	rec := &recorder{ResponseWriter: w, tee: tee}
	rp := &httputil.ReverseProxy{
		ModifyResponse: func(resp *http.Response) error {
			if onResponse != nil {
				onResponse(resp)
			}
			return nil
		},
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
			http.Error(w, blockedMessage(ev.Host, ev.Port, ev.Reason, ""), status)
		},
	}
	rp.ServeHTTP(rec, r)
	ev.Status = rec.status
	if ev.Status == 0 {
		ev.Status = http.StatusOK
	}
	ev.BytesOut, ev.BytesIn = body.n.Load(), rec.n
	if done != nil {
		done()
	}
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
	tee    func() io.Writer
	w      io.Writer
	teeSet bool
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
	if !r.teeSet {
		r.teeSet = true
		if r.tee != nil {
			r.w = r.tee()
		}
	}
	if r.w != nil && n > 0 {
		_, _ = r.w.Write(b[:n])
	}
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) record(ev Event, start time.Time) {
	ev.Time = start.UTC()
	ev.DurationMS = time.Since(start).Milliseconds()
	ev = s.events.add(ev)
	if !ev.Allowed {
		s.log.Info("request blocked", "host", ev.Host, "port", ev.Port, "reason", ev.Reason)
	} else {
		s.log.Debug("request", "kind", ev.Kind, "host", ev.Host, "port", ev.Port, "status", ev.Status)
	}
}

// eventLog is a ring buffer of the latest events with notification for long polling.
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

// since returns events after after (at most limit), the next position and a channel that
// closes on new events.
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
		next = l.seq // counter of an earlier proxy run
	}
	return out, next, l.notify
}
