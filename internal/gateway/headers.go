package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-route/internal/convert"
	"ai-route/internal/hdrtpl"
	"ai-route/internal/store"
)

// requestMeta is per-request data that header templates can use. It travels
// in the request context so every upstream call of the request sees it.
type requestMeta struct {
	RequestID    string
	Conversation string // ses_...; random when the request has no user message
	Key          *store.APIKey
	Header       http.Header // the caller's headers
}

type metaKey struct{}

func withMeta(ctx context.Context, m *requestMeta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

func metaFrom(ctx context.Context) *requestMeta {
	m, _ := ctx.Value(metaKey{}).(*requestMeta)
	return m
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// conversationID turns the routing affinity (API key + first user message)
// into a session id that stays the same for the whole conversation. Without
// one (no user message, health checks, admin tests) it is random: upstreams
// such as OpenCode Go reject requests without a session header.
func conversationID(affinity string) string {
	if affinity == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		return "ses_" + hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(affinity))
	return "ses_" + hex.EncodeToString(sum[:12])
}

// probeMeta is the template context for the gateway's own upstream calls
// (health checks, model lists, admin tests): a fresh request and session id,
// and the caller headers given (if any).
func probeMeta(h http.Header) *requestMeta {
	return &requestMeta{RequestID: newRequestID(), Conversation: conversationID(""), Header: h}
}

// dropHeaders are never copied from the caller: credentials for the gateway
// itself, headers the gateway sets, hop-by-hop headers, and browser / proxy
// headers that leak the user's identity or trip upstream CORS checks.
var dropHeaders = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Proxy-Authorization": true, "Cookie": true,
	"Host": true, "Content-Length": true, "Content-Type": true, "User-Agent": true,
	// the transport only decompresses responses when it asked for gzip itself
	"Accept-Encoding": true,
	// hop-by-hop, as in net/http/httputil.ReverseProxy (RFC 9110 7.6.1)
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true,
	"Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Http2-Settings": true, "Expect": true,
	"Forwarded": true, "X-Real-Ip": true, "True-Client-Ip": true, "Via": true,
	"Origin": true, "Referer": true,
	// would select another org / project / key on the operator's account
	"Openai-Organization": true, "Openai-Project": true, "Api-Key": true, "X-Goog-Api-Key": true,
}

var dropPrefixes = []string{"X-Forwarded-", "Cf-", "Sec-", "Proxy-"}

// copyClientHeaders forwards the caller's headers that make sense for the
// upstream protocol.
func copyClientHeaders(dst, src http.Header, upstream string) {
	// fields named in Connection are hop-by-hop too (RFC 9110 7.6.1)
	named := map[string]bool{}
	for _, v := range src.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				named[http.CanonicalHeaderKey(f)] = true
			}
		}
	}
	for k, vs := range src {
		if dropHeaders[k] || named[k] {
			continue
		}
		skip := false
		for _, p := range dropPrefixes {
			if strings.HasPrefix(k, p) {
				skip = true
				break
			}
		}
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "anthropic-") && upstream != convert.ProtoAnthropic ||
			strings.HasPrefix(lower, "openai-") && upstream == convert.ProtoAnthropic {
			skip = true
		}
		if !skip {
			dst[k] = append([]string(nil), vs...)
		}
	}
}

// applyProviderHeaders sets the provider's configured headers on req,
// resolving templates. It returns the resolved values of dynamic headers.
func applyProviderHeaders(h http.Header, p *store.Provider, model string, meta *requestMeta) map[string]string {
	tctx := &hdrtpl.Context{Model: model, Now: time.Now()}
	if meta != nil {
		tctx.Header, tctx.Conversation, tctx.RequestID = meta.Header, meta.Conversation, meta.RequestID
		if meta.Key != nil {
			tctx.KeyName, tctx.KeyID = meta.Key.Name, meta.Key.ID
		}
	}
	var resolved map[string]string
	for k, v := range p.Headers {
		if v == "" {
			h.Del(k)
			continue
		}
		t, err := hdrtpl.Parse(v)
		if err != nil { // rejected on save; keep the literal for old rows
			h.Set(k, v)
			continue
		}
		val, ok := t.Eval(tctx)
		if t.Dynamic() {
			if resolved == nil {
				resolved = map[string]string{}
			}
			resolved[k] = val
			if !ok {
				resolved[k] = "(omitted)"
			}
		}
		if !ok || val == "" {
			h.Del(k)
			continue
		}
		h.Set(k, val)
	}
	return resolved
}

// missingRequiredHeaders checks the headers the model's first-priority
// targets require from the caller. Fallback targets are not checked: there
// a missing header is simply not sent.
func missingRequiredHeaders(snap *store.Snapshot, m *store.Model, caller http.Header) string {
	for _, entry := range m.Targets {
		var enabled []*store.Provider
		for _, member := range store.ParseTargetEntry(entry) {
			prefix, _ := splitTarget(member.Target)
			if p, ok := snap.Providers[prefix]; ok && p.Enabled {
				enabled = append(enabled, p)
			}
		}
		if len(enabled) == 0 {
			continue // the first level that can actually serve is what counts
		}
		var missing []string
		for _, p := range enabled {
			for _, v := range p.Headers {
				t, err := hdrtpl.Parse(v)
				if err != nil {
					continue
				}
				for _, name := range t.Required() {
					if caller.Get(name) == "" {
						missing = append(missing, fmt.Sprintf("%s (required by provider %s)", name, p.Prefix))
					}
				}
			}
		}
		if len(missing) == 0 {
			return ""
		}
		return fmt.Sprintf("missing required request header: %s", strings.Join(missing, ", "))
	}
	return ""
}
