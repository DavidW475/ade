package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// allowedHost meldet, ob ein Hostname zu localhost gehört. Subdomains von localhost
// (ws-<id>.localhost, ADR-004) sind erlaubt, alles andere nicht – das verhindert
// DNS-Rebinding, bei dem eine fremde Domain auf 127.0.0.1 zeigt.
func allowedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasSuffix(host, ".localhost")
}

// dashboardHost meldet, ob ein Hostname das Dashboard selbst ist (keine IDE-Subdomain).
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

// hostGuard lehnt Anfragen ab, deren Host-Header nicht localhost mit dem Port des
// Daemons ist.
func hostGuard(port string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p := splitHost(r.Host)
		if !allowedHost(host) || p != port {
			http.Error(w, "unzulässiger Host-Header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGuard schützt zustandsändernde Anfragen und WebSocket-Upgrades vor fremden Webseiten
// (CSRF): Ein mitgeschickter Origin muss das Dashboard auf dem eigenen Port sein.
// Anfragen ohne Origin stammen nicht aus einem Browser-Kontext (z. B. CLI) und brauchen
// weiterhin eine gültige Authentifizierung.
func originGuard(port string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
		if !safe || upgrade {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
				http.Error(w, "Cross-Site-Anfrage abgelehnt", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !sameDashboardOrigin(origin, port) {
				http.Error(w, "unzulässiger Origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameDashboardOrigin(origin, port string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	p := u.Port()
	if p == "" {
		p = "80"
	}
	return dashboardHost(u.Hostname()) && p == port
}

// securityHeaders setzt Header, die für alle Antworten gelten.
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
