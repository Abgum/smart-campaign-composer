package turns_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// The language cases, played by the demo model through real turns: what it
// proposes goes through the same Go checks, and the approval works out
// every recipient's language again.

var demoNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type langVersion struct {
	id  int64
	loc *language.Settings
	sum *language.Summary
	sms *content.SMS
}

func currentLanguages(t *testing.T, db *testutil.DB, campaignID int64) langVersion {
	t.Helper()
	var v langVersion
	if err := db.Pool.QueryRow(context.Background(), `
		SELECT v.id, v.localization, v.language_summary, v.sms FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.id = $1`, campaignID).Scan(&v.id, &v.loc, &v.sum, &v.sms); err != nil {
		t.Fatal(err)
	}
	return v
}

// addLanguageUsers adds the seed's language users, all in the brief's
// segment: Deniz (no preference, TR), Léa (none, FR), Ava (none, US), Noah
// (none, CA), Lukas (none, DE, no phone), Élodie (fr, TR), Harry (en-GB,
// GB), Anna (de, TR: unsupported, so Türkiye's mapping) and Jonas (de, DE,
// no phone).
func addLanguageUsers(t *testing.T, db *testutil.DB) {
	t.Helper()
	if _, err := seed.InsertLanguageUsers(context.Background(), db.Pool, demoNow); err != nil {
		t.Fatal(err)
	}
}

// addCartLeaver adds a Turkish-province recipient who left a cart two days
// ago, with a preference (nil: none).
func addCartLeaver(t *testing.T, db *testutil.DB, first string, pref *string) {
	t.Helper()
	addCartLeaverIn(t, db, first, pref, "TR", true)
}

// addCartLeaverIn adds a cart leaver in country (outside Türkiye with no
// region or city), with or without consent on both channels.
func addCartLeaverIn(t *testing.T, db *testutil.DB, first string, pref *string, country string, consent bool) {
	t.Helper()
	var (
		admin1 *string
		city   *int64
	)
	if country == "TR" {
		a, c := "TR-34", int64(745044)
		admin1, city = &a, &c
	}
	id := count(t, db, `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id, language,
		                   birth_date, consent_sms, consent_email)
		VALUES ($1, 'Test', $2, $3, $4, $5, $6, $7, '1990-01-01', $8, $8) RETURNING id`,
		first, strings.ToLower(first)+"@example.com", "+90554"+uuid()[:7], country, admin1, city, pref, consent)
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO user_events (user_id, type, occurred_at) VALUES ($1, 'cart_add', $2)`,
		id, demoNow.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// sentLanguages maps each recipient's first name to the language of what
// they got on channel.
func sentLanguages(t *testing.T, db *testutil.DB, campaignID int64, channel string) map[string]string {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), `
		SELECT u.first_name, coalesce(d.language, '') FROM deliveries d JOIN users u ON u.id = d.user_id
		WHERE d.campaign_id = $1 AND d.channel = $2`, campaignID, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, lang string
		if err := rows.Scan(&name, &lang); err != nil {
			t.Fatal(err)
		}
		out[name] = lang
	}
	return out
}

func approve(t *testing.T, db *testutil.DB, campaignID, versionID int64) (campaign.ApproveResult, error) {
	t.Helper()
	return campaign.NewStore(db.Pool).Approve(context.Background(), campaignID, versionID, segment.DefaultCatalog(), demoNow)
}

// A Turkish brief that names no language gets Turkish content.
func TestDemo_Language_TurkishBriefTurkishContent(t *testing.T) {
	db := testutil.NewDB(t)
	res := runFocused(t, newDemoRunner(t, db), newChat(t, db), "Sepete ürün ekleyip almayanlara SMS gönder", nil)
	v := currentLanguages(t, db, res.CampaignRefs[0].CampaignID)
	if v.loc == nil || v.loc.Mode != language.ModeSingle || v.loc.Language != "tr" || v.sms == nil ||
		!strings.Contains(v.sms.Text, "sepet") {
		t.Fatalf("draft = %+v / %+v; want one Turkish content", v.loc, v.sms)
	}
}

// "İngilizce hazırla" in a Turkish brief: one English draft for everyone,
// whatever their preference, and the reply stays Turkish.
func TestDemo_Language_EnglishInstructionGivesOneEnglishDraft(t *testing.T) {
	db := testutil.NewDB(t)
	addLanguageUsers(t, db)
	addCartLeaver(t, db, "Ayşe", ptr("tr"))
	res := runFocused(t, newDemoRunner(t, db), newChat(t, db), "Sepette ürün bırakanlara İngilizce hazırla #scenario:english", nil)
	id := res.CampaignRefs[0].CampaignID
	v := currentLanguages(t, db, id)
	if v.loc == nil || v.loc.Mode != language.ModeSingle || v.loc.Language != "en" || v.sms == nil || !strings.Contains(v.sms.Text, "cart") {
		t.Fatalf("draft = %+v / %+v; want one English content", v.loc, v.sms)
	}
	if m := res.AssistantMessage.Text; !strings.Contains(m, "İngilizce") || !strings.Contains(m, "herkese") {
		t.Fatalf("reply = %q; want Turkish, about one English content for everyone", m)
	}
	if _, err := approve(t, db, id, v.id); err != nil {
		t.Fatalf("approve: %v", err)
	}
	for name, lang := range sentLanguages(t, db, id, "email") {
		if lang != "en" {
			t.Errorf("%s got %q, want en", name, lang)
		}
	}
}

// Each recipient in their language: a supported preference beats the
// country; without one (no preference, or an unsupported one) the country's
// mapping decides; a country without a mapping waits and the model asks,
// offering the supported languages. The user's answer, group by group,
// unlocks approval.
func TestDemo_Language_EachRecipientTheirLanguage(t *testing.T) {
	db := testutil.NewDB(t)
	addLanguageUsers(t, db)
	r, chatID := newDemoRunner(t, db), newChat(t, db)
	res := runFocused(t, r, chatID, "Herkese tercih ettiği dilde gönder #scenario:multilingual", nil)
	id := res.CampaignRefs[0].CampaignID
	v := currentLanguages(t, db, id)
	if v.loc == nil || v.loc.Mode != language.ModePerRecipient || len(v.loc.Variants) != 3 {
		t.Fatalf("setup = %+v", v.loc)
	}
	langs := map[string]int{}
	for l, reach := range v.sum.Languages {
		langs[l] = reach.Total
	}
	if want := map[string]int{"tr": 2, "en": 2, "fr": 2}; !reflect.DeepEqual(langs, want) {
		t.Fatalf("languages = %v, want %v (Anna's German preference follows Türkiye)", langs, want)
	}
	waiting := []language.GroupCount{{Group: "country:DE", Count: 2}, {Group: "country:CA", Count: 1}}
	if !slices.Equal(v.sum.Unresolved, waiting) {
		t.Fatalf("waiting = %+v, want %+v", v.sum.Unresolved, waiting)
	}
	m := res.AssistantMessage.Text
	if !strings.Contains(m, "Türkçe, İngilizce, Fransızca") || !strings.Contains(m, "Almanya") || !strings.Contains(m, "Kanada") {
		t.Fatalf("reply = %q; want a question about each waiting group offering the supported languages", m)
	}
	// The counts are the draft's, of the recipients who can get a message.
	if !strings.Contains(m, "Taslak anında mesaj alabilecek alıcılar: Türkçe 2, İngilizce 2, Fransızca 2") {
		t.Fatalf("reply = %q; want the language counts as of the draft", m)
	}
	// People read language and country names, never codes or group keys.
	for _, code := range []string{"tr, en, fr", "(de)", "(CA)", "preference:", "country:"} {
		if strings.Contains(m, code) {
			t.Errorf("reply = %q; names a language or group by its code %q", m, code)
		}
	}
	var de *campaign.DecisionError
	if _, err := approve(t, db, id, v.id); !errors.As(err, &de) {
		t.Fatalf("approve with recipients waiting: %v", err)
	}

	res = runFocused(t, r, chatID, "Almanya'dakilere İngilizce gönder, Kanada'dakileri çıkar #scenario:group_decision", &id)
	v = currentLanguages(t, db, id)
	if res.Outcome != "ok" || len(v.sum.Unresolved) != 0 || len(v.loc.Decisions) != 2 {
		t.Fatalf("after the answer: %+v / %+v / %+v", res, v.sum, v.loc.Decisions)
	}
	if _, err := approve(t, db, id, v.id); err != nil {
		t.Fatalf("approve after the decisions: %v", err)
	}
	want := map[string]string{"Deniz": "tr", "Anna": "tr", "Léa": "fr", "Élodie": "fr", "Ava": "en", "Harry": "en", "Jonas": "en", "Lukas": "en"}
	if got := sentLanguages(t, db, id, "email"); !reflect.DeepEqual(got, want) {
		t.Fatalf("e-mails = %v, want %v (Noah left out)", got, want)
	}
}

// Only Turkish and French variants: English readers wait for the English
// variant. "Everyone waiting gets English" adds it; the new version waits
// for approval like any other.
func TestDemo_Language_MissingEnglishVariantIsAddedAndWaitsForApproval(t *testing.T) {
	db := testutil.NewDB(t)
	addLanguageUsers(t, db)
	r, chatID := newDemoRunner(t, db), newChat(t, db)
	res := runFocused(t, r, chatID, "Türkçe ve Fransızca hazırla, herkese kendi dilinde #scenario:bilingual", nil)
	id := res.CampaignRefs[0].CampaignID
	first := currentLanguages(t, db, id)
	if !slices.Contains(first.sum.Unresolved, language.GroupCount{Group: "variant:en", Count: 2}) {
		t.Fatalf("waiting = %+v; want Ava and Harry waiting for an English variant", first.sum.Unresolved)
	}

	res = runFocused(t, r, chatID, "Karar bekleyen herkese İngilizce gönder #scenario:pending_to_english", &id)
	v := currentLanguages(t, db, id)
	if _, ok := v.loc.Variant("en"); !ok || len(v.sum.Unresolved) != 0 || res.Outcome != "ok" {
		t.Fatalf("after the general answer: variants %+v, waiting %+v", v.loc.Variants, v.sum.Unresolved)
	}
	for _, d := range v.loc.Decisions {
		if d.Action != language.ActionAssign || d.Language != "en" {
			t.Fatalf("decision %+v; want every waiting group assigned to English", d)
		}
	}
	if st := count(t, db, `SELECT count(*) FROM campaigns WHERE id = $1 AND status = 'draft'`, id); st != 1 {
		t.Fatal("the campaign must still wait for approval")
	}
	if _, err := approve(t, db, id, first.id); err == nil {
		t.Fatal("the version without English must not be approvable any more")
	}
	p, err := campaign.NewStore(db.Pool).PreviewApproval(context.Background(), id, v.id, segment.DefaultCatalog(), demoNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.Languages["en"]["email"].Selected != 5 || len(p.Unresolved) != 0 {
		t.Fatalf("preview = %+v; want five English e-mails (Ava, Harry, Jonas, Lukas, Noah)", p.Languages["en"])
	}
}

// The audience is worked out again at approval: a new recipient whose group
// was never decided stops it until the user decides; a mere change in the
// numbers does not, nor does a new recipient no channel reaches.
func TestDemo_Language_AudienceChangedAtApproval(t *testing.T) {
	db := testutil.NewDB(t)
	addLanguageUsers(t, db)
	r, chatID := newDemoRunner(t, db), newChat(t, db)
	id := runFocused(t, r, chatID, "Herkese tercih ettiği dilde gönder #scenario:multilingual", nil).CampaignRefs[0].CampaignID
	runFocused(t, r, chatID, "Almanya'dakiler İngilizce, Kanada çıkarılsın #scenario:group_decision", &id)
	v := currentLanguages(t, db, id)
	store := campaign.NewStore(db.Pool)
	before, err := store.PreviewApproval(context.Background(), id, v.id, segment.DefaultCatalog(), demoNow)
	if err != nil {
		t.Fatal(err)
	}

	addCartLeaver(t, db, "Omar", ptr("ar")) // unsupported: Türkiye's mapping
	addCartLeaver(t, db, "Ayşe", ptr("tr"))
	addCartLeaverIn(t, db, "Lucía", nil, "ES", true)   // Spain: nobody decided
	addCartLeaverIn(t, db, "Giulia", nil, "IT", false) // Italy, but no channel reaches her
	_, err = approve(t, db, id, v.id)
	var de *campaign.DecisionError
	if !errors.As(err, &de) || !slices.Equal(de.Groups, []language.GroupCount{{Group: "country:ES", Count: 1}}) {
		t.Fatalf("approve = %v; want only the new group country:ES to wait", err)
	}
	if n := count(t, db, `SELECT count(*) FROM deliveries WHERE campaign_id = $1`, id); n != 0 {
		t.Fatalf("%d deliveries queued before the decision", n)
	}

	// The user decides in the approval dialog: a manual version.
	decided := *v.loc
	decided.Decisions = append(slices.Clone(decided.Decisions), language.Decision{Group: "country:ES", Action: language.ActionExclude})
	next, err := store.EditVersionWithRule(context.Background(), id, v.id, campaign.Edit{Name: "Çok dilli", Localization: &decided},
		segment.DefaultCatalog(), demoNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := approve(t, db, id, next.VersionID)
	if err != nil {
		t.Fatalf("approve after the decision: %v", err)
	}
	if got, was := res.Languages["tr"]["sms"].Selected, before.Languages["tr"]["sms"].Selected; got != was+2 {
		t.Fatalf("Turkish SMS = %d, want %d (Omar and Ayşe since the preview)", got, was+2)
	}
}

func ptr(s string) *string { return &s }
