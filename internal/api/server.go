// Package api provides the HTTP API and the embedded dashboard.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/gateway"
	"github.com/DavidW475/ade/internal/ide"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/trigger"
	"github.com/DavidW475/ade/internal/workspace"
)

// Store is the part of the database the API needed in M0.
type Store interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
}

// Options configures the API server.
type Options struct {
	Version string
	// Port is the port the daemon listens on (for the origin check).
	Port   string
	Store  Store
	Auth   *Auth
	Logger *slog.Logger
	// Projects and Workspaces are optional; without them the corresponding routes are missing.
	Projects   *project.Service
	Workspaces *workspace.Manager
	// Agents is optional; without it the routes for sessions are missing.
	Agents *agent.Manager
	// DefaultAdapter is the global default adapter (for the dashboard).
	DefaultAdapter string
	// IDE is optional; without it the IDE routes and the gateway (ws-<id>.localhost) are missing.
	IDE *ide.Manager
	// Sandbox is optional; without it the sandbox routes are missing.
	Sandbox *sandbox.Manager
	// Egress returns the sandbox's egress events (optional, with sandbox).
	Egress EgressStore
	// Approvals is optional; without it the approval routes are missing.
	Approvals *approval.Service
	// Usage returns costs and tokens (optional; without it GET /api/usage is missing).
	Usage UsageStore
	// Runs is the factory runner (optional; without it /api/factories and /api/runs are missing).
	Runs *factory.Runner
	// Triggers are the factories' triggers (optional).
	Triggers *trigger.Service
}

// UsageStore is the part of the database for costs.
type UsageStore interface {
	UsageTotal(ctx context.Context, f store.UsageFilter) (store.UsageTotal, error)
	UsageByProject(ctx context.Context, since time.Time) (map[string]store.UsageTotal, error)
	UsageByWorkspace(ctx context.Context, projectID string) (map[string]store.UsageTotal, error)
}

// EgressStore is the part of the database for the network view.
type EgressStore interface {
	ListEgress(ctx context.Context, workspaceID string, f store.EgressFilter) ([]store.EgressEvent, error)
	EgressHosts(ctx context.Context, workspaceID string) ([]store.EgressHost, error)
	ListEgressGrants(ctx context.Context, projectID, workspaceID string) ([]store.EgressGrant, error)
}

// Server is the daemon's HTTP handler.
type Server struct {
	opts    Options
	handler http.Handler
}

// New builds the handler with all routes and protection layers.
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
	s.routesProjects(mux)
	s.routesAgents(mux)
	s.routesReview(mux)
	s.routesIDE(mux)
	s.routesSandbox(mux)
	s.routesApprovals(mux)
	s.routesUsage(mux)
	s.routesRuns(mux)
	s.routesTriggers(mux)
	unknown := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "unknown API path")
	}
	mux.HandleFunc("/api/", unknown)
	mux.HandleFunc("/ws/", unknown)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.Handle("/", staticHandler())

	dashboard := securityHeaders(noStoreAPI(mux))
	var ides http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "browser IDE is not enabled", http.StatusNotFound)
	})
	if opts.IDE != nil {
		ides = gateway.New(gateway.Options{
			Auth:         opts.Auth,
			IDEs:         opts.IDE,
			DashboardURL: "http://localhost:" + opts.Port + "/",
			Logger:       opts.Logger,
		})
	}
	// IDE subdomains go to the gateway – without the dashboard CSP, which would break VS Code.
	byHost := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _ := splitHost(r.Host)
		if dashboardHost(host) {
			dashboard.ServeHTTP(w, r)
			return
		}
		ides.ServeHTTP(w, r)
	})
	s.handler = hostGuard(opts.Port, originGuard(opts.Port, byHost))
	return s
}

func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.opts.Logger.Debug("request", "method", r.Method, "host", r.Host, "path", r.URL.Path, "remote", r.RemoteAddr)
	s.handler.ServeHTTP(w, r)
}

// HealthResponse is the answer of GET /api/health.
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
		s.opts.Logger.Error("health: database not reachable", "err", err)
		resp.Status, code = "degraded", http.StatusServiceUnavailable
	} else if v, err := s.opts.Store.SchemaVersion(ctx); err == nil {
		resp.Schema = v
	}
	writeJSON(w, code, resp)
}

// TicketResponse is the answer of POST /api/auth/tickets.
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
		http.Error(w, "login only through the dashboard", http.StatusForbidden)
		return
	}
	id, ok := s.opts.Auth.Redeem(r.URL.Query().Get("ticket"))
	if !ok {
		s.opts.Logger.Warn("login with an invalid or expired ticket", "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Login link invalid or expired.\nPlease run `ade start` again.\n"))
		return
	}
	// No Secure flag: the dashboard runs on http://localhost.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124, siehe oben
		Name:     SessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(SessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	s.opts.Logger.Info("browser logged in", "remote", r.RemoteAddr)
	// The redirect removes the ticket from the address bar and the history.
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther) //nolint:gosec // G710: safeNext only allows local paths
}

// safeNext only allows paths on the dashboard as the target after login (no open redirect
// to foreign sites).
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n") || strings.HasPrefix(next, "/auth/") {
		return "/"
	}
	return next
}

func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		s.opts.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: no Secure flag, http://localhost
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// requireSecret only lets requests with the daemon secret through (CLI).
func (s *Server) requireSecret(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Auth.CheckSecret(bearer(r)) {
			writeError(w, http.StatusUnauthorized, "daemon secret missing or wrong")
			return
		}
		h(w, r)
	}
}

// requireAuth lets requests with a valid browser session or the daemon secret through.
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
		writeError(w, http.StatusUnauthorized, "not logged in – run `ade start`")
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
