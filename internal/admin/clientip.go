package admin

import (
	"net/http"
	"net/netip"

	"ai-route/internal/clientip"
)

// ParseTrustedProxies reads a comma-separated list of IPs / CIDRs ("private"
// for all private ranges).
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	t, err := clientip.Parse(s)
	return []netip.Prefix(t), err
}

// TrustProxies sets the proxies whose X-Forwarded-For is believed.
func (a *Admin) TrustProxies(p []netip.Prefix) { a.proxies = clientip.Trusted(p) }

// clientIP is the address the login lockout counts: the TCP peer, or, when
// the peer is a trusted proxy, the nearest untrusted address in
// X-Forwarded-For.
func (a *Admin) clientIP(r *http.Request) string { return a.proxies.Client(r) }
