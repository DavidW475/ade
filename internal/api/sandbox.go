package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

func (s *Server) routesSandbox(mux *http.ServeMux) {
	if s.opts.Sandbox == nil || s.opts.Workspaces == nil {
		return
	}
	mux.HandleFunc("GET /api/sandbox", s.requireAuth(s.handleSandboxInfo))
	mux.HandleFunc("POST /api/sandbox/gc", s.requireAuth(s.handleSandboxGC))
	mux.HandleFunc("GET /api/workspaces/{id}/sandbox", s.requireAuth(s.handleWorkspaceSandbox))
	if s.opts.Egress != nil {
		mux.HandleFunc("GET /api/workspaces/{id}/egress", s.requireAuth(s.handleWorkspaceEgress))
	}
}

// Netzwerk-Modi eines Sandbox-Containers (SandboxInfo.Network, WorkspaceSandbox.Network).
const (
	// NetworkProxy: internes Netz, Internet nur über den Egress-Proxy (M6).
	NetworkProxy = "proxy"
	// NetworkOpen: normales Docker-Netz (Container aus M5, bis zum nächsten Start).
	NetworkOpen = "open"
)

// SandboxInfo ist die Antwort von GET /api/sandbox.
type SandboxInfo struct {
	// Available: Die Docker-CLI erreicht einen Docker-Daemon.
	Available     bool   `json:"available"`
	Error         string `json:"error,omitempty"`
	DockerVersion string `json:"docker_version,omitempty"`
	// Image ist das Standard-Image.
	Image string `json:"image"`
	// Default ist die Voreinstellung für neue Workspaces (defaults.sandbox).
	Default bool `json:"default"`
	// Network beschreibt die Netzwerk-Isolation neuer Container ("proxy").
	Network string `json:"network"`
	// Presets sind die verfügbaren egress.presets mit ihren Hosts.
	Presets map[string][]string `json:"presets"`
	// DefaultPresets gelten ohne egress-Abschnitt.
	DefaultPresets []string `json:"default_presets"`
}

func (s *Server) handleSandboxInfo(w http.ResponseWriter, r *http.Request) {
	info := SandboxInfo{
		Image: s.opts.Sandbox.Image(), Default: s.opts.Workspaces.SandboxDefault(), Network: NetworkProxy,
		Presets: sandbox.Presets, DefaultPresets: sandbox.DefaultPresets,
	}
	v, err := s.opts.Sandbox.Available(r.Context())
	info.Available, info.DockerVersion = err == nil, v
	if err != nil {
		info.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, info)
}

// GCResponse ist die Antwort von POST /api/sandbox/gc.
type GCResponse struct {
	DryRun  bool                `json:"dry_run"`
	Removed []sandbox.Container `json:"removed"`
}

func (s *Server) handleSandboxGC(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	known := func(id string) bool {
		_, err := s.opts.Workspaces.Get(context.WithoutCancel(r.Context()), id)
		return !errors.Is(err, apperr.ErrNotFound)
	}
	removed, err := s.opts.Sandbox.GC(r.Context(), known, dry)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	if removed == nil {
		removed = []sandbox.Container{}
	}
	writeJSON(w, http.StatusOK, GCResponse{DryRun: dry, Removed: removed})
}

// WorkspaceSandbox ist die Antwort von GET /api/workspaces/{id}/sandbox.
type WorkspaceSandbox struct {
	Sandbox bool   `json:"sandbox"`
	Image   string `json:"image,omitempty"`
	// Container fehlt, wenn er (noch) nicht existiert.
	Container *sandbox.Container `json:"container,omitempty"`
	Error     string             `json:"error,omitempty"`
	Network   string             `json:"network,omitempty"`
}

func (s *Server) handleWorkspaceSandbox(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	out := WorkspaceSandbox{Sandbox: ws.Sandbox, Image: ws.SandboxImage}
	if ws.Sandbox {
		out.Network = NetworkProxy
		c, err := s.opts.Sandbox.Get(r.Context(), ws.ID)
		if err != nil {
			out.Error = err.Error()
		}
		if c != nil {
			c.Mounts = nil
			out.Container = c
			if c.Net != sandbox.NetVersion {
				out.Network = NetworkOpen // wird beim nächsten Start neu angelegt
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// EgressPolicy ist die wirksame Netzwerk-Policy eines Workspaces.
type EgressPolicy struct {
	*sandbox.Egress
	// EffectiveAllow sind alle erlaubten Einträge (Presets aufgelöst plus allow).
	EffectiveAllow []string `json:"effective_allow"`
	// Error ist gesetzt, wenn .ade/sandbox.yaml ungültig ist (dann gilt die Voreinstellung).
	Error string `json:"error,omitempty"`
	// Secrets nennt die Anbieter mit Secret-Injection (nur Namen, nie Keys).
	Providers []string `json:"providers"`
}

// WorkspaceEgress ist die Antwort von GET /api/workspaces/{id}/egress.
type WorkspaceEgress struct {
	Events []store.EgressEvent `json:"events"`
	Hosts  []store.EgressHost  `json:"hosts"`
	Policy *EgressPolicy       `json:"policy,omitempty"`
	// Grants sind Ausnahmen aus Freigaben (projektweit und für diesen Workspace).
	Grants []store.EgressGrant `json:"grants"`
}

func (s *Server) handleWorkspaceEgress(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := store.EgressFilter{BlockedOnly: q.Get("blocked") == "1" || q.Get("blocked") == "true", Limit: limit}
	out := WorkspaceEgress{}
	if out.Events, err = s.opts.Egress.ListEgress(r.Context(), ws.ID, f); err != nil {
		s.writeAppError(w, err)
		return
	}
	if out.Hosts, err = s.opts.Egress.EgressHosts(r.Context(), ws.ID); err != nil {
		s.writeAppError(w, err)
		return
	}
	if out.Grants, err = s.opts.Egress.ListEgressGrants(r.Context(), ws.ProjectID, ws.ID); err != nil {
		s.writeAppError(w, err)
		return
	}
	if ws.Sandbox && s.opts.Projects != nil {
		if p, err := s.opts.Projects.Get(r.Context(), ws.ProjectID); err == nil && p.Sandbox != nil {
			pol := &EgressPolicy{Egress: p.Sandbox.Egress, Error: p.SandboxError, Providers: s.opts.Sandbox.SecretProviders()}
			pol.EffectiveAllow, _ = p.Sandbox.Egress.Rules()
			out.Policy = pol
		}
	}
	writeJSON(w, http.StatusOK, out)
}
