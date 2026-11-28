package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// allowedHost reports whether a host name belongs to localhost. Subdomains of localhost
// (ws-<id>.localhost, ADR-004) are allowed, everything else is not – this prevents DNS
// rebinding, where a foreign domain points to 127.0.0.1.
func allowedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasSuffix(host, ".localhost")
}

// dashboardHost reports whether a host name is the dashboard itself (no IDE subdomain).
func dashboardHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func splitHost(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return strings.Trim(hostport, "[]"), ""
}

// hostGuard rejects requests whose Host header is not localhost with the daemon's
// port.
func hostGuard(port string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p := splitHost(r.Host)
		if !allowedHost(host) || p != port {
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGuard protects state-changing requests and WebSocket upgrades against foreign websites
// (CSRF): an Origin that is sent along must be the dashboard on its own port, for an IDE
// subdomain exactly this subdomain (an IDE may neither address the dashboard nor another
// IDE).
// Requests without an Origin do not come from a browser context (e.g. CLI) and still need
// valid authentication.
func originGuard(port string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
		if !safe || upgrade {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !allowedOrigin(origin, r.Host, port) {
				http.Error(w, "invalid Origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(origin, reqHost, port string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	p := u.Port()
	if p == "" {
		p = "80"
	}
	if p != port {
		return false
	}
	host, _ := splitHost(reqHost)
	if dashboardHost(host) {
		return dashboardHost(u.Hostname())
	}
	return strings.EqualFold(u.Hostname(), strings.TrimSuffix(host, "."))
}

// securityHeaders sets headers that apply to all responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
