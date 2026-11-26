// Package gateway leitet Anfragen an ws-<id>.localhost:<port> an die Browser-IDE des
// Workspaces weiter (ADR-004), einschließlich WebSocket-Upgrades.
//
// Ablauf: Das Dashboard holt ein kurzlebiges IDE-Ticket und öffnet
// http://ws-<id>.localhost:<port>/_ade/login?ticket=…. Das Gateway tauscht das Ticket gegen
// ein HttpOnly-Cookie, das nur für diese Subdomain gilt. Jede weitere Anfrage braucht dieses
// Cookie. Den Connection-Token der IDE setzt das Gateway selbst ein; der Browser sieht ihn nie.
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

// Cookie ist der Name des Session-Cookies auf einer IDE-Subdomain.
const Cookie = "ade_ide"

// LoginPath ist der Pfad, unter dem das Gateway ein IDE-Ticket einlöst.
const LoginPath = "/_ade/login"

// tokenCookie ist das Cookie, über das openvscode-server den Connection-Token prüft.
const tokenCookie = "vscode-tkn" //nolint:gosec // G101: Cookie-Name, kein Secret

// AcquireTimeout begrenzt das Warten auf eine startende IDE.
const AcquireTimeout = 60 * time.Second

var hostRE = regexp.MustCompile(`^ws-([a-z0-9]{1,32})\.localhost$`)

// WorkspaceFromHost liefert die Workspace-ID aus einem Hostnamen ws-<id>.localhost
// (ohne Port).
func WorkspaceFromHost(host string) (string, bool) {
	m := hostRE.FindStringSubmatch(strings.ToLower(strings.TrimSuffix(host, ".")))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Host liefert den Hostnamen (mit Port) der IDE eines Workspaces.
func Host(workspace, port string) string { return "ws-" + workspace + ".localhost:" + port }

// Auth prüft IDE-Tickets und -Sessions.
type Auth interface {
	RedeemIDE(ticket, workspace string) (string, bool)
	ValidIDESession(id, workspace string) bool
}

// IDEs liefert laufende IDEs.
type IDEs interface {
	Acquire(ctx context.Context, workspace string) (*ide.Upstream, error)
}

// Options konfiguriert das Gateway.
type Options struct {
	Auth Auth
	IDEs IDEs
	// DashboardURL ist die Adresse des Dashboards (für Links auf Fehlerseiten).
	DashboardURL string
	Logger       *slog.Logger
}

// Gateway ist der HTTP-Handler für IDE-Subdomains. Host- und Origin-Prüfung erledigt der
// API-Server davor.
type Gateway struct {
	opts Options
}

// New erzeugt ein Gateway.
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
		http.Error(w, "unbekannte IDE-Adresse", http.StatusNotFound)
		return
	}
	if r.URL.Path == LoginPath {
		g.login(w, r, ws)
		return
	}
	c, err := r.Cookie(Cookie)
	if err != nil || !g.opts.Auth.ValidIDESession(c.Value, ws) {
		g.page(w, http.StatusUnauthorized, "Nicht angemeldet",
			"Die IDE ist nur über das ADE-Dashboard erreichbar. Bitte dort „IDE öffnen“ wählen.")
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
			code, msg = http.StatusGatewayTimeout, "Die IDE startet noch. Bitte die Seite neu laden."
		}
		g.opts.Logger.Warn("IDE nicht erreichbar", "workspace", ws, "err", err)
		g.page(w, code, "IDE nicht verfügbar", msg)
		return
	}
	defer up.Release()

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.URL)
			// Host und Origin bleiben die der Subdomain; openvscode-server vergleicht sie.
			pr.Out.Host = pr.In.Host
			setCookies(pr.Out, up.Token)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				g.opts.Logger.Warn("Gateway: Weiterleitung fehlgeschlagen", "workspace", ws, "err", err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
		// Antworten (z. B. Server-Sent Events, Logs) sofort weiterreichen.
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// setCookies entfernt das ADE-Cookie und den vom Browser gesendeten Connection-Token und
// setzt den echten Token ein.
func setCookies(r *http.Request, token string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == Cookie || c.Name == tokenCookie {
			continue
		}
		r.AddCookie(c)
	}
	r.AddCookie(&http.Cookie{Name: tokenCookie, Value: token}) //nolint:gosec // G124: Request-Cookie an den Upstream, Attribute gelten nicht
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request, ws string) {
	if r.Method != http.MethodGet {
		http.Error(w, "nur GET", http.StatusMethodNotAllowed)
		return
	}
	id, ok := g.opts.Auth.RedeemIDE(r.URL.Query().Get("ticket"), ws)
	if !ok {
		g.opts.Logger.Warn("IDE-Anmeldung mit ungültigem Ticket", "workspace", ws, "remote", r.RemoteAddr)
		g.page(w, http.StatusUnauthorized, "Anmeldelink ungültig",
			"Der Link ist abgelaufen oder wurde schon benutzt. Bitte im Dashboard erneut „IDE öffnen“ wählen.")
		return
	}
	// Host-only-Cookie (ohne Domain): gilt nur für ws-<id>.localhost. SameSite=Lax statt
	// Strict, weil der Weg vom Dashboard hierher eine Cross-Site-Navigation ist
	// (localhost und ws-<id>.localhost sind verschiedene Sites). Kein Secure-Flag: http://localhost.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124, siehe oben
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
		link = fmt.Sprintf(`<p><a href="%s">Zum Dashboard</a></p>`, html.EscapeString(g.opts.DashboardURL))
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="de"><meta charset="utf-8"><title>ADE – %s</title>
<style>body{font:16px system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}</style>
<h1>%s</h1><p>%s</p>%s</html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg), link)
}
