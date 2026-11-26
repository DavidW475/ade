// Package api stellt die HTTP-API und das eingebettete Dashboard bereit.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Store ist der Teil der Datenbank, den die API in M0 braucht.
type Store interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
}

// Options konfiguriert den API-Server.
type Options struct {
	Version string
	// Port ist der Port, auf dem der Daemon lauscht (für die Origin-Prüfung).
	Port   string
	Store  Store
	Auth   *Auth
	Logger *slog.Logger
}

// Server ist der HTTP-Handler des Daemons.
type Server struct {
	opts    Options
	handler http.Handler
}

// New baut den Handler mit allen Routen und Schutzschichten.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/auth/tickets", s.requireSecret(s.handleNewTicket))
	mux.HandleFunc("GET /api/session", s.requireAuth(s.handleSession))
	mux.HandleFunc("POST /api/auth/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "unbekannter API-Pfad")
	})
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.Handle("/", staticHandler())

	s.handler = securityHeaders(hostGuard(originGuard(opts.Port, mux)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.opts.Logger.Debug("Anfrage", "method", r.Method, "host", r.Host, "path", r.URL.Path, "remote", r.RemoteAddr)
	s.handler.ServeHTTP(w, r)
}

// HealthResponse ist die Antwort von GET /api/health.
type HealthResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Schema  int    `json:"schema"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := HealthResponse{Service: "ade", Status: "ok", Version: s.opts.Version}
	code := http.StatusOK
	if err := s.opts.Store.Ping(ctx); err != nil {
		s.opts.Logger.Error("Health: Datenbank nicht erreichbar", "err", err)
		resp.Status, code = "degraded", http.StatusServiceUnavailable
	} else if v, err := s.opts.Store.SchemaVersion(ctx); err == nil {
		resp.Schema = v
	}
	writeJSON(w, code, resp)
}

// TicketResponse ist die Antwort von POST /api/auth/tickets.
type TicketResponse struct {
	Ticket    string    `json:"ticket"`
	LoginPath string    `json:"login_path"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleNewTicket(w http.ResponseWriter, _ *http.Request) {
	t := s.opts.Auth.NewTicket()
	writeJSON(w, http.StatusCreated, TicketResponse{
		Ticket:    t,
		LoginPath: "/auth/login?ticket=" + t,
		ExpiresAt: time.Now().Add(TicketTTL).UTC(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	host, _ := splitHost(r.Host)
	if !dashboardHost(host) {
		http.Error(w, "Anmeldung nur über das Dashboard", http.StatusForbidden)
		return
	}
	id, ok := s.opts.Auth.Redeem(r.URL.Query().Get("ticket"))
	if !ok {
		s.opts.Logger.Warn("Anmeldung mit ungültigem oder abgelaufenem Ticket", "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Anmeldelink ungültig oder abgelaufen.\nBitte `ade start` erneut ausführen.\n"))
		return
	}
	// Kein Secure-Flag: Das Dashboard läuft über http://localhost.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124, siehe oben
		Name:     SessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(SessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	s.opts.Logger.Info("Browser angemeldet", "remote", r.RemoteAddr)
	// Redirect entfernt das Ticket aus der Adresszeile und dem Verlauf.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		s.opts.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: kein Secure-Flag, http://localhost
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// requireSecret lässt nur Anfragen mit dem Daemon-Secret durch (CLI).
func (s *Server) requireSecret(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Auth.CheckSecret(bearer(r)) {
			writeError(w, http.StatusUnauthorized, "Daemon-Secret fehlt oder ist falsch")
			return
		}
		h(w, r)
	}
}

// requireAuth lässt Anfragen mit gültiger Browser-Session oder Daemon-Secret durch.
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookie); err == nil && s.opts.Auth.ValidSession(c.Value) {
			h(w, r)
			return
		}
		if s.opts.Auth.CheckSecret(bearer(r)) {
			h(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "nicht angemeldet – `ade start` ausführen")
	}
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(v, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
