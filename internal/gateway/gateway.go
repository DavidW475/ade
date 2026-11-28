// Package gateway forwards requests to ws-<id>.localhost:<port> to the workspace's browser IDE
// (ADR-004), including WebSocket upgrades.
//
// Flow: the dashboard fetches a short-lived IDE ticket and opens
// http://ws-<id>.localhost:<port>/_ade/login?ticket=…. The gateway exchanges the ticket for
// an HttpOnly cookie that only applies to this subdomain. Every further request needs this
// cookie. The gateway inserts the IDE's connection token itself; the browser never sees it.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/ide"
)

// Cookie is the name of the session cookie on an IDE subdomain.
const Cookie = "ade_ide"

// LoginPath is the path at which the gateway redeems an IDE ticket.
const LoginPath = "/_ade/login"

// tokenCookie is the cookie through which openvscode-server checks the connection token.
const tokenCookie = "vscode-tkn" //nolint:gosec // G101: cookie name, not a secret

// AcquireTimeout limits waiting for a starting IDE.
const AcquireTimeout = 60 * time.Second

var hostRE = regexp.MustCompile(`^ws-([a-z0-9]{1,32})\.localhost$`)

// WorkspaceFromHost returns the workspace ID from a host name ws-<id>.localhost
// (without port).
func WorkspaceFromHost(host string) (string, bool) {
	m := hostRE.FindStringSubmatch(strings.ToLower(strings.TrimSuffix(host, ".")))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Host returns the host name (with port) of a workspace's IDE.
func Host(workspace, port string) string { return "ws-" + workspace + ".localhost:" + port }

// Auth checks IDE tickets and sessions.
type Auth interface {
	RedeemIDE(ticket, workspace string) (string, bool)
	ValidIDESession(id, workspace string) bool
}

// IDEs returns running IDEs.
type IDEs interface {
	Acquire(ctx context.Context, workspace string) (*ide.Upstream, error)
}

// Options configures the gateway.
type Options struct {
	Auth Auth
	IDEs IDEs
	// DashboardURL is the address of the dashboard (for links on error pages).
	DashboardURL string
	Logger       *slog.Logger
}

// Gateway is the HTTP handler for IDE subdomains. The API server in front of it does the host
// and origin check.
type Gateway struct {
	opts Options
}

// New creates a gateway.
func New(opts Options) *Gateway {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Gateway{opts: opts}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := strings.Cut(r.Host, ":")
	ws, ok := WorkspaceFromHost(host)
	if !ok {
		http.Error(w, "unknown IDE address", http.StatusNotFound)
		return
	}
	if r.URL.Path == LoginPath {
		g.login(w, r, ws)
		return
	}
	c, err := r.Cookie(Cookie)
	if err != nil || !g.opts.Auth.ValidIDESession(c.Value, ws) {
		g.page(w, http.StatusUnauthorized, "Not logged in",
			"The IDE is only reachable through the ADE dashboard. Please choose “Open IDE” there.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), AcquireTimeout)
	up, err := g.opts.IDEs.Acquire(ctx, ws)
	cancel()
	if err != nil {
		code, msg := http.StatusBadGateway, err.Error()
		switch {
		case errors.Is(err, apperr.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, context.DeadlineExceeded):
			code, msg = http.StatusGatewayTimeout, "The IDE is still starting. Please reload the page."
		}
		g.opts.Logger.Warn("IDE not reachable", "workspace", ws, "err", err)
		g.page(w, code, "IDE not available", msg)
		return
	}
	defer up.Release()

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.URL)
			// Host and origin stay those of the subdomain; openvscode-server compares them.
			pr.Out.Host = pr.In.Host
			setCookies(pr.Out, up.Token)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				g.opts.Logger.Warn("gateway: forwarding failed", "workspace", ws, "err", err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
		// Pass responses on right away (e.g. server-sent events, logs).
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// setCookies removes the ADE cookie and the connection token sent by the browser and
// inserts the real token.
func setCookies(r *http.Request, token string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == Cookie || c.Name == tokenCookie {
			continue
		}
		r.AddCookie(c)
	}
	r.AddCookie(&http.Cookie{Name: tokenCookie, Value: token}) //nolint:gosec // G124: request cookie to the upstream, attributes do not apply
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request, ws string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id, ok := g.opts.Auth.RedeemIDE(r.URL.Query().Get("ticket"), ws)
	if !ok {
		g.opts.Logger.Warn("IDE login with an invalid ticket", "workspace", ws, "remote", r.RemoteAddr)
		g.page(w, http.StatusUnauthorized, "Login link invalid",
			"The link has expired or was already used. Please choose “Open IDE” in the dashboard again.")
		return
	}
	// Host-only cookie (without domain): only applies to ws-<id>.localhost. SameSite=Lax instead
	// of Strict, because the way from the dashboard to here is a cross-site navigation
	// (localhost and ws-<id>.localhost are different sites). No Secure flag: http://localhost.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124, see above
		Name:     Cookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (g *Gateway) page(w http.ResponseWriter, code int, title, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(code)
	link := ""
	if g.opts.DashboardURL != "" {
		link = fmt.Sprintf(`<p><a href="%s">To the dashboard</a></p>`, html.EscapeString(g.opts.DashboardURL))
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><title>ADE – %s</title>
<style>body{font:16px system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}</style>
<h1>%s</h1><p>%s</p>%s</html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg), link)
}
