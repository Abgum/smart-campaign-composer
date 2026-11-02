// Package testutil holds helpers shared by tests across packages.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"smart-campaign-composer/backend/internal/store"
)

// DB is a migrated, test-private Postgres schema.
type DB struct {
	Pool *pgxpool.Pool
	DSN  string // DSN with search_path pinned to the private schema
}

// NewDB creates a fresh schema on TEST_DATABASE_URL, applies all migrations
// into it and drops it when the test ends. Without TEST_DATABASE_URL the test
// is skipped, unless REQUIRE_DB=1 (set by `make test`), in which case it fails
// so a misconfigured run can never look green.
func NewDB(t *testing.T) *DB {
	t.Helper()
	return newDB(t, true)
}

// NewSchema is NewDB without migrations: an empty private schema, for tests
// that migrate step by step.
func NewSchema(t *testing.T) *DB {
	t.Helper()
	return newDB(t, false)
}

func newDB(t *testing.T, migrate bool) *DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatal("REQUIRE_DB=1 but TEST_DATABASE_URL is empty")
		}
		t.Skip("TEST_DATABASE_URL not set; skipping DB test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schema := "t_" + randHex(8)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
	})

	dsn, err := withSearchPath(base, schema)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if migrate {
		sqlDB, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open schema db: %v", err)
		}
		defer sqlDB.Close()
		if err := store.Migrate(ctx, sqlDB); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &DB{Pool: pool, DSN: dsn}
}

func withSearchPath(base, schema string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
