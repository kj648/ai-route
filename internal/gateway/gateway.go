// Package gateway serves the OpenAI / Anthropic compatible endpoints and
// routes each request through a public model's ordered list of targets,
// falling back to the next target when one fails.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-route/internal/alert"
	"ai-route/internal/convert"
	"ai-route/internal/store"
	"ai-route/internal/version"
)

const maxBodyBytes = 64 << 20

// bodyReadTimeout bounds how long a client may take to send its request
// body; streamWriteTimeout bounds each write to a client that stops reading.
const (
	bodyReadTimeout    = 2 * time.Minute
	streamWriteTimeout = time.Minute
	maxModelNameLen    = 256
)

type Gateway struct {
	store   *store.Store
	Breaker *Breaker
	Limiter *Limiter
	Alerts  *alert.Notifier
	Health  *HealthChecker
	client  *http.Client
	slots   *slots

	touchMu sync.Mutex
	touched map[int64]time.Time
}

func New(s *store.Store) *Gateway {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	g := &Gateway{
		store:   s,
		Breaker: NewBreaker(s.GetSettings),
		Limiter: NewLimiter(s),
		Alerts:  alert.New(s.GetAlerts),
		client:  &http.Client{Transport: transport},
		slots:   newSlots(),
		touched: map[int64]time.Time{},
	}
	g.Health = newHealthChecker(g)
	return g
}

// InFlight returns the number of in-flight requests per provider that has
// a concurrency cap.
func (g *Gateway) InFlight() map[string]int { return g.slots.snapshot() }

// Register mounts the public API routes.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoOpenAI)
	})
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoOpenAI)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoAnthropic)
	})
	mux.HandleFunc("POST /v1/messages/count_tokens", g.countTokens)
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoResponses)
	})
	mux.HandleFunc("POST /responses", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoResponses)
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoEmbeddings)
	})
	mux.HandleFunc("POST /embeddings", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoEmbeddings)
	})
	mux.HandleFunc("POST /v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoRerank)
	})
	mux.HandleFunc("POST /rerank", func(w http.ResponseWriter, r *http.Request) {
		g.handle(w, r, convert.ProtoRerank)
	})
	mux.HandleFunc("GET /v1/models", g.listModels)
	mux.HandleFunc("GET /models", g.listModels)
}

// ---------- auth ----------

func clientKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func (g *Gateway) authenticate(r *http.Request, snap *store.Snapshot) (*store.APIKey, int, string) {
	raw := clientKey(r)
	if raw == "" {
		return nil, http.StatusUnauthorized, "missing API key"
	}
	k, ok := snap.Keys[raw]
	if !ok || !k.Enabled {
		return nil, http.StatusUnauthorized, "invalid API key"
	}
	if k.ExpiresAt > 0 && time.Now().UnixMilli() > k.ExpiresAt {
		return nil, http.StatusUnauthorized, "API key expired"
	}
	g.touch(k.ID)
	return k, 0, ""
}

func (g *Gateway) touch(id int64) {
	g.touchMu.Lock()
	last := g.touched[id]
	if time.Since(last) < time.Minute {
		g.touchMu.Unlock()
		return
	}
	g.touched[id] = time.Now()
	g.touchMu.Unlock()
	go g.store.TouchKey(id)
}

func keyAllows(k *store.APIKey, model string) bool {
	if k == nil || len(k.AllowedModels) == 0 {
		return true
	}
	for _, m := range k.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, proto string, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(convert.ErrorBody(proto, status, msg))
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- routing ----------

type candidate struct {
	target   string // prefix/model
	prefix   string
	model    string
	provider *store.Provider
	proto    string
	openTill time.Time
}

func splitTarget(t string) (string, string) {
	prefix, model, _ := strings.Cut(t, "/")
	return prefix, model
}

// chooseProtocol picks the upstream protocol; "" means the provider cannot
// serve this kind of request (embeddings need an OpenAI endpoint).
func chooseProtocol(p *store.Provider, model, inbound string) string {
	if inbound == convert.ProtoEmbeddings || inbound == convert.ProtoRerank {
		if p.OpenAIBaseURL == "" {
			return ""
		}
		return inbound
	}
	if f := p.ForcedProtocol(model); f != "" {
		return f
	}
	if inbound == convert.ProtoResponses {
		// only some OpenAI-compatible vendors serve /responses
		if p.ResponsesAPI {
			return convert.ProtoResponses
		}
		if p.OpenAIBaseURL != "" {
			return convert.ProtoOpenAI
		}
		return convert.ProtoAnthropic
	}
	if inbound == convert.ProtoOpenAI && p.OpenAIBaseURL != "" || inbound == convert.ProtoAnthropic && p.AnthropicBaseURL != "" {
		return inbound
	}
	if p.OpenAIBaseURL != "" {
		return convert.ProtoOpenAI
	}
	return convert.ProtoAnthropic
}

// plan orders the targets: healthy ones in configured order, then cooled-down
// ones (soonest recovery first) as a last resort. Weighted groups are
// ordered per request by orderGroup; affinity keeps a conversation on the
// same member.
func (g *Gateway) plan(snap *store.Snapshot, m *store.Model, inbound, affinity string) []candidate {
	var healthy, cooling []candidate
	var targets []string
	for _, entry := range m.Targets {
		targets = append(targets, orderGroup(store.ParseTargetEntry(entry), affinity)...)
	}
	for _, t := range targets {
		prefix, model := splitTarget(t)
		p, ok := snap.Providers[prefix]
		if !ok || !p.Enabled {
			continue
		}
		c := candidate{target: t, prefix: prefix, model: model, provider: p, proto: chooseProtocol(p, model, inbound)}
		if c.proto == "" {
			continue
		}
		c.openTill = g.Breaker.OpenUntil(prefix, t)
		if c.openTill.IsZero() {
			healthy = append(healthy, c)
		} else {
			cooling = append(cooling, c)
		}
	}
	sort.SliceStable(cooling, func(i, j int) bool { return cooling[i].openTill.Before(cooling[j].openTill) })
	return append(healthy, cooling...)
}

func (g *Gateway) handle(w http.ResponseWriter, r *http.Request, inbound string) {
	snap := g.store.Snapshot()
	key, status, msg := g.authenticate(r, snap)
	if key == nil {
		writeError(w, inbound, status, msg)
		return
	}
	// limits first: a rejected request costs neither a body read nor a
	// large log row
	if rej := g.Limiter.Admit(key); rej != nil {
		g.reject(w, r, inbound, key, rej)
		return
	}
	if rej := g.Limiter.Enter(key); rej != nil {
		g.reject(w, r, inbound, key, rej)
		return
	}
	defer g.Limiter.Leave(key)
	body, ok := readBody(w, r, inbound)
	if !ok {
		return
	}
	g.route(w, r, inbound, body, key)
}

// readBody reads the request body with a size cap and a deadline.
func readBody(w http.ResponseWriter, r *http.Request, inbound string) ([]byte, bool) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		writeError(w, inbound, http.StatusBadRequest, "failed to read body: "+err.Error())
		return nil, false
	}
	if len(body) > maxBodyBytes {
		writeError(w, inbound, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	return body, true
}

// reject answers a request refused by the key's limits and logs it.
func (g *Gateway) reject(w http.ResponseWriter, r *http.Request, inbound string, key *store.APIKey, rej *rejection) {
	g.store.AddLog(&store.RequestLog{
		CreatedAt: time.Now().UnixMilli(), KeyID: key.ID, KeyName: key.Name,
		Inbound: inbound, HTTPStatus: rej.status, Error: rej.msg, ClientIP: clientIP(r),
	})
	if rej.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(rej.retryAfter.Seconds())+1))
	}
	writeError(w, inbound, rej.status, rej.msg)
}

// route resolves the public model and tries its targets in order.
func (g *Gateway) route(w http.ResponseWriter, r *http.Request, inbound string, body []byte, key *store.APIKey) (entry *store.RequestLog) {
	start := time.Now()
	snap := g.store.Snapshot()
	entry = &store.RequestLog{
		CreatedAt: start.UnixMilli(),
		KeyID:     key.ID,
		KeyName:   key.Name,
		Inbound:   inbound,
		ClientIP:  clientIP(r),
		RequestID: newRequestID(),
	}
	w.Header().Set("X-Route-Request-Id", entry.RequestID)
	defer func() {
		entry.LatencyMs = time.Since(start).Milliseconds()
		// count usage before queueing the log: a concurrent budget reload
		// replaces the cached spend with the logged total, so a request is
		// never counted twice (at worst missed until the next reload)
		g.Limiter.Record(key, entry)
		g.store.AddLog(entry)
	}()

	info, err := convert.ParseRequestInfo(body)
	if err != nil {
		entry.HTTPStatus, entry.Error = 400, "invalid JSON body"
		writeError(w, inbound, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if inbound == convert.ProtoEmbeddings || inbound == convert.ProtoRerank {
		info.Stream = false
	}
	if len(info.Model) > maxModelNameLen {
		entry.HTTPStatus, entry.Error = 400, "model name too long"
		writeError(w, inbound, http.StatusBadRequest, "model name too long")
		return
	}
	entry.RequestedModel, entry.Stream = info.Model, info.Stream
	m := snap.ResolveModel(info.Model)
	if m == nil {
		entry.HTTPStatus, entry.Error = 404, "model not found"
		writeError(w, inbound, http.StatusNotFound, fmt.Sprintf("model %q is not configured on this gateway", info.Model))
		return
	}
	entry.PublicModel = m.Name
	if !keyAllows(key, m.Name) {
		entry.HTTPStatus, entry.Error = 403, "model not allowed for key"
		writeError(w, inbound, http.StatusForbidden, fmt.Sprintf("this API key may not use model %q", m.Name))
		return
	}
	if msg := missingRequiredHeaders(snap, m, r.Header); msg != "" {
		entry.HTTPStatus, entry.Error = 400, msg
		writeError(w, inbound, http.StatusBadRequest, msg)
		return
	}
	affinity := ""
	if conv := convert.ConversationKey(body, inbound); conv != "" {
		affinity = strconv.FormatInt(key.ID, 10) + "\x00" + conv
	}
	meta := &requestMeta{RequestID: entry.RequestID, Conversation: conversationID(affinity), Key: key, Header: r.Header}
	r = r.WithContext(withMeta(r.Context(), meta))
	cands := g.plan(snap, m, inbound, affinity)
	if len(cands) == 0 {
		entry.HTTPStatus, entry.Error = 503, "no enabled targets"
		g.Alerts.Notify(alert.Alert{Event: alert.EventAllFailed, Subject: m.Name,
			Title: fmt.Sprintf(g.Alerts.Pick("模型 %s 没有可用的上游", "Model %s has no available upstream"), m.Name),
			Text:  g.Alerts.Pick("调度顺序里的供应商都已停用，或都不支持这类请求。", "Every provider in its routing order is disabled or cannot serve this kind of request.")})
		writeError(w, inbound, http.StatusServiceUnavailable, fmt.Sprintf("model %q has no enabled upstream targets", m.Name))
		return
	}

	lastStatus, lastMsg := http.StatusBadGateway, ""
	st := snap.Settings
	tried := false
	// attempt runs one target (with retries) and reports whether the request
	// is finished: answered, or the client went away.
	attempt := func(i int, c candidate) bool {
		if c.provider.MaxConcurrency > 0 {
			defer g.slots.release(c.prefix) // the slot was taken by the caller
		}
		tried = true
		var res tryResult
		// transient errors are retried on the same target before switching,
		// so a network blip doesn't move the conversation to another plan
		// (losing its prompt cache). Cooling targets get a single shot.
		maxRetries := st.MaxRetries
		if !c.openTill.IsZero() {
			maxRetries = 0
		}
		for retry := 0; ; retry++ {
			res = g.try(r.Context(), w, r, c, inbound, body, info.Stream, m.Name, st)
			res.attempt.Cooling = !c.openTill.IsZero()
			res.attempt.Retry = retry
			entry.Attempts = append(entry.Attempts, res.attempt)
			if res.committed || res.clientGone || !res.retryable || retry >= maxRetries {
				break
			}
			delay := time.Duration(st.RetryBackoffMs) * time.Millisecond << retry
			if res.retryAfter > delay {
				delay = res.retryAfter
			}
			log.Printf("[%s] target %s failed (%d), retry %d/%d in %s: %s", m.Name, c.target, res.attempt.HTTPStatus, retry+1, maxRetries, delay, truncate(res.attempt.Error, 200))
			if !sleepCtx(r.Context(), delay) {
				res.clientGone = true
				break
			}
		}
		if res.clientGone {
			entry.HTTPStatus, entry.Error = 499, "client disconnected"
			entry.Provider, entry.UpstreamModel, entry.UpstreamProtocol = c.prefix, c.model, c.proto
			return true
		}
		if res.committed {
			entry.Provider, entry.UpstreamModel, entry.UpstreamProtocol = c.prefix, c.model, c.proto
			entry.Success = res.streamErr == ""
			entry.HTTPStatus = 200
			entry.Error = res.streamErr
			if res.aborted && entry.Error == "" {
				entry.Error = "client disconnected mid-stream"
				if res.usageEstimated {
					entry.Error += " (usage estimated)"
				}
			}
			entry.TTFBMs = res.ttfb
			entry.InputTokens, entry.OutputTokens, entry.CachedTokens = res.usage.Input, res.usage.Output, res.usage.Cached
			entry.Cost, entry.Currency, entry.CostSource = costOf(c.provider, c.model, res.usage)
			entry.Fallback = i > 0
			if res.streamErr == "" {
				g.Breaker.Success(c.prefix, c.target)
			} else {
				g.failure(c, failSoft, 0, 200, res.streamErr)
			}
			return true
		}
		g.failure(c, res.kind, res.retryAfter, res.attempt.HTTPStatus, res.attempt.Error)
		lastStatus, lastMsg = res.attempt.HTTPStatus, res.attempt.Error
		log.Printf("[%s] target %s failed (%d): %s", m.Name, c.target, res.attempt.HTTPStatus, truncate(res.attempt.Error, 200))
		return false
	}

	// targets at their concurrency cap are skipped (overflow to the next one)
	var full []int
	for i, c := range cands {
		if c.provider.MaxConcurrency > 0 && !g.slots.tryAcquire(c.prefix, c.provider.MaxConcurrency) {
			full = append(full, i)
			entry.Attempts = append(entry.Attempts, store.Attempt{Target: c.target, Protocol: c.proto,
				Error: fmt.Sprintf("at capacity (%d concurrent requests), skipped", c.provider.MaxConcurrency)})
			continue
		}
		if attempt(i, c) {
			return
		}
	}
	// everything else failed: wait for a slot on one of the full targets
	if len(full) > 0 && st.QueueTimeoutSeconds > 0 {
		waiting := make([]candidate, len(full))
		for k, i := range full {
			waiting[k] = cands[i]
		}
		k := g.slots.acquireAny(r.Context(), waiting, time.Duration(st.QueueTimeoutSeconds)*time.Second)
		if k >= 0 {
			if attempt(full[k], waiting[k]) {
				return
			}
		} else if r.Context().Err() != nil {
			entry.HTTPStatus, entry.Error = 499, "client disconnected while queued"
			return
		} else {
			tried = false // the request ends waiting for capacity
		}
	}
	if !tried {
		lastStatus, lastMsg = http.StatusTooManyRequests, "all upstream targets are at capacity"
		w.Header().Set("Retry-After", "1")
	}
	if lastStatus < 400 {
		lastStatus = http.StatusBadGateway
	}
	var parts []string
	for _, a := range entry.Attempts {
		st := "error"
		if strings.HasPrefix(a.Error, "build request: ") {
			st = strings.TrimPrefix(a.Error, "build request: ") // the gateway's own message, safe to show
		} else if a.HTTPStatus > 0 {
			st = fmt.Sprintf("HTTP %d", a.HTTPStatus)
		} else if strings.Contains(a.Error, "at capacity") {
			st = "at capacity"
		} else if strings.Contains(a.Error, "timeout") {
			st = "timeout"
		}
		parts = append(parts, a.Target+" "+st)
	}
	entry.HTTPStatus = lastStatus
	entry.Error = lastMsg
	g.alertAllFailed(m.Name, entry.Attempts)
	// upstream error texts can carry internal hosts or account details:
	// clients get the statuses, the request log keeps the full story
	writeError(w, inbound, lastStatus, fmt.Sprintf("all upstream targets failed (%s); see request %s in the gateway log",
		strings.Join(parts, ", "), entry.RequestID))
	return entry
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// maxRetryAfter is the longest Retry-After we wait for on the same target;
// longer waits mean the plan is exhausted, so we switch instead.
const maxRetryAfter = 10 * time.Second

func retryableStatus(status int, retryAfter time.Duration) bool {
	switch status {
	case 408, 500, 502, 503, 504, 520, 522, 524, 529:
		return true
	case 429:
		return retryAfter <= maxRetryAfter
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- one upstream attempt ----------

type tryResult struct {
	committed  bool // response (or stream) was sent to the client
	clientGone bool // client went away before anything was committed
	retryable  bool // transient failure worth retrying on the same target
	aborted    bool // client went away after the stream was committed
	attempt    store.Attempt
	kind       failKind
	retryAfter time.Duration
	usage      convert.Usage
	// usageEstimated: the client left before the usage event and it could
	// not be drained; usage is an estimate
	usageEstimated bool
	ttfb           int64
	streamErr      string // error after the stream was committed
}

func openaiURL(base string) string {
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func responsesURL(base string) string {
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	return strings.TrimSuffix(base, "/chat/completions") + "/responses"
}

func rerankURL(base string) string {
	if strings.HasSuffix(base, "/rerank") {
		return base
	}
	return strings.TrimSuffix(base, "/chat/completions") + "/rerank"
}

func embeddingsURL(base string) string {
	if strings.HasSuffix(base, "/embeddings") {
		return base
	}
	return strings.TrimSuffix(base, "/chat/completions") + "/embeddings"
}

func anthropicURL(base string) string {
	switch {
	case strings.HasSuffix(base, "/messages"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/messages"
	default:
		return base + "/v1/messages"
	}
}

// costOf prices a completed request. OpenRouter reports the actual charge
// (USD credits), which wins over configured unit prices.
func costOf(p *store.Provider, model string, u convert.Usage) (float64, string, string) {
	if u.Cost != nil && isOpenRouter(p) {
		return *u.Cost, store.CurrencyUSD, "upstream"
	}
	if u.Input == 0 && u.Output == 0 {
		return 0, "", ""
	}
	if price, ok := p.PriceFor(model); ok {
		return price.Cost(u.Input, u.Cached, u.Output), p.Currency, "price"
	}
	return 0, "", ""
}

func isOpenRouter(p *store.Provider) bool {
	if p.Vendor == "openrouter" {
		return true
	}
	for _, base := range []string{p.OpenAIBaseURL, p.AnthropicBaseURL} {
		if u, err := url.Parse(base); err == nil && strings.EqualFold(u.Hostname(), "openrouter.ai") {
			return true
		}
	}
	return false
}

// applyBodyRules merges the provider's matching body rules into the
// upstream request body.
func applyBodyRules(body []byte, c candidate) ([]byte, error) {
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	for _, rule := range c.provider.RulesFor(c.model, c.proto, probe.Stream) {
		var err error
		if body, err = convert.MergeJSON(body, rule.Set); err != nil {
			return nil, fmt.Errorf("body rule for %s: %w", rule.Model, err)
		}
	}
	return body, nil
}

// isOfficialAnthropic reports whether base points at the Anthropic API itself.
// Compatible endpoints of other vendors may reject newer request fields such
// as output_config, so those are only sent here.
func isOfficialAnthropic(base string) bool {
	u, err := url.Parse(base)
	return err == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com")
}

// buildUpstream converts the client body for the candidate and creates the request.
// It also returns the resolved values of the provider's dynamic headers.
func buildUpstream(ctx context.Context, r *http.Request, c candidate, inbound string, body []byte, st store.Settings, path string) (*http.Request, bool, map[string]string, error) {
	upBody, clientUsage, err := convert.ConvertRequest(inbound, c.proto, body, c.model, st.DefaultMaxTokens, isOfficialAnthropic(c.provider.AnthropicBaseURL))
	if err == nil && len(c.provider.BodyRules) > 0 {
		upBody, err = applyBodyRules(upBody, c)
	}
	if err != nil {
		return nil, false, nil, err
	}
	var url string
	switch c.proto {
	case convert.ProtoEmbeddings:
		url = embeddingsURL(c.provider.OpenAIBaseURL)
	case convert.ProtoRerank:
		url = rerankURL(c.provider.OpenAIBaseURL)
	case convert.ProtoResponses:
		url = responsesURL(c.provider.OpenAIBaseURL)
	case convert.ProtoOpenAI:
		url = openaiURL(c.provider.OpenAIBaseURL)
	default:
		url = anthropicURL(c.provider.AnthropicBaseURL)
		if path != "" {
			url = strings.TrimSuffix(url, "/messages") + path
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(upBody)))
	if err != nil {
		return nil, false, nil, err
	}
	if r != nil && !c.provider.DropClientHeaders {
		copyClientHeaders(req.Header, r.Header, c.proto)
	}
	req.Header.Set("Content-Type", "application/json")
	clientUA := ""
	if r != nil {
		clientUA = r.Header.Get("User-Agent")
	}
	req.Header.Set("User-Agent", upstreamUA(c.provider, clientUA))
	if c.proto == convert.ProtoAnthropic {
		req.Header.Set("x-api-key", c.provider.APIKey)
		req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)
		ver := ""
		if r != nil && inbound == convert.ProtoAnthropic {
			ver = r.Header.Get("anthropic-version")
			if beta := r.Header.Get("anthropic-beta"); beta != "" {
				req.Header.Set("anthropic-beta", beta)
			}
		}
		if ver == "" {
			ver = "2023-06-01"
		}
		req.Header.Set("anthropic-version", ver)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)
	}
	resolved := applyProviderHeaders(req.Header, c.provider, c.model, metaFrom(ctx))
	return req, clientUsage, resolved, nil
}

// upstreamUA applies the provider's User-Agent policy. Some plans only
// accept specific clients, so by default the real client UA is forwarded;
// the gateway's own fingerprint (ai-route/<version>) is the last resort.
func upstreamUA(p *store.Provider, clientUA string) string {
	switch p.UAMode {
	case "override":
		if p.UserAgent != "" {
			return p.UserAgent
		}
	case "platform":
		return version.UserAgent()
	}
	if clientUA != "" {
		return clientUA
	}
	if p.UserAgent != "" {
		return p.UserAgent
	}
	return version.UserAgent()
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return time.Until(t)
	}
	return 0
}

func upstreamErrorMessage(body []byte) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
	}
	if json.Unmarshal(body, &e) == nil {
		if !isNullOrEmpty(e.Error) {
			var inner struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(e.Error, &inner) == nil && inner.Message != "" {
				return inner.Message
			}
			return string(e.Error)
		}
		if e.Message != "" {
			return e.Message
		}
		if e.Msg != "" {
			return e.Msg
		}
	}
	return strings.TrimSpace(string(body))
}

func isNullOrEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func (g *Gateway) try(parent context.Context, w http.ResponseWriter, r *http.Request, c candidate, inbound string, body []byte, stream bool, publicModel string, st store.Settings) (res tryResult) {
	start := time.Now()
	res.attempt = store.Attempt{Target: c.target, Protocol: c.proto}
	defer func() { res.attempt.LatencyMs = time.Since(start).Milliseconds() }()

	// The upstream call outlives a client that disconnects mid-stream (for
	// up to drainTimeout) so the final usage event can still be read and the
	// request billed; before anything is committed a disconnect cancels it.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	var committed atomic.Bool
	stopWatch := context.AfterFunc(parent, func() {
		if committed.Load() {
			time.AfterFunc(drainTimeout, cancel)
		} else {
			cancel()
		}
	})
	defer stopWatch()
	timeout := time.Duration(c.provider.TimeoutSeconds) * time.Second
	// a stream may have a shorter deadline for its first event (a queued
	// self-hosted server should hand over to the next target quickly)
	firstWait, firstLabel := timeout, "timeout"
	if ft := time.Duration(c.provider.FirstTokenTimeoutSeconds) * time.Second; stream && ft > 0 && ft < timeout {
		firstWait, firstLabel = ft, "first token timeout"
	}
	timedOut := false
	var timerMu sync.Mutex
	timer := time.AfterFunc(firstWait, func() {
		timerMu.Lock()
		timedOut = true
		timerMu.Unlock()
		cancel()
	})
	defer timer.Stop()
	isTimeout := func() bool { timerMu.Lock(); defer timerMu.Unlock(); return timedOut }

	fail := func(status int, kind failKind, msg string) tryResult {
		res.attempt.HTTPStatus = status
		res.attempt.Error = msg
		res.kind = kind
		return res
	}
	netFail := func(err error) tryResult {
		if parent.Err() != nil {
			res.clientGone = true
			res.attempt.Error = "client disconnected"
			return res
		}
		if isTimeout() {
			// we already waited the full timeout: switch rather than wait again
			return fail(504, failSoft, fmt.Sprintf("%s after %s", firstLabel, firstWait))
		}
		res.retryable = true
		return fail(502, failSoft, err.Error())
	}

	req, clientUsage, resolved, err := buildUpstream(ctx, r, c, inbound, body, st, "")
	res.attempt.Headers = resolved
	if err != nil {
		return fail(400, failIgnore, "build request: "+err.Error())
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return netFail(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		res.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		res.retryable = retryableStatus(resp.StatusCode, res.retryAfter)
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(upstreamErrorMessage(b), 1000))
		if resp.StatusCode == 404 || resp.StatusCode == 405 {
			// usually a wrong base URL (e.g. missing /v1): show what we called
			msg += " (POST " + req.URL.Redacted() + ")"
		}
		return fail(resp.StatusCode, classifyResponse(resp.StatusCode, res.retryAfter, b), msg)
	}
	res.attempt.HTTPStatus = resp.StatusCode

	if !stream {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		if err != nil {
			return netFail(err)
		}
		if err := convert.ValidateResponse(b, c.proto); err != nil {
			res.retryable = true
			return fail(502, failSoft, err.Error())
		}
		var out []byte
		out, res.usage, err = convert.ConvertResponse(inbound, c.proto, b, publicModel, body)
		if err != nil {
			return fail(502, failSoft, "convert response: "+err.Error())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Route-Target", c.target)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		res.committed = true
		res.ttfb = time.Since(start).Milliseconds()
		return res
	}

	// streaming: peek the first event before committing so that an upstream
	// that fails right away can still be replaced by the next target.
	reader := convert.NewSSEReader(resp.Body)
	first, err := reader.Next()
	if err != nil {
		res.retryable = true
		if errors.Is(err, io.EOF) {
			raw := strings.TrimSpace(reader.Raw.String())
			if raw != "" {
				return fail(502, failSoft, "non-SSE response: "+truncate(upstreamErrorMessage([]byte(raw)), 1000))
			}
			return fail(502, failSoft, "empty stream")
		}
		return netFail(err)
	}
	if msg, isErr := convert.StreamErrorMessage(first, c.proto); isErr {
		res.retryable = true
		return fail(502, failSoft, "stream error: "+msg)
	}

	conv := convert.NewStreamConverter(inbound, c.proto, publicModel, clientUsage, body)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Route-Target", c.target)
	committed.Store(true)
	w.WriteHeader(http.StatusOK)
	res.committed = true
	res.ttfb = time.Since(start).Milliseconds()
	rc := http.NewResponseController(w)

	emitted := 0
	emit := func(evs []convert.SSEEvent) bool {
		if len(evs) == 0 {
			return true
		}
		// a client that stops reading must not hold the stream forever
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		for _, e := range evs {
			if err := convert.WriteSSE(w, e); err != nil {
				return false
			}
			emitted++
		}
		return rc.Flush() == nil
	}

	ev := first
	for {
		if !emit(conv.Process(ev)) {
			res.aborted = true
			drainForUsage(reader, conv, cancel)
			if u := conv.Usage(); u.Input == 0 && u.Output == 0 {
				// the usage event never came: estimate rather than bill nothing
				res.usage = convert.Usage{Input: req.ContentLength / 4, Output: int64(emitted)}
				res.usageEstimated = true
			}
			break
		}
		timer.Reset(timeout) // idle timeout while waiting for the upstream
		ev, err = reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if conv.Complete() || conv.Err() != "" {
					emit(conv.Finish())
				} else {
					res.streamErr = "upstream stream ended before completion"
					emit(streamErrorEvents(inbound, res.streamErr))
				}
			} else if parent.Err() != nil {
				res.aborted = true
			} else {
				if isTimeout() {
					res.streamErr = fmt.Sprintf("stream idle timeout after %s", timeout)
				} else {
					res.streamErr = "stream interrupted: " + err.Error()
				}
				emit(streamErrorEvents(inbound, res.streamErr))
			}
			break
		}
	}
	if !res.usageEstimated {
		res.usage = conv.Usage()
	}
	if res.streamErr == "" && conv.Err() != "" {
		res.streamErr = "upstream stream error: " + conv.Err()
	}
	return res
}

// drainTimeout is how long an upstream stream is still read after the
// client went away, to collect the usage reported at its end.
const drainTimeout = 30 * time.Second

// drainForUsage reads the rest of an upstream stream without forwarding it,
// so the converter sees the final usage event.
func drainForUsage(reader *convert.SSEReader, conv convert.StreamConverter, cancel context.CancelFunc) {
	stop := time.AfterFunc(drainTimeout, cancel)
	defer stop.Stop()
	for {
		// OpenAI sends usage after finish_reason: completion alone is not enough
		if u := conv.Usage(); conv.Complete() && (u.Input > 0 || u.Output > 0) {
			return
		}
		ev, err := reader.Next()
		if err != nil {
			return
		}
		conv.Process(ev)
	}
}

func streamErrorEvents(proto, msg string) []convert.SSEEvent {
	if proto == convert.ProtoResponses {
		return convert.ResponsesErrorEvents(msg)
	}
	if proto == convert.ProtoAnthropic {
		b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}})
		return []convert.SSEEvent{{Event: "error", Data: string(b)}}
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}})
	return []convert.SSEEvent{{Data: string(b)}, {Data: "[DONE]"}}
}

// ---------- models & count_tokens ----------

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	snap := g.store.Snapshot()
	proto := convert.ProtoOpenAI
	if r.Header.Get("anthropic-version") != "" {
		proto = convert.ProtoAnthropic
	}
	key, status, msg := g.authenticate(r, snap)
	if key == nil {
		writeError(w, proto, status, msg)
		return
	}
	var data []map[string]any
	for _, m := range snap.Models {
		if !m.Enabled || !keyAllows(key, m.Name) {
			continue
		}
		if proto == convert.ProtoAnthropic {
			data = append(data, map[string]any{
				"type": "model", "id": m.Name, "display_name": m.Name,
				"created_at":  time.UnixMilli(m.CreatedAt).UTC().Format(time.RFC3339),
				"tags":        m.Tags,
				"description": m.Description,
			})
		} else {
			// tags / description are extensions; OpenAI clients ignore them
			data = append(data, map[string]any{
				"id": m.Name, "object": "model", "created": m.CreatedAt / 1000, "owned_by": "ai-route",
				"tags": m.Tags, "description": m.Description,
			})
		}
	}
	if data == nil {
		data = []map[string]any{}
	}
	var out map[string]any
	if proto == convert.ProtoAnthropic {
		out = map[string]any{"data": data, "has_more": false}
		if len(data) > 0 {
			out["first_id"], out["last_id"] = data[0]["id"], data[len(data)-1]["id"]
		}
	} else {
		out = map[string]any{"object": "list", "data": data}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// countTokens forwards to the first healthy Anthropic-capable target and
// falls back to a rough local estimate.
func (g *Gateway) countTokens(w http.ResponseWriter, r *http.Request) {
	snap := g.store.Snapshot()
	key, status, msg := g.authenticate(r, snap)
	if key == nil {
		writeError(w, convert.ProtoAnthropic, status, msg)
		return
	}
	// count_tokens uses the operator's upstream quota too: same limits
	if rej := g.Limiter.Admit(key); rej != nil {
		g.reject(w, r, "count_tokens", key, rej)
		return
	}
	body, ok := readBody(w, r, convert.ProtoAnthropic)
	if !ok {
		return
	}
	info, _ := convert.ParseRequestInfo(body)
	g.store.AddLog(&store.RequestLog{
		CreatedAt: time.Now().UnixMilli(), KeyID: key.ID, KeyName: key.Name, RequestedModel: info.Model,
		Inbound: "count_tokens", Success: true, HTTPStatus: 200, ClientIP: clientIP(r), RequestID: newRequestID(),
	})
	if m := snap.ResolveModel(info.Model); m != nil && keyAllows(key, m.Name) {
		for _, c := range g.plan(snap, m, convert.ProtoAnthropic, "") {
			if c.proto != convert.ProtoAnthropic || !c.openTill.IsZero() {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			req, _, _, err := buildUpstream(ctx, r, c, convert.ProtoAnthropic, body, snap.Settings, "/messages/count_tokens")
			if err == nil {
				resp, err := g.client.Do(req)
				if err == nil {
					b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
					resp.Body.Close()
					if resp.StatusCode == 200 && strings.Contains(string(b), "input_tokens") {
						cancel()
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(b)
						return
					}
				}
			}
			cancel()
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": estimateTokens(body)})
}

func estimateTokens(body []byte) int {
	n := 0
	for _, r := range string(body) {
		if r > 0x2E80 {
			n += 3 // CJK: roughly one token per char
		} else {
			n++
		}
	}
	return n/4 + 1
}
