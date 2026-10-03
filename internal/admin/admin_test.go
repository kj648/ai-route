package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"ai-route/internal/gateway"
	"ai-route/internal/store"
)

func TestRoutesAndAuth(t *testing.T) {
	st, err := store.Open(t.TempDir())
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
