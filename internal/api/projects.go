package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/workspace"
)

func (s *Server) routesProjects(mux *http.ServeMux) {
	if s.opts.Projects == nil || s.opts.Workspaces == nil {
		return
	}
	mux.HandleFunc("GET /api/projects", s.requireAuth(s.handleListProjects))
	mux.HandleFunc("POST /api/projects", s.requireAuth(s.handleAddProject))
	mux.HandleFunc("GET /api/projects/{id}", s.requireAuth(s.handleGetProject))
	mux.HandleFunc("DELETE /api/projects/{id}", s.requireAuth(s.handleRemoveProject))
	mux.HandleFunc("GET /api/projects/{id}/workspaces", s.requireAuth(s.handleListWorkspaces))
	mux.HandleFunc("POST /api/projects/{id}/workspaces", s.requireAuth(s.handleCreateWorkspace))
	mux.HandleFunc("GET /api/workspaces/{id}", s.requireAuth(s.handleGetWorkspace))
	mux.HandleFunc("DELETE /api/workspaces/{id}", s.requireAuth(s.handleDeleteWorkspace))
	mux.HandleFunc("GET /api/workspaces/{id}/diff", s.requireAuth(s.handleWorkspaceDiff))
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.opts.Projects.List(r.Context())
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// AddProjectRequest is the body of POST /api/projects.
type AddProjectRequest struct {
	Path string `json:"path"`
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	var req AddProjectRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	p, err := s.opts.Projects.Add(r.Context(), req.Path)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	s.opts.Logger.Info("project registered", "id", p.ID, "name", p.Name, "path", p.RepoPath)
	writeJSON(w, http.StatusCreated, p)
}

// handleGetProject accepts ID, name or repository path (for the CLI).
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.opts.Projects.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.opts.Projects.Find(ctx, r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		if err := s.opts.Workspaces.DeleteAll(ctx, p.ID); err != nil {
			s.writeAppError(w, err)
			return
		}
	}
	if err := s.opts.Projects.Remove(ctx, p.ID); err != nil {
		if errors.Is(err, apperr.ErrConflict) {
			err = apperr.Conflict("project %s still has workspaces; delete them first or force the removal (ade project remove --force)", p.Name)
		}
		s.writeAppError(w, err)
		return
	}
	s.opts.Logger.Info("project removed", "id", p.ID, "name", p.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.List(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var opts workspace.CreateOptions
	if err := decodeBody(r, &opts); err != nil {
		s.writeAppError(w, err)
		return
	}
	ws, err := s.opts.Workspaces.Create(r.Context(), r.PathValue("id"), opts)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.GetView(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.opts.Workspaces.Delete(r.Context(), r.PathValue("id")); err != nil {
		s.writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkspaceDiff(w http.ResponseWriter, r *http.Request) {
	withPatch := r.URL.Query().Get("patch") != "false"
	d, err := s.opts.Workspaces.Diff(r.Context(), r.PathValue("id"), withPatch)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// decodeBody reads a JSON body (at most 1 MiB, no unknown fields).
// An empty body results in the zero value.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return apperr.Invalid("invalid JSON body: %v", err)
	}
	return nil
}

func (s *Server) writeAppError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, apperr.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, apperr.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, apperr.ErrConflict):
		code = http.StatusConflict
	default:
		s.opts.Logger.Error("API error", "err", err)
	}
	writeError(w, code, fmt.Sprint(err))
}
