package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/store"
)

func (s *Server) routesApprovals(mux *http.ServeMux) {
	if s.opts.Approvals == nil {
		return
	}
	mux.HandleFunc("GET /api/approvals", s.requireAuth(s.handleListApprovals))
	mux.HandleFunc("GET /api/approvals/{id}", s.requireAuth(s.handleGetApproval))
	mux.HandleFunc("POST /api/approvals/{id}", s.requireAuth(s.handleDecideApproval))
	// The hook of local agents authenticates with the token of its session (not with the
	// browser session or the daemon secret); it only applies to questions of this session.
	mux.HandleFunc("POST /api/hook/pretooluse", s.handleHook)
	if s.opts.Workspaces != nil {
		mux.HandleFunc("POST /api/workspaces/{id}/egress/grants", s.requireAuth(s.handleAddGrant))
		mux.HandleFunc("DELETE /api/egress-grants/{id}", s.requireAuth(s.handleDeleteGrant))
	}
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	as, err := s.opts.Approvals.List(r.Context(), store.ApprovalFilter{
		Status: q.Get("status"), SessionID: q.Get("session_id"), WorkspaceID: q.Get("workspace_id"), Limit: limit,
	})
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, as)
}

func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	a, err := s.opts.Approvals.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// DecideRequest is the body of POST /api/approvals/{id}.
type DecideRequest struct {
	// Decision: allow | deny, for egress also allow_workspace | allow_project.
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	var req DecideRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	a, err := s.opts.Approvals.Decide(r.Context(), r.PathValue("id"), req.Decision, req.Reason)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	session, ok := s.opts.Approvals.TokenSession(bearer(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: hook token invalid"})
		return
	}
	var call proxy.HookCall
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&call); err != nil || len(call.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, proxy.Answer{Decision: proxy.AnswerDeny, Reason: "ADE: invalid hook request"})
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Approvals.PreToolUse(r.Context(), approval.HookRequest{Call: call, TokenSession: session}))
}

// GrantRequest is the body of POST /api/workspaces/{id}/egress/grants.
type GrantRequest struct {
	// Rule is an entry in the allowlist format without wildcards (host or host:port).
	Rule string `json:"rule"`
	// Scope: workspace (only this workspace) or project (all workspaces of the project).
	Scope string `json:"scope"`
}

func (s *Server) handleAddGrant(w http.ResponseWriter, r *http.Request) {
	var req GrantRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	ws, err := s.opts.Workspaces.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	wsID := ws.ID
	switch req.Scope {
	case "project":
		wsID = ""
	case "workspace", "":
	default:
		writeError(w, http.StatusBadRequest, "scope: workspace or project")
		return
	}
	g, err := s.opts.Approvals.AddGrant(r.Context(), ws.ProjectID, wsID, req.Rule, "")
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := s.opts.Approvals.DeleteGrant(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
