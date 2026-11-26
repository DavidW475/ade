package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/claude"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/store"
)

// WebSocket-Close-Codes für Clients: 4000 heißt „mit ?since=<letzte seq> neu verbinden“.
const (
	wsCloseLagging = 4000
	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 30 * time.Second
	replayPage     = 500
)

func (s *Server) routesAgents(mux *http.ServeMux) {
	if s.opts.Agents == nil {
		return
	}
	mux.HandleFunc("GET /api/adapters", s.requireAuth(s.handleListAdapters))
	mux.HandleFunc("POST /api/agents", s.requireAuth(s.handleStartAgent))
	mux.HandleFunc("GET /api/sessions", s.requireAuth(s.handleListSessions))
	mux.HandleFunc("GET /api/sessions/{id}", s.requireAuth(s.handleGetSession))
	mux.HandleFunc("GET /api/sessions/{id}/events", s.requireAuth(s.handleSessionEvents))
	mux.HandleFunc("POST /api/sessions/{id}/input", s.requireAuth(s.handleSessionInput))
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.requireAuth(s.handleSessionStop))
	mux.HandleFunc("GET /ws/sessions/{id}/events", s.requireAuth(s.handleEventsWS))
	mux.HandleFunc("GET /ws/sessions/{id}/pty", s.requireAuth(s.handlePTYWS))
}

// AdaptersResponse ist die Antwort von GET /api/adapters.
type AdaptersResponse struct {
	Adapters []adapter.Info `json:"adapters"`
	// PermissionModes sind die Berechtigungsmodi des claude-Adapters.
	PermissionModes []string `json:"permission_modes"`
	Default         string   `json:"default"`
}

func (s *Server) handleListAdapters(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, AdaptersResponse{
		Adapters:        s.opts.Agents.Adapters().Infos(),
		PermissionModes: claude.PermissionModes,
		Default:         s.opts.DefaultAdapter,
	})
}

func (s *Server) handleStartAgent(w http.ResponseWriter, r *http.Request) {
	var req agent.StartRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	sess, err := s.opts.Agents.Start(r.Context(), req)
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

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SessionFilter{
		ProjectID:   q.Get("project_id"),
		WorkspaceID: q.Get("workspace_id"),
		Active:      q.Get("active") == "true",
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 1000 {
			s.writeAppError(w, apperr.Invalid("limit muss zwischen 1 und 1000 liegen"))
			return
		}
		f.Limit = n
	}
	ss, err := s.opts.Agents.List(r.Context(), f)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ss)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	v, err := s.opts.Agents.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func parseSince(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, apperr.Invalid("since muss eine nicht-negative Zahl sein")
	}
	return n, nil
}

// handleSessionEvents liefert gespeicherte Events als JSON (für CLI und Tests; das Dashboard
// nutzt den WebSocket).
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since, err := parseSince(r)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	limit := 1000
	if l := r.URL.Query().Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > 10000 {
			s.writeAppError(w, apperr.Invalid("limit muss zwischen 1 und 10000 liegen"))
			return
		}
	}
	if _, err := s.opts.Agents.Get(r.Context(), id); err != nil {
		s.writeAppError(w, err)
		return
	}
	evs, err := s.opts.Agents.Events(r.Context(), id, since, 0, limit)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evs)
}

// InputRequest ist der Body von POST /api/sessions/{id}/input.
type InputRequest struct {
	Text string `json:"text"`
}

func (s *Server) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	var req InputRequest
	if err := decodeBody(r, &req); err != nil {
		s.writeAppError(w, err)
		return
	}
	v, err := s.opts.Agents.Input(r.Context(), r.PathValue("id"), req.Text)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	v, err := s.opts.Agents.Stop(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// acceptWS nimmt ein WebSocket-Upgrade an. Host und Origin hat originGuard bereits geprüft;
// coder/websocket prüft zusätzlich, dass der Origin zum Host passt.
func acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
}

// handleEventsWS streamt die Events einer Session: zuerst alle gespeicherten mit
// seq > since, danach live. Jede Nachricht ist ein Event als JSON.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since, err := parseSince(r)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	sub, upto, err := s.opts.Agents.Subscribe(r.Context(), id)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	defer sub.Close()
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := c.CloseRead(r.Context())

	last := since
	for last < upto {
		evs, err := s.opts.Agents.Events(ctx, id, last, upto, replayPage)
		if err != nil {
			_ = c.Close(websocket.StatusInternalError, "Events nicht lesbar")
			return
		}
		if len(evs) == 0 {
			break
		}
		for _, e := range evs {
			if err := writeWSJSON(ctx, c, e); err != nil {
				return
			}
			last = e.Seq
		}
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case e, ok := <-sub.C():
			if !ok {
				if sub.Lagged() {
					_ = c.Close(wsCloseLagging, "zu langsam – mit since neu verbinden")
				} else {
					_ = c.Close(websocket.StatusGoingAway, "Stream beendet")
				}
				return
			}
			if e.Seq <= last {
				continue
			}
			if err := writeWSJSON(ctx, c, e); err != nil {
				return
			}
			last = e.Seq
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func writeWSJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeWS(ctx, c, websocket.MessageText, data)
}

func writeWS(ctx context.Context, c *websocket.Conn, typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return c.Write(ctx, typ, data)
}

// ptyControl ist eine Steuernachricht auf dem Terminal-WebSocket (Textnachricht).
// Binärnachrichten sind Terminal-Daten in beide Richtungen.
type ptyControl struct {
	Type string `json:"type"` // resize | input (Client → Daemon), exit (Daemon → Client)
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Data string `json:"data,omitempty"`
}

// handlePTYWS verbindet den Browser mit dem Terminal einer Session.
func (s *Server) handlePTYWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.opts.Agents.AttachTerminal(r.Context(), id)
	if err != nil {
		s.writeAppError(w, err)
		return
	}
	defer t.Close()
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if len(t.Replay) > 0 {
		if err := writeWS(ctx, c, websocket.MessageBinary, t.Replay); err != nil {
			return
		}
	}

	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if !t.Live {
				continue
			}
			if typ == websocket.MessageBinary {
				if err := t.Write(data); err != nil {
					return
				}
				continue
			}
			var msg ptyControl
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "resize":
				_ = t.Resize(msg.Cols, msg.Rows)
			case "input":
				if err := t.Write([]byte(msg.Data)); err != nil {
					return
				}
			}
		}
	}()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case chunk, ok := <-t.Output:
			if !ok {
				if !t.Ended() {
					_ = c.Close(wsCloseLagging, "zu langsam – neu verbinden")
					return
				}
				_ = writeWSJSON(ctx, c, ptyControl{Type: "exit"})
				_ = c.Close(websocket.StatusNormalClosure, "Prozess beendet")
				return
			}
			if err := writeWS(ctx, c, websocket.MessageBinary, chunk); err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), context.Canceled) {
				_ = c.Close(websocket.StatusNormalClosure, "")
			}
			return
		}
	}
}
