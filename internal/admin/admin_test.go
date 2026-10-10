package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"ai-route/internal/gateway"
	"ai-route/internal/store/storetest"
)

func TestRoutesAndAuth(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gw := gateway.New(st)
	mux := http.NewServeMux()
	gw.Register(mux) // must not conflict with admin routes
	New(st, gw, "tok", fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}}).Register(mux)

	cases := []struct {
		method, path, token string
		want                int
		contains            string
	}{
		{"GET", "/admin/api/providers", "", 401, ""},
		{"GET", "/admin/api/providers", "wrong", 401, ""},
		{"GET", "/admin/api/providers", "tok", 200, "[]"},
		{"GET", "/admin/", "", 200, "console"},
		{"GET", "/", "", 302, ""},
		{"POST", "/admin/api/providers", "tok", 400, "base_url is required"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s %s: got %d %q", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminLockoutAndConsoleHeaders(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gw := gateway.New(st)
	mux := http.NewServeMux()
	New(st, gw, "the-right-token-123", fstest.MapFS{"index.html": {Data: []byte("<html>console</html>")}}).Register(mux)
	call := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/admin/api/providers", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 10; i++ {
		if c := call("wrong").Code; c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if w := call("wrong"); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("locked out IP should get 429 on further wrong tokens, got %d", w.Code)
	}
	// the right token is never locked out: behind a shared address someone
	// else's failures must not shut the administrator out
	if w := call("the-right-token-123"); w.Code != 200 {
		t.Fatalf("right token from a locked IP: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/admin/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("console headers: %v", w.Header())
	}
}

func TestCaptureEndpoints(t *testing.T) {
	st, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gw := gateway.New(st)
	mux := http.NewServeMux()
	New(st, gw, "tok", fstest.MapFS{"index.html": {Data: []byte("x")}}).Register(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}
	if rec := call("POST", "/admin/api/captures/rules", `{"count":0}`); rec.Code != 400 {
		t.Fatalf("zero count: %d %s", rec.Code, rec.Body)
	}
	if rec := call("POST", "/admin/api/captures/rules", `{"model":"coder","count":3}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"remaining":3`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := call("GET", "/admin/api/captures", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"model":"coder"`) || !strings.Contains(rec.Body.String(), `"retention_hours":24`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if len(st.Snapshot().Captures) != 1 {
		t.Fatal("rule not in the snapshot")
	}
	if rec := call("GET", "/admin/api/captures/99", ""); rec.Code != 404 {
		t.Fatalf("missing capture: %d", rec.Code)
	}
	if rec := call("DELETE", "/admin/api/captures/rules/1", ""); rec.Code != 200 || len(st.Snapshot().Captures) != 0 {
		t.Fatalf("delete rule: %d, %d left", rec.Code, len(st.Snapshot().Captures))
	}
	if rec := call("DELETE", "/admin/api/captures", ""); rec.Code != 200 {
		t.Fatalf("delete captures: %d", rec.Code)
	}
}
