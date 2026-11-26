package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/sandbox"
)

func (s *Server) routesSandbox(mux *http.ServeMux) {
	if s.opts.Sandbox == nil || s.opts.Workspaces == nil {
		return
	}
	mux.HandleFunc("GET /api/sandbox", s.requireAuth(s.handleSandboxInfo))
	mux.HandleFunc("POST /api/sandbox/gc", s.requireAuth(s.handleSandboxGC))
	mux.HandleFunc("GET /api/workspaces/{id}/sandbox", s.requireAuth(s.handleWorkspaceSandbox))
}

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
	// Network beschreibt die Netzwerk-Isolation (M5: "open" – normales Netz).
	Network string `json:"network"`
}

func (s *Server) handleSandboxInfo(w http.ResponseWriter, r *http.Request) {
	info := SandboxInfo{Image: s.opts.Sandbox.Image(), Default: s.opts.Workspaces.SandboxDefault(), Network: "open"}
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
		out.Network = "open"
		c, err := s.opts.Sandbox.Get(r.Context(), ws.ID)
		if err != nil {
			out.Error = err.Error()
		}
		if c != nil {
			c.Mounts = nil
			out.Container = c
		}
	}
	writeJSON(w, http.StatusOK, out)
}
