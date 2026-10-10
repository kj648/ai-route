package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"ai-route/internal/gateway"
	"ai-route/internal/store/storetest"
)

func TestClientIPBehindTrustedProxy(t *testing.T) {
	a := &Admin{}
	ip := func(peer string, xff ...string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer + ":1234"
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return a.clientIP(r)
	}
	// nothing trusted: the TCP peer, whatever the header says
	if got := ip("10.0.0.2", "203.0.113.9"); got != "10.0.0.2" {
		t.Fatalf("untrusted peer: %s", got)
	}
	p, err := ParseTrustedProxies("private, 198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	a.TrustProxies(p)
	cases := []struct {
		peer string
		xff  []string
		want string
	}{
		{"10.0.0.2", []string{"203.0.113.9"}, "203.0.113.9"},
		// a client-supplied entry left of the real one is ignored
		{"10.0.0.2", []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		// two trusted hops
		{"10.0.0.2", []string{"203.0.113.9", "198.51.100.7"}, "203.0.113.9"},
		{"10.0.0.2", nil, "10.0.0.2"},
		{"203.0.113.50", []string{"1.2.3.4"}, "203.0.113.50"}, // peer not trusted
		{"10.0.0.2", []string{"garbage"}, "10.0.0.2"},
	}
	for _, c := range cases {
		if got := ip(c.peer, c.xff...); got != c.want {
			t.Errorf("peer %s xff %v: %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
	if _, err := ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

// With several instances, wrong tokens count over all of them, and the
// instance list shows both.
func TestClusterLockoutAndInstances(t *testing.T) {
	loc := storetest.Loc(t)
	open := func() *http.ServeMux {
		st, err := storetest.OpenAt(loc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if st.Cluster() == nil {
			t.Skip("multi-instance tests need PostgreSQL (AI_ROUTE_TEST_DATABASE_URL)")
		}
		st.Cluster().Heartbeat()
		mux := http.NewServeMux()
		New(st, gateway.New(st), "the-right-token-123", fstest.MapFS{}).Register(mux)
		return mux
	}
	a, b := open(), open()
	call := func(mux *http.ServeMux, path, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	var info struct {
		Enabled   bool `json:"enabled"`
		Instances []struct {
			Alive bool `json:"alive"`
		} `json:"instances"`
	}
	_ = json.Unmarshal(call(a, "/admin/api/cluster", "the-right-token-123").Body.Bytes(), &info)
	if !info.Enabled || len(info.Instances) != 2 || !info.Instances[0].Alive {
		t.Fatalf("cluster info: %+v", info)
	}
	for i := 0; i < 10; i++ {
		call([]*http.ServeMux{a, b}[i%2], "/admin/api/providers", "wrong")
	}
	if w := call(a, "/admin/api/providers", "wrong"); w.Code != 429 {
		t.Fatalf("10 failures over two instances should lock the IP out: %d", w.Code)
	}
	if w := call(b, "/admin/api/providers", "the-right-token-123"); w.Code != 200 {
		t.Fatalf("the right token is never locked out: %d", w.Code)
	}
}
