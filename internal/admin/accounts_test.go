package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"ai-route/internal/gateway"
	"ai-route/internal/store"
	"ai-route/internal/store/storetest"
)

type consoleHarness struct {
	t   *testing.T
	st  *store.Store
	mux *http.ServeMux
}

func newConsole(t *testing.T) *consoleHarness {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gw := gateway.New(st)
	mux := http.NewServeMux()
	gw.Register(mux)
	adm := New(st, gw, "the-admin-token-123", fstest.MapFS{"index.html": {Data: []byte("x")}})
	adm.Register(mux)
	mux.Handle("GET /metrics", adm.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("metrics")) })))
	return &consoleHarness{t, st, mux}
}

func (c *consoleHarness) call(tok, method, path, body string) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	r.RemoteAddr = "192.0.2.1:1234"
	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

func (c *consoleHarness) login(user, pw string) string {
	c.t.Helper()
	code, body := c.call("", "POST", "/admin/api/login", `{"username":"`+user+`","password":"`+pw+`"}`)
	if code != 200 {
		c.t.Fatalf("login %s: %d %s", user, code, body)
	}
	var out struct{ Token string }
	_ = json.Unmarshal([]byte(body), &out)
	return out.Token
}

func TestAccountsConsole(t *testing.T) {
	c := newConsole(t)
	const admin = "the-admin-token-123"
	mustOK := func(code int, body string) string {
		t.Helper()
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		return body
	}
	mustOK(c.call(admin, "POST", "/admin/api/models", `{"name":"coder","targets":[],"enabled":true}`))
	mustOK(c.call(admin, "POST", "/admin/api/models", `{"name":"secret","targets":[],"enabled":true}`))
	mustOK(c.call(admin, "POST", "/admin/api/users", `{"username":"alice","password":"secret-pass","enabled":true,"allowed_models":["coder"]}`))
	mustOK(c.call(admin, "POST", "/admin/api/users", `{"username":"root","password":"secret-pass","enabled":true,"role":"admin"}`))
	if body := mustOK(c.call(admin, "GET", "/admin/api/users", "")); strings.Contains(body, "pbkdf2") {
		t.Fatalf("user list leaks password hashes: %s", body)
	}

	tok := c.login("alice", "secret-pass")
	if code, _ := c.call(tok, "GET", "/admin/api/providers", ""); code != 403 {
		t.Fatalf("user reached the admin API: %d", code)
	}
	if code, _ := c.call(tok, "GET", "/metrics", ""); code != 403 {
		t.Fatalf("user reached /metrics: %d", code)
	}
	me := mustOK(c.call(tok, "GET", "/admin/api/me", ""))
	if !strings.Contains(me, `"role":"user"`) || strings.Contains(me, "pbkdf2") {
		t.Fatalf("me: %s", me)
	}
	models := mustOK(c.call(tok, "GET", "/admin/api/my/models", ""))
	if !strings.Contains(models, `"coder"`) || strings.Contains(models, "secret") || strings.Contains(models, "targets") {
		t.Fatalf("my models: %s", models)
	}
	created := mustOK(c.call(tok, "POST", "/admin/api/my/keys", `{"name":"laptop","enabled":true}`))
	var k struct {
		ID   int64
		Key  string
		Hint string
	}
	_ = json.Unmarshal([]byte(created), &k)
	if !strings.HasPrefix(k.Key, "sk-route-") || k.Hint == "" {
		t.Fatalf("created key: %s", created)
	}
	if list := mustOK(c.call(tok, "GET", "/admin/api/my/keys", "")); strings.Contains(list, k.Key) || !strings.Contains(list, k.Hint) {
		t.Fatalf("key list shows the value again: %s", list)
	}
	if code, _ := c.call(tok, "POST", "/admin/api/my/keys", `{"name":"x","allowed_models":["secret"]}`); code != 400 {
		t.Fatalf("key for a model outside the account: %d", code)
	}
	// the administrator sees the key with its owner, never its value or hash
	keys := mustOK(c.call(admin, "GET", "/admin/api/keys", ""))
	if !strings.Contains(keys, `"owner":"alice"`) || strings.Contains(keys, k.Key) || strings.Contains(keys, "sha256:") {
		t.Fatalf("admin key list: %s", keys)
	}

	// the user's logs carry no upstream details
	c.st.AddLog(&store.RequestLog{CreatedAt: 1, KeyID: k.ID, KeyName: "laptop", UserID: 1, PublicModel: "coder", Provider: "plan",
		UpstreamModel: "glm-5", HTTPStatus: 502, Error: "HTTP 502 from plan.internal", Attempts: []store.Attempt{{Target: "plan/glm-5"}}})
	c.st.AddLog(&store.RequestLog{CreatedAt: 2, KeyID: 99, UserID: 0, PublicModel: "coder", Provider: "plan"})
	c.st.FlushLogs()
	logs := mustOK(c.call(tok, "GET", "/admin/api/my/logs", ""))
	if !strings.Contains(logs, `"total":1`) || strings.Contains(logs, "plan") || strings.Contains(logs, "glm") || strings.Contains(logs, "attempts") {
		t.Fatalf("my logs: %s", logs)
	}
	stats := mustOK(c.call(tok, "GET", "/admin/api/my/stats?range=30d", ""))
	if strings.Contains(stats, "plan") {
		t.Fatalf("my stats name the provider: %s", stats)
	}

	// password change; sign-out ends the session
	if code, _ := c.call(tok, "POST", "/admin/api/my/password", `{"old_password":"bad","new_password":"another-pass"}`); code != 400 {
		t.Fatalf("wrong old password: %d", code)
	}
	mustOK(c.call(tok, "POST", "/admin/api/my/password", `{"old_password":"secret-pass","new_password":"another-pass"}`))
	mustOK(c.call(tok, "POST", "/admin/api/logout", ""))
	if code, _ := c.call(tok, "GET", "/admin/api/me", ""); code != 401 {
		t.Fatalf("session after logout: %d", code)
	}

	// an admin account gets the whole console but cannot lock itself out
	root := c.login("root", "secret-pass")
	mustOK(c.call(root, "GET", "/admin/api/providers", ""))
	if code, _ := c.call(root, "DELETE", "/admin/api/users/2", ""); code != 400 {
		t.Fatalf("admin deleted itself: %d", code)
	}
	if code, _ := c.call(root, "PUT", "/admin/api/users/2", `{"username":"root","role":"user","enabled":true}`); code != 400 {
		t.Fatalf("admin demoted itself: %d", code)
	}
	// ADMIN_TOKEN has no account of its own
	if code, _ := c.call(admin, "GET", "/admin/api/my/keys", ""); code != 400 {
		t.Fatalf("my keys with ADMIN_TOKEN: %d", code)
	}
}

func TestLoginLockoutPerAccount(t *testing.T) {
	c := newConsole(t)
	if code, body := c.call("the-admin-token-123", "POST", "/admin/api/users", `{"username":"alice","password":"secret-pass","enabled":true}`); code != 200 {
		t.Fatal(code, body)
	}
	for i := 0; i < 10; i++ {
		if code, _ := c.call("", "POST", "/admin/api/login", `{"username":"alice","password":"guess-`+string(rune('a'+i))+`"}`); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	// locked: even the right password waits (the attacker cannot tell it was right)
	if code, _ := c.call("", "POST", "/admin/api/login", `{"username":"alice","password":"secret-pass"}`); code != 429 {
		t.Fatalf("after 10 failures: %d", code)
	}
}
