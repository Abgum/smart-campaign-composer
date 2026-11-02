package seed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LanguageUser is a fixed recipient for the multilingual cases: no recorded
// preference, a preference that differs from the country, an unsupported
// preference (which follows the country's mapping) or a regional code.
type LanguageUser struct {
	FirstName, LastName, Email string
	Phone                      *string
	Country, Admin1            string
	CityGeonameID              *int64
	Language                   *string // nil: no preference recorded
}

// LanguageUsers returns the fixed language users. Phone numbers come from
// ranges reserved for fiction where a country has one (NANP 555-01xx, Ofcom
// 07700 900xxx, ARCEP 01 99 00 xx xx); the Turkish ones use an operator
// prefix the generator never draws (559), and the German users have none.
func LanguageUsers() []LanguageUser {
	pref := func(s string) *string { return &s }
	phone := func(s string) *string { return &s }
	city := func(id int64) *int64 { return &id }
	return []LanguageUser{
		// No preference: the country decides.
		{"Deniz", "Arslan", "deniz.arslan.lang1@example.com", phone("+905590000101"), "TR", "TR-34", city(745044), nil},
		{"Léa", "Moreau", "lea.moreau.lang2@example.com", phone("+33199000104"), "FR", "FR-IDF", city(2988507), nil},
		{"Ava", "Miller", "ava.miller.lang3@example.com", phone("+19035550103"), "US", "US-TX", city(4717560), nil},
		{"Noah", "Roy", "noah.roy.lang4@example.com", phone("+15195550102"), "CA", "CA-ON", city(6058560), nil},
		{"Lukas", "Schneider", "lukas.schneider.lang5@example.com", nil, "DE", "DE-BE", city(2950159), nil},
		// A preference beats the country.
		{"Élodie", "Laurent", "elodie.laurent.lang6@example.com", phone("+905590000102"), "TR", "TR-34", city(745044), pref("fr")},
		{"Harry", "Wilson", "harry.wilson.lang7@example.com", phone("+447700900103"), "GB", "GB-ENG", city(2643743), pref("en-GB")},
		// Unsupported preferences: the country's mapping decides (Türkiye: tr;
		// Germany has none, so Jonas waits for a decision).
		{"Anna", "Müller", "anna.muller.lang8@example.com", phone("+905590000103"), "TR", "TR-07", city(323777), pref("de")},
		{"Jonas", "Weber", "jonas.weber.lang9@example.com", nil, "DE", "DE-BE", city(2950159), pref("de")},
	}
}

// InsertLanguageUsers adds the language users a database lacks, seeded or
// not, and returns how many it added. A user whose e-mail or phone is
// already there is skipped, and no existing row changes, so it can run on
// every seed. Each added user viewed a product and left it in the cart three
// days before now, so the brief's segment (added to the cart, did not buy)
// includes them.
func InsertLanguageUsers(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('scc-seed'))`); err != nil {
		return 0, err
	}
	added := 0
	for i, u := range LanguageUsers() {
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id, language,
			                   birth_date, consent_sms, consent_email, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true, true, $10)
			ON CONFLICT DO NOTHING
			RETURNING id`,
			u.FirstName, u.LastName, u.Email, u.Phone, u.Country, u.Admin1, u.CityGeonameID, u.Language,
			time.Date(1984+i, time.Month(1+i), 12, 0, 0, 0, 0, time.UTC), now.Add(-200*24*time.Hour)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // already there
		}
		if err != nil {
			return 0, fmt.Errorf("insert %s: %w", u.Email, err)
		}
		viewed := now.Add(-72 * time.Hour)
		if _, err := tx.Exec(ctx, `INSERT INTO user_events (user_id, type, occurred_at) VALUES ($1, 'product_view', $2), ($1, 'cart_add', $3)`,
			id, viewed, viewed.Add(10*time.Minute)); err != nil {
			return 0, fmt.Errorf("events of %s: %w", u.Email, err)
		}
		added++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return added, nil
}
