package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		migrate(os.Args[2:])
		return
	}
	listen := flag.String("listen", env("LISTEN", ":8080"), "listen address (env LISTEN)")
	dataDir := flag.String("data", env("DATA_DIR", "./data"), "SQLite data directory (env DATA_DIR)")
	dbURL := flag.String("db", os.Getenv("DATABASE_URL"), "PostgreSQL URL, e.g. postgres://user:pass@host:5432/airoute (env DATABASE_URL); empty = SQLite in the data directory")
	flag.Parse()

	st, err := store.OpenAuto(*dbURL, *dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if st.Backend() == "postgres" {
		log.Printf("store: PostgreSQL %s", redactURL(*dbURL))
	} else {
		log.Printf("store: SQLite in %s", *dataDir)
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

// migrate copies all data between SQLite and PostgreSQL:
//
//	ai-route migrate -from ./data -to postgres://user:pass@host:5432/airoute
//
// Either side may be a data directory or a PostgreSQL URL; the target must
// be empty. Stop the gateway first so no requests are logged meanwhile.
func migrate(args []string) {
	flags := flag.NewFlagSet("migrate", flag.ExitOnError)
	from := flags.String("from", env("DATA_DIR", "./data"), "source: SQLite data directory or PostgreSQL URL")
	to := flags.String("to", os.Getenv("DATABASE_URL"), "target: PostgreSQL URL or SQLite data directory (must be empty)")
	_ = flags.Parse(args)
	if *to == "" || *from == *to {
		flags.Usage()
		os.Exit(2)
	}
	if !store.IsPostgresURL(*from) {
		if _, err := os.Stat(filepath.Join(*from, "ai-route.db")); err != nil {
			log.Fatalf("no database in %s: %v", *from, err)
		}
	}
	open := func(loc string) *store.Store {
		var st *store.Store
		var err error
		if store.IsPostgresURL(loc) {
			st, err = store.OpenPostgres(loc)
		} else {
			st, err = store.Open(loc)
		}
		if err != nil {
			log.Fatalf("open %s: %v", redactURL(loc), err)
		}
		return st
	}
	src, dst := open(*from), open(*to)
	log.Printf("copying %s (%s) -> %s (%s)", redactURL(*from), src.Backend(), redactURL(*to), dst.Backend())
	start := time.Now()
	err := src.CopyTo(dst, func(table string, n int64) {
		if table != "request_logs" || n%100000 == 0 || n < 5000 {
			log.Printf("  %s: %d", table, n)
		}
	})
	_ = src.Close()
	_ = dst.Close()
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("done in %s", time.Since(start).Round(time.Millisecond))
}

// redactURL hides the password in a database URL for logs.
func redactURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
			return u.String()
		}
	}
	return s
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
