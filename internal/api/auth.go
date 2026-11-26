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

	// IDE-Tickets und -Sessions gelten nur für einen Workspace (ADR-004, ws-<id>.localhost).
	ideTickets  map[string]ideGrant
	ideSessions map[string]ideGrant
}

// ideGrant bindet ein IDE-Ticket bzw. eine IDE-Session an einen Workspace und an die
// Dashboard-Session, aus der sie entstand: Endet diese (Logout), endet auch die IDE-Session.
type ideGrant struct {
	workspace string
	parent    string // Dashboard-Session; leer bei Anfragen mit Daemon-Secret
	expires   time.Time
}

// NewAuth erzeugt eine Auth-Instanz mit dem gegebenen Daemon-Secret.
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

// Logout beendet eine Session.
func (a *Auth) Logout(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

// NewIDETicket erzeugt ein Einmal-Token für die IDE eines Workspaces. parent ist die
// Dashboard-Session des Aufrufers (leer bei Aufrufen mit Daemon-Secret).
func (a *Auth) NewIDETicket(workspace, parent string) string {
	t := RandomToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked()
	a.ideTickets[t] = ideGrant{workspace: workspace, parent: parent, expires: a.now().Add(TicketTTL)}
	return t
}

// RedeemIDE löst ein IDE-Ticket für workspace ein und liefert eine IDE-Session-ID.
// Ein Ticket für einen anderen Workspace wird verworfen.
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

// ValidIDESession meldet, ob id eine gültige IDE-Session für workspace ist.
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
