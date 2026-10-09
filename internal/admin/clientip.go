package admin

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// privateNets is what TRUSTED_PROXIES=private stands for: loopback and
// private ranges, where load balancers and reverse proxies usually live.
var privateNets = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}

// ParseTrustedProxies reads a comma-separated list of IPs / CIDRs ("private"
// for all private ranges).
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		switch {
		case f == "":
			continue
		case f == "private":
			for _, n := range privateNets {
				out = append(out, netip.MustParsePrefix(n))
			}
		case strings.Contains(f, "/"):
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
			}
			out = append(out, p.Masked())
		default:
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out, nil
}

// TrustProxies sets the proxies whose X-Forwarded-For is believed.
func (a *Admin) TrustProxies(p []netip.Prefix) { a.proxies = p }

func (a *Admin) trusted(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range a.proxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP is the address the login lockout counts: the TCP peer, or, when
// the peer is a trusted proxy, the nearest untrusted address in
// X-Forwarded-For (addresses further left could be forged by the client).
func (a *Admin) clientIP(r *http.Request) string {
	ip := remoteIP(r)
	if !a.trusted(ip) {
		return ip
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if _, err := netip.ParseAddr(h); err != nil {
			break
		}
		ip = h
		if !a.trusted(h) {
			break
		}
	}
	return ip
}
