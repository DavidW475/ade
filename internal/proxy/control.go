package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Health is the answer of GET /healthz of the control API.
type Health struct {
	// Boot changes with every start of the proxy (event counters start anew).
	Boot   string `json:"boot"`
	Policy bool   `json:"policy"`
}

// EventsResponse is the answer of GET /events.
type EventsResponse struct {
	Boot   string  `json:"boot"`
	Events []Event `json:"events"`
	// Next is the value for after on the next call.
	Next int64 `json:"next"`
}

// MaxEventWait limits the long polling of GET /events.
const MaxEventWait = 30 * time.Second

// ControlHandler is the control API for the daemon. Without a token it is locked.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Health{Boot: s.boot, Policy: s.policy.Load() != nil})
	})
	mux.HandleFunc("PUT /policy", func(w http.ResponseWriter, r *http.Request) {
		var p Policy
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			http.Error(w, "read policy: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.SetPolicy(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		if q.Get("boot") != "" && q.Get("boot") != s.boot {
			after = 0 // new proxy run: from the start
		}
		wait, _ := time.ParseDuration(q.Get("wait"))
		wait = min(max(wait, 0), MaxEventWait)
		limit := 500
		evs, next, ch := s.events.since(after, limit)
		if len(evs) == 0 && wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ch:
			case <-t.C:
			case <-r.Context().Done():
			}
			t.Stop()
			evs, next, _ = s.events.since(after, limit)
		}
		if evs == nil {
			evs = []Event{}
		}
		writeJSON(w, http.StatusOK, EventsResponse{Boot: s.boot, Events: evs, Next: next})
	})
	mux.HandleFunc("GET /questions", s.handleQuestions)
	mux.HandleFunc("POST /questions/{id}", s.handleAnswer)
	return s.requireToken(mux)
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.opts.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) != 1 {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Client talks to a proxy's control API.
type Client struct {
	// Base is e.g. http://127.0.0.1:49153.
	Base  string
	Token string
	HTTP  *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &StatusError{Code: resp.StatusCode, Msg: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// StatusError is an error response of the control API.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("proxy: %d %s", e.Code, e.Msg) }

// Health queries the state.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

// SetPolicy sets policy and secrets.
func (c *Client) SetPolicy(ctx context.Context, p Policy) error {
	return c.do(ctx, http.MethodPut, "/policy", p, nil)
}

// Events fetches events after after; wait > 0 waits that long for new ones.
func (c *Client) Events(ctx context.Context, boot string, after int64, wait time.Duration) (EventsResponse, error) {
	var r EventsResponse
	path := "/events?after=" + strconv.FormatInt(after, 10) + "&wait=" + wait.String()
	if boot != "" {
		path += "&boot=" + boot
	}
	err := c.do(ctx, http.MethodGet, path, nil, &r)
	return r, err
}

// IsUnauthorized reports a rejected token.
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusUnauthorized
}
