package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testKeyA = "0123456789abcdef-key-a"
	testKeyB = "0123456789abcdef-key-b"
)

func rawProviderKey(t *testing.T, s *Store, prefix string) string {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`SELECT api_key FROM providers WHERE prefix=?`, prefix).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rawAlerts(t *testing.T, s *Store) string {
	t.Helper()
	v, _ := s.GetKV("alerts")
	return v
}

func TestSecretsSealedAtRest(t *testing.T) {
	loc := testLoc(t)
	// written before encryption was turned on
	s, err := openLoc(loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProvider(&Provider{Prefix: "p", OpenAIBaseURL: "https://x/v1", APIKey: "sk-upstream-1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAlerts(AlertConfig{Webhooks: []Webhook{{Type: "feishu", URL: "https://open.feishu.cn/hook/tok", Secret: "sign-secret", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// SECRET_KEY set: existing values are sealed on open, the snapshot is plain
	s, err = openLoc(loc, WithSecretKey(testKeyA, ""))
	if err != nil {
		t.Fatal(err)
	}
	if raw := rawProviderKey(t, s, "p"); !IsSealed(raw) || strings.Contains(raw, "sk-upstream") {
		t.Fatalf("api key not sealed: %q", raw)
	}
	if raw := rawAlerts(t, s); strings.Contains(raw, "tok") || strings.Contains(raw, "sign-secret") {
		t.Fatalf("webhook not sealed: %s", raw)
	}
	if got := s.Snapshot().Providers["p"].APIKey; got != "sk-upstream-1" {
		t.Fatalf("snapshot key %q", got)
	}
	if h := s.GetAlerts().Webhooks[0]; h.URL != "https://open.feishu.cn/hook/tok" || h.Secret != "sign-secret" {
		t.Fatalf("snapshot webhook %+v", h)
	}
	// new writes are sealed too, and UpdateProvider keeps the stored key
	p := *s.Snapshot().Providers["p"]
	p.APIKey, p.Name = "", "renamed"
	if err := s.UpdateProvider(&p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProvider(&Provider{Prefix: "q", OpenAIBaseURL: "https://y/v1", APIKey: "sk-upstream-2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !IsSealed(rawProviderKey(t, s, "q")) || s.Snapshot().Providers["p"].APIKey != "sk-upstream-1" {
		t.Fatal("write path did not seal or lost the key")
	}
	// export carries sealed values and imports back
	exp, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range exp.Providers {
		if !IsSealed(x.APIKey) {
			t.Fatalf("export leaks %s key", x.Prefix)
		}
	}
	if err := s.Import(exp); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Providers["q"].APIKey != "sk-upstream-2" {
		t.Fatal("import of a sealed export lost the key")
	}
	s.Close()

	// without the key the database does not open
	if _, err := openLoc(loc); !errors.Is(err, ErrSecretKeyMissing) {
		t.Fatalf("open without key: %v", err)
	}
	// with the wrong key neither
	if _, err := openLoc(loc, WithSecretKey(testKeyB, "")); err == nil || !strings.Contains(err.Error(), "wrong key") {
		t.Fatalf("open with wrong key: %v", err)
	}

	// rotation: new key, old one as previous
	s, err = openLoc(loc, WithSecretKey(testKeyB, testKeyA))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openLoc(loc, WithSecretKey(testKeyB, ""))
	if err != nil {
		t.Fatalf("rotated values do not open with the new key alone: %v", err)
	}
	s.Close()

	// turning encryption off: previous key only
	s, err = openLoc(loc, WithSecretKey("", testKeyB))
	if err != nil {
		t.Fatal(err)
	}
	if raw := rawProviderKey(t, s, "p"); raw != "sk-upstream-1" {
		t.Fatalf("not decrypted back: %q", raw)
	}
	s.Close()
	s, err = openLoc(loc)
	if err != nil {
		t.Fatal(err)
	}
	if h := s.GetAlerts().Webhooks[0]; h.Secret != "sign-secret" {
		t.Fatalf("webhook after decrypt: %+v", h)
	}
	s.Close()
}

func TestSecretKeyTooShort(t *testing.T) {
	if _, err := openLoc(testLoc(t), WithSecretKey("short", "")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestCapturesSealedAndCapped(t *testing.T) {
	s, err := openLoc(testLoc(t), WithSecretKey(testKeyA, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &Capture{RequestID: "req_1", CreatedAt: now(), CaptureData: &CaptureData{ClientRequest: `{"secret prompt":1}`}}
	if err := s.SaveCapture(c); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT data FROM captures WHERE id=?`, c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !IsSealed(raw) || strings.Contains(raw, "secret prompt") {
		t.Fatalf("capture not sealed: %q", raw)
	}
	got, err := s.GetCapture(c.ID)
	if err != nil || got.ClientRequest != `{"secret prompt":1}` {
		t.Fatalf("capture %+v %v", got, err)
	}
	for i := 0; i < maxCaptures+5; i++ {
		if err := s.SaveCapture(&Capture{CreatedAt: now(), CaptureData: &CaptureData{}}); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := s.ListCaptures()
	if len(list) != maxCaptures {
		t.Fatalf("%d captures kept, want %d", len(list), maxCaptures)
	}
	if err := s.CreateCaptureRule(&CaptureRule{Total: MaxCaptureCount + 1}, time.Hour); err == nil {
		t.Fatal("oversized rule accepted")
	}
}
