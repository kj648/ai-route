package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-route/internal/store"
)

// userKeys creates a user with limits and n keys, returning the key values.
func (h *harness) userKeys(u store.User, n int) (*store.User, []string) {
	h.t.Helper()
	u.Enabled, u.Password = true, "secret-pass"
	if u.Username == "" {
		u.Username = "alice"
	}
	mustNil(h.t, h.st.CreateUser(&u))
	var keys []string
	for i := 0; i < n; i++ {
		v, err := h.st.CreateUserKey(&u, &store.APIKey{Name: "k", Enabled: true})
		mustNil(h.t, err)
		keys = append(keys, v)
	}
	return &u, keys
}

func TestUserKeysShareTheAccountLimits(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	_, keys := h.userKeys(store.User{RPM: 2}, 2)
	if code, _, body := h.postAs(keys[0], "/v1/chat/completions", oaReq("coder", false)); code != 200 {
		t.Fatalf("first: %d %s", code, body)
	}
	if code, _, body := h.postAs(keys[1], "/v1/chat/completions", oaReq("coder", false)); code != 200 {
		t.Fatalf("second: %d %s", code, body)
	}
	// a third key would not help: the account's RPM covers all of them
	code, _, body := h.postAs(keys[0], "/v1/chat/completions", oaReq("coder", false))
	if code != 429 || !strings.Contains(body, "your account") {
		t.Fatalf("over account RPM: %d %s", code, body)
	}
	l := h.logs()[0]
	if l.UserID == 0 || l.HTTPStatus != 429 {
		t.Fatalf("log %+v", l)
	}
}

func TestUserBudgetOverAllKeys(t *testing.T) {
	h := newHarness(t)
	oa := *h.st.Snapshot().Providers["oa"]
	oa.APIKey, oa.Prices = "", map[string]store.Price{"*": {Input: 100_000, Output: 0}} // ¥1 per request
	mustNil(t, h.st.UpdateProvider(&oa))
	h.model("coder", "oa/ok")
	u, keys := h.userKeys(store.User{MonthlyBudget: 1.5}, 2)
	h.postAs(keys[0], "/v1/chat/completions", oaReq("coder", false))
	h.postAs(keys[1], "/v1/chat/completions", oaReq("coder", false))
	code, _, body := h.postAs(keys[1], "/v1/chat/completions", oaReq("coder", false))
	if code != 402 || !strings.Contains(body, "your account") {
		t.Fatalf("over account budget: %d %s", code, body)
	}
	// counted from the rollup after a restart too
	l := NewLimiter(h.st)
	k, _ := h.st.Snapshot().LookupKey(keys[0])
	if rej := l.Admit(k, h.st.Snapshot().Users[u.ID]); rej == nil || rej.status != 402 {
		t.Fatalf("restarted limiter: %+v", rej)
	}
	spend, err := h.st.UserSpend(store.MonthStart(time.Now()))
	mustNil(t, err)
	if spend[u.ID] < 1.9 {
		t.Fatalf("user spend %v", spend)
	}
}

func TestUserModelsAndHiddenUpstream(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/fail400", "oa/ok")
	h.model("secret", "oa/ok")
	h.model("broken", "oa/fail400")
	_, keys := h.userKeys(store.User{AllowedModels: []string{"coder", "broken"}}, 1)
	key := keys[0]

	resp, body := h.postWith("/v1/chat/completions", oaReq("coder", false), map[string]string{"Authorization": "Bearer " + key})
	if resp.StatusCode != 200 || resp.Header.Get("X-Route-Target") != "" || strings.Contains(body, `"ok"`) || !strings.Contains(body, `"coder"`) {
		t.Fatalf("coder: %d target=%q %s", resp.StatusCode, resp.Header.Get("X-Route-Target"), body)
	}
	_, body = h.postWith("/v1/chat/completions", oaReq("coder", true), map[string]string{"Authorization": "Bearer " + key})
	if strings.Contains(body, `"model":"ok"`) {
		t.Fatalf("stream names the upstream model: %s", body)
	}
	if code, _, _ := h.postAs(key, "/v1/chat/completions", oaReq("secret", false)); code != 403 {
		t.Fatalf("model outside the account: %d", code)
	}
	code, _, body := h.postAs(key, "/v1/chat/completions", oaReq("broken", false))
	if code != 400 || strings.Contains(body, "oa/") || strings.Contains(body, "fail400") {
		t.Fatalf("error names the upstream: %d %s", code, body)
	}
	// the administrator's keys still see it
	if _, body := h.post("/v1/chat/completions", oaReq("broken", false)); !strings.Contains(body, "oa/fail400") {
		t.Fatalf("admin error lost the target: %s", body)
	}
	req := h.getWith("/v1/models", key)
	if !strings.Contains(req, `"coder"`) || strings.Contains(req, `"secret"`) {
		t.Fatalf("models: %s", req)
	}
}

func TestDisabledAccountKeysStop(t *testing.T) {
	h := newHarness(t)
	h.model("coder", "oa/ok")
	u, keys := h.userKeys(store.User{}, 1)
	if code, _, _ := h.postAs(keys[0], "/v1/chat/completions", oaReq("coder", false)); code != 200 {
		t.Fatal("enabled account refused")
	}
	cur := *h.st.Snapshot().Users[u.ID]
	cur.Enabled = false
	mustNil(t, h.st.UpdateUser(&cur))
	if code, _, body := h.postAs(keys[0], "/v1/chat/completions", oaReq("coder", false)); code != 401 || !strings.Contains(body, "disabled") {
		t.Fatalf("disabled account: %d %s", code, body)
	}
}

func (h *harness) getWith(path, key string) string {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
