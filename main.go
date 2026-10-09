package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
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
	"ai-route/internal/metrics"
	"ai-route/internal/store"
	"ai-route/internal/version"
)

//go:embed web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// setupLogging configures the process logger: LOG_FORMAT=text (default) or
// json, LOG_LEVEL=debug|info|warn|error. The standard log package is routed
// through the same handler.
func setupLogging() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// fatal logs the message and exits.
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func main() {
	setupLogging()
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
		fatal("open store failed", "err", err)
	}
	if st.Backend() == "postgres" {
		slog.Info("store opened", "backend", "postgres", "url", redactURL(*dbURL))
	} else {
		slog.Info("store opened", "backend", "sqlite", "dir", *dataDir)
	}

	token := os.Getenv("ADMIN_TOKEN")
	if token != "" && (len(token) < 16 || token == "change-me-to-a-long-random-string") {
		fatal("ADMIN_TOKEN is too weak: use at least 16 random characters (e.g. `openssl rand -hex 24`), not the example value")
	}
	if token == "" {
		if t, ok := st.GetKV("admin_token"); ok {
			token = t
		} else {
			generated := store.RandomToken("admin-", 16)
			// another instance starting at the same moment may win; use its token
			if token, err = st.InitKV("admin_token", generated); err != nil {
				fatal("save admin token failed", "err", err)
			}
			if token == generated {
				slog.Warn("generated an admin token; set ADMIN_TOKEN to choose your own", "token", token)
			}
		}
	}

	proxies, err := admin.ParseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		fatal(err.Error())
	}

	gw := gateway.New(st)
	gw.TrustProxies(proxies)
	bg, stopBG := context.WithCancel(context.Background())
	go gw.Health.Run(bg)
	clusterDone := make(chan struct{})
	if c := st.Cluster(); c != nil {
		c.SetVersion(version.Version)
		c.Heartbeat() // registered before serving: slots taken from the first request on count
		slog.Info("cluster mode: other instances on this database share limits, cooldowns and config", "instance", c.String())
	}
	go func() {
		defer close(clusterDone)
		gw.RunCluster(bg)
	}()
	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	gw.Register(mux)
	adm := admin.New(st, gw, token, web)
	adm.TrustProxies(proxies)
	adm.Register(mux)
	mux.Handle("GET /metrics", adm.Protect(metrics.Handler()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "database unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

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
		slog.Info("ai-route listening", "addr", *listen, "console", "http://"+host+"/admin/", "version", version.Version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("listen failed", "err", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	stopBG()
	<-clusterDone // leaves the instance registry
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
			fatal("no database found", "dir", *from, "err", err)
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
			fatal("open failed", "location", redactURL(loc), "err", err)
		}
		return st
	}
	src, dst := open(*from), open(*to)
	slog.Info("copying", "from", redactURL(*from), "from_backend", src.Backend(), "to", redactURL(*to), "to_backend", dst.Backend())
	start := time.Now()
	err := src.CopyTo(dst, func(table string, n int64) {
		if table != "request_logs" || n%100000 == 0 || n < 5000 {
			slog.Info("copied", "table", table, "rows", n)
		}
	})
	_ = src.Close()
	_ = dst.Close()
	if err != nil {
		fatal("migrate failed", "err", err)
	}
	slog.Info("done", "took", time.Since(start).Round(time.Millisecond))
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
