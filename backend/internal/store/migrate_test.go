package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"smart-campaign-composer/backend/internal/store"
	"smart-campaign-composer/backend/internal/testutil"
)

func TestMigrationsApply_CoreTablesExist(t *testing.T) {
	db := testutil.NewDB(t)
	tables := []string{
		"users", "user_events", "chats", "turns", "messages", "campaigns",
		"campaign_versions", "turn_campaign_refs", "llm_calls", "deliveries",
		"worker_heartbeats",
	}
	for _, name := range tables {
		var reg *string
		if err := db.Pool.QueryRow(context.Background(), "SELECT to_regclass($1)::text", name).Scan(&reg); err != nil {
			t.Fatalf("to_regclass(%s): %v", name, err)
		}
		if reg == nil {
			t.Errorf("table %q missing after migrations", name)
		}
	}
}

func TestSchema_TurnIdempotencyAndSingleRunningTurn(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	chat := mustScalar(t, db, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`)

	insertTurn := `INSERT INTO turns (chat_id, client_turn_id, request_hash, status)
	               VALUES ($1, $2, 'h', $3) RETURNING id::text`
	first := mustScalar(t, db, insertTurn, chat, "00000000-0000-0000-0000-000000000001", "running")

	// Same client_turn_id in the same chat must be rejected (turn dedupe key).
	expectUniqueViolation(t, db, insertTurn, chat, "00000000-0000-0000-0000-000000000001", "completed")
	// A second running turn in the same chat must be rejected (one running turn per chat).
	expectUniqueViolation(t, db, insertTurn, chat, "00000000-0000-0000-0000-000000000002", "running")

	if _, err := db.Pool.Exec(ctx, `UPDATE turns SET status='completed', outcome='ok' WHERE id=$1`, first); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	mustScalar(t, db, insertTurn, chat, "00000000-0000-0000-0000-000000000002", "running")
}

func TestSchema_DeliveryContentDedupe(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	user := mustScalar(t, db, `INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, birth_date)
	                           VALUES ('Ayşe','Yılmaz','a@example.com','+905550000001','TR','TR-34','1990-01-01')
	                           RETURNING id::text`)
	chat := mustScalar(t, db, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`)
	campA := mustScalar(t, db, `INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, 1, 'A', 'approved') RETURNING id::text`, chat)
	campB := mustScalar(t, db, `INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, 2, 'B', 'approved') RETURNING id::text`, chat)

	insert := `INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status)
	           VALUES ($1, $2, $3, 'Merhaba', $4, $5) RETURNING id::text`
	first := mustScalar(t, db, insert, campA, user, "sms", "hash-1", "pending")

	// Same campaign/user/channel twice is rejected even with different content.
	expectUniqueViolation(t, db, insert, campA, user, "sms", "hash-2", "pending")

	// Same message (user, channel, content) from another campaign is rejected
	// while the first delivery is live or its outcome is unknown.
	for _, live := range []string{"pending", "in_flight", "retry", "sent", "unknown"} {
		if _, err := db.Pool.Exec(ctx, `UPDATE deliveries SET status=$2 WHERE id=$1`, first, live); err != nil {
			t.Fatalf("set status %s: %v", live, err)
		}
		expectUniqueViolation(t, db, insert, campB, user, "sms", "hash-1", "pending")
	}

	// Same content on another channel is a different message.
	mustScalar(t, db, insert, campB, user, "email", "hash-1", "pending")

	// Only definite non-delivery frees the slot.
	for _, freed := range []string{"failed", "skipped_no_consent"} {
		if _, err := db.Pool.Exec(ctx, `UPDATE deliveries SET status=$2 WHERE id=$1`, first, freed); err != nil {
			t.Fatalf("set status %s: %v", freed, err)
		}
		id := mustScalar(t, db, insert, campB, user, "sms", "hash-1", "pending")
		if _, err := db.Pool.Exec(ctx, `DELETE FROM deliveries WHERE id=$1`, id); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}
}

func TestSchema_UserContactsUnique(t *testing.T) {
	db := testutil.NewDB(t)
	insert := `INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, birth_date)
	           VALUES ('A','B',$1,$2,'TR','TR-35','1990-01-01') RETURNING id::text`
	mustScalar(t, db, insert, "x@example.com", "+905550000009")
	expectUniqueViolation(t, db, insert, "x@example.com", "+905550000010")
	expectUniqueViolation(t, db, insert, "y@example.com", "+905550000009")
}

func mustScalar(t *testing.T, db *testutil.DB, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.Pool.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func expectUniqueViolation(t *testing.T, db *testutil.DB, q string, args ...any) {
	t.Helper()
	var s string
	err := db.Pool.QueryRow(context.Background(), q, args...).Scan(&s)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected unique violation (23505), got %v", err)
	}
}

// Migration 2 adds what the overview counts by: the recipient's city and the
// end time of a delivery, and the approval time of a campaign. Rows written
// before it are filled in, so a database migrated in place keeps its
// history.
func TestMigration2_BackfillsAnalyticsColumns(t *testing.T) {
	db := testutil.NewSchema(t)
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", db.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := store.MigrateTo(ctx, sqlDB, 1); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, first_name, last_name, phone, country_code, admin1_code, city_geoname_id, birth_date)
	      VALUES (1, 'Ayşe', 'Yılmaz', '+905550000001', 'TR', 'TR-34', 745044, '1990-01-01'),
	             (2, 'Mehmet', 'Kaya', '+905550000002', 'TR', 'TR-34', NULL, '1990-01-01')`)
	exec(`INSERT INTO chats (id) VALUES ('00000000-0000-4000-8000-000000000001')`)
	exec(`INSERT INTO campaigns (id, chat_id, ordinal, name, status, updated_at)
	      VALUES (7, '00000000-0000-4000-8000-000000000001', 1, 'A', 'completed', '2026-10-27 12:00:00+00')`)
	exec(`INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status, sent_at, created_at, updated_at) VALUES
	      (7, 1, 'sms', 'a', 'h1', 'sent', '2026-10-27 10:05:00+00', '2026-10-27 10:00:00+00', '2026-10-27 10:05:00+00'),
	      (7, 2, 'sms', 'b', 'h2', 'failed', NULL, '2026-10-27 10:00:01+00', '2026-10-27 10:07:00+00'),
	      (7, 1, 'email', 'c', 'h3', 'pending', NULL, '2026-10-27 10:00:02+00', '2026-10-27 10:00:02+00')`)

	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	type row struct {
		user     int64
		channel  string
		city     *int64
		finished *time.Time
	}
	rows, err := db.Pool.Query(ctx, `SELECT user_id, channel, city_geoname_id, finished_at FROM deliveries ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.user, &x.channel, &x.city, &x.finished)
		return x, err
	})
	if err != nil || len(got) != 3 {
		t.Fatalf("rows = %+v, %v", got, err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if got[0].city == nil || *got[0].city != 745044 || got[0].finished == nil || !got[0].finished.Equal(at("2026-10-27T10:05:00Z")) {
		t.Errorf("sent row = city %v, finished %v; want 745044 and the sent time", got[0].city, got[0].finished)
	}
	if got[1].city != nil || got[1].finished == nil || !got[1].finished.Equal(at("2026-10-27T10:07:00Z")) {
		t.Errorf("failed row = city %v, finished %v; want no city and the last update", got[1].city, got[1].finished)
	}
	if got[2].finished != nil {
		t.Errorf("pending row finished at %v; want none", got[2].finished)
	}
	var approved *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT approved_at FROM campaigns WHERE id = 7`).Scan(&approved); err != nil ||
		approved == nil || !approved.Equal(at("2026-10-27T10:00:00Z")) {
		t.Errorf("approved_at = %v, %v; want the first delivery's creation", approved, err)
	}
}

func TestMigration3_PublicCampaignIDsPreserveExistingRelations(t *testing.T) {
	db := testutil.NewSchema(t)
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", db.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := store.MigrateTo(ctx, sqlDB, 2); err != nil {
		t.Fatal(err)
	}
	chat := mustScalar(t, db, `INSERT INTO chats DEFAULT VALUES RETURNING id::text`)
	for _, ordinal := range []int{1, 2} {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO campaigns (id, chat_id, ordinal, name, status) VALUES ($1, $2, $3, 'Existing', 'draft')`,
			6+ordinal, chat, ordinal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO turns (chat_id, client_turn_id, request_hash, status, focus_campaign_id)
		 VALUES ($1, '00000000-0000-4000-8000-000000000001', 'h', 'completed', 7)`, chat); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var first, second string
	if err := db.Pool.QueryRow(ctx, `SELECT public_id::text FROM campaigns WHERE id = 7`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT public_id::text FROM campaigns WHERE id = 8`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if len(first) != 36 || len(second) != 36 || first == second {
		t.Fatalf("public UUIDs = %q, %q", first, second)
	}
	var focused int64
	if err := db.Pool.QueryRow(ctx, `SELECT focus_campaign_id FROM turns WHERE chat_id = $1`, chat).Scan(&focused); err != nil || focused != 7 {
		t.Fatalf("focus after migration = %d, %v; want 7", focused, err)
	}
	var fresh string
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, 3, 'New', 'draft') RETURNING public_id::text`, chat).Scan(&fresh); err != nil || len(fresh) != 36 || fresh == first || fresh == second {
		t.Fatalf("new public UUID = %q, %v", fresh, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE campaigns SET public_id = $1 WHERE id = 8`, first); err == nil {
		t.Fatal("duplicate public UUID must be rejected")
	}
}

// Migration 4 lets a user have no language preference. It rewrites no row:
// every value stored before (the old default 'tr' included) stays as it was.
func TestMigration4_LanguagePreferenceOptionalWithoutRewritingUsers(t *testing.T) {
	db := testutil.NewSchema(t)
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", db.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := store.MigrateTo(ctx, sqlDB, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO users (first_name, last_name, email, country_code, birth_date, language)
		VALUES ('Emily', 'Johnson', 'emily@example.com', 'US', '1990-01-01', 'en')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO users (first_name, last_name, email, country_code, birth_date)
		VALUES ('Ayşe', 'Yılmaz', 'ayse@example.com', 'TR', '1990-01-01')`); err != nil {
		t.Fatal(err)
	}
	rows := `SELECT string_agg(u::text, ' | ' ORDER BY u.id) FROM users u`
	before := mustScalar(t, db, rows)

	if err := store.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	if after := mustScalar(t, db, rows); after != before {
		t.Fatalf("users changed by the migration:\nbefore %s\nafter  %s", before, after)
	}
	var lang *string
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (first_name, last_name, email, country_code, birth_date)
		VALUES ('Léa', 'Moreau', 'lea@example.com', 'FR', '1990-01-01') RETURNING language`).Scan(&lang); err != nil {
		t.Fatal(err)
	}
	if lang != nil {
		t.Fatalf("a user stored without a preference has language %q, want NULL", *lang)
	}
	// The columns the multilingual campaigns write.
	for _, col := range []string{
		"campaign_versions.localization", "campaign_versions.language_summary",
		"deliveries.language", "campaigns.approval_languages",
	} {
		table, column, _ := strings.Cut(col, ".")
		var n int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, column).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s missing (%d, %v)", col, n, err)
		}
	}
}
