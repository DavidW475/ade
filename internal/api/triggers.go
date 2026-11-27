package api

import (
	"net/http"
)

func (s *Server) routesTriggers(mux *http.ServeMux) {
	if s.opts.Triggers == nil {
		return
	}
	mux.HandleFunc("GET /api/projects/{id}/triggers", s.requireAuth(s.handleListTriggers))
	mux.HandleFunc("POST /api/projects/{id}/triggers/{name}/pause", s.requireAuth(s.handlePauseTrigger(true)))
	mux.HandleFunc("POST /api/projects/{id}/triggers/{name}/resume", s.requireAuth(s.handlePauseTrigger(false)))
}

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request) {
	list, err := s.opts.Triggers.List(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handlePauseTrigger(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.opts.Triggers.SetPaused(r.Context(), r.PathValue("id"), r.PathValue("name"), paused)
		if err != nil {
			s.writeAppError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}
