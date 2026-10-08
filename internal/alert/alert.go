// Package alert pushes failure notifications to chat-bot webhooks
// (Feishu / Lark, DingTalk, WeCom or any endpoint accepting JSON).
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-route/internal/store"
)

// Event kinds; also the first part of an alert's dedupe key.
const (
	EventAuthFailure  = "auth_failure"
	EventAllFailed    = "all_failed"
	EventLongCooldown = "long_cooldown"
	EventTest         = "test"
)

// Alert is one notification.
type Alert struct {
	Event   string
	Subject string // what it is about (provider prefix, model name ...); dedupe key with Event
	Title   string
	Text    string
}

type Notifier struct {
	config func() store.AlertConfig
	client *http.Client
	now    func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup
}

func New(config func() store.AlertConfig) *Notifier {
	return &Notifier{
		config: config,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		now:    time.Now,
		last:   map[string]time.Time{},
	}
}

// Enabled reports whether alerts of this event are wanted at all.
func (n *Notifier) Enabled(event string) bool {
	c := n.config()
	if !hasEnabledHook(c) {
		return false
	}
	switch event {
	case EventAuthFailure:
		return c.OnAuthFailure
	case EventAllFailed:
		return c.OnAllFailed
	case EventLongCooldown:
		return c.OnLongCooldown
	}
	return false
}

func hasEnabledHook(c store.AlertConfig) bool {
	for _, h := range c.Webhooks {
		if h.Enabled {
			return true
		}
	}
	return false
}

// LongCooldown is the cooldown length that triggers EventLongCooldown.
func (n *Notifier) LongCooldown() time.Duration {
	return time.Duration(n.config().LongCooldownMinutes) * time.Minute
}

// Notify sends the alert to every enabled webhook in the background, unless
// the same event/subject was sent within the silence window.
func (n *Notifier) Notify(a Alert) {
	if !n.Enabled(a.Event) {
		return
	}
	c := n.config()
	key := a.Event + "|" + a.Subject
	now := n.now()
	n.mu.Lock()
	if last, ok := n.last[key]; ok && now.Sub(last) < time.Duration(c.SilenceMinutes)*time.Minute {
		n.mu.Unlock()
		return
	}
	n.last[key] = now
	n.mu.Unlock()
	for _, h := range c.Webhooks {
		if !h.Enabled {
			continue
		}
		n.wg.Add(1)
		go func(h store.Webhook) {
			defer n.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := n.Send(ctx, h, a); err != nil {
				log.Printf("alert webhook %s (%s): %v", h.Name, h.Type, err)
			}
		}(h)
	}
}

// Wait blocks until alerts sent so far have been delivered (tests, shutdown).
func (n *Notifier) Wait() { n.wg.Wait() }

// Send delivers one alert to one webhook synchronously.
func (n *Notifier) Send(ctx context.Context, h store.Webhook, a Alert) error {
	text := "[AI Route] " + a.Title + "\n" + a.Text + "\n时间：" + n.now().Format("2006-01-02 15:04:05")
	target := h.URL
	var body any
	switch h.Type {
	case store.WebhookFeishu:
		m := map[string]any{"msg_type": "text", "content": map[string]any{"text": text}}
		if h.Secret != "" {
			ts := strconv.FormatInt(n.now().Unix(), 10)
			m["timestamp"], m["sign"] = ts, feishuSign(ts, h.Secret)
		}
		body = m
	case store.WebhookDingTalk:
		body = map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}
		if h.Secret != "" {
			ts := strconv.FormatInt(n.now().UnixMilli(), 10)
			sep := "?"
			if strings.Contains(target, "?") {
				sep = "&"
			}
			target += sep + "timestamp=" + ts + "&sign=" + url.QueryEscape(dingTalkSign(ts, h.Secret))
		}
	case store.WebhookWeCom:
		body = map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}
	default:
		body = map[string]any{"event": a.Event, "subject": a.Subject, "title": a.Title, "text": a.Text, "time": n.now().Format(time.RFC3339)}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// the bot platforms answer 200 with an error code in the body
	var res struct {
		Code    *int   `json:"code"`    // feishu
		Msg     string `json:"msg"`     // feishu
		ErrCode *int   `json:"errcode"` // dingtalk / wecom
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &res) == nil {
		if res.Code != nil && *res.Code != 0 {
			return fmt.Errorf("code %d: %s", *res.Code, res.Msg)
		}
		if res.ErrCode != nil && *res.ErrCode != 0 {
			return fmt.Errorf("errcode %d: %s", *res.ErrCode, res.ErrMsg)
		}
	}
	return nil
}

// feishuSign: HMAC-SHA256 keyed with "timestamp\nsecret" over an empty
// message, base64 encoded.
func feishuSign(ts, secret string) string {
	h := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// dingTalkSign: HMAC-SHA256 keyed with the secret over "timestamp\nsecret",
// base64 encoded (URL-escaped by the caller).
func dingTalkSign(ts, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts + "\n" + secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
