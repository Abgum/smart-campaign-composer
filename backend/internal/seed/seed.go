// Package seed generates deterministic fake recipients and behaviour events.
// Nobody real is ever contacted: emails use example.com and phones are
// synthetic.
package seed

import (
	"context"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User is one generated recipient.
type User struct {
	FirstName       string
	LastName        string
	Email           string
	Phone           *string
	Country         string // ISO 3166-1 alpha-2
	Admin1          string // ISO 3166-2 province code
	CityGeonameID   *int64 // GeoNames id; nil when the settlement is unknown
	Language        string
	BirthDate       time.Time
	TotalOrders     int
	TotalSpentCents int64
	ConsentSMS      bool
	ConsentEmail    bool
	CreatedAt       time.Time
}

// Event is one generated behaviour event; UserIndex points into Data.Users.
type Event struct {
	UserIndex   int
	Type        string
	OccurredAt  time.Time
	AmountCents int64 // purchases only
}

// ForeignUsers is the number of fixed non-Turkish users appended to every
// generated dataset.
const ForeignUsers = 8

// foreignUsers are appended after the generated Turkish users. They are
// fixed (not drawn from the seed), so the Turkish data and its audiences do
// not change. They cover same-named cities in different countries. Phone
// numbers come from ranges reserved for fiction (NANP 555-01xx, Ofcom
// 07700 900xxx, ARCEP 01 99 00 xx xx).
var foreignUsers = []struct {
	first, last, phone, country, admin1 string
	city                                int64
}{
	{"Camille", "Martin", "+33199000101", "FR", "FR-IDF", 2988507},
	{"Lucas", "Bernard", "+33199000102", "FR", "FR-IDF", 2988507},
	{"Chloé", "Dubois", "+33199000103", "FR", "FR-IDF", 2988507},
	{"Emily", "Johnson", "+19035550101", "US", "US-TX", 4717560},
	{"Michael", "Brown", "+19035550102", "US", "US-TX", 4717560},
	{"Oliver", "Smith", "+447700900101", "GB", "GB-ENG", 2643743},
	{"Amelia", "Jones", "+447700900102", "GB", "GB-ENG", 2643743},
	{"Liam", "Tremblay", "+15195550101", "CA", "CA-ON", 6058560},
}

// Data is a generated dataset.
type Data struct {
	Users  []User
	Events []Event
}

const historyDays = 120

// Generate returns n users and their events, deterministic for (n, seed, now).
// Each user has 0-5 sessions in the last 120 days; a session views a product,
// may add it to the cart and may then buy it, which yields a realistic mix of
// buyers and abandoned carts.
func Generate(n int, seed uint64, now time.Time) Data {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	d := Data{Users: make([]User, 0, n)}
	for i := 0; i < n; i++ {
		first := firstNames[r.IntN(len(firstNames))]
		last := lastNames[r.IntN(len(lastNames))]
		// The province is drawn exactly where the former city field was, so
		// the random stream (and every audience) stays the same.
		prov := provinces[pickProvince(r)]
		u := User{
			FirstName:    first,
			LastName:     last,
			Email:        fmt.Sprintf("%s.%s.%d@example.com", ascii(first), ascii(last), i),
			Country:      "TR",
			Admin1:       prov.admin1,
			Language:     "tr",
			BirthDate:    time.Date(1960+r.IntN(47), time.Month(1+r.IntN(12)), 1+r.IntN(28), 0, 0, 0, 0, time.UTC),
			ConsentSMS:   r.Float64() < 0.70,
			ConsentEmail: r.Float64() < 0.80,
			CreatedAt:    now.Add(-time.Duration(1+r.IntN(900)) * 24 * time.Hour),
		}
		if i%17 != 0 {
			// Every 17th user has a province but no known settlement, which
			// keeps province and city targeting distinguishable.
			id := prov.capital
			u.CityGeonameID = &id
		}
		if r.Float64() >= 0.04 {
			// The last 7 digits are the index, so phones are unique by construction.
			p := fmt.Sprintf("+905%02d%07d", 30+r.IntN(26), i)
			u.Phone = &p
		}
		if r.Float64() < 0.08 {
			u.Language = "en"
		}

		start := now.Add(-historyDays * 24 * time.Hour)
		if u.CreatedAt.After(start) {
			start = u.CreatedAt
		}
		span := now.Add(-2*time.Hour).Sub(start) / time.Minute
		for s, sessions := 0, r.IntN(6); s < sessions && span > 0; s++ {
			at := start.Add(time.Duration(r.Int64N(int64(span))) * time.Minute)
			d.Events = append(d.Events, Event{UserIndex: i, Type: "product_view", OccurredAt: at})
			if r.Float64() >= 0.5 {
				continue
			}
			at = at.Add(time.Duration(1+r.IntN(30)) * time.Minute)
			d.Events = append(d.Events, Event{UserIndex: i, Type: "cart_add", OccurredAt: at})
			if r.Float64() >= 0.45 {
				continue
			}
			amount := int64(5000 + r.IntN(195000)) // 50.00 - 2000.00 TL
			d.Events = append(d.Events, Event{UserIndex: i, Type: "purchase", OccurredAt: at.Add(time.Hour), AmountCents: amount})
			u.TotalOrders++
			u.TotalSpentCents += amount
		}
		d.Users = append(d.Users, u)
	}
	for k, f := range foreignUsers {
		phone, city := f.phone, f.city
		d.Events = append(d.Events, Event{UserIndex: len(d.Users), Type: "product_view", OccurredAt: now.Add(-48 * time.Hour)})
		d.Users = append(d.Users, User{
			FirstName: f.first, LastName: f.last,
			Email:   fmt.Sprintf("%s.%s.foreign%d@example.com", ascii(f.first), ascii(f.last), k),
			Phone:   &phone,
			Country: f.country, Admin1: f.admin1, CityGeonameID: &city,
			Language:     "en",
			BirthDate:    time.Date(1985+k, time.Month(1+k), 10, 0, 0, 0, 0, time.UTC),
			ConsentSMS:   true,
			ConsentEmail: true,
			CreatedAt:    now.Add(-200 * 24 * time.Hour),
		})
	}
	return d
}

// Insert loads d into an empty database and reports whether it did; a
// database that already has users is left untouched. An advisory lock makes
// concurrent seed runs safe.
func Insert(ctx context.Context, pool *pgxpool.Pool, d Data) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('scc-seed'))`); err != nil {
		return false, err
	}
	var seeded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&seeded); err != nil {
		return false, err
	}
	if seeded {
		return false, nil
	}

	userRows := make([][]any, len(d.Users))
	for i, u := range d.Users {
		userRows[i] = []any{
			int64(i + 1), u.FirstName, u.LastName, u.Email, u.Phone, u.Country, u.Admin1, u.CityGeonameID, u.Language, u.BirthDate,
			u.TotalOrders, cents(u.TotalSpentCents), u.ConsentSMS, u.ConsentEmail, u.CreatedAt,
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"users"}, []string{
		"id", "first_name", "last_name", "email", "phone", "country_code", "admin1_code", "city_geoname_id", "language", "birth_date",
		"total_orders", "total_spent", "consent_sms", "consent_email", "created_at",
	}, pgx.CopyFromRows(userRows)); err != nil {
		return false, fmt.Errorf("copy users: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence('users', 'id'), $1)`, int64(len(d.Users))); err != nil {
		return false, err
	}

	eventRows := make([][]any, len(d.Events))
	for i, e := range d.Events {
		var amount pgtype.Numeric
		if e.Type == "purchase" {
			amount = cents(e.AmountCents)
		}
		eventRows[i] = []any{int64(e.UserIndex + 1), e.Type, e.OccurredAt, amount}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"user_events"},
		[]string{"user_id", "type", "occurred_at", "amount"}, pgx.CopyFromRows(eventRows)); err != nil {
		return false, fmt.Errorf("copy events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func cents(c int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(c), Exp: -2, Valid: true}
}

type weightedProvince struct {
	name   string
	weight int
}

// Population-like weights; they sum to 100.
var weights = []weightedProvince{
	{"İstanbul", 30}, {"Ankara", 12}, {"İzmir", 10}, {"Bursa", 6}, {"Antalya", 5},
	{"Adana", 4}, {"Konya", 4}, {"Gaziantep", 3}, {"Kocaeli", 3}, {"Mersin", 3},
	{"Kayseri", 2}, {"Eskişehir", 2}, {"Samsun", 2}, {"Trabzon", 2}, {"Diyarbakır", 2},
	{"Denizli", 2}, {"Sakarya", 2}, {"Muğla", 2}, {"Tekirdağ", 2}, {"Manisa", 2},
}

// provinces maps each seeded province to its ISO 3166-2 code and the GeoNames
// id of its central settlement. Every code and id is in the geo catalog
// (checked by tests).
var provinces = map[string]struct {
	admin1  string
	capital int64
}{
	"İstanbul": {"TR-34", 745044}, "Ankara": {"TR-06", 323786}, "İzmir": {"TR-35", 311046},
	"Bursa": {"TR-16", 750269}, "Antalya": {"TR-07", 323777}, "Adana": {"TR-01", 325363},
	"Konya": {"TR-42", 306571}, "Gaziantep": {"TR-27", 314830}, "Kocaeli": {"TR-41", 745028},
	"Mersin": {"TR-33", 304531}, "Kayseri": {"TR-38", 308464}, "Eskişehir": {"TR-26", 315202},
	"Samsun": {"TR-55", 740264}, "Trabzon": {"TR-61", 738648}, "Diyarbakır": {"TR-21", 316541},
	"Denizli": {"TR-20", 317109}, "Sakarya": {"TR-54", 752850}, "Muğla": {"TR-48", 304184},
	"Tekirdağ": {"TR-59", 738927}, "Manisa": {"TR-45", 304827},
}

func pickProvince(r *rand.Rand) string {
	n := r.IntN(100)
	for _, p := range weights {
		if n < p.weight {
			return p.name
		}
		n -= p.weight
	}
	return weights[0].name
}

func ascii(s string) string {
	return strings.ToLower(strings.NewReplacer(
		"İ", "i", "I", "i", "ı", "i", "Ş", "s", "ş", "s", "Ğ", "g", "ğ", "g",
		"Ü", "u", "ü", "u", "Ö", "o", "ö", "o", "Ç", "c", "ç", "c", " ", "",
	).Replace(s))
}

var firstNames = []string{
	"Ahmet", "Mehmet", "Mustafa", "Ali", "Hüseyin", "Hasan", "İbrahim", "Murat", "Emre", "Burak",
	"Can", "Kerem", "Oğuz", "Serkan", "Tolga", "Umut", "Volkan", "Yusuf", "Enes", "Furkan",
	"Ayşe", "Fatma", "Zeynep", "Elif", "Emine", "Merve", "Esra", "Büşra", "Selin", "Deniz",
	"Ece", "Gizem", "İrem", "Melis", "Nazlı", "Özge", "Pınar", "Sena", "Tuğba", "Yasemin",
	"Ayşegül", "Gülşen", "Şeyma", "Çağla", "Damla", "Ebru", "Hande", "Kübra",
	"Muhammed Emin", "Fatma Nur", "Kadir Can", "Ahmet Selim", "Hatice Kübra", "Abdülkadir",
}

var lastNames = []string{
	"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir",
	"Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek",
	"Polat", "Özer", "Erdoğan", "Güneş", "Aksoy", "Avcı", "Taş", "Bulut", "Korkmaz", "Karaca",
	"Ünal", "Uçar", "Tekin", "Bozkurt", "Akın", "Güler", "Sarı", "Acar", "Kalkan", "Karakaya",
}
