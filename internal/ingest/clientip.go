package ingest

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPFilter restricts webhook sources to an allowlist. With no trusted
// proxies, the client is the TCP peer. When the peer is a trusted proxy,
// the client is the rightmost X-Forwarded-For entry that is not itself a
// trusted proxy; entries to its left can be forged by the client, so they
// are never consulted.
type IPFilter struct {
	Allowed        []netip.Prefix // empty: allow all
	TrustedProxies []netip.Prefix
}

func contains(prefixes []netip.Prefix, a netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the request's client address as described on IPFilter.
func (f *IPFilter) ClientIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	peer = peer.Unmap()
	if !contains(f.TrustedProxies, peer) {
		return peer, true
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, false // malformed chain: refuse to guess
		}
		client = a.Unmap()
		if !contains(f.TrustedProxies, client) {
			break
		}
	}
	return client, true
}

// Allows reports whether the request's client may send webhooks.
func (f *IPFilter) Allows(r *http.Request) (netip.Addr, bool) {
	if f == nil || len(f.Allowed) == 0 {
		return netip.Addr{}, true
	}
	ip, ok := f.ClientIP(r)
	return ip, ok && contains(f.Allowed, ip)
}
