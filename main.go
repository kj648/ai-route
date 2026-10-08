package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // correct local time in minimal containers

	"ai-route/internal/admin"
	"ai-route/internal/gateway"
	"ai-route/internal/store"
)

//go:embed web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("LISTEN", ":8080"), "listen address (env LISTEN)")
	dataDir := flag.String("data", env("DATA_DIR", "./data"), "data directory (env DATA_DIR)")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	token := os.Getenv("ADMIN_TOKEN")
	if token != "" && (len(token) < 16 || token == "change-me-to-a-long-random-string") {
		log.Fatalf("ADMIN_TOKEN is too weak: use at least 16 random characters (e.g. `openssl rand -hex 24`), not the example value")
	}
	if token == "" {
		if t, ok := st.GetKV("admin_token"); ok {
			token = t
		} else {
			token = store.RandomToken("admin-", 16)
			if err := st.SetKV("admin_token", token); err != nil {
				log.Fatalf("save admin token: %v", err)
			}
			log.Printf("generated admin token: %s  (set ADMIN_TOKEN to override)", token)
		}
	}

	gw := gateway.New(st)
	bg, stopBG := context.WithCancel(context.Background())
	go gw.Health.Run(bg)
	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	gw.Register(mux)
	admin.New(st, gw, token, web).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{
		Addr:              *listen,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// no WriteTimeout: streams may run for many minutes
	}
	go func() {
		host := *listen
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		log.Printf("ai-route listening on %s (admin console: http://%s/admin/)", *listen, host)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	stopBG()
	gw.Alerts.Wait()
	_ = st.Close()
}

// withCORS allows browser-based clients to call the public API directly.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !strings.HasPrefix(r.URL.Path, "/admin") {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, anthropic-beta")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
