package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddr(t *testing.T) {
	t.Parallel()
	proxies := clientResolver{trusted: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/8"),
	}}
	for _, tc := range []struct {
		name     string
		resolver clientResolver
		remote   string
		xff      []string
		want     string
	}{
		{"no proxies trusted: header ignored", clientResolver{}, "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9"},
		{"untrusted peer cannot spoof", proxies, "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted proxy forwards", proxies, "10.0.0.2:1234", []string{"198.51.100.7"}, "198.51.100.7"},
		{"client-written hops are ignored", proxies, "10.0.0.2:1234", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"chain of proxies", proxies, "10.0.0.2:1234", []string{"198.51.100.7, 10.0.0.9"}, "198.51.100.7"},
		{"split headers", proxies, "10.0.0.2:1234", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"garbage stops the walk", proxies, "10.0.0.2:1234", []string{"198.51.100.7, nonsense"}, "10.0.0.2"},
		{"IPv6 proxy and client", proxies, "[fd00::1]:1234", []string{"2001:db8::42"}, "2001:db8::42"},
		{"IPv4-mapped peer", proxies, "[::ffff:10.0.0.2]:1234", []string{"198.51.100.7"}, "198.51.100.7"},
		{"only proxies", proxies, "10.0.0.2:1234", []string{"10.0.0.3"}, "10.0.0.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := tc.resolver.addr(r).String(); got != tc.want {
				t.Errorf("addr = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestClientKeyGroupsIPv6ByPrefix(t *testing.T) {
	t.Parallel()
	a := clientKey(netip.MustParseAddr("2001:db8:1:2::1"))
	b := clientKey(netip.MustParseAddr("2001:db8:1:2:ffff::9"))
	c := clientKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Errorf("keys %s %s %s: one /64 should share a key", a, b, c)
	}
	if clientKey(netip.MustParseAddr("192.0.2.1")) != "192.0.2.1" {
		t.Error("IPv4 keys should be the address")
	}
}
