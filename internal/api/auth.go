package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

const (
	// SessionCookie is the name of the HttpOnly cookie for browser sessions.
	SessionCookie = "ade_session"
	// TicketTTL is the validity of a one-time token from `ade start`.
	TicketTTL = 2 * time.Minute
	// SessionTTL is the validity of a browser session.
	SessionTTL = 7 * 24 * time.Hour
)

// Auth manages the daemon secret (for the CLI), one-time tokens (tickets) and browser sessions.
//
// Flow (ADR-008): `ade start` authenticates with the secret from the file daemon.secret,
// fetches a ticket and opens /auth/login?ticket=… in the browser. The daemon exchanges the
// ticket for an HttpOnly cookie. Sessions only live in memory; after a daemon restart
// `ade start` logs the browser in again.
type Auth struct {
	secret string
	now    func() time.Time

	mu       sync.Mutex
	tickets  map[string]time.Time // Ticket → Ablaufzeit
	sessions map[string]time.Time // Session-ID → Ablaufzeit

	// IDE tickets and sessions only apply to one workspace (ADR-004, ws-<id>.localhost).
	ideTickets  map[string]ideGrant
	ideSessions map[string]ideGrant
}

// ideGrant binds an IDE ticket or IDE session to a workspace and to the dashboard session it
// came from: if that ends (logout), the IDE session ends as well.
type ideGrant struct {
	workspace string
	parent    string // dashboard session; empty for requests with the daemon secret
	expires   time.Time
}

// NewAuth creates an Auth instance with the given daemon secret.
func NewAuth(secret string) *Auth {
	return &Auth{
		secret:      secret,
		now:         time.Now,
		tickets:     map[string]time.Time{},
		sessions:    map[string]time.Time{},
		ideTickets:  map[string]ideGrant{},
		ideSessions: map[string]ideGrant{},
	}
}

// RandomToken creates a random, URL-safe token with 256 bits.
func RandomToken() string {
	b := make([]byte, 32)
	// According to its docs, crypto/rand.Read never fails.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// CheckSecret compares a presented secret in constant time.
func (a *Auth) CheckSecret(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.secret)) == 1
}

// NewTicket creates a one-time token that must be redeemed within TicketTTL.
func (a *Auth) NewTicket() string {
	t := RandomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked()
	a.tickets[t] = a.now().Add(TicketTTL)
	return t
}

// Redeem redeems a ticket and returns a new session ID. Every ticket works exactly once.
func (a *Auth) Redeem(ticket string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tickets[ticket]
	if !ok {
		return "", false
	}
	delete(a.tickets, ticket)
	if !a.now().Before(exp) {
		return "", false
	}
	id := RandomToken()
	a.sessions[id] = a.now().Add(SessionTTL)
	return id, true
}

// ValidSession reports whether the session ID exists and has not expired.
func (a *Auth) ValidSession(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.validSessionLocked(id)
}

func (a *Auth) validSessionLocked(id string) bool {
	if id == "" {
		return false
	}
	exp, ok := a.sessions[id]
	if !ok {
		return false
	}
	if !a.now().Before(exp) {
		delete(a.sessions, id)
		return false
	}
	return true
}

// Logout ends a session.
func (a *Auth) Logout(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

// NewIDETicket creates a one-time token for a workspace's IDE. parent is the caller's
// dashboard session (empty for calls with the daemon secret).
func (a *Auth) NewIDETicket(workspace, parent string) string {
	t := RandomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked()
	a.ideTickets[t] = ideGrant{workspace: workspace, parent: parent, expires: a.now().Add(TicketTTL)}
	return t
}

// RedeemIDE redeems an IDE ticket for workspace and returns an IDE session ID.
// A ticket for another workspace is discarded.
func (a *Auth) RedeemIDE(ticket, workspace string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.ideTickets[ticket]
	if !ok {
		return "", false
	}
	delete(a.ideTickets, ticket)
	if g.workspace != workspace || !a.now().Before(g.expires) {
		return "", false
	}
	if g.parent != "" && !a.validSessionLocked(g.parent) {
		return "", false
	}
	id := RandomToken()
	a.ideSessions[id] = ideGrant{workspace: workspace, parent: g.parent, expires: a.now().Add(SessionTTL)}
	return id, true
}

// ValidIDESession reports whether id is a valid IDE session for workspace.
func (a *Auth) ValidIDESession(id, workspace string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.ideSessions[id]
	if !ok {
		return false
	}
	if !a.now().Before(g.expires) || (g.parent != "" && !a.validSessionLocked(g.parent)) {
		delete(a.ideSessions, id)
		return false
	}
	return g.workspace == workspace
}

func (a *Auth) gcLocked() {
	now := a.now()
	for t, exp := range a.tickets {
		if !now.Before(exp) {
			delete(a.tickets, t)
		}
	}
	for s, exp := range a.sessions {
		if !now.Before(exp) {
			delete(a.sessions, s)
		}
	}
	for t, g := range a.ideTickets {
		if !now.Before(g.expires) {
			delete(a.ideTickets, t)
		}
	}
	for s, g := range a.ideSessions {
		if !now.Before(g.expires) || (g.parent != "" && !a.validSessionLocked(g.parent)) {
			delete(a.ideSessions, s)
		}
	}
}
