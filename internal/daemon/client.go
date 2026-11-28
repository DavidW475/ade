package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/config"
)

// ErrNotRunning means that no daemon answers at the configured address.
var ErrNotRunning = errors.New("ADE daemon is not running")

// Client talks to a running daemon.
type Client struct {
	cfg  *config.Config
	http *http.Client
}

// NewClient creates a client for the address from cfg.
func NewClient(cfg *config.Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) url(p string) string { return "http://" + c.cfg.Listen + p }

func (c *Client) newRequest(ctx context.Context, method, p string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(p), nil)
	if err != nil {
		return nil, err
	}
	// The daemon only accepts localhost as host (ADR-008).
	req.Host = "localhost:" + c.cfg.Port()
	return req, nil
}

// Health queries GET /api/health. If nobody answers, the error is ErrNotRunning.
func (c *Client) Health(ctx context.Context) (*api.HealthResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/health")
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %w", ErrNotRunning, c.cfg.Listen, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var h api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || h.Service != "ade" {
		return nil, fmt.Errorf("something other than ADE answers on %s (HTTP %d)", c.cfg.Listen, resp.StatusCode)
	}
	return &h, nil
}

// WaitHealthy waits until the daemon answers or ctx expires.
func (c *Client) WaitHealthy(ctx context.Context) (*api.HealthResponse, error) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		h, err := c.Health(ctx)
		if err == nil {
			return h, nil
		}
		if !errors.Is(err, ErrNotRunning) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-t.C:
		}
	}
}

// LoginURL fetches a one-time token and returns the login URL for the browser.
func (c *Client) LoginURL(ctx context.Context) (string, error) {
	data, err := os.ReadFile(c.cfg.SecretPath())
	if err != nil {
		return "", fmt.Errorf("read daemon secret: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/auth/tickets")
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(data)))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("request login ticket: HTTP %d", resp.StatusCode)
	}
	var tr api.TicketResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	return c.cfg.BaseURL() + tr.LoginPath, nil
}

// APIError is an error response of the API.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// Do calls the API with the daemon secret. in is sent as JSON (nil: no body), the
// response is decoded into out (nil: discard).
func (c *Client) Do(ctx context.Context, method, p string, in, out any) error {
	secret, err := os.ReadFile(c.cfg.SecretPath())
	if err != nil {
		return fmt.Errorf("read daemon secret: %w", err)
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(p), body)
	if err != nil {
		return err
	}
	req.Host = "localhost:" + c.cfg.Port()
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Creating workspaces can take a while; the context's timeout applies here.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
