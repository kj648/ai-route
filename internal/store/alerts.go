package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Webhook is a chat-bot endpoint that receives alerts.
type Webhook struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // feishu | dingtalk | wecom | generic
	URL     string `json:"url"`
	Secret  string `json:"secret"` // signing secret (feishu / dingtalk), optional
	Enabled bool   `json:"enabled"`
}

const (
	WebhookFeishu   = "feishu"
	WebhookDingTalk = "dingtalk"
	WebhookWeCom    = "wecom"
	WebhookGeneric  = "generic"
)

// AlertConfig decides which failures are pushed to the webhooks. It is kept
// apart from Settings so Settings stays a comparable value.
type AlertConfig struct {
	Webhooks []Webhook `json:"webhooks"`
	// OnAuthFailure: an upstream answered 401 / 402 (key invalid, arrears).
	OnAuthFailure bool `json:"on_auth_failure"`
	// OnAllFailed: every target of a public model failed for a request.
	OnAllFailed bool `json:"on_all_failed"`
	// OnLongCooldown: a provider or target was cooled down for at least
	// LongCooldownMinutes in one go.
	OnLongCooldown      bool `json:"on_long_cooldown"`
	LongCooldownMinutes int  `json:"long_cooldown_minutes"`
	// OnHealthCheck: a provider with active health checks goes down or
	// recovers.
	OnHealthCheck bool `json:"on_health_check"`
	// SilenceMinutes: the same alert (same event and subject) is sent at
	// most once per this many minutes.
	SilenceMinutes int `json:"silence_minutes"`
}

func DefaultAlertConfig() AlertConfig {
	return AlertConfig{
		Webhooks:            []Webhook{},
		OnAuthFailure:       true,
		OnAllFailed:         true,
		OnLongCooldown:      true,
		LongCooldownMinutes: 10,
		OnHealthCheck:       true,
		SilenceMinutes:      30,
	}
}

func normalizeAlerts(a *AlertConfig) error {
	d := DefaultAlertConfig()
	if a.LongCooldownMinutes <= 0 {
		a.LongCooldownMinutes = d.LongCooldownMinutes
	}
	if a.SilenceMinutes <= 0 {
		a.SilenceMinutes = d.SilenceMinutes
	}
	hooks := []Webhook{}
	for i, h := range a.Webhooks {
		h.Name = strings.TrimSpace(h.Name)
		h.URL = strings.TrimSpace(h.URL)
		h.Secret = strings.TrimSpace(h.Secret)
		h.Type = strings.ToLower(strings.TrimSpace(h.Type))
		if h.URL == "" {
			continue
		}
		switch h.Type {
		case WebhookFeishu, WebhookDingTalk, WebhookWeCom, WebhookGeneric:
		case "":
			h.Type = WebhookGeneric
		default:
			return fmt.Errorf("webhooks[%d]: unknown type %q", i, h.Type)
		}
		if u, err := url.Parse(h.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("webhooks[%d]: invalid url %q", i, h.URL)
		}
		hooks = append(hooks, h)
	}
	a.Webhooks = hooks
	return nil
}

// ValidateAlerts normalizes an alert configuration in place.
func ValidateAlerts(a *AlertConfig) error { return normalizeAlerts(a) }

func (s *Store) getAlerts() (AlertConfig, error) {
	a := DefaultAlertConfig()
	var raw string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k='alerts'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	} else if err != nil {
		return a, err
	}
	_ = json.Unmarshal([]byte(raw), &a)
	if a.Webhooks == nil {
		a.Webhooks = []Webhook{}
	}
	return a, nil
}

func (s *Store) GetAlerts() AlertConfig { return s.Snapshot().Alerts }

func (s *Store) UpdateAlerts(a AlertConfig) error {
	if err := normalizeAlerts(&a); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.SetKV("alerts", mustJSON(a)); err != nil {
		return err
	}
	return s.reloadLocked()
}
