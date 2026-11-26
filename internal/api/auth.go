package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

const (
	// SessionCookie ist der Name des HttpOnly-Cookies für Browser-Sessions.
	SessionCookie = "ade_session"
	// TicketTTL ist die Gültigkeit eines Einmal-Tokens aus `ade start`.
	TicketTTL = 2 * time.Minute
	// SessionTTL ist die Gültigkeit einer Browser-Session.
	SessionTTL = 7 * 24 * time.Hour
)

// Auth verwaltet das Daemon-Secret (für die CLI), Einmal-Tokens (Tickets) und Browser-Sessions.
//
// Ablauf (ADR-008): `ade start` weist sich mit dem Secret aus der Datei daemon.secret aus,
// holt ein Ticket und öffnet /auth/login?ticket=… im Browser. Der Daemon tauscht das Ticket
// gegen ein HttpOnly-Cookie. Sessions liegen nur im Speicher; nach einem Daemon-Neustart
// meldet `ade start` den Browser neu an.
type Auth struct {
	secret string
	now    func() time.Time

	mu       sync.Mutex
	tickets  map[string]time.Time // Ticket → Ablaufzeit
	sessions map[string]time.Time // Session-ID → Ablaufzeit
}

// NewAuth erzeugt eine Auth-Instanz mit dem gegebenen Daemon-Secret.
func NewAuth(secret string) *Auth {
	return &Auth{
		secret:   secret,
		now:      time.Now,
		tickets:  map[string]time.Time{},
		sessions: map[string]time.Time{},
	}
}

// RandomToken erzeugt einen zufälligen, URL-sicheren Token mit 256 Bit.
func RandomToken() string {
	b := make([]byte, 32)
	// crypto/rand.Read schlägt laut Doku nie fehl.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// CheckSecret vergleicht ein vorgelegtes Secret in konstanter Zeit.
func (a *Auth) CheckSecret(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.secret)) == 1
}

// NewTicket erzeugt ein Einmal-Token, das innerhalb von TicketTTL eingelöst werden muss.
func (a *Auth) NewTicket() string {
	t := RandomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked()
	a.tickets[t] = a.now().Add(TicketTTL)
	return t
}

// Redeem löst ein Ticket ein und liefert eine neue Session-ID. Jedes Ticket gilt genau einmal.
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

// ValidSession meldet, ob die Session-ID existiert und nicht abgelaufen ist.
func (a *Auth) ValidSession(id string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
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

// Logout beendet eine Session.
func (a *Auth) Logout(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
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
}
