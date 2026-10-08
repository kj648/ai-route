package alert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-route/internal/store"
)

type received struct {
	query map[string]string
	body  map[string]any
}

type sink struct {
	mu    sync.Mutex
	reqs  []received
	reply string
	srv   *httptest.Server
}

func newSink(t *testing.T, reply string) *sink {
	s := &sink{reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, received{q, body})
		s.mu.Unlock()
		_, _ = w.Write([]byte(s.reply))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) all() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.reqs...)
}

var fixed = time.Date(2026, 10, 8, 9, 30, 0, 0, time.Local)

func notifier(c store.AlertConfig) *Notifier {
	n := New(func() store.AlertConfig { return c })
	n.now = func() time.Time { return fixed }
	return n
}

func hmacB64(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestSendFormats(t *testing.T) {
	a := Alert{Event: EventAuthFailure, Subject: "kimi", Title: "供应商 kimi 欠费", Text: "detail"}
	ctx := context.Background()

	t.Run("feishu signed", func(t *testing.T) {
		s := newSink(t, `{"code":0,"msg":"success"}`)
		n := notifier(store.AlertConfig{})
		if err := n.Send(ctx, store.Webhook{Type: store.WebhookFeishu, URL: s.srv.URL, Secret: "sec"}, a); err != nil {
			t.Fatal(err)
		}
		b := s.all()[0].body
		ts := b["timestamp"].(string)
		text := b["content"].(map[string]any)["text"].(string)
		if b["msg_type"] != "text" || !strings.HasPrefix(text, "[AI Route] 供应商 kimi 欠费\ndetail\n时间：2026-10-08 09:30:00") {
			t.Fatalf("body: %v", b)
		}
		if ts != strconv.FormatInt(fixed.Unix(), 10) || b["sign"] != hmacB64(ts+"\nsec", "") {
			t.Fatalf("signature: ts=%s sign=%v", ts, b["sign"])
		}
	})
	t.Run("dingtalk signed", func(t *testing.T) {
		s := newSink(t, `{"errcode":0,"errmsg":"ok"}`)
		n := notifier(store.AlertConfig{})
		if err := n.Send(ctx, store.Webhook{Type: store.WebhookDingTalk, URL: s.srv.URL + "/robot/send?access_token=abc", Secret: "SECx"}, a); err != nil {
			t.Fatal(err)
		}
		r := s.all()[0]
		if r.query["access_token"] != "abc" || r.query["sign"] != hmacB64("SECx", r.query["timestamp"]+"\nSECx") {
			t.Fatalf("query: %v", r.query)
		}
		if r.body["msgtype"] != "text" || !strings.Contains(r.body["text"].(map[string]any)["content"].(string), "欠费") {
			t.Fatalf("body: %v", r.body)
		}
	})
	t.Run("wecom", func(t *testing.T) {
		s := newSink(t, `{"errcode":0,"errmsg":"ok"}`)
		n := notifier(store.AlertConfig{})
		if err := n.Send(ctx, store.Webhook{Type: store.WebhookWeCom, URL: s.srv.URL}, a); err != nil {
			t.Fatal(err)
		}
		if b := s.all()[0].body; b["msgtype"] != "text" || b["text"].(map[string]any)["content"] == nil {
			t.Fatalf("body: %v", b)
		}
	})
	t.Run("generic", func(t *testing.T) {
		s := newSink(t, `ok`)
		n := notifier(store.AlertConfig{})
		if err := n.Send(ctx, store.Webhook{Type: store.WebhookGeneric, URL: s.srv.URL}, a); err != nil {
			t.Fatal(err)
		}
		if b := s.all()[0].body; b["event"] != EventAuthFailure || b["subject"] != "kimi" || b["text"] != "detail" {
			t.Fatalf("body: %v", b)
		}
	})
	t.Run("errors in a 200 body", func(t *testing.T) {
		for typ, reply := range map[string]string{
			store.WebhookFeishu:   `{"code":19021,"msg":"sign match fail"}`,
			store.WebhookDingTalk: `{"errcode":310000,"errmsg":"keywords not in content"}`,
		} {
			s := newSink(t, reply)
			err := notifier(store.AlertConfig{}).Send(ctx, store.Webhook{Type: typ, URL: s.srv.URL}, a)
			if err == nil || !strings.Contains(err.Error(), "19021") && !strings.Contains(err.Error(), "310000") {
				t.Errorf("%s: %v", typ, err)
			}
		}
	})
}

func TestNotifySilenceAndSwitches(t *testing.T) {
	s := newSink(t, `ok`)
	c := store.DefaultAlertConfig()
	c.OnLongCooldown = false
	c.Webhooks = []store.Webhook{
		{Type: store.WebhookGeneric, URL: s.srv.URL, Enabled: true},
		{Type: store.WebhookGeneric, URL: s.srv.URL + "/off", Enabled: false},
	}
	n := notifier(c)
	now := fixed
	n.now = func() time.Time { return now }

	n.Notify(Alert{Event: EventAuthFailure, Subject: "kimi", Title: "t"})
	n.Notify(Alert{Event: EventAuthFailure, Subject: "kimi", Title: "t"}) // silenced
	n.Notify(Alert{Event: EventAuthFailure, Subject: "glm", Title: "t"})  // other subject
	n.Notify(Alert{Event: EventLongCooldown, Subject: "kimi", Title: "t"})
	n.Wait()
	if got := len(s.all()); got != 2 {
		t.Fatalf("want 2 deliveries (disabled hook, silenced repeat and disabled event skipped), got %d", got)
	}
	now = now.Add(31 * time.Minute)
	n.Notify(Alert{Event: EventAuthFailure, Subject: "kimi", Title: "t"})
	n.Wait()
	if got := len(s.all()); got != 3 {
		t.Fatalf("after the silence window: %d", got)
	}

	if notifier(store.DefaultAlertConfig()).Enabled(EventAllFailed) {
		t.Fatal("enabled without any webhook")
	}
}

func TestSendEnglishTimeLabel(t *testing.T) {
	s := newSink(t, `{"errcode":0}`)
	n := notifier(store.AlertConfig{Language: "en"})
	if err := n.Send(context.Background(), store.Webhook{Type: store.WebhookWeCom, URL: s.srv.URL}, Alert{Title: "T", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if c := s.all()[0].body["text"].(map[string]any)["content"].(string); !strings.Contains(c, "\nTime: 2026-10-08 09:30:00") {
		t.Fatalf("content: %q", c)
	}
}
