package api

import (
	"net/http"
	"strconv"

	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/store"
)

func (s *Server) routesRuns(mux *http.ServeMux) {
	if s.opts.Runs == nil {
		return
	}
	mux.HandleFunc("GET /api/factories", s.requireAuth(s.handleListFactories))
	mux.HandleFunc("GET /api/runs", s.requireAuth(s.handleListRuns))
	mux.HandleFunc("POST /api/runs", s.requireAuth(s.handleStartRun))
	mux.HandleFunc("GET /api/runs/{id}", s.requireAuth(s.handleGetRun))
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.requireAuth(s.handleCancelRun))
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{name}", s.requireAuth(s.handleRunArtifact))
}

func (s *Server) handleListFactories(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("project_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "project_id fehlt")
		return
	}
	list, err := s.opts.Runs.Factories(r.Context(), id)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	runs, err := s.opts.Runs.List(r.Context(), store.RunFilter{
		ProjectID: q.Get("project_id"), WorkspaceID: q.Get("workspace_id"), Active: q.Get("active") == "1", Limit: limit,
	})
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req factory.StartRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	v, err := s.opts.Runs.Start(r.Context(), req)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	v, err := s.opts.Runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	v, err := s.opts.Runs.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleRunArtifact liefert ein Artefakt als Text. Es stammt von einem Agenten: nie als HTML
// ausliefern (nosniff setzt securityHeaders).
func (s *Server) handleRunArtifact(w http.ResponseWriter, r *http.Request) {
	iteration := 0
	if q := r.URL.Query().Get("iteration"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "iteration: ganze Zahl ab 1")
			return
		}
		iteration = n
	}
	data, err := s.opts.Runs.ReadArtifactIteration(r.Context(), r.PathValue("id"), r.PathValue("name"), iteration)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
