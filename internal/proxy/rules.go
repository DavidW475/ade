package proxy

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Rule is an entry of an allow or deny list:
//
//	example.com        exactly this host, ports 80 and 443
//	*.example.com      all subdomains (not example.com itself), ports 80 and 443
//	example.com:8443   exactly this host and port
//
// In the deny list an entry without a port applies to all ports.
type Rule struct {
	// Host without the “*.” prefix, lowercase.
	Host string
	// Wildcard: subdomains of Host.
	Wildcard bool
	// Port 0 means 80 and 443.
	Port int
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// ParseRule reads a list entry.
func ParseRule(s string) (Rule, error) {
	raw := s
	s = strings.ToLower(strings.TrimSpace(s))
	var r Rule
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Rule{}, fmt.Errorf("%q: invalid port", raw)
		}
		s, r.Port = h, n
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		s, r.Wildcard = rest, true
	}
	s = strings.TrimSuffix(s, ".")
	if ip := net.ParseIP(s); ip != nil {
		if r.Wildcard {
			return Rule{}, fmt.Errorf("%q: wildcard on an IP address", raw)
		}
		r.Host = ip.String()
		return r, nil
	}
	if len(s) > 253 || !hostRE.MatchString(s) {
		return Rule{}, fmt.Errorf("%q is not a valid host name (allowed: host, *.host, host:port)", raw)
	}
	if r.Wildcard && !strings.Contains(s, ".") {
		return Rule{}, fmt.Errorf("%q: wildcard only below a domain (e.g. *.example.com)", raw)
	}
	r.Host = s
	return r, nil
}

// String returns the notation of the entry.
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

// Match reports whether the rule matches host:port (host normalized, see normalizeHost).
// Without a port an allow rule matches 80 and 443, a deny rule (anyPort) all ports.
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

// normalizeHost brings a host from a request into the form of the rules.
func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// Rules is a checked allow/deny list.
type Rules struct {
	allow, deny []Rule
}

// Reasons of a decision (Event.Reason).
const (
	ReasonAllowed    = "allowlist"
	ReasonApproved   = "approval" // approved in the dashboard (egress.on_blocked: ask)
	ReasonNotAllowed = "not_allowed"
	ReasonDenied     = "denylist"
	ReasonInternal   = "internal_address"
	ReasonNoPolicy   = "no_policy"
	ReasonProvider   = "provider"
	ReasonUpstream   = "upstream_error"
	ReasonBadRequest = "bad_request"
)

// CompileRules checks the lists of a policy.
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

// Decide decides about host:port: deny wins over allow, otherwise everything is blocked.
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
