package api

import (
	"net/http"
	"net/url"

	"github.com/DavidW475/ade/internal/gateway"
	"github.com/DavidW475/ade/internal/ide"
)

func (s *Server) routesIDE(mux *http.ServeMux) {
	if s.opts.IDE == nil || s.opts.Workspaces == nil {
		return
	}
	mux.HandleFunc("GET /api/workspaces/{id}/ide", s.requireAuth(s.handleIDEStatus))
	mux.HandleFunc("POST /api/workspaces/{id}/ide", s.requireAuth(s.handleIDEStart))
	mux.HandleFunc("DELETE /api/workspaces/{id}/ide", s.requireAuth(s.handleIDEStop))
	mux.HandleFunc("POST /api/workspaces/{id}/ide/ticket", s.requireAuth(s.handleIDETicket))
}

// IDEStatus is the answer of the IDE routes.
type IDEStatus struct {
	ide.Status
	// URL is the address of the IDE (without login; POST …/ide/ticket provides that).
	URL string `json:"url"`
}

func (s *Server) ideStatus(id string) IDEStatus {
	return IDEStatus{Status: s.opts.IDE.Status(id), URL: "http://" + gateway.Host(id, s.opts.Port) + "/"}
}

func (s *Server) handleIDEStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Workspaces.Get(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.ideStatus(id))
}

func (s *Server) handleIDEStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.IDE.Start(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.ideStatus(id))
}

func (s *Server) handleIDEStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Workspaces.Get(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	s.opts.IDE.Stop(id)
	w.WriteHeader(http.StatusNoContent)
}

// IDETicketResponse is the answer of POST /api/workspaces/{id}/ide/ticket.
type IDETicketResponse struct {
	// URL logs the browser in at the IDE subdomain and redirects to the IDE.
	URL string `json:"url"`
}

func (s *Server) handleIDETicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Workspaces.Get(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	// The IDE session depends on the dashboard session: logout ends it as well.
	parent := ""
	if c, err := r.Cookie(SessionCookie); err == nil && s.opts.Auth.ValidSession(c.Value) {
		parent = c.Value
	}
	t := s.opts.Auth.NewIDETicket(id, parent)
	writeJSON(w, http.StatusCreated, IDETicketResponse{
		URL: "http://" + gateway.Host(id, s.opts.Port) + gateway.LoginPath + "?ticket=" + url.QueryEscape(t),
	})
}
