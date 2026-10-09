// Package clientip resolves the address a request really came from: the
// TCP peer, or, when the peer is a trusted reverse proxy or load balancer,
// the nearest untrusted address in X-Forwarded-For.
package clientip

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

// Trusted is the set of proxies whose X-Forwarded-For is believed.
type Trusted []netip.Prefix

// Parse reads a comma-separated list of IPs / CIDRs ("private" for all
// private ranges).
func Parse(s string) (Trusted, error) {
	var out Trusted
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

// Contains reports whether ip is a trusted proxy.
func (t Trusted) Contains(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range t {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Remote is the TCP peer's address.
func Remote(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Client is the address a request came from: the TCP peer, or, when the
// peer is trusted, the nearest untrusted address in X-Forwarded-For
// (addresses further left could be forged by the client). X-Real-IP is
// used when a trusted proxy sends it without X-Forwarded-For.
func (t Trusted) Client(r *http.Request) string {
	ip := Remote(r)
	if !t.Contains(ip) {
		return ip
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if len(hops) == 1 && strings.TrimSpace(hops[0]) == "" {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if _, err := netip.ParseAddr(xr); err == nil {
				return xr
			}
		}
		return ip
	}
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		if _, err := netip.ParseAddr(h); err != nil {
			break
		}
		ip = h
		if !t.Contains(h) {
			break
		}
	}
	return ip
}
