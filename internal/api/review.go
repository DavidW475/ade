package api

import (
	"errors"
	"net/http"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/workspace"
)

// Review und Übernahme (M4): Stand übernehmen, Datei-Inhalte für den Diff-Editor, Mergen,
// Pull Request und Feedback.
func (s *Server) routesReview(mux *http.ServeMux) {
	if s.opts.Workspaces == nil {
		return
	}
	mux.HandleFunc("GET /api/workspaces/{id}/file", s.requireAuth(s.handleWorkspaceFile))
	mux.HandleFunc("POST /api/workspaces/{id}/collect", s.requireAuth(s.handleCollect))
	mux.HandleFunc("POST /api/workspaces/{id}/merge", s.requireAuth(s.handleMerge))
	mux.HandleFunc("GET /api/workspaces/{id}/pull-request", s.requireAuth(s.handlePRDraft))
	mux.HandleFunc("POST /api/workspaces/{id}/pull-request", s.requireAuth(s.handlePullRequest))
	if s.opts.Agents != nil {
		mux.HandleFunc("POST /api/workspaces/{id}/feedback", s.requireAuth(s.handleFeedback))
	}
}

func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fc, err := s.opts.Workspaces.File(r.Context(), r.PathValue("id"), q.Get("path"), q.Get("old_path"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fc)
}

func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.Collect(r.Context(), r.PathValue("id"), "")
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// MergeConflictResponse ist die Antwort von POST /api/workspaces/{id}/merge bei Konflikten.
type MergeConflictResponse struct {
	Error     string   `json:"error"`
	Conflicts []string `json:"conflicts"`
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	ws, err := s.opts.Workspaces.Merge(r.Context(), r.PathValue("id"))
	var mc *workspace.MergeConflictError
	if errors.As(err, &mc) {
		writeJSON(w, http.StatusConflict, MergeConflictResponse{Error: mc.Error(), Conflicts: mc.Files})
		return
	}
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handlePRDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.opts.Workspaces.PullRequestDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handlePullRequest(w http.ResponseWriter, r *http.Request) {
	var req workspace.PRRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	ws, err := s.opts.Workspaces.PullRequest(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req agent.FeedbackRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	sess, err := s.opts.Agents.Feedback(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	v, err := s.opts.Agents.Get(r.Context(), sess.ID)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}
