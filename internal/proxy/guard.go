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

// blockedPrefixes sind Netze, in die der Proxy nie verbindet – auch nicht, wenn ein Name
// auf der Allowlist dorthin auflöst: der Proxy-Container selbst (Loopback, Steuer-API),
// der Host, andere Container und lokale Netze.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",          // „dieses“ Netz
		"10.0.0.0/8",         // privat
		"100.64.0.0/10",      // Carrier-Grade NAT
		"127.0.0.0/8",        // Loopback
		"169.254.0.0/16",     // Link-Local (u. a. Cloud-Metadaten)
		"172.16.0.0/12",      // privat (Docker-Netze)
		"192.0.0.0/24",       // IETF-Protokollzuweisungen
		"192.168.0.0/16",     // privat
		"198.18.0.0/15",      // Benchmark-Netze
		"224.0.0.0/4",        // Multicast
		"240.0.0.0/4",        // reserviert, Broadcast
		"::/96",              // unspezifiziert, Loopback, IPv4-kompatibel (veraltet, z. B. ::127.0.0.1)
		"fec0::/10",          // Site-Local (veraltet)
		"64:ff9b:1::/48",     // lokales NAT64
		"fc00::/7",           // Unique Local
		"fe80::/10",          // Link-Local
		"ff00::/8",           // Multicast
		"2001:db8::/32",      // Dokumentation
		"100::/64",           // Discard
		"2002::/16",          // 6to4 (kann private IPv4 tunneln)
		"2001::/32",          // Teredo (dito)
		"::ffff:0:0/96",      // IPv4-mapped – wird vorher entpackt, hier zur Sicherheit
		"64:ff9b::/96",       // NAT64 (kann private IPv4 abbilden)
		"192.88.99.0/24",     // 6to4-Relay
		"203.0.113.0/24",     // Dokumentation
		"198.51.100.0/24",    // Dokumentation
		"192.0.2.0/24",       // Dokumentation
		"255.255.255.255/32", // Broadcast
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ErrInternal meldet ein gesperrtes internes Ziel.
var ErrInternal = errors.New("Ziel ist eine interne Adresse")

// Internal meldet, ob ip in einem gesperrten Netz liegt.
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

// guardedDialer verbindet nur zu öffentlichen Adressen. Geprüft wird die tatsächlich
// verwendete IP direkt vor dem connect, damit ein Name nicht zwischen Prüfung und
// Verbindung umgebogen werden kann (DNS-Rebinding).
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

// dialFunc verbindet zu einem Ziel (in Tests ersetzbar).
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
