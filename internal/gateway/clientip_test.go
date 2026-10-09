package gateway

import (
	"net/netip"
	"testing"
)

// The request log takes the client address from X-Forwarded-For only when
// the TCP peer is a trusted proxy.
func TestClientIPInLogsHonoursTrustedProxies(t *testing.T) {
	h := newHarness(t)
	h.model("m", "oa/ok")
	hdr := map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Real-IP": "203.0.113.9"}

	h.postWith("/v1/chat/completions", oaReq("m", false), hdr)
	logs := h.logs()
	if len(logs) != 1 || logs[0].ClientIP != "127.0.0.1" {
		t.Fatalf("untrusted peer must be logged as itself: %+v", logs)
	}

	h.gw.TrustProxies([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	h.postWith("/v1/chat/completions", oaReq("m", false), hdr)
	logs = h.logs()
	if len(logs) != 2 || logs[0].ClientIP != "203.0.113.9" {
		t.Fatalf("trusted peer: expected the forwarded address, got %+v", logs[0])
	}
}
