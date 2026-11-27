package proxy

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Rule ist ein Eintrag einer Allow- oder Deny-Liste:
//
//	example.com        genau dieser Host, Ports 80 und 443
//	*.example.com      alle Subdomains (nicht example.com selbst), Ports 80 und 443
//	example.com:8443   genau dieser Host und Port
//
// In der Deny-Liste gilt ein Eintrag ohne Port für alle Ports.
type Rule struct {
	// Host ohne „*.“-Präfix, klein geschrieben.
	Host string
	// Wildcard: Subdomains von Host.
	Wildcard bool
	// Port 0 heißt 80 und 443.
	Port int
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// ParseRule liest einen Listeneintrag.
func ParseRule(s string) (Rule, error) {
	raw := s
	s = strings.ToLower(strings.TrimSpace(s))
	var r Rule
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Rule{}, fmt.Errorf("%q: ungültiger Port", raw)
		}
		s, r.Port = h, n
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s, r.Wildcard = rest, true
	}
	s = strings.TrimSuffix(s, ".")
	if ip := net.ParseIP(s); ip != nil {
		if r.Wildcard {
			return Rule{}, fmt.Errorf("%q: Wildcard bei einer IP-Adresse", raw)
		}
		r.Host = ip.String()
		return r, nil
	}
	if len(s) > 253 || !hostRE.MatchString(s) {
		return Rule{}, fmt.Errorf("%q ist kein gültiger Hostname (erlaubt: host, *.host, host:port)", raw)
	}
	if r.Wildcard && !strings.Contains(s, ".") {
		return Rule{}, fmt.Errorf("%q: Wildcard nur unterhalb einer Domain (z. B. *.example.com)", raw)
	}
	r.Host = s
	return r, nil
}

// String liefert die Schreibweise des Eintrags.
func (r Rule) String() string {
	h := r.Host
	if r.Wildcard {
		h = "*." + h
	}
	if r.Port != 0 {
		return net.JoinHostPort(h, strconv.Itoa(r.Port))
	}
	return h
}

// Match meldet, ob die Regel auf host:port passt (host normalisiert, siehe normalizeHost).
// Ohne Port passt eine Allow-Regel auf 80 und 443, eine Deny-Regel (anyPort) auf alle Ports.
func (r Rule) Match(host string, port int, anyPort bool) bool {
	switch {
	case r.Port != 0:
		if port != r.Port {
			return false
		}
	case !anyPort && port != 80 && port != 443:
		return false
	}
	if r.Wildcard {
		return strings.HasSuffix(host, "."+r.Host)
	}
	return host == r.Host
}

// normalizeHost bringt einen Host aus einer Anfrage in die Form der Regeln.
func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// Rules ist eine geprüfte Allow-/Deny-Liste.
type Rules struct {
	allow, deny []Rule
}

// Gründe einer Entscheidung (Event.Reason).
const (
	ReasonAllowed    = "allowlist"
	ReasonApproved   = "approval" // im Dashboard freigegeben (egress.on_blocked: ask)
	ReasonNotAllowed = "not_allowed"
	ReasonDenied     = "denylist"
	ReasonInternal   = "internal_address"
	ReasonNoPolicy   = "no_policy"
	ReasonProvider   = "provider"
	ReasonUpstream   = "upstream_error"
	ReasonBadRequest = "bad_request"
)

// CompileRules prüft die Listen einer Policy.
func CompileRules(allow, deny []string) (*Rules, error) {
	rs := &Rules{}
	for _, s := range allow {
		r, err := ParseRule(s)
		if err != nil {
			return nil, fmt.Errorf("allow: %w", err)
		}
		rs.allow = append(rs.allow, r)
	}
	for _, s := range deny {
		r, err := ParseRule(s)
		if err != nil {
			return nil, fmt.Errorf("deny: %w", err)
		}
		rs.deny = append(rs.deny, r)
	}
	return rs, nil
}

// Decide entscheidet über host:port: deny geht vor allow, sonst ist alles gesperrt.
func (rs *Rules) Decide(host string, port int) (bool, string) {
	host = normalizeHost(host)
	for _, r := range rs.deny {
		if r.Match(host, port, true) {
			return false, ReasonDenied
		}
	}
	for _, r := range rs.allow {
		if r.Match(host, port, false) {
			return true, ReasonAllowed
		}
	}
	return false, ReasonNotAllowed
}
