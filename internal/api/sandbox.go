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

// Network modes of a sandbox container (SandboxInfo.Network, WorkspaceSandbox.Network).
const (
	// NetworkProxy: internal network, internet only through the egress proxy (M6).
	NetworkProxy = "proxy"
	// NetworkOpen: normal Docker network (container from M5, until the next start).
	NetworkOpen = "open"
)

// SandboxInfo is the answer of GET /api/sandbox.
type SandboxInfo struct {
	// Available: the Docker CLI reaches a Docker daemon.
	Available     bool   `json:"available"`
	Error         string `json:"error,omitempty"`
	DockerVersion string `json:"docker_version,omitempty"`
	// Runtime is the detected container runtime (e.g. “Podman (rootless)”).
	Runtime string `json:"runtime,omitempty"`
	// Image is the default image.
	Image string `json:"image"`
	// Default is the default for new workspaces (defaults.sandbox).
	Default bool `json:"default"`
	// Network describes the network isolation of new containers ("proxy").
	Network string `json:"network"`
	// Presets are the available egress.presets with their hosts.
	Presets map[string][]string `json:"presets"`
	// DefaultPresets apply without an egress section.
	DefaultPresets []string `json:"default_presets"`
}

func (s *Server) handleSandboxInfo(w http.ResponseWriter, r *http.Request) {
	info := SandboxInfo{
		Image: s.opts.Sandbox.Image(), Default: s.opts.Workspaces.SandboxDefault(), Network: NetworkProxy,
		Presets: sandbox.Presets, DefaultPresets: sandbox.DefaultPresets,
	}
	v, err := s.opts.Sandbox.Available(r.Context())
	info.Available, info.DockerVersion = err == nil, v
	if rt, ok := s.opts.Sandbox.Runtime(); ok && err == nil {
		info.Runtime = rt.Name()
	}
	if err != nil {
		info.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, info)
}

// GCResponse is the answer of POST /api/sandbox/gc.
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

// WorkspaceSandbox is the answer of GET /api/workspaces/{id}/sandbox.
type WorkspaceSandbox struct {
	Sandbox bool   `json:"sandbox"`
	Image   string `json:"image,omitempty"`
	// Container is missing if it does not exist (yet).
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
				out.Network = NetworkOpen // recreated on the next start
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// EgressPolicy is the effective network policy of a workspace.
type EgressPolicy struct {
	*sandbox.Egress
	// EffectiveAllow are all allowed entries (presets resolved plus allow).
	EffectiveAllow []string `json:"effective_allow"`
	// Error is set if .ade/sandbox.yaml is invalid (then the defaults apply).
	Error string `json:"error,omitempty"`
	// Secrets names the providers with secret injection (names only, never keys).
	Providers []string `json:"providers"`
}

// WorkspaceEgress is the answer of GET /api/workspaces/{id}/egress.
type WorkspaceEgress struct {
	Events []store.EgressEvent `json:"events"`
	Hosts  []store.EgressHost  `json:"hosts"`
	Policy *EgressPolicy       `json:"policy,omitempty"`
	// Grants are exceptions from approvals (project-wide and for this workspace).
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
