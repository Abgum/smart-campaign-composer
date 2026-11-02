package campaign_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

var approveNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type recipient struct {
	first          string
	admin1         string
	phone, email   bool
	smsOK, emailOK bool
	cityID         int64
	noCity         bool // a province but no known settlement
}

func addUser(t *testing.T, db *testutil.DB, r recipient) int64 {
	t.Helper()
	if r.admin1 == "" {
		r.admin1 = "TR-34"
	}
	var city *int64
	if !r.noCity {
		c := r.cityID
		if c == 0 {
			c = 745044
		}
		city = &c
	}
	var phone, email *string
	if r.phone {
		p := "+90555" + randDigits(7)
		phone = &p
	}
	if r.email {
		e := strings.ToLower(r.first) + "." + randDigits(6) + "@example.com"
		email = &e
	}
	return scalar[int64](t, db, `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id,
		                   birth_date, consent_sms, consent_email)
		VALUES ($1, 'Yılmaz', $2, $3, 'TR', $4, $5, '1990-01-01', $6, $7) RETURNING id`,
		r.first, email, phone, r.admin1, city, r.smsOK, r.emailOK)
}

var digitsMu sync.Mutex
var digitsN int

func randDigits(n int) string {
	digitsMu.Lock()
	defer digitsMu.Unlock()
	digitsN++
	s := strings.Repeat("0", n) + itoa(digitsN)
	return s[len(s)-n:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func istanbul() segment.Rule {
	return segment.Rule{Match: "all", Conditions: []segment.Condition{
		{Kind: "location", Operator: "in", Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}
}

// newDraft creates a draft campaign and returns it with its current version id.
func newDraft(t *testing.T, db *testutil.DB, sms *content.SMS, email *content.Email) (chatID string, s campaign.Saved) {
	t.Helper()
	chatID, _ = newChatTurn(t, db)
	s, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{Name: "İstanbul", Rule: istanbul(), SMS: sms, Email: email, ChangeSummary: "ilk"})
	if err != nil {
		t.Fatal(err)
	}
	return chatID, s
}

func approve(t *testing.T, db *testutil.DB, s campaign.Saved) (campaign.ApproveResult, error) {
	t.Helper()
	return campaign.NewStore(db.Pool).Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
}

func TestApprove_ConsentFilterAndSnapshot(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, email: true, smsOK: true, emailOK: true})
	addUser(t, db, recipient{first: "Mehmet", phone: true, email: true, smsOK: true})
	addUser(t, db, recipient{first: "Zeynep", email: true, smsOK: true, emailOK: true}) // no phone
	addUser(t, db, recipient{first: "Can", phone: true, email: true})                   // no consent
	addUser(t, db, recipient{first: "Ankara", admin1: "TR-06", cityID: 323786, phone: true, email: true, smsOK: true, emailOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}, %30 indirim!"},
		&content.Email{Subject: "{{first_name}}, fırsat", Body: "Merhaba {{first_name}}"})

	res, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "approved" || res.AlreadyApproved {
		t.Fatalf("result = %+v", res)
	}
	want := map[string]campaign.ChannelCounts{
		"sms":   {Selected: 2, ExcludedConsent: 2},
		"email": {Selected: 2, ExcludedConsent: 2},
	}
	for ch, w := range want {
		if got := res.Channels[ch]; got != w {
			t.Errorf("%s = %+v, want %+v", ch, got, w)
		}
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, s.CampaignID); n != 4 {
		t.Fatalf("deliveries = %d", n)
	}
	if st := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, s.CampaignID); st != "approved" {
		t.Fatalf("status = %s", st)
	}
	if v := scalar[int64](t, db, `SELECT approved_version_id FROM campaigns WHERE id=$1`, s.CampaignID); v != s.VersionID {
		t.Fatalf("approved_version_id = %d", v)
	}
}

func TestApprove_StoresRenderedContent(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, email: true, smsOK: true, emailOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}, {{city}} mağazamızda %30!"},
		&content.Email{Subject: "{{first_name}}, fırsat", Body: "Merhaba {{first_name}} {{last_name}}"})
	if _, err := approve(t, db, s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var body, hash string
	if err := db.Pool.QueryRow(ctx, `SELECT body, content_hash FROM deliveries WHERE campaign_id=$1 AND channel='sms'`,
		s.CampaignID).Scan(&body, &hash); err != nil {
		t.Fatal(err)
	}
	if body != "Merhaba Ayşe, İstanbul mağazamızda %30!" || hash != content.Hash("sms", "", body) {
		t.Fatalf("sms body=%q hash=%s", body, hash)
	}
	var subject *string
	if err := db.Pool.QueryRow(ctx, `SELECT subject, body FROM deliveries WHERE campaign_id=$1 AND channel='email'`,
		s.CampaignID).Scan(&subject, &body); err != nil {
		t.Fatal(err)
	}
	if subject == nil || *subject != "Ayşe, fırsat" || body != "Merhaba Ayşe Yılmaz" {
		t.Fatalf("email subject=%v body=%q", subject, body)
	}
}

func TestApprove_Twice_SingleSnapshot(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)
	first, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := approve(t, db, s)
	if err != nil || !again.AlreadyApproved || again.Channels["sms"] != first.Channels["sms"] {
		t.Fatalf("again = %+v, %v", again, err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, s.CampaignID); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
}

func TestApprove_StaleVersion(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	chatID, s := newDraft(t, db, &content.SMS{Text: "v1"}, nil)
	if _, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(),
		campaign.DraftCmd{CampaignID: &s.CampaignID, BaseVersion: ptr(1), Name: "İstanbul", Rule: istanbul(), SMS: &content.SMS{Text: "v2"}}); err != nil {
		t.Fatal(err)
	}
	_, err := approve(t, db, s)
	var stale *campaign.StaleError
	if !errors.As(err, &stale) || stale.CurrentVersion != 2 {
		t.Fatalf("err = %v, want stale (current 2)", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("deliveries = %d", n)
	}
}

// The draft-time estimate counts {{first_name}} as ~10 characters; the
// approval renders real names and must reject a message that no longer fits.
func TestApprove_RevalidatesAtApproveTime(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	text := strings.Repeat("a", 145) + " {{first_name}}" // 146 + ~10 estimated: fits at draft time
	_, s := newDraft(t, db, &content.SMS{Text: text}, nil)
	addUser(t, db, recipient{first: "Abdülkadir Muhammed Emin", phone: true, smsOK: true}) // joins after the draft

	_, err := approve(t, db, s)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) == 0 || ve.Issues[0].Code != "too_long" || !strings.Contains(ve.Issues[0].Message, "1 alıcı") {
		t.Fatalf("err = %v, want a too_long validation error naming 1 recipient", err)
	}
	if st := scalar[string](t, db, `SELECT status FROM campaigns WHERE id=$1`, s.CampaignID); st != "draft" {
		t.Fatalf("status = %s; a rejected approval must change nothing", st)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("deliveries = %d", n)
	}
}

func TestApprove_AudienceBecameZero(t *testing.T) {
	db := testutil.NewDB(t)
	id := addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
	if _, err := db.Pool.Exec(context.Background(), `UPDATE users SET consent_sms=false WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, err := approve(t, db, s)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Code != "empty_audience" {
		t.Fatalf("err = %v, want empty_audience", err)
	}
}

// There is no approval TTL: an old draft is approvable while it is current
// and valid.
func TestApprove_NoTTL_OldDraftApprovable(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
	if _, err := db.Pool.Exec(context.Background(),
		`UPDATE campaign_versions SET created_at = now() - interval '30 days' WHERE id=$1`, s.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := approve(t, db, s); err != nil {
		t.Fatalf("old draft rejected: %v", err)
	}
}

func TestSaveAfterApprove_Rejected(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	chatID, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
	if _, err := approve(t, db, s); err != nil {
		t.Fatal(err)
	}
	_, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "llm"}).SaveDraft(context.Background(),
		campaign.DraftCmd{CampaignID: &s.CampaignID, BaseVersion: ptr(1), Name: "x", Rule: istanbul(), SMS: &content.SMS{Text: "yeni"}})
	if !errors.Is(err, campaign.ErrNotDraft) {
		t.Fatalf("err = %v, want ErrNotDraft", err)
	}
}

// Approval and a concurrent revision are serialized on the campaign row:
// never both succeed (which would approve v1 while v2 is current).
func TestApprove_vs_ConcurrentSave_Serialized(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	for i := 0; i < 10; i++ {
		chatID, s := newDraft(t, db, &content.SMS{Text: "Merhaba " + itoa(i)}, nil)
		var wg sync.WaitGroup
		var approveErr, saveErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, approveErr = approve(t, db, s) }()
		go func() {
			defer wg.Done()
			_, saveErr = campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "llm"}).SaveDraft(context.Background(),
				campaign.DraftCmd{CampaignID: &s.CampaignID, BaseVersion: ptr(1), Name: "x", Rule: istanbul(), SMS: &content.SMS{Text: "v2 " + itoa(i)}})
		}()
		wg.Wait()
		var stale *campaign.StaleError
		switch {
		case approveErr == nil && saveErr == nil:
			t.Fatalf("round %d: both approval and revision succeeded", i)
		case approveErr == nil && !errors.Is(saveErr, campaign.ErrNotDraft):
			t.Fatalf("round %d: approval won but save err = %v", i, saveErr)
		case saveErr == nil && !errors.As(approveErr, &stale):
			t.Fatalf("round %d: save won but approve err = %v", i, approveErr)
		}
		if _, err := db.Pool.Exec(context.Background(), `DELETE FROM deliveries`); err != nil {
			t.Fatal(err)
		}
	}
}

// "Aynı kişiye aynı mesaj iki kez gitmez", across campaigns.
func TestApprove_SameContentOtherCampaign_ExcludedAsDuplicate(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	addUser(t, db, recipient{first: "Mehmet", phone: true, smsOK: true})
	_, a := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}, %30 indirim!"}, nil)
	_, b := newDraft(t, db, &content.SMS{Text: "Merhaba  {{first_name}},  %30 indirim! "}, nil) // same after normalization
	_, c := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}, %40 indirim!"}, nil)

	if _, err := approve(t, db, a); err != nil {
		t.Fatal(err)
	}
	rb, err := approve(t, db, b)
	if err != nil {
		t.Fatal(err)
	}
	if got := rb.Channels["sms"]; got.Selected != 0 || got.ExcludedDuplicate != 2 {
		t.Fatalf("same content: %+v, want 0 selected, 2 excluded as duplicate", got)
	}
	rc, err := approve(t, db, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.Channels["sms"]; got.Selected != 2 || got.ExcludedDuplicate != 0 {
		t.Fatalf("different content: %+v", got)
	}
}

func TestEditVersion(t *testing.T) {
	db := testutil.NewDB(t)
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba"}, nil)
	store := campaign.NewStore(db.Pool)
	ctx := context.Background()

	v, err := store.EditVersion(ctx, s.CampaignID, s.VersionID, campaign.Edit{Name: "İstanbul v2", SMS: &content.SMS{Text: "Selam {{first_name}}"}})
	if err != nil || v.Version != 2 {
		t.Fatalf("edit = %+v, %v", v, err)
	}
	var source, rule string
	if err := db.Pool.QueryRow(ctx, `SELECT source, segment_rule::text FROM campaign_versions WHERE id=$1`, v.VersionID).Scan(&source, &rule); err != nil {
		t.Fatal(err)
	}
	if source != "user" || !strings.Contains(rule, "745044") {
		t.Fatalf("source=%s rule=%s; a manual edit keeps the audience rule", source, rule)
	}

	_, err = store.EditVersion(ctx, s.CampaignID, s.VersionID, campaign.Edit{Name: "x", SMS: &content.SMS{Text: "y"}})
	var stale *campaign.StaleError
	if !errors.As(err, &stale) || stale.CurrentVersion != 2 {
		t.Fatalf("stale base: err = %v", err)
	}
	_, err = store.EditVersion(ctx, s.CampaignID, v.VersionID, campaign.Edit{Name: "x", SMS: &content.SMS{Text: strings.Repeat("a", 161)}})
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Code != "too_long" {
		t.Fatalf("invalid content: err = %v", err)
	}
}

func TestManualDraft_CreatesAndRecountsEditedRule(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	addUser(t, db, recipient{first: "Ankara", admin1: "TR-06", cityID: 323786, phone: true, smsOK: true})
	chatID, _ := newChatTurn(t, db)
	store, ctx := campaign.NewStore(db.Pool), context.Background()
	first, err := store.CreateManual(ctx, chatID, campaign.Edit{Name: "İstanbul", Rule: ptrRule(istanbul()), SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if err != nil || first.Version != 1 || !first.Created {
		t.Fatalf("create = %+v, %v", first, err)
	}
	var source string
	var total int
	if err := db.Pool.QueryRow(ctx, `SELECT source, (audience->>'total')::int FROM campaign_versions WHERE id=$1`, first.VersionID).Scan(&source, &total); err != nil {
		t.Fatal(err)
	}
	if source != "user" || total != 1 {
		t.Fatalf("source=%s audience=%d", source, total)
	}
	anywhere := segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "attribute", Field: "total_orders", Operator: "gte", Values: []string{"0"}}}}
	second, err := store.EditVersionWithRule(ctx, first.CampaignID, first.VersionID, campaign.Edit{Name: "Her yer", Rule: &anywhere, SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if err != nil || second.Version != 2 {
		t.Fatalf("edit = %+v, %v", second, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT (audience->>'total')::int FROM campaign_versions WHERE id=$1`, second.VersionID).Scan(&total); err != nil || total != 2 {
		t.Fatalf("audience=%d, %v", total, err)
	}
}

func ptrRule(r segment.Rule) *segment.Rule { return &r }

func TestManualDraft_InvalidStaleAndApprovedDoNotCreateVersions(t *testing.T) {
	db := testutil.NewDB(t)
	addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	chatID, _ := newChatTurn(t, db)
	store, ctx := campaign.NewStore(db.Pool), context.Background()
	bad := segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "attribute", Field: "secret", Operator: "eq", Values: []string{"x"}}}}
	_, err := store.CreateManual(ctx, chatID, campaign.Edit{Name: "Hatalı", Rule: &bad, SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) == 0 {
		t.Fatalf("invalid create: %v", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM campaigns WHERE chat_id=$1`, chatID); n != 0 {
		t.Fatalf("invalid create left %d campaigns", n)
	}
	first, err := store.CreateManual(ctx, chatID, campaign.Edit{Name: "İstanbul", Rule: ptrRule(istanbul()), SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EditVersionWithRule(ctx, first.CampaignID, first.VersionID, campaign.Edit{Name: "Hatalı", Rule: &bad, SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if !errors.As(err, &ve) {
		t.Fatalf("invalid edit: %v", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM campaign_versions WHERE campaign_id=$1`, first.CampaignID); n != 1 {
		t.Fatalf("invalid edit left %d versions", n)
	}
	second, err := store.EditVersionWithRule(ctx, first.CampaignID, first.VersionID, campaign.Edit{Name: "Yeni", Rule: ptrRule(istanbul()), SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.EditVersionWithRule(ctx, first.CampaignID, first.VersionID, campaign.Edit{Name: "Eski", Rule: ptrRule(istanbul()), SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	var stale *campaign.StaleError
	if !errors.As(err, &stale) || stale.CurrentVersion != 2 {
		t.Fatalf("stale edit: %v", err)
	}
	if _, err := store.Approve(ctx, first.CampaignID, second.VersionID, segment.DefaultCatalog(), approveNow); err != nil {
		t.Fatal(err)
	}
	_, err = store.EditVersionWithRule(ctx, first.CampaignID, second.VersionID, campaign.Edit{Name: "Onaylı", Rule: ptrRule(istanbul()), SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if !errors.Is(err, campaign.ErrNotDraft) {
		t.Fatalf("approved edit: %v", err)
	}
}

// Approval records when it happened and, per delivery, the recipient's city
// at that moment: the overview counts sent deliveries by it. A person with
// no known settlement is outside a city-targeted audience.
func TestApprove_RecordsCityAndTime(t *testing.T) {
	db := testutil.NewDB(t)
	ayse := addUser(t, db, recipient{first: "Ayşe", phone: true, smsOK: true})
	can := addUser(t, db, recipient{first: "Can", phone: true, smsOK: true, noCity: true})
	_, s := newDraft(t, db, &content.SMS{Text: "Merhaba {{first_name}}"}, nil)
	if _, err := approve(t, db, s); err != nil {
		t.Fatal(err)
	}
	city := func(user int64) *int64 {
		return scalar[*int64](t, db, `SELECT city_geoname_id FROM deliveries WHERE campaign_id = $1 AND user_id = $2`, s.CampaignID, user)
	}
	if c := city(ayse); c == nil || *c != 745044 {
		t.Errorf("Ayşe's delivery city = %v, want 745044 (İstanbul)", c)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id = $1 AND user_id = $2`, s.CampaignID, can); got != 0 {
		t.Errorf("Can has no known city; deliveries = %d, want 0", got)
	}
	if at := scalar[*time.Time](t, db, `SELECT approved_at FROM campaigns WHERE id = $1`, s.CampaignID); at == nil || !at.Equal(approveNow) {
		t.Errorf("approved_at = %v, want %v", at, approveNow)
	}
}
