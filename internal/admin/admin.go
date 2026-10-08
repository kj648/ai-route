// Package admin serves the management API under /admin/api and the embedded
// web console under /admin/.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-route/internal/alert"
	"ai-route/internal/gateway"
	"ai-route/internal/store"
)

type Admin struct {
	store *store.Store
	gw    *gateway.Gateway
	token string
	web   fs.FS
}

func New(s *store.Store, gw *gateway.Gateway, token string, web fs.FS) *Admin {
	return &Admin{store: s, gw: gw, token: token, web: web}
}

func (a *Admin) Register(mux *http.ServeMux) {
	api := http.NewServeMux()
	api.HandleFunc("GET /admin/api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"ok": true}) })

	api.HandleFunc("GET /admin/api/providers", a.listProviders)
	api.HandleFunc("POST /admin/api/providers", a.createProvider)
	api.HandleFunc("PUT /admin/api/providers/{id}", a.updateProvider)
	api.HandleFunc("DELETE /admin/api/providers/{id}", a.deleteProvider)
	api.HandleFunc("POST /admin/api/providers/{id}/test", a.testProvider)
	api.HandleFunc("POST /admin/api/providers/fetch-models", a.fetchModels)
	api.HandleFunc("POST /admin/api/providers/{id}/sync-models", a.syncModels)

	api.HandleFunc("GET /admin/api/models", a.listModels)
	api.HandleFunc("POST /admin/api/models", a.createModel)
	api.HandleFunc("PUT /admin/api/models/{id}", a.updateModel)
	api.HandleFunc("DELETE /admin/api/models/{id}", a.deleteModel)
	api.HandleFunc("POST /admin/api/models/test", a.testModel)

	api.HandleFunc("GET /admin/api/keys", a.listKeys)
	api.HandleFunc("POST /admin/api/keys", a.createKey)
	api.HandleFunc("PUT /admin/api/keys/{id}", a.updateKey)
	api.HandleFunc("DELETE /admin/api/keys/{id}", a.deleteKey)
	api.HandleFunc("POST /admin/api/keys/{id}/rotate", a.rotateKey)

	api.HandleFunc("GET /admin/api/status", a.status)
	api.HandleFunc("POST /admin/api/status/reset", a.resetBreaker)
	api.HandleFunc("GET /admin/api/runtime", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"in_flight": a.gw.InFlight(), "health": a.gw.Health.Status()})
	})
	api.HandleFunc("GET /admin/api/logs", a.logs)
	api.HandleFunc("GET /admin/api/stats", a.stats)

	api.HandleFunc("GET /admin/api/settings", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.store.GetSettings()) })
	api.HandleFunc("PUT /admin/api/settings", a.updateSettings)
	api.HandleFunc("GET /admin/api/alerts", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.store.GetAlerts()) })
	api.HandleFunc("PUT /admin/api/alerts", a.updateAlerts)
	api.HandleFunc("POST /admin/api/alerts/test", a.testAlert)
	api.HandleFunc("GET /admin/api/export", a.export)
	api.HandleFunc("POST /admin/api/import", a.importConfig)

	mux.Handle("/admin/api/", a.auth(api))

	static := http.FileServerFS(a.web)
	// no-cache: embedded files carry no modification time, so without it
	// browsers keep serving the old console after an upgrade
	mux.Handle("/admin/", http.StripPrefix("/admin/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
}

func (a *Admin) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeErr(w, http.StatusBadRequest, err)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20))
	return dec.Decode(v)
}

// ---------- providers ----------

// maskKey always contains "******", which updateProvider treats as "unchanged".
func maskKey(k string) string {
	if len(k) <= 12 {
		return "********"
	}
	return k[:4] + strings.Repeat("*", 6) + k[len(k)-4:]
}

type providerView struct {
	*store.Provider
	APIKey    string `json:"api_key"`
	HasAPIKey bool   `json:"has_api_key"`
}

func (a *Admin) listProviders(w http.ResponseWriter, r *http.Request) {
	ps, err := a.store.ListProviders()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := make([]providerView, 0, len(ps))
	for _, p := range ps {
		out = append(out, providerView{Provider: p, APIKey: maskKey(p.APIKey), HasAPIKey: p.APIKey != ""})
	}
	writeJSON(w, out)
}

func (a *Admin) createProvider(w http.ResponseWriter, r *http.Request) {
	var p store.Provider
	if err := decode(r, &p); err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(p.Models) == 0 && p.APIKey != "" {
		// best effort: fill the model list from the upstream
		if ids, err := a.gw.FetchUpstreamModels(r.Context(), &p); err == nil {
			p.Models = ids
		}
	}
	if err := a.store.CreateProvider(&p); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": p.ID, "prefix": p.Prefix})
}

func (a *Admin) updateProvider(w http.ResponseWriter, r *http.Request) {
	var p store.Provider
	if err := decode(r, &p); err != nil {
		writeErr(w, 400, err)
		return
	}
	p.ID = pathID(r)
	if strings.Contains(p.APIKey, "******") {
		p.APIKey = "" // masked value echoed back: keep existing key
	}
	if err := a.store.UpdateProvider(&p); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteProvider(pathID(r)); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) testProvider(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetProvider(pathID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
		Stream   bool   `json:"stream"`
	}
	if err := decode(r, &req); err != nil || req.Model == "" {
		writeErr(w, 400, errors.New("model is required"))
		return
	}
	if req.Protocol == "openai" && p.OpenAIBaseURL == "" || req.Protocol == "anthropic" && p.AnthropicBaseURL == "" {
		writeErr(w, 400, errors.New("this provider has no "+req.Protocol+" base URL"))
		return
	}
	writeJSON(w, a.gw.TestTarget(r.Context(), p, req.Model, req.Protocol, req.Stream))
}

// fetchModels lists upstream models for a provider form that may not be
// saved yet. With an id and no new key, the stored key is used.
func (a *Admin) fetchModels(w http.ResponseWriter, r *http.Request) {
	var p store.Provider
	if err := decode(r, &p); err != nil {
		writeErr(w, 400, err)
		return
	}
	if p.ID > 0 && (p.APIKey == "" || strings.Contains(p.APIKey, "******")) {
		old, err := a.store.GetProvider(p.ID)
		if err != nil {
			storeErr(w, err)
			return
		}
		p.APIKey = old.APIKey
	}
	p.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	p.AnthropicBaseURL = strings.TrimRight(strings.TrimSpace(p.AnthropicBaseURL), "/")
	if p.APIKey == "" {
		writeErr(w, 400, errors.New("API key is required to fetch models"))
		return
	}
	ids, err := a.gw.FetchUpstreamModels(r.Context(), &p)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, ids)
}

// syncModels fetches the upstream list and merges it into the saved provider.
func (a *Admin) syncModels(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.GetProvider(pathID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	ids, err := a.gw.FetchUpstreamModels(r.Context(), p)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	have := map[string]bool{}
	for _, m := range p.Models {
		have[m] = true
	}
	var added []string
	for _, id := range ids {
		if !have[id] {
			added = append(added, id)
			p.Models = append(p.Models, id)
		}
	}
	p.APIKey = "" // keep stored key
	if err := a.store.UpdateProvider(p); err != nil {
		storeErr(w, err)
		return
	}
	if added == nil {
		added = []string{}
	}
	writeJSON(w, map[string]any{"models": p.Models, "added": added})
}

// ---------- models ----------

func (a *Admin) listModels(w http.ResponseWriter, r *http.Request) {
	ms, err := a.store.ListModels()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if ms == nil {
		ms = []*store.Model{}
	}
	writeJSON(w, ms)
}

func (a *Admin) createModel(w http.ResponseWriter, r *http.Request) {
	var m store.Model
	if err := decode(r, &m); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.store.CreateModel(&m); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": m.ID})
}

func (a *Admin) updateModel(w http.ResponseWriter, r *http.Request) {
	var m store.Model
	if err := decode(r, &m); err != nil {
		writeErr(w, 400, err)
		return
	}
	m.ID = pathID(r)
	if err := a.store.UpdateModel(&m); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteModel(pathID(r)); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) testModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
		Stream   bool   `json:"stream"`
		Prompt   string `json:"prompt"`
	}
	if err := decode(r, &req); err != nil || req.Model == "" {
		writeErr(w, 400, errors.New("model is required"))
		return
	}
	writeJSON(w, a.gw.TestModel(r.Context(), req.Model, req.Protocol, req.Stream, req.Prompt))
}

// ---------- keys ----------

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := a.store.ListKeys()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	spend, err := a.store.KeySpend(store.MonthStart(time.Now()))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	type keyView struct {
		*store.APIKey
		MonthCost float64 `json:"month_cost"` // month-to-date, display currency
	}
	out := make([]keyView, 0, len(ks))
	for _, k := range ks {
		out = append(out, keyView{k, spend[k.ID]})
	}
	writeJSON(w, out)
}

func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var k store.APIKey
	if err := decode(r, &k); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.store.CreateKey(&k); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, k)
}

func (a *Admin) updateKey(w http.ResponseWriter, r *http.Request) {
	var k store.APIKey
	if err := decode(r, &k); err != nil {
		writeErr(w, 400, err)
		return
	}
	k.ID = pathID(r)
	if err := a.store.UpdateKey(&k); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) rotateKey(w http.ResponseWriter, r *http.Request) {
	v, err := a.store.RotateKey(pathID(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"key": v})
}

func (a *Admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteKey(pathID(r)); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------- status / logs / stats ----------

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.gw.Breaker.Status())
}

func (a *Admin) resetBreaker(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	_ = decode(r, &req)
	a.gw.Breaker.Reset(req.Key)
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := store.LogQuery{
		Model:    q.Get("model"),
		Provider: q.Get("provider"),
		Status:   q.Get("status"),
		Fallback: q.Get("fallback") == "1",
	}
	lq.KeyID, _ = strconv.ParseInt(q.Get("key_id"), 10, 64)
	lq.Limit, _ = strconv.Atoi(q.Get("limit"))
	lq.Offset, _ = strconv.Atoi(q.Get("offset"))
	logs, total, err := a.store.QueryLogs(lq)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"total": total, "items": logs})
}

func (a *Admin) stats(w http.ResponseWriter, r *http.Request) {
	var d, bucket time.Duration
	switch r.URL.Query().Get("range") {
	case "1h":
		d, bucket = time.Hour, 5*time.Minute
	case "7d":
		d, bucket = 7*24*time.Hour, 6*time.Hour
	case "30d":
		d, bucket = 30*24*time.Hour, 24*time.Hour
	default:
		d, bucket = 24*time.Hour, time.Hour
	}
	st, err := a.store.GetStats(time.Now().Add(-d).UnixMilli(), bucket.Milliseconds())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, st)
}

// ---------- settings / backup ----------

func (a *Admin) updateSettings(w http.ResponseWriter, r *http.Request) {
	var st store.Settings
	if err := decode(r, &st); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.store.UpdateSettings(st); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.gw.Limiter.Forget() // budgets are in the display currency
	writeJSON(w, a.store.GetSettings())
}

func (a *Admin) export(w http.ResponseWriter, r *http.Request) {
	e, err := a.store.Export()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="ai-route-config-`+time.Now().Format("20060102-150405")+`.json"`)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}

func (a *Admin) updateAlerts(w http.ResponseWriter, r *http.Request) {
	var c store.AlertConfig
	if err := decode(r, &c); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.store.UpdateAlerts(c); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, a.store.GetAlerts())
}

// testAlert sends a test message to one webhook (not necessarily saved yet).
func (a *Admin) testAlert(w http.ResponseWriter, r *http.Request) {
	var h store.Webhook
	if err := decode(r, &h); err != nil {
		writeErr(w, 400, err)
		return
	}
	c := store.AlertConfig{Webhooks: []store.Webhook{h}}
	if err := store.ValidateAlerts(&c); err != nil || len(c.Webhooks) == 0 {
		if err == nil {
			err = errors.New("webhook url is required")
		}
		writeErr(w, 400, err)
		return
	}
	err := a.gw.Alerts.Send(r.Context(), c.Webhooks[0], alert.Alert{Event: alert.EventTest, Subject: "test",
		Title: "测试消息", Text: "告警通知配置成功。上游鉴权失败、整条调度链全部失败、长时间冷却时会推送到这里。"})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *Admin) importConfig(w http.ResponseWriter, r *http.Request) {
	e := store.Export{Settings: store.DefaultSettings()} // fields missing from older exports keep defaults
	if err := decode(r, &e); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := a.store.Import(&e); err != nil {
		writeErr(w, 400, err)
		return
	}
	a.gw.Breaker.Reset("")
	a.gw.Limiter.Forget()
	writeJSON(w, map[string]any{"ok": true})
}
