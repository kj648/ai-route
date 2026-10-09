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
