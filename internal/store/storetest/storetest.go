// Package storetest opens throwaway stores for tests: SQLite in a temporary
// directory, or a fresh PostgreSQL schema when AI_ROUTE_TEST_DATABASE_URL is
// set (e.g. postgres://postgres:test@127.0.0.1:5432/airoute?sslmode=disable).
package storetest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"ai-route/internal/store"
)

// EnvURL names the variable that switches tests to PostgreSQL.
const EnvURL = "AI_ROUTE_TEST_DATABASE_URL"

// Loc returns where a test database lives: a directory, or a PostgreSQL
// URL whose search_path is a new schema (dropped when the test ends).
// Opening the same Loc again reopens the same database.
func Loc(tb testing.TB) string {
	tb.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		return tb.TempDir()
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	db, err := sql.Open("pgx", base)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		db.Close()
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		db.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		tb.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// OpenAt opens the store at a Loc.
func OpenAt(loc string, opts ...store.Option) (*store.Store, error) {
	if store.IsPostgresURL(loc) {
		return store.OpenPostgres(loc, opts...)
	}
	return store.Open(loc, opts...)
}

// Open opens a new, empty store.
func Open(tb testing.TB) (*store.Store, error) {
	tb.Helper()
	return OpenAt(Loc(tb))
}
