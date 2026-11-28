package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// blockedPrefixes are networks the proxy never connects to – not even if a name on the
// allowlist resolves there: the proxy container itself (loopback, control API), the host,
// other containers and local networks.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",          // "this" network
		"10.0.0.0/8",         // private
		"100.64.0.0/10",      // carrier-grade NAT
		"127.0.0.0/8",        // loopback
		"169.254.0.0/16",     // link-local (among others cloud metadata)
		"172.16.0.0/12",      // private (Docker networks)
		"192.0.0.0/24",       // IETF protocol assignments
		"192.168.0.0/16",     // private
		"198.18.0.0/15",      // benchmark networks
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved, broadcast
		"::/96",              // unspecified, loopback, IPv4-compatible (deprecated, e.g. ::127.0.0.1)
		"fec0::/10",          // site-local (deprecated)
		"64:ff9b:1::/48",     // local NAT64
		"fc00::/7",           // unique local
		"fe80::/10",          // link-local
		"ff00::/8",           // multicast
		"2001:db8::/32",      // documentation
		"100::/64",           // discard
		"2002::/16",          // 6to4 (can tunnel private IPv4)
		"2001::/32",          // Teredo (ditto)
		"::ffff:0:0/96",      // IPv4-mapped – unpacked before, here for safety
		"64:ff9b::/96",       // NAT64 (can map private IPv4)
		"192.88.99.0/24",     // 6to4 relay
		"203.0.113.0/24",     // documentation
		"198.51.100.0/24",    // documentation
		"192.0.2.0/24",       // documentation
		"255.255.255.255/32", // broadcast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ErrInternal reports a blocked internal target.
var ErrInternal = errors.New("target is an internal address")

// Internal reports whether ip is in a blocked network.
func Internal(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// guardedDialer only connects to public addresses. The IP actually used is checked right
// before connect, so a name cannot be redirected between the check and the connection
// (DNS rebinding).
func guardedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrInternal, address)
			}
			if Internal(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrInternal, ap.Addr())
			}
			return nil
		},
	}
}

// dialFunc connects to a target (replaceable in tests).
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
