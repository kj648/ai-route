// Package store persists providers, model mappings, API keys, settings and
// request logs in SQLite, and keeps an immutable in-memory snapshot of the
// routing configuration that the gateway reads on every request.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"ai-route/internal/hdrtpl"
)

var ErrNotFound = errors.New("not found")

// Provider is one upstream plan (e.g. kimi coding plan). Its Prefix is used in
// model targets: "<prefix>/<upstream model id>".
type Provider struct {
	ID               int64             `json:"id"`
	Prefix           string            `json:"prefix"`
	Name             string            `json:"name"`
	OpenAIBaseURL    string            `json:"openai_base_url"`
	AnthropicBaseURL string            `json:"anthropic_base_url"`
	APIKey           string            `json:"api_key"`
	Headers          map[string]string `json:"headers"`
	// ModelProtocols forces a protocol for matching upstream models
	// (glob pattern -> "openai" | "anthropic"), for plans where some models
	// are only served on one endpoint.
	ModelProtocols map[string]string `json:"model_protocols"`
	// Models is the plan's model list (fetched from the upstream or typed in);
	// they appear in the model-mapping picker as "<prefix>/<model>".
	Models []string `json:"models"`
	// Vendor is the preset this provider was created from ("" = custom).
	Vendor string `json:"vendor"`
	// UAMode: "passthrough" forwards the client's User-Agent (UserAgent is
	// used only when the client sends none); "override" always sends UserAgent.
	UAMode         string `json:"ua_mode"`
	UserAgent      string `json:"user_agent"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// Prices are per-million-token unit prices by upstream model (exact
	// name or '*' glob), in Currency. Models without a price are not costed.
	Prices   map[string]Price `json:"prices"`
	Currency string           `json:"currency"` // CNY | USD
	// BodyRules patch the upstream request body for matching models, e.g.
	// enable_thinking=false for Qwen3 non-stream calls on Bailian.
	BodyRules []BodyRule `json:"body_rules"`
	// Self-hosted upstreams: MaxConcurrency caps in-flight requests (excess
	// overflows to the next target); FirstTokenTimeoutSeconds bounds the wait
	// for the first stream event; HealthCheckSeconds probes HealthCheckURL
	// (default: the models endpoint) and takes the provider out of rotation
	// while it fails. 0 = off.
	MaxConcurrency           int    `json:"max_concurrency"`
	FirstTokenTimeoutSeconds int    `json:"first_token_timeout_seconds"`
	HealthCheckSeconds       int    `json:"health_check_seconds"`
	HealthCheckURL           string `json:"health_check_url"`
	// DropClientHeaders stops forwarding the caller's request headers
	// (they are forwarded by default, minus credentials and hop-by-hop ones).
	DropClientHeaders bool   `json:"drop_client_headers"`
	Enabled           bool   `json:"enabled"`
	Remark            string `json:"remark"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// BodyRule merges Set into the request body sent upstream (after protocol
// conversion) when the upstream model matches.
type BodyRule struct {
	Model string `json:"model"` // exact name or '*' glob
	// When: "" (always) | "stream" | "nonstream"
	When string `json:"when,omitempty"`
	// Protocol: "" (any) | "openai" | "anthropic" | "embeddings" | "rerank"
	Protocol string `json:"protocol,omitempty"`
	// Set is a JSON object deep-merged into the body; a null value deletes
	// the field.
	Set json.RawMessage `json:"set"`
}

// RulesFor returns the body rules that apply to a request, in order.
func (p *Provider) RulesFor(model, proto string, stream bool) []BodyRule {
	var out []BodyRule
	for _, r := range p.BodyRules {
		if r.Model != model && !(strings.Contains(r.Model, "*") && globMatch(r.Model, model)) {
			continue
		}
		if r.Protocol != "" && r.Protocol != proto || r.When == "stream" && !stream || r.When == "nonstream" && stream {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Price is a unit price per million tokens.
type Price struct {
	Input float64 `json:"input"`
	// Cache is the price of cached (cache-read) input tokens; nil means
	// they are billed at the input price.
	Cache  *float64 `json:"cache,omitempty"`
	Output float64  `json:"output"`
}

// Cost prices a request. input includes cached tokens.
func (p Price) Cost(input, cached, output int64) float64 {
	cache := p.Input
	if p.Cache != nil {
		cache = *p.Cache
	}
	if cached > input {
		cached = input
	}
	return (float64(input-cached)*p.Input + float64(cached)*cache + float64(output)*p.Output) / 1e6
}

const (
	CurrencyCNY = "CNY"
	CurrencyUSD = "USD"
)

// Model is a public model exposed to clients. Targets are tried in order
// ("<prefix>/<model>", or a weighted same-priority group, see
// ParseTargetEntry), later entries are fallbacks.
type Model struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	// Tags describe what the model is for (chat, embedding, vision,
	// reasoning, ctx-1m ...); free-form, the console knows the common ones.
	Tags        []string `json:"tags"`
	Targets     []string `json:"targets"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

// APIKey is a client-facing key issued by the platform.
type APIKey struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Key           string   `json:"key"`
	Enabled       bool     `json:"enabled"`
	AllowedModels []string `json:"allowed_models"` // empty = all
	ExpiresAt     int64    `json:"expires_at"`     // unix ms, 0 = never
	// MonthlyBudget caps the calendar month's cost in Settings.Currency;
	// RPM / TPM cap requests and tokens per rolling minute. 0 = unlimited.
	MonthlyBudget float64 `json:"monthly_budget"`
	RPM           int     `json:"rpm"`
	TPM           int     `json:"tpm"`
	CreatedAt     int64   `json:"created_at"`
	LastUsedAt    int64   `json:"last_used_at"`
}

// Settings are global tunables.
type Settings struct {
	MaxRetries         int `json:"max_retries"`          // retries on the same target for transient errors before switching
	RetryBackoffMs     int `json:"retry_backoff_ms"`     // first retry delay, doubled for each further retry
	FailureThreshold   int `json:"failure_threshold"`    // consecutive failed requests (after retries) before a target is cooled down
	CooldownSeconds    int `json:"cooldown_seconds"`     // first cooldown length
	MaxCooldownSeconds int `json:"max_cooldown_seconds"` // cap for exponential backoff
	LogRetentionDays   int `json:"log_retention_days"`
	DefaultMaxTokens   int `json:"default_max_tokens"` // used when converting OpenAI -> Anthropic without max_tokens
	// Currency is the currency stats are shown in; costs recorded in the
	// other currency are converted with USDToCNY.
	Currency string  `json:"currency"`
	USDToCNY float64 `json:"usd_to_cny"`
	// QueueTimeoutSeconds is how long a request waits for a free slot when
	// every target with a concurrency cap is full.
	QueueTimeoutSeconds int `json:"queue_timeout_seconds"`
}

func DefaultSettings() Settings {
	return Settings{
		MaxRetries:          2,
		RetryBackoffMs:      1000,
		FailureThreshold:    2,
		CooldownSeconds:     60,
		MaxCooldownSeconds:  1800,
		LogRetentionDays:    30,
		DefaultMaxTokens:    8192,
		Currency:            CurrencyCNY,
		USDToCNY:            7.2,
		QueueTimeoutSeconds: 30,
	}
}

// Snapshot is an immutable view of the routing configuration.
type Snapshot struct {
	Providers   map[string]*Provider // by prefix
	Models      []*Model
	modelByName map[string]*Model
	Keys        map[string]*APIKey // by key string
	Settings    Settings
	Alerts      AlertConfig
}

// ResolveModel finds a public model by exact name, exact alias, then glob alias.
func (s *Snapshot) ResolveModel(name string) *Model {
	if m, ok := s.modelByName[name]; ok {
		return m
	}
	for _, m := range s.Models {
		if !m.Enabled {
			continue
		}
		for _, a := range m.Aliases {
			if strings.Contains(a, "*") && globMatch(a, name) {
				return m
			}
		}
	}
	return nil
}

// globMatch supports '*' wildcards only.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

type Store struct {
	db   *sql.DB
	snap atomic.Pointer[Snapshot]
	mu   sync.Mutex // serializes writes + snapshot rebuilds

	logMu     sync.RWMutex
	logClosed bool
	logCh     chan *RequestLog
	logDone   chan struct{}
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "ai-route.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, logCh: make(chan *RequestLog, 4096), logDone: make(chan struct{})}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.Reload(); err != nil {
		db.Close()
		return nil, err
	}
	go s.logWriter()
	return s, nil
}

func (s *Store) Close() error {
	s.logMu.Lock()
	s.logClosed = true
	close(s.logCh)
	s.logMu.Unlock()
	<-s.logDone
	return s.db.Close()
}

func (s *Store) Snapshot() *Snapshot { return s.snap.Load() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS providers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	prefix TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	openai_base_url TEXT NOT NULL DEFAULT '',
	anthropic_base_url TEXT NOT NULL DEFAULT '',
	api_key TEXT NOT NULL DEFAULT '',
	headers TEXT NOT NULL DEFAULT '{}',
	model_protocols TEXT NOT NULL DEFAULT '{}',
	models TEXT NOT NULL DEFAULT '[]',
	vendor TEXT NOT NULL DEFAULT '',
	ua_mode TEXT NOT NULL DEFAULT 'passthrough',
	user_agent TEXT NOT NULL DEFAULT '',
	timeout_seconds INTEGER NOT NULL DEFAULT 300,
	prices TEXT NOT NULL DEFAULT '{}',
	body_rules TEXT NOT NULL DEFAULT '[]',
	max_concurrency INTEGER NOT NULL DEFAULT 0,
	first_token_timeout_seconds INTEGER NOT NULL DEFAULT 0,
	health_check_seconds INTEGER NOT NULL DEFAULT 0,
	health_check_url TEXT NOT NULL DEFAULT '',
	drop_client_headers INTEGER NOT NULL DEFAULT 0,
	currency TEXT NOT NULL DEFAULT 'CNY',
	enabled INTEGER NOT NULL DEFAULT 1,
	remark TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS models (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	aliases TEXT NOT NULL DEFAULT '[]',
	tags TEXT NOT NULL DEFAULT '[]',
	targets TEXT NOT NULL DEFAULT '[]',
	enabled INTEGER NOT NULL DEFAULT 1,
	description TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL DEFAULT '',
	key TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL DEFAULT 1,
	allowed_models TEXT NOT NULL DEFAULT '[]',
	expires_at INTEGER NOT NULL DEFAULT 0,
	monthly_budget REAL NOT NULL DEFAULT 0,
	rpm INTEGER NOT NULL DEFAULT 0,
	tpm INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at INTEGER NOT NULL,
	key_id INTEGER NOT NULL DEFAULT 0,
	key_name TEXT NOT NULL DEFAULT '',
	requested_model TEXT NOT NULL DEFAULT '',
	public_model TEXT NOT NULL DEFAULT '',
	inbound TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	provider TEXT NOT NULL DEFAULT '',
	upstream_model TEXT NOT NULL DEFAULT '',
	upstream_protocol TEXT NOT NULL DEFAULT '',
	success INTEGER NOT NULL DEFAULT 0,
	http_status INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttfb_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	fallback INTEGER NOT NULL DEFAULT 0,
	attempts TEXT NOT NULL DEFAULT '[]',
	error TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	cost REAL NOT NULL DEFAULT 0,
	currency TEXT NOT NULL DEFAULT '',
	cost_source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_logs_created ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_model ON request_logs(public_model, created_at);
CREATE INDEX IF NOT EXISTS idx_logs_provider ON request_logs(provider, created_at);
`)
	if err != nil {
		return err
	}
	// columns added after the first release
	for _, c := range [][2]string{
		{"models", `TEXT NOT NULL DEFAULT '[]'`},
		{"vendor", `TEXT NOT NULL DEFAULT ''`},
		{"ua_mode", `TEXT NOT NULL DEFAULT 'passthrough'`},
		{"user_agent", `TEXT NOT NULL DEFAULT ''`},
		{"prices", `TEXT NOT NULL DEFAULT '{}'`},
		{"currency", `TEXT NOT NULL DEFAULT 'CNY'`},
		{"body_rules", `TEXT NOT NULL DEFAULT '[]'`},
		{"max_concurrency", `INTEGER NOT NULL DEFAULT 0`},
		{"first_token_timeout_seconds", `INTEGER NOT NULL DEFAULT 0`},
		{"health_check_seconds", `INTEGER NOT NULL DEFAULT 0`},
		{"health_check_url", `TEXT NOT NULL DEFAULT ''`},
		{"drop_client_headers", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn("providers", c[0], c[1]); err != nil {
			return err
		}
	}
	for _, c := range [][2]string{
		{"cost", `REAL NOT NULL DEFAULT 0`},
		{"currency", `TEXT NOT NULL DEFAULT ''`},
		{"cost_source", `TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.ensureColumn("request_logs", c[0], c[1]); err != nil {
			return err
		}
	}
	for _, c := range [][2]string{
		{"monthly_budget", `REAL NOT NULL DEFAULT 0`},
		{"rpm", `INTEGER NOT NULL DEFAULT 0`},
		{"tpm", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn("api_keys", c[0], c[1]); err != nil {
			return err
		}
	}
	if err := s.ensureColumn("request_logs", "request_id", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.migrateOpenCodeSession(); err != nil {
		return err
	}
	return s.ensureColumn("models", "tags", `TEXT NOT NULL DEFAULT '[]'`)
}

// OpenCodeSessionHeader is the OpenCode Go preset's session header value:
// the caller's own session id, else Claude Code's session id (documented
// x-claude-code-session-id), else one generated per conversation.
const OpenCodeSessionHeader = "{{header.x-opencode-session ?? header.x-claude-code-session-id ?? $conversation}}"

// earlier values of the preset's session header, replaced on startup
var oldOpenCodeSessionHeaders = map[string]bool{
	"ai-route": true, // one fixed id for all traffic
	"{{header.x-opencode-session ?? $conversation}}": true,
}

// migrateOpenCodeSession replaces earlier OpenCode Go session header values
// (a fixed id for all traffic, then a template without Claude Code's id)
// with the current template; values the user wrote are left alone.
func (s *Store) migrateOpenCodeSession() error {
	rows, err := s.db.Query(`SELECT id, headers FROM providers WHERE vendor = 'opencode-go'`)
	if err != nil {
		return err
	}
	type upd struct {
		id      int64
		headers string
	}
	var updates []upd
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var h map[string]string
		_ = json.Unmarshal([]byte(raw), &h)
		for k, v := range h {
			if strings.EqualFold(k, "x-opencode-session") && oldOpenCodeSessionHeaders[v] {
				h[k] = OpenCodeSessionHeader
				updates = append(updates, upd{id, mustJSON(h)})
			}
		}
	}
	rows.Close()
	for _, u := range updates {
		if _, err := s.db.Exec(`UPDATE providers SET headers=? WHERE id=?`, u.headers, u.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(table, col, def string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, def))
	return err
}

func now() int64 { return time.Now().UnixMilli() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Reload rebuilds the in-memory snapshot from the database.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

func (s *Store) reloadLocked() error {
	providers, err := s.listProviders()
	if err != nil {
		return err
	}
	models, err := s.listModels()
	if err != nil {
		return err
	}
	keys, err := s.listKeys()
	if err != nil {
		return err
	}
	settings, err := s.getSettings()
	if err != nil {
		return err
	}
	alerts, err := s.getAlerts()
	if err != nil {
		return err
	}
	snap := &Snapshot{
		Alerts:      alerts,
		Providers:   map[string]*Provider{},
		Models:      models,
		modelByName: map[string]*Model{},
		Keys:        map[string]*APIKey{},
		Settings:    settings,
	}
	for _, p := range providers {
		snap.Providers[p.Prefix] = p
	}
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		snap.modelByName[m.Name] = m
	}
	// exact aliases (lower priority than names)
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		for _, a := range m.Aliases {
			if strings.Contains(a, "*") {
				continue
			}
			if _, exists := snap.modelByName[a]; !exists {
				snap.modelByName[a] = m
			}
		}
	}
	for _, k := range keys {
		snap.Keys[k.Key] = k
	}
	s.snap.Store(snap)
	return nil
}

// ---------- providers ----------

const providerCols = `id, prefix, name, openai_base_url, anthropic_base_url, api_key, headers, model_protocols, models, vendor, ua_mode, user_agent, timeout_seconds, prices, currency, body_rules, max_concurrency, first_token_timeout_seconds, health_check_seconds, health_check_url, drop_client_headers, enabled, remark, created_at, updated_at`

func scanProvider(sc interface{ Scan(...any) error }) (*Provider, error) {
	p := &Provider{}
	var headers, protos, models, prices, rules string
	var enabled, dropHeaders int
	if err := sc.Scan(&p.ID, &p.Prefix, &p.Name, &p.OpenAIBaseURL, &p.AnthropicBaseURL, &p.APIKey, &headers, &protos, &models, &p.Vendor, &p.UAMode, &p.UserAgent, &p.TimeoutSeconds, &prices, &p.Currency, &rules, &p.MaxConcurrency, &p.FirstTokenTimeoutSeconds, &p.HealthCheckSeconds, &p.HealthCheckURL, &dropHeaders, &enabled, &p.Remark, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Enabled, p.DropClientHeaders = enabled == 1, dropHeaders == 1
	_ = json.Unmarshal([]byte(headers), &p.Headers)
	_ = json.Unmarshal([]byte(protos), &p.ModelProtocols)
	_ = json.Unmarshal([]byte(models), &p.Models)
	_ = json.Unmarshal([]byte(prices), &p.Prices)
	if p.Prices == nil {
		p.Prices = map[string]Price{}
	}
	_ = json.Unmarshal([]byte(rules), &p.BodyRules)
	if p.BodyRules == nil {
		p.BodyRules = []BodyRule{}
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	if p.Headers == nil {
		p.Headers = map[string]string{}
	}
	if p.ModelProtocols == nil {
		p.ModelProtocols = map[string]string{}
	}
	return p, nil
}

func (s *Store) listProviders() ([]*Provider, error) {
	rows, err := s.db.Query(`SELECT ` + providerCols + ` FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListProviders() ([]*Provider, error) { return s.listProviders() }

func (s *Store) GetProvider(id int64) (*Provider, error) {
	p, err := scanProvider(s.db.QueryRow(`SELECT `+providerCols+` FROM providers WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func normalizeProvider(p *Provider) error {
	p.Prefix = strings.TrimSpace(p.Prefix)
	if p.Prefix == "" {
		return errors.New("prefix is required")
	}
	if strings.ContainsAny(p.Prefix, "/ \t") {
		return errors.New("prefix must not contain '/' or spaces")
	}
	p.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(p.OpenAIBaseURL), "/")
	p.AnthropicBaseURL = strings.TrimRight(strings.TrimSpace(p.AnthropicBaseURL), "/")
	if p.OpenAIBaseURL == "" && p.AnthropicBaseURL == "" {
		return errors.New("at least one of openai_base_url / anthropic_base_url is required")
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = 300
	}
	headers := make(map[string]string, len(p.Headers))
	for k, v := range p.Headers {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || strings.ContainsAny(k, " \t:\r\n{}") {
			return fmt.Errorf("invalid header name %q", k)
		}
		if _, err := hdrtpl.Parse(v); err != nil {
			return fmt.Errorf("header %s: %w", k, err)
		}
		headers[k] = v
	}
	p.Headers = headers
	if p.ModelProtocols == nil {
		p.ModelProtocols = map[string]string{}
	}
	p.Models = cleanList(p.Models)
	p.Vendor = strings.TrimSpace(p.Vendor)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	switch p.Currency {
	case "":
		p.Currency = CurrencyCNY
	case CurrencyCNY, CurrencyUSD:
	default:
		return fmt.Errorf("currency must be CNY or USD, got %q", p.Currency)
	}
	prices := make(map[string]Price, len(p.Prices))
	for k, v := range p.Prices {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if v.Input < 0 || v.Output < 0 || v.Cache != nil && *v.Cache < 0 {
			return fmt.Errorf("prices[%s] must not be negative", k)
		}
		prices[k] = v
	}
	p.Prices = prices
	rules := []BodyRule{}
	for i, r := range p.BodyRules {
		r.Model = strings.TrimSpace(r.Model)
		if r.Model == "" {
			return fmt.Errorf("body_rules[%d]: model is required", i)
		}
		switch r.When {
		case "", "stream", "nonstream":
		default:
			return fmt.Errorf("body_rules[%d]: when must be stream or nonstream, got %q", i, r.When)
		}
		switch r.Protocol {
		case "", "openai", "anthropic", "embeddings", "rerank":
		default:
			return fmt.Errorf("body_rules[%d]: unknown protocol %q", i, r.Protocol)
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(r.Set, &obj) != nil || obj == nil {
			return fmt.Errorf("body_rules[%d]: set must be a JSON object", i)
		}
		rules = append(rules, r)
	}
	p.BodyRules = rules
	if p.MaxConcurrency < 0 || p.FirstTokenTimeoutSeconds < 0 || p.HealthCheckSeconds < 0 {
		return errors.New("max_concurrency, first_token_timeout_seconds and health_check_seconds must not be negative")
	}
	if p.HealthCheckSeconds > 0 && p.HealthCheckSeconds < 5 {
		p.HealthCheckSeconds = 5
	}
	if p.HealthCheckURL = strings.TrimSpace(p.HealthCheckURL); p.HealthCheckURL != "" {
		if u, err := url.Parse(p.HealthCheckURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid health_check_url %q", p.HealthCheckURL)
		}
	}
	p.UserAgent = strings.TrimSpace(p.UserAgent)
	switch p.UAMode {
	case "", "passthrough":
		p.UAMode = "passthrough"
	case "platform": // always the gateway's own ai-route/<version>
	case "override":
		if p.UserAgent == "" {
			return errors.New("user_agent is required when ua_mode is override")
		}
	default:
		return fmt.Errorf("ua_mode must be passthrough, platform or override, got %q", p.UAMode)
	}
	for k, v := range p.ModelProtocols {
		if v != "openai" && v != "anthropic" {
			return fmt.Errorf("model_protocols[%s] must be openai or anthropic", k)
		}
		if v == "openai" && p.OpenAIBaseURL == "" || v == "anthropic" && p.AnthropicBaseURL == "" {
			return fmt.Errorf("model_protocols[%s]=%s but that base url is empty", k, v)
		}
	}
	return nil
}

// DerivePrefix builds a model prefix from the provider's API host
// (api.deepseek.com -> deepseek, coding.dashscope.aliyuncs.com -> dashscope),
// falling back to the name and finally to "provider".
func DerivePrefix(p *Provider) string {
	for _, raw := range []string{p.OpenAIBaseURL, p.AnthropicBaseURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" || net.ParseIP(u.Hostname()) != nil {
			continue
		}
		labels := strings.Split(strings.ToLower(u.Hostname()), ".")
		if len(labels) > 1 {
			labels = labels[:len(labels)-1] // drop TLD
		}
		for _, l := range labels {
			if genericHostLabel[l] {
				continue
			}
			if s := sanitizePrefix(l); s != "" {
				return s
			}
		}
	}
	if s := sanitizePrefix(p.Name); s != "" {
		return s
	}
	return "provider"
}

var genericHostLabel = map[string]bool{
	"api": true, "www": true, "open": true, "platform": true, "coding": true, "gateway": true,
	"localhost": true, "cn": true, "com": true, "ai": true, "io": true,
}

// sanitizePrefix lowercases and keeps [a-z0-9._-]; other runs become "-".
func sanitizePrefix(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-._")
}

func uniquePrefix(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if c := fmt.Sprintf("%s-%d", base, i); !taken[c] {
			return c
		}
	}
}

// ForcedProtocol returns the protocol forced for an upstream model, or "".
func (p *Provider) ForcedProtocol(model string) string {
	if v, ok := p.ModelProtocols[model]; ok {
		return v
	}
	for pat, v := range p.ModelProtocols {
		if strings.Contains(pat, "*") && globMatch(pat, model) {
			return v
		}
	}
	return ""
}

// PriceFor returns the unit price of an upstream model: an exact entry
// first, then the longest matching glob.
func (p *Provider) PriceFor(model string) (Price, bool) {
	if v, ok := p.Prices[model]; ok {
		return v, true
	}
	best, found := "", false
	for pat := range p.Prices {
		if strings.Contains(pat, "*") && globMatch(pat, model) && (!found || len(pat) > len(best) || len(pat) == len(best) && pat < best) {
			best, found = pat, true
		}
	}
	return p.Prices[best], found
}

// CreateProvider stores a new provider. The prefix is generated when empty
// (see DerivePrefix) and made unique with a numeric suffix (kimi, kimi-2 ...).
func (s *Store) CreateProvider(p *Provider) error {
	if strings.TrimSpace(p.Prefix) == "" {
		p.Prefix = DerivePrefix(p)
	} else {
		p.Prefix = sanitizePrefix(p.Prefix)
	}
	if err := normalizeProvider(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := map[string]bool{}
	for _, x := range s.Snapshot().Providers {
		taken[x.Prefix] = true
	}
	p.Prefix = uniquePrefix(p.Prefix, taken)
	t := now()
	res, err := s.db.Exec(`INSERT INTO providers (prefix, name, openai_base_url, anthropic_base_url, api_key, headers, model_protocols, models, vendor, ua_mode, user_agent, timeout_seconds, prices, currency, body_rules, max_concurrency, first_token_timeout_seconds, health_check_seconds, health_check_url, drop_client_headers, enabled, remark, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Prefix, p.Name, p.OpenAIBaseURL, p.AnthropicBaseURL, p.APIKey, mustJSON(p.Headers), mustJSON(p.ModelProtocols), mustJSON(p.Models), p.Vendor, p.UAMode, p.UserAgent, p.TimeoutSeconds, mustJSON(p.Prices), p.Currency, mustJSON(p.BodyRules), p.MaxConcurrency, p.FirstTokenTimeoutSeconds, p.HealthCheckSeconds, p.HealthCheckURL, b2i(p.DropClientHeaders), b2i(p.Enabled), p.Remark, t, t)
	if err != nil {
		return friendlyErr(err)
	}
	p.ID, _ = res.LastInsertId()
	p.CreatedAt, p.UpdatedAt = t, t
	return s.reloadLocked()
}

// UpdateProvider updates a provider. An empty APIKey keeps the stored key.
// If the prefix changes, model targets referencing the old prefix are rewritten.
func (s *Store) UpdateProvider(p *Provider) error {
	if err := normalizeProvider(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := scanProvider(s.db.QueryRow(`SELECT `+providerCols+` FROM providers WHERE id=?`, p.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if p.APIKey == "" {
		p.APIKey = old.APIKey
	}
	if p.Prefix == "" {
		p.Prefix = old.Prefix
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	if _, err := tx.Exec(`UPDATE providers SET prefix=?, name=?, openai_base_url=?, anthropic_base_url=?, api_key=?, headers=?, model_protocols=?, models=?, vendor=?, ua_mode=?, user_agent=?, timeout_seconds=?, prices=?, currency=?, body_rules=?, max_concurrency=?, first_token_timeout_seconds=?, health_check_seconds=?, health_check_url=?, drop_client_headers=?, enabled=?, remark=?, updated_at=? WHERE id=?`,
		p.Prefix, p.Name, p.OpenAIBaseURL, p.AnthropicBaseURL, p.APIKey, mustJSON(p.Headers), mustJSON(p.ModelProtocols), mustJSON(p.Models), p.Vendor, p.UAMode, p.UserAgent, p.TimeoutSeconds, mustJSON(p.Prices), p.Currency, mustJSON(p.BodyRules), p.MaxConcurrency, p.FirstTokenTimeoutSeconds, p.HealthCheckSeconds, p.HealthCheckURL, b2i(p.DropClientHeaders), b2i(p.Enabled), p.Remark, t, p.ID); err != nil {
		return friendlyErr(err)
	}
	if old.Prefix != p.Prefix {
		if err := renameTargetPrefix(tx, old.Prefix, p.Prefix); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reloadLocked()
}

func renameTargetPrefix(tx *sql.Tx, oldPrefix, newPrefix string) error {
	rows, err := tx.Query(`SELECT id, targets FROM models`)
	if err != nil {
		return err
	}
	type upd struct {
		id      int64
		targets string
	}
	var updates []upd
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var targets []string
		_ = json.Unmarshal([]byte(raw), &targets)
		changed := false
		for i, t := range targets {
			if renamed, ok := renameEntryPrefix(t, oldPrefix, newPrefix); ok {
				targets[i] = renamed
				changed = true
			}
		}
		if changed {
			updates = append(updates, upd{id, mustJSON(targets)})
		}
	}
	rows.Close()
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE models SET targets=? WHERE id=?`, u.targets, u.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteProvider(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// ---------- models ----------

const modelCols = `id, name, aliases, tags, targets, enabled, description, created_at, updated_at`

func scanModel(sc interface{ Scan(...any) error }) (*Model, error) {
	m := &Model{}
	var aliases, tags, targets string
	var enabled int
	if err := sc.Scan(&m.ID, &m.Name, &aliases, &tags, &targets, &enabled, &m.Description, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(aliases), &m.Aliases)
	_ = json.Unmarshal([]byte(targets), &m.Targets)
	_ = json.Unmarshal([]byte(tags), &m.Tags)
	if m.Tags == nil {
		m.Tags = []string{}
	}
	if m.Aliases == nil {
		m.Aliases = []string{}
	}
	if m.Targets == nil {
		m.Targets = []string{}
	}
	return m, nil
}

func (s *Store) listModels() ([]*Model, error) {
	rows, err := s.db.Query(`SELECT ` + modelCols + ` FROM models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListModels() ([]*Model, error) { return s.listModels() }

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func normalizeModel(m *Model) error {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return errors.New("name is required")
	}
	m.Aliases = cleanList(m.Aliases)
	m.Tags = cleanList(m.Tags)
	for i, t := range m.Targets {
		norm, err := normalizeTargetEntry(t)
		if err != nil {
			return err
		}
		m.Targets[i] = norm
	}
	m.Targets = cleanList(m.Targets)
	return nil
}

func (s *Store) CreateModel(m *Model) error {
	if err := normalizeModel(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	res, err := s.db.Exec(`INSERT INTO models (name, aliases, tags, targets, enabled, description, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		m.Name, mustJSON(m.Aliases), mustJSON(m.Tags), mustJSON(m.Targets), b2i(m.Enabled), m.Description, t, t)
	if err != nil {
		return friendlyErr(err)
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt, m.UpdatedAt = t, t
	return s.reloadLocked()
}

func (s *Store) UpdateModel(m *Model) error {
	if err := normalizeModel(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE models SET name=?, aliases=?, tags=?, targets=?, enabled=?, description=?, updated_at=? WHERE id=?`,
		m.Name, mustJSON(m.Aliases), mustJSON(m.Tags), mustJSON(m.Targets), b2i(m.Enabled), m.Description, now(), m.ID)
	if err != nil {
		return friendlyErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

func (s *Store) DeleteModel(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM models WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// ---------- api keys ----------

const keyCols = `id, name, key, enabled, allowed_models, expires_at, monthly_budget, rpm, tpm, created_at, last_used_at`

func scanKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	k := &APIKey{}
	var allowed string
	var enabled int
	if err := sc.Scan(&k.ID, &k.Name, &k.Key, &enabled, &allowed, &k.ExpiresAt, &k.MonthlyBudget, &k.RPM, &k.TPM, &k.CreatedAt, &k.LastUsedAt); err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	_ = json.Unmarshal([]byte(allowed), &k.AllowedModels)
	if k.AllowedModels == nil {
		k.AllowedModels = []string{}
	}
	return k, nil
}

func (s *Store) listKeys() ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT ` + keyCols + ` FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) ListKeys() ([]*APIKey, error) { return s.listKeys() }

func RandomToken(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// NewAPIKeyValue returns a fresh client key. Keys are always generated by
// the platform; custom values are not accepted.
func NewAPIKeyValue() string { return RandomToken("sk-route-", 24) }

func normalizeKey(k *APIKey) error {
	k.Name = strings.TrimSpace(k.Name)
	k.AllowedModels = cleanList(k.AllowedModels)
	if k.MonthlyBudget < 0 || k.RPM < 0 || k.TPM < 0 {
		return errors.New("monthly_budget, rpm and tpm must not be negative")
	}
	return nil
}

func (s *Store) CreateKey(k *APIKey) error {
	k.Key = NewAPIKeyValue()
	if err := normalizeKey(k); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	res, err := s.db.Exec(`INSERT INTO api_keys (name, key, enabled, allowed_models, expires_at, monthly_budget, rpm, tpm, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		k.Name, k.Key, b2i(k.Enabled), mustJSON(k.AllowedModels), k.ExpiresAt, k.MonthlyBudget, k.RPM, k.TPM, t)
	if err != nil {
		return friendlyErr(err)
	}
	k.ID, _ = res.LastInsertId()
	k.CreatedAt = t
	return s.reloadLocked()
}

// UpdateKey updates name/enabled/allowed models/expiry/limits. The key value
// itself never changes here; use RotateKey.
func (s *Store) UpdateKey(k *APIKey) error {
	if err := normalizeKey(k); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE api_keys SET name=?, enabled=?, allowed_models=?, expires_at=?, monthly_budget=?, rpm=?, tpm=? WHERE id=?`,
		k.Name, b2i(k.Enabled), mustJSON(k.AllowedModels), k.ExpiresAt, k.MonthlyBudget, k.RPM, k.TPM, k.ID)
	if err != nil {
		return friendlyErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// RotateKey replaces a key's value with a newly generated one; the old value
// stops working immediately.
func (s *Store) RotateKey(id int64) (string, error) {
	v := NewAPIKeyValue()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE api_keys SET key=? WHERE id=?`, v, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return v, s.reloadLocked()
}

func (s *Store) DeleteKey(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.reloadLocked()
}

// TouchKey records last usage time (best effort, not reflected in snapshot).
func (s *Store) TouchKey(id int64) {
	_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, now(), id)
}

// ---------- settings ----------

func (s *Store) getSettings() (Settings, error) {
	st := DefaultSettings()
	var raw string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k='settings'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	} else if err != nil {
		return st, err
	}
	_ = json.Unmarshal([]byte(raw), &st)
	return st, nil
}

func (s *Store) GetSettings() Settings { return s.Snapshot().Settings }

func (s *Store) UpdateSettings(st Settings) error {
	st = normalizeSettings(st)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`INSERT INTO settings (k, v) VALUES ('settings', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, mustJSON(st)); err != nil {
		return err
	}
	return s.reloadLocked()
}

func normalizeSettings(st Settings) Settings {
	d := DefaultSettings()
	if st.MaxRetries < 0 {
		st.MaxRetries = 0
	}
	if st.MaxRetries > 10 {
		st.MaxRetries = 10
	}
	if st.RetryBackoffMs <= 0 {
		st.RetryBackoffMs = d.RetryBackoffMs
	}
	if st.FailureThreshold <= 0 {
		st.FailureThreshold = d.FailureThreshold
	}
	if st.CooldownSeconds <= 0 {
		st.CooldownSeconds = d.CooldownSeconds
	}
	if st.MaxCooldownSeconds < st.CooldownSeconds {
		st.MaxCooldownSeconds = st.CooldownSeconds
	}
	if st.LogRetentionDays <= 0 {
		st.LogRetentionDays = d.LogRetentionDays
	}
	if st.DefaultMaxTokens <= 0 {
		st.DefaultMaxTokens = d.DefaultMaxTokens
	}
	if st.Currency = strings.ToUpper(strings.TrimSpace(st.Currency)); st.Currency != CurrencyUSD {
		st.Currency = CurrencyCNY
	}
	if st.USDToCNY <= 0 {
		st.USDToCNY = d.USDToCNY
	}
	if st.QueueTimeoutSeconds < 0 {
		st.QueueTimeoutSeconds = 0
	}
	return st
}

// GetKV / SetKV store small internal values (e.g. generated admin token).
func (s *Store) GetKV(k string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ---------- export / import ----------

type Export struct {
	Version   int         `json:"version"`
	Providers []*Provider `json:"providers"`
	Models    []*Model    `json:"models"`
	Keys      []*APIKey   `json:"api_keys"`
	Settings  Settings    `json:"settings"`
	// Alerts is nil in exports made before alerts existed; importing such a
	// file keeps the current alert configuration.
	Alerts *AlertConfig `json:"alerts,omitempty"`
}

func (s *Store) Export() (*Export, error) {
	p, err := s.listProviders()
	if err != nil {
		return nil, err
	}
	m, err := s.listModels()
	if err != nil {
		return nil, err
	}
	k, err := s.listKeys()
	if err != nil {
		return nil, err
	}
	alerts := s.GetAlerts()
	return &Export{Version: 1, Providers: p, Models: m, Keys: k, Settings: s.GetSettings(), Alerts: &alerts}, nil
}

// Import replaces the whole configuration (providers, models, keys, settings).
func (s *Store) Import(e *Export) error {
	for _, p := range e.Providers {
		if err := normalizeProvider(p); err != nil {
			return fmt.Errorf("provider %q: %w", p.Prefix, err)
		}
	}
	for _, m := range e.Models {
		if err := normalizeModel(m); err != nil {
			return fmt.Errorf("model %q: %w", m.Name, err)
		}
	}
	if e.Alerts != nil {
		if err := normalizeAlerts(e.Alerts); err != nil {
			return fmt.Errorf("alerts: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM providers`, `DELETE FROM models`, `DELETE FROM api_keys`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	t := now()
	for _, p := range e.Providers {
		if _, err := tx.Exec(`INSERT INTO providers (prefix, name, openai_base_url, anthropic_base_url, api_key, headers, model_protocols, models, vendor, ua_mode, user_agent, timeout_seconds, prices, currency, body_rules, max_concurrency, first_token_timeout_seconds, health_check_seconds, health_check_url, drop_client_headers, enabled, remark, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.Prefix, p.Name, p.OpenAIBaseURL, p.AnthropicBaseURL, p.APIKey, mustJSON(p.Headers), mustJSON(p.ModelProtocols), mustJSON(p.Models), p.Vendor, p.UAMode, p.UserAgent, p.TimeoutSeconds, mustJSON(p.Prices), p.Currency, mustJSON(p.BodyRules), p.MaxConcurrency, p.FirstTokenTimeoutSeconds, p.HealthCheckSeconds, p.HealthCheckURL, b2i(p.DropClientHeaders), b2i(p.Enabled), p.Remark, t, t); err != nil {
			return friendlyErr(err)
		}
	}
	for _, m := range e.Models {
		if _, err := tx.Exec(`INSERT INTO models (name, aliases, tags, targets, enabled, description, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			m.Name, mustJSON(m.Aliases), mustJSON(m.Tags), mustJSON(m.Targets), b2i(m.Enabled), m.Description, t, t); err != nil {
			return friendlyErr(err)
		}
	}
	for _, k := range e.Keys {
		if k.Key == "" {
			continue
		}
		if err := normalizeKey(k); err != nil {
			return fmt.Errorf("api key %q: %w", k.Name, err)
		}
		// keep the id: request logs (and so monthly budgets) refer to it
		var id any
		if k.ID > 0 {
			id = k.ID
		}
		if _, err := tx.Exec(`INSERT INTO api_keys (id, name, key, enabled, allowed_models, expires_at, monthly_budget, rpm, tpm, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, k.Name, k.Key, b2i(k.Enabled), mustJSON(k.AllowedModels), k.ExpiresAt, k.MonthlyBudget, k.RPM, k.TPM, t); err != nil {
			return friendlyErr(err)
		}
	}
	if e.Settings != (Settings{}) {
		if _, err := tx.Exec(`INSERT INTO settings (k, v) VALUES ('settings', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, mustJSON(normalizeSettings(e.Settings))); err != nil {
			return err
		}
	}
	if e.Alerts != nil {
		if _, err := tx.Exec(`INSERT INTO settings (k, v) VALUES ('alerts', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, mustJSON(e.Alerts)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reloadLocked()
}

func friendlyErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		field := err.Error()[strings.LastIndex(err.Error(), ".")+1:]
		return fmt.Errorf("%s already exists", strings.TrimRight(field, ")"))
	}
	return err
}
