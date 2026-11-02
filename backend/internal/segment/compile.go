package segment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/language"
)

// Compiled is a WHERE expression over users aliased as u, with bind args.
type Compiled struct {
	Where string
	Args  []any
}

// Audience is the size of a segment, overall and per reachable channel.
type Audience struct {
	Total          int `json:"total"`
	SMSReachable   int `json:"sms_reachable"`
	EmailReachable int `json:"email_reachable"`
}

// DB is the subset of pgxpool.Pool that Count needs.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// countTimeout bounds audience queries so a pathological rule cannot hold a
// connection.
const countTimeout = "3s"

// Compile turns a rule into parameterized SQL. The rule is validated first;
// only column expressions and operators from this file reach the SQL text,
// every user/LLM-supplied value is a bind parameter.
func Compile(r Rule, cat Catalog, now time.Time) (Compiled, error) {
	res := Validate(r, cat)
	if !res.OK() {
		return Compiled{}, fmt.Errorf("segment rule invalid: %s %s", res.Errors[0].Path, res.Errors[0].Code)
	}
	b := &builder{nowValue: now}

	parts := make([]string, 0, len(res.Normalized.Conditions))
	for _, c := range res.Normalized.Conditions {
		var (
			expr string
			err  error
		)
		switch c.Kind {
		case "attribute":
			expr, err = b.attribute(cat.Fields[c.Field], c)
		case "location":
			expr = b.location(c)
		default:
			expr = b.event(c)
		}
		if err != nil {
			return Compiled{}, err
		}
		parts = append(parts, "("+expr+")")
	}
	joiner := " AND "
	if res.Normalized.Match == "any" {
		joiner = " OR "
	}
	return Compiled{Where: "(" + strings.Join(parts, joiner) + ")", Args: b.args}, nil
}

// Count returns the audience size for r, overall and per channel. Channel
// reachability (consent + contact present) is always added by Go, never by
// the rule.
func Count(ctx context.Context, db DB, r Rule, cat Catalog, now time.Time) (Audience, error) {
	c, err := Compile(r, cat, now)
	if err != nil {
		return Audience{}, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return Audience{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+countTimeout+"'"); err != nil {
		return Audience{}, err
	}
	var a Audience
	err = tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE `+SMSReachable+`),
		       count(*) FILTER (WHERE `+EmailReachable+`)
		FROM users u WHERE `+c.Where, c.Args...).Scan(&a.Total, &a.SMSReachable, &a.EmailReachable)
	if err != nil {
		return Audience{}, fmt.Errorf("count audience: %w", err)
	}
	return a, nil
}

// LanguageBuckets counts r's audience by the facts a content language is
// worked out from: the recorded preference (NULL for none), the country and
// the reach on each channel. The groups are few, so the language of each is
// worked out in Go (language.Summarize), not in SQL.
func LanguageBuckets(ctx context.Context, db DB, r Rule, cat Catalog, now time.Time) ([]language.Bucket, error) {
	c, err := Compile(r, cat, now)
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+countTimeout+"'"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT u.language, u.country_code, (`+SMSReachable+`), (`+EmailReachable+`), count(*)
		FROM users u WHERE `+c.Where+` GROUP BY 1, 2, 3, 4`, c.Args...)
	if err != nil {
		return nil, fmt.Errorf("language buckets: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (language.Bucket, error) {
		var b language.Bucket
		err := row.Scan(&b.Preference, &b.Country, &b.SMS, &b.Email, &b.Count)
		return b, err
	})
}

// Channel reachability predicates over users aliased as u.
const (
	SMSReachable   = "u.consent_sms AND u.phone IS NOT NULL"
	EmailReachable = "u.consent_email AND u.email IS NOT NULL"
)

type builder struct {
	args     []any
	nowValue time.Time
	nowRef   string
}

// now returns the typed placeholder for the evaluation instant, binding it on
// first use (Postgres rejects bind parameters it cannot type, so an unused
// $n must not exist).
func (b *builder) now() string {
	if b.nowRef == "" {
		b.nowRef = b.arg(b.nowValue) + "::timestamptz"
	}
	return b.nowRef
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// column returns the SQL expression for a whitelisted field and the cast its
// bind parameters need.
func (b *builder) column(name string) (expr, cast string, err error) {
	switch name {
	case "language":
		// The primary language of the recorded preference, read as
		// language.Normalize reads it: "en-GB", "en_US" and "EN" are "en".
		return "lower(split_part(replace(trim(u.language), '_', '-'), '-', 1))", "::text", nil
	case "age":
		return "date_part('year', age((" + b.now() + ")::date, u.birth_date))::int", "::int", nil
	case "total_orders":
		return "u.total_orders", "::int", nil
	case "signup_days_ago":
		return "((" + b.now() + ")::date - u.created_at::date)", "::int", nil
	case "total_spent":
		return "u.total_spent", "::numeric", nil
	}
	return "", "", fmt.Errorf("segment: no column for field %q", name)
}

func (b *builder) attribute(spec FieldSpec, c Condition) (string, error) {
	col, cast, err := b.column(spec.Name)
	if err != nil {
		return "", err
	}
	val := func(i int) (string, error) {
		v := c.Values[i]
		if spec.Type == TypeInt {
			n, err := strconv.Atoi(v)
			if err != nil {
				return "", err
			}
			return b.arg(n) + cast, nil
		}
		return b.arg(v) + cast, nil
	}
	switch c.Operator {
	case "eq", "neq", "gte", "lte":
		p, err := val(0)
		if err != nil {
			return "", err
		}
		op := map[string]string{"eq": "=", "neq": "<>", "gte": ">=", "lte": "<="}[c.Operator]
		return col + " " + op + " " + p, nil
	case "between":
		lo, err := val(0)
		if err != nil {
			return "", err
		}
		hi, err := val(1)
		if err != nil {
			return "", err
		}
		return col + " BETWEEN " + lo + " AND " + hi, nil
	case "in":
		return col + " = ANY(" + b.arg(c.Values) + "::text[])", nil
	case "not_in":
		return "NOT (" + col + " = ANY(" + b.arg(c.Values) + "::text[]))", nil
	}
	return "", fmt.Errorf("segment: unsupported operator %q", c.Operator)
}

// location matches a set of places. Each place is matched along its full
// hierarchy (a city also checks its country); there is no separate,
// OR'ed country condition. coalesce keeps users without a city on the right
// side of not_in.
func (b *builder) location(c Condition) string {
	parts := make([]string, 0, len(c.Places))
	for _, p := range c.Places {
		country := "u.country_code = " + b.arg(p.Country) + "::text"
		switch p.Level {
		case "country":
			parts = append(parts, country)
		case "admin1":
			parts = append(parts, "("+country+" AND u.admin1_code = "+b.arg(p.Code)+"::text)")
		case "city":
			parts = append(parts, "("+country+" AND u.city_geoname_id = "+b.arg(p.GeonameID)+"::bigint)")
		}
	}
	expr := "coalesce(" + strings.Join(parts, " OR ") + ", false)"
	if c.Operator == "not_in" {
		return "NOT " + expr
	}
	return expr
}

func (b *builder) event(c Condition) string {
	typ := b.arg(c.EventType)
	from := b.now() + " - make_interval(days => " + b.arg(c.WithinDays) + ")"
	window := "e.occurred_at >= " + from + " AND e.occurred_at <= " + b.now()

	if c.NotFollowedBy != "" {
		// At least one qualifying event in the window with no follow-up event
		// at or after it (same instant counts as followed).
		next := b.arg(c.NotFollowedBy)
		return "EXISTS (SELECT 1 FROM user_events e WHERE e.user_id = u.id AND e.type = " + typ +
			" AND " + window +
			" AND NOT EXISTS (SELECT 1 FROM user_events f WHERE f.user_id = u.id AND f.type = " + next +
			" AND f.occurred_at >= e.occurred_at AND f.occurred_at <= " + b.now() + "))"
	}

	var expr string
	if c.MinCount <= 1 {
		expr = "EXISTS (SELECT 1 FROM user_events e WHERE e.user_id = u.id AND e.type = " + typ + " AND " + window + ")"
	} else {
		expr = "(SELECT count(*) FROM user_events e WHERE e.user_id = u.id AND e.type = " + typ + " AND " + window +
			") >= " + b.arg(c.MinCount)
	}
	if c.Negate {
		return "NOT " + expr
	}
	return expr
}
