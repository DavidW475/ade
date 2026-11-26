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

// IDEStatus ist die Antwort der IDE-Routen.
type IDEStatus struct {
	ide.Status
	// URL ist die Adresse der IDE (ohne Anmeldung; die liefert POST …/ide/ticket).
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

// IDETicketResponse ist die Antwort von POST /api/workspaces/{id}/ide/ticket.
type IDETicketResponse struct {
	// URL meldet den Browser an der IDE-Subdomain an und leitet zur IDE weiter.
	URL string `json:"url"`
}

func (s *Server) handleIDETicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.opts.Workspaces.Get(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	// Die IDE-Session hängt an der Dashboard-Session: Logout beendet auch sie.
	parent := ""
	if c, err := r.Cookie(SessionCookie); err == nil && s.opts.Auth.ValidSession(c.Value) {
		parent = c.Value
	}
	t := s.opts.Auth.NewIDETicket(id, parent)
	writeJSON(w, http.StatusCreated, IDETicketResponse{
		URL: "http://" + gateway.Host(id, s.opts.Port) + gateway.LoginPath + "?ticket=" + url.QueryEscape(t),
	})
}
