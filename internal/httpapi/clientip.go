package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// clientResolver finds the address a request came from. X-Forwarded-For is
// believed only when the direct peer is a trusted proxy; otherwise any
// client could claim any address and dodge per-client limits.
type clientResolver struct {
	trusted []netip.Prefix
}

func (c clientResolver) isTrusted(a netip.Addr) bool {
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// addr returns the client's address. Walking X-Forwarded-For from the
// right, it skips trusted proxies and stops at the first other hop: hops to
// the left of that were written by the client and prove nothing.
func (c clientResolver) addr(r *http.Request) netip.Addr {
	peer := remoteAddr(r)
	if !peer.IsValid() || len(c.trusted) == 0 || !c.isTrusted(peer) {
		return peer
	}
	for _, hop := range slices.Backward(r.Header.Values("X-Forwarded-For")) {
		for _, part := range slices.Backward(strings.Split(hop, ",")) {
			a, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return peer // a malformed hop: stop trusting the chain
			}
			a = a.Unmap()
			if !c.isTrusted(a) {
				return a
			}
			peer = a
		}
	}
	return peer
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{} // e.g. a Unix socket peer
	}
	return a.Unmap()
}

// clientKey is the rate-limit identity of an address. IPv6 clients are
// grouped by /64: one subscriber is normally handed a whole /64, so keying
// on full addresses would let them rotate through 2^64 identities.
func clientKey(a netip.Addr) string {
	if !a.IsValid() {
		return "local"
	}
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}
