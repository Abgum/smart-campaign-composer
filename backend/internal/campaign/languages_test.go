package campaign_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// speaker is a reachable recipient (SMS and e-mail) with a language
// preference (nil: none) living in a country.
type speaker struct {
	first   string
	pref    *string
	country string
}

var places = map[string]struct {
	admin1 string
	city   int64
}{
	"TR": {"TR-34", 745044}, "FR": {"FR-IDF", 2988507}, "CA": {"CA-ON", 6058560}, "DE": {"DE-BE", 2950159},
}

func pref(s string) *string { return &s }

func addSpeaker(t *testing.T, db *testutil.DB, s speaker) int64 {
	t.Helper()
	return addSpeakerWith(t, db, s, true)
}

// addSpeakerWith adds s with or without consent on both channels.
func addSpeakerWith(t *testing.T, db *testutil.DB, s speaker, consent bool) int64 {
	t.Helper()
	var (
		admin1 *string
		city   *int64
	)
	if p, ok := places[s.country]; ok { // a country outside the catalog has no region or city
		admin1, city = &p.admin1, &p.city
	}
	return scalar[int64](t, db, `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id, language,
		                   birth_date, consent_sms, consent_email)
		VALUES ($1, 'Test', $2, $3, $4, $5, $6, $7, '1990-01-01', $8, $8) RETURNING id`,
		s.first, "u"+randDigits(8)+"@example.com", "+90555"+randDigits(7), s.country, admin1, city, s.pref, consent)
}

// adults is everyone born before 2008: every speaker.
func adults() segment.Rule {
	return segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "attribute", Field: "age", Operator: "gte", Values: []string{"18"}}}}
}

func langVariant(lang, greeting string) language.Variant {
	return language.Variant{Language: lang, SMS: &content.SMS{Text: greeting + " {{first_name}}!"},
		Email: &content.Email{Subject: greeting, Body: greeting + " {{first_name}}."}}
}

// threeLanguages has TR, EN and FR variants, maps Türkiye and France, and
// takes the given decisions.
func threeLanguages(decisions ...language.Decision) *language.Settings {
	return &language.Settings{
		Mode:      language.ModePerRecipient,
		Variants:  []language.Variant{langVariant("tr", "Merhaba"), langVariant("en", "Hi"), langVariant("fr", "Bonjour")},
		Countries: []language.CountryLanguage{{Country: "TR", Language: "tr"}, {Country: "FR", Language: "fr"}},
		Decisions: decisions,
	}
}

var (
	germanyToEnglish = language.Decision{Group: "country:DE", Action: language.ActionAssign, Language: "en"}
	leaveOutCanada   = language.Decision{Group: "country:CA", Action: language.ActionExclude}
)

func saveLocalized(t *testing.T, db *testutil.DB, chatID string, campaignID *int64, base *int, loc *language.Settings) campaign.Saved {
	t.Helper()
	cmd := campaign.DraftCmd{CampaignID: campaignID, BaseVersion: base, Name: "Diller", Rule: adults(),
		ChangeSummary: "diller", Localization: loc}
	if loc.Mode == language.ModeSingle {
		cmd.SMS, cmd.Email = &content.SMS{Text: "Hi {{first_name}}!"}, &content.Email{Subject: "Hi", Body: "Hi {{first_name}}."}
	}
	s, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// speakers are one recipient per way a language is worked out.
func speakers(t *testing.T, db *testutil.DB) map[string]int64 {
	ids := map[string]int64{}
	for _, s := range []speaker{
		{"Ayşe", pref("tr"), "TR"},
		{"Deniz", nil, "TR"},        // no preference: Türkiye → tr
		{"Emily", pref("en"), "FR"}, // the preference beats France
		{"Léa", nil, "FR"},          // no preference: France → fr
		{"Élodie", pref("fr"), "TR"},
		{"Anna", pref("de"), "TR"},  // unsupported, so Türkiye's mapping: tr
		{"Jonas", pref("de"), "DE"}, // unsupported, and no mapping for Germany: waits
		{"Noah", nil, "CA"},         // no mapping for Canada: waits
	} {
		ids[s.first] = addSpeaker(t, db, s)
	}
	return ids
}

type sent struct{ language, body string }

func deliveriesByName(t *testing.T, db *testutil.DB, campaignID int64, channel string) map[string]sent {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), `
		SELECT u.first_name, coalesce(d.language, ''), d.body FROM deliveries d JOIN users u ON u.id = d.user_id
		WHERE d.campaign_id = $1 AND d.channel = $2`, campaignID, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sent{}
	for rows.Next() {
		var name string
		var s sent
		if err := rows.Scan(&name, &s.language, &s.body); err != nil {
			t.Fatal(err)
		}
		out[name] = s
	}
	return out
}

func TestApprove_PerRecipient_EachRecipientGetsTheirVariant(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages(germanyToEnglish, leaveOutCanada))
	store := campaign.NewStore(db.Pool)

	res, err := store.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]sent{
		"Ayşe": {"tr", "Merhaba Ayşe!"}, "Deniz": {"tr", "Merhaba Deniz!"}, "Anna": {"tr", "Merhaba Anna!"},
		"Emily": {"en", "Hi Emily!"}, "Jonas": {"en", "Hi Jonas!"},
		"Léa": {"fr", "Bonjour Léa!"}, "Élodie": {"fr", "Bonjour Élodie!"},
	}
	if got := deliveriesByName(t, db, s.CampaignID, "sms"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SMS sent =\n%v\nwant\n%v", got, want)
	}
	for lang, n := range map[string]int{"tr": 3, "en": 2, "fr": 2} {
		if c := res.Languages[lang]["sms"]; c.Selected != n {
			t.Errorf("%s SMS selected = %+v, want %d", lang, c, n)
		}
	}
	if res.Channels["sms"].Selected != 7 || res.Channels["email"].Selected != 7 {
		t.Errorf("channel totals = %+v", res.Channels)
	}
	if want := []language.GroupCount{{Group: "country:CA", Count: 1}}; !slices.Equal(res.ExcludedLanguage, want) {
		t.Errorf("left out by a decision = %+v, want %+v", res.ExcludedLanguage, want)
	}

	again, err := store.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil || !again.AlreadyApproved || !reflect.DeepEqual(again.Languages, res.Languages) ||
		!slices.Equal(again.ExcludedLanguage, res.ExcludedLanguage) {
		t.Fatalf("approving again = %+v, %v; want the stored counts", again, err)
	}
}

// Recipients waiting for a decision stop the approval: nothing is queued
// and the campaign stays a draft.
func TestApprove_WaitingRecipients_NothingQueued(t *testing.T) {
	db := testutil.NewDB(t)
	speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())

	_, err := campaign.NewStore(db.Pool).Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	var de *campaign.DecisionError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want a decision request", err)
	}
	if want := []language.GroupCount{{Group: "country:CA", Count: 1}, {Group: "country:DE", Count: 1}}; !slices.Equal(de.Groups, want) {
		t.Fatalf("waiting = %+v, want %+v", de.Groups, want)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("%d deliveries queued", n)
	}
	if st := scalar[string](t, db, `SELECT status FROM campaigns WHERE id = $1`, s.CampaignID); st != "draft" {
		t.Fatalf("status = %s", st)
	}
}

// The audience and the languages are worked out again at approval. Someone
// who joined a decided group follows its decision; a group that appeared
// since the decisions stops the approval until it is decided too.
func TestApprove_RecomputesLanguagesAtApproval(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages(germanyToEnglish, leaveOutCanada))
	store := campaign.NewStore(db.Pool)

	addSpeaker(t, db, speaker{"Lukas", nil, "DE"})       // the decided German group
	addSpeaker(t, db, speaker{"Omar", pref("ar"), "TR"}) // unsupported: Türkiye's mapping
	addSpeaker(t, db, speaker{"Lucía", nil, "ES"})       // a country nobody decided about
	_, err := store.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	var de *campaign.DecisionError
	if !errors.As(err, &de) || !slices.Equal(de.Groups, []language.GroupCount{{Group: "country:ES", Count: 1}}) {
		t.Fatalf("err = %v, want only the new group country:ES", err)
	}

	base := s.Version
	next := saveLocalized(t, db, chatID, &s.CampaignID, &base, threeLanguages(germanyToEnglish, leaveOutCanada,
		language.Decision{Group: "country:ES", Action: language.ActionAssign, Language: "en"}))
	res, err := store.Approve(ctx, s.CampaignID, next.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	got := deliveriesByName(t, db, s.CampaignID, "sms")
	if got["Lukas"] != (sent{"en", "Hi Lukas!"}) || got["Lucía"] != (sent{"en", "Hi Lucía!"}) || got["Omar"] != (sent{"tr", "Merhaba Omar!"}) ||
		res.Languages["en"]["sms"].Selected != 4 {
		t.Fatalf("Lukas %v, Lucía %v, Omar %v, English %+v", got["Lukas"], got["Lucía"], got["Omar"], res.Languages["en"])
	}
}

// A recipient whose language has no variant waits too; the draft gets the
// variant in a new version and waits for approval again.
func TestApprove_MissingVariant_WaitsForTheVariant(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	addSpeaker(t, db, speaker{"Ayşe", pref("tr"), "TR"})
	addSpeaker(t, db, speaker{"Emily", pref("en"), "FR"})
	chatID, _ := newChatTurn(t, db)
	noEnglish := threeLanguages()
	noEnglish.Variants = slices.DeleteFunc(noEnglish.Variants, func(v language.Variant) bool { return v.Language == "en" })
	s := saveLocalized(t, db, chatID, nil, nil, noEnglish)
	store := campaign.NewStore(db.Pool)

	_, err := store.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	var de *campaign.DecisionError
	if !errors.As(err, &de) || !slices.Equal(de.Groups, []language.GroupCount{{Group: "variant:en", Count: 1}}) {
		t.Fatalf("err = %v, want variant:en waiting", err)
	}
	base := s.Version
	next := saveLocalized(t, db, chatID, &s.CampaignID, &base, threeLanguages())
	if _, err := store.Approve(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow); err == nil {
		t.Fatal("the old version must not be approvable once the English variant is added")
	}
	if _, err := store.Approve(ctx, s.CampaignID, next.VersionID, segment.DefaultCatalog(), approveNow); err != nil {
		t.Fatal(err)
	}
	if got := deliveriesByName(t, db, s.CampaignID, "sms")["Emily"]; got != (sent{"en", "Hi Emily!"}) {
		t.Fatalf("Emily got %v", got)
	}
}

// One content goes to everyone whatever their preference, and a missing
// first name is filled in the content's language.
func TestApprove_SingleLanguage_EveryoneGetsIt(t *testing.T) {
	db := testutil.NewDB(t)
	speakers(t, db)
	addSpeaker(t, db, speaker{"", pref("tr"), "TR"})
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, &language.Settings{Mode: language.ModeSingle, Language: "en"})

	res, err := campaign.NewStore(db.Pool).Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	got := deliveriesByName(t, db, s.CampaignID, "sms")
	if len(got) != 9 || got["Anna"] != (sent{"en", "Hi Anna!"}) || got["Noah"] != (sent{"en", "Hi Noah!"}) ||
		got[""] != (sent{"en", "Hi Valued Customer!"}) {
		t.Fatalf("sent = %v", got)
	}
	if res.Languages["en"]["sms"].Selected != 9 || len(res.ExcludedLanguage) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// The preview counts what approving would do now and writes nothing: per
// language and channel, who waits, who a decision leaves out, who already
// got the same message, and one rendered message per language and channel.
func TestPreviewApproval_CountsWithoutWriting(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	ids := speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages(leaveOutCanada))
	// Ayşe already got the Turkish SMS from another campaign.
	other := scalar[int64](t, db, `INSERT INTO campaigns (chat_id, ordinal, name, status) VALUES ($1, 9, 'Önceki', 'completed') RETURNING id`, chatID)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO deliveries (campaign_id, user_id, channel, body, content_hash, status)
		VALUES ($1, $2, 'sms', 'Merhaba Ayşe!', $3, 'sent')`, other, ids["Ayşe"], content.Hash("sms", "", "Merhaba Ayşe!")); err != nil {
		t.Fatal(err)
	}

	p, err := campaign.NewStore(db.Pool).PreviewApproval(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Languages["tr"]["sms"]; c.Selected != 2 || c.ExcludedDuplicate != 1 {
		t.Errorf("Turkish SMS = %+v, want 2 selected (Deniz, Anna) and 1 duplicate (Ayşe)", c)
	}
	if want := []language.GroupCount{{Group: "country:DE", Count: 1}}; !slices.Equal(p.Unresolved, want) {
		t.Errorf("waiting = %+v", p.Unresolved)
	}
	if want := []language.GroupCount{{Group: "country:CA", Count: 1}}; !slices.Equal(p.Excluded, want) {
		t.Errorf("left out = %+v", p.Excluded)
	}
	var samples []string
	for _, m := range p.Samples {
		samples = append(samples, m.Language+"/"+m.Channel)
	}
	if want := []string{"en/email", "en/sms", "fr/email", "fr/sms", "tr/email", "tr/sms"}; !slices.Equal(samples, want) {
		t.Errorf("samples = %v, want %v", samples, want)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries WHERE campaign_id = $1`, s.CampaignID); n != 0 {
		t.Fatalf("the preview queued %d deliveries", n)
	}
	if st := scalar[string](t, db, `SELECT status FROM campaigns WHERE id = $1`, s.CampaignID); st != "draft" {
		t.Fatalf("status after preview = %s", st)
	}
}

func versionLanguages(t *testing.T, db *testutil.DB, versionID int64) (*language.Settings, *language.Summary) {
	t.Helper()
	var (
		loc *language.Settings
		sum *language.Summary
	)
	if err := db.Pool.QueryRow(context.Background(), `SELECT localization, language_summary FROM campaign_versions WHERE id = $1`,
		versionID).Scan(&loc, &sum); err != nil {
		t.Fatal(err)
	}
	return loc, sum
}

// A manual edit that sends no language setup keeps the base version's; one
// that sends a setup (a variant's text, a decision made in the approval
// dialog) is checked and the audience's language split is worked out again.
func TestEditVersion_LanguageSetup(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())
	store := campaign.NewStore(db.Pool)
	cat := segment.DefaultCatalog()

	renamed, err := store.EditVersionWithRule(ctx, s.CampaignID, s.VersionID, campaign.Edit{Name: "Diller v2"}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	if loc, _ := versionLanguages(t, db, renamed.VersionID); loc == nil || len(loc.Variants) != 3 {
		t.Fatalf("a rename dropped the variants: %+v", loc)
	}

	decided := threeLanguages(germanyToEnglish, leaveOutCanada)
	decided.Variants[1].SMS = &content.SMS{Text: "Hello {{first_name}}!"}
	edited, err := store.EditVersionWithRule(ctx, s.CampaignID, renamed.VersionID, campaign.Edit{Name: "Diller v3", Localization: decided}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	loc, sum := versionLanguages(t, db, edited.VersionID)
	if loc.Variants[1].SMS.Text != "Hello {{first_name}}!" || len(loc.Decisions) != 2 {
		t.Fatalf("edited setup = %+v", loc)
	}
	if sum == nil || len(sum.Unresolved) != 0 || sum.Languages["en"].Total != 2 {
		t.Fatalf("summary after the decisions = %+v", sum)
	}
	if _, err := store.Approve(ctx, s.CampaignID, edited.VersionID, cat, approveNow); err != nil {
		t.Fatalf("approve after the decisions: %v", err)
	}
	if got := deliveriesByName(t, db, s.CampaignID, "sms")["Jonas"]; got != (sent{"en", "Hello Jonas!"}) {
		t.Fatalf("Jonas got %v", got)
	}
}

func TestEditVersion_RejectsAnInvalidLanguageSetup(t *testing.T) {
	db := testutil.NewDB(t)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())
	bad := threeLanguages(language.Decision{Group: "country:DE", Action: language.ActionAssign, Language: "en"})
	bad.Variants = bad.Variants[:1]
	_, err := campaign.NewStore(db.Pool).EditVersionWithRule(context.Background(), s.CampaignID, s.VersionID,
		campaign.Edit{Name: "Diller", Localization: bad}, segment.DefaultCatalog(), approveNow)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || !slices.ContainsFunc(ve.Issues, func(i campaign.Issue) bool {
		return i.Path == "per_recipient.decisions[0].language" && i.Code == "missing_variant"
	}) {
		t.Fatalf("err = %v, want missing_variant", err)
	}
	// Content of a per-recipient draft lives in its variants only.
	_, err = campaign.NewStore(db.Pool).EditVersionWithRule(context.Background(), s.CampaignID, s.VersionID,
		campaign.Edit{Name: "Diller", SMS: &content.SMS{Text: "Merhaba"}}, segment.DefaultCatalog(), approveNow)
	if !errors.As(err, &ve) || ve.Issues[0].Code != "per_recipient_content" {
		t.Fatalf("err = %v, want per_recipient_content", err)
	}
}

// Recipients the campaign cannot reach on any channel get no message, so
// they do not wait for a language decision and do not stop the approval;
// they count as left out for consent.
func TestApprove_UnreachableRecipientsDoNotWait(t *testing.T) {
	db := testutil.NewDB(t)
	addSpeaker(t, db, speaker{"Ayşe", pref("tr"), "TR"})
	addSpeakerWith(t, db, speaker{"Lucía", nil, "ES"}, false) // no mapping for Spain, and no consent at all
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())
	res, err := campaign.NewStore(db.Pool).Approve(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if c := res.Channels["sms"]; c.Selected != 1 || c.ExcludedConsent != 1 {
		t.Fatalf("SMS = %+v, want Ayşe sent and Lucía left out for consent", c)
	}
}

// The country mapping is the marketer's to change: a manual edit with a new
// mapping is a new version, and nobody from a mapped country waits any more.
func TestEditVersion_CountryMappingByHand(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	speakers(t, db)
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())
	store := campaign.NewStore(db.Pool)
	before, err := store.PreviewApproval(ctx, s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil || len(before.Unresolved) != 2 {
		t.Fatalf("before: waiting = %+v (%v), want Canada and Germany", before.Unresolved, err)
	}
	mapped := threeLanguages()
	mapped.Countries = append(mapped.Countries, language.CountryLanguage{Country: "CA", Language: "fr"},
		language.CountryLanguage{Country: "DE", Language: "en"})
	edited, err := store.EditVersionWithRule(ctx, s.CampaignID, s.VersionID, campaign.Edit{Name: "Diller", Localization: mapped},
		segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	loc, sum := versionLanguages(t, db, edited.VersionID)
	if len(loc.Countries) != 4 || len(sum.Unresolved) != 0 || edited.Version != 2 {
		t.Fatalf("after: v%d, mapping %+v, waiting %+v", edited.Version, loc.Countries, sum.Unresolved)
	}
	if src := scalar[string](t, db, `SELECT source FROM campaign_versions WHERE id = $1`, edited.VersionID); src != "user" {
		t.Fatalf("source = %s, want user", src)
	}
	if _, err := store.Approve(ctx, s.CampaignID, edited.VersionID, segment.DefaultCatalog(), approveNow); err != nil {
		t.Fatal(err)
	}
	got := deliveriesByName(t, db, s.CampaignID, "sms")
	if got["Noah"].language != "fr" || got["Jonas"].language != "en" {
		t.Fatalf("Noah %v, Jonas %v", got["Noah"], got["Jonas"])
	}
}

// A saved draft's language split counts the recipients it can reach then:
// consent and contact on at least one of its channels, each person once in
// a language's total, per channel apart. The same draft saved by hand as
// SMS only, e-mail only and on both; then one content for everyone by SMS.
func TestEditVersion_LanguageCountsWhoTheDraftCanReach(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	addSpeaker(t, db, speaker{"Ayşe", pref("tr"), "TR"}) // both channels
	mehmet := addSpeaker(t, db, speaker{"Mehmet", pref("tr"), "TR"})
	zeynep := addSpeaker(t, db, speaker{"Zeynep", pref("tr"), "TR"})
	addSpeakerWith(t, db, speaker{"Can", pref("tr"), "TR"}, false) // neither
	addSpeaker(t, db, speaker{"Léa", nil, "FR"})                   // French by the mapping
	for q, id := range map[string]int64{
		`UPDATE users SET email = NULL WHERE id = $1`:        mehmet, // no address: SMS only
		`UPDATE users SET consent_sms = false WHERE id = $1`: zeynep, // no SMS consent: e-mail only
	} {
		if _, err := db.Pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	chatID, _ := newChatTurn(t, db)
	s := saveLocalized(t, db, chatID, nil, nil, threeLanguages())
	store := campaign.NewStore(db.Pool)
	cat := segment.DefaultCatalog()

	on := func(sms, email bool) *language.Settings {
		loc := threeLanguages()
		for i := range loc.Variants {
			if !sms {
				loc.Variants[i].SMS = nil
			}
			if !email {
				loc.Variants[i].Email = nil
			}
		}
		return loc
	}
	base := s.VersionID
	for _, c := range []struct {
		name       string
		sms, email bool
		tr, fr     language.Reach
	}{
		{"SMS only", true, false, language.Reach{Total: 2, SMS: 2}, language.Reach{Total: 1, SMS: 1}},
		{"e-mail only", false, true, language.Reach{Total: 2, Email: 2}, language.Reach{Total: 1, Email: 1}},
		{"both: Ayşe once", true, true, language.Reach{Total: 3, SMS: 2, Email: 2}, language.Reach{Total: 1, SMS: 1, Email: 1}},
	} {
		edited, err := store.EditVersionWithRule(ctx, s.CampaignID, base, campaign.Edit{Name: "Diller", Localization: on(c.sms, c.email)}, cat, approveNow)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		base = edited.VersionID
		_, sum := versionLanguages(t, db, edited.VersionID)
		if sum.Languages["tr"] != c.tr || sum.Languages["fr"] != c.fr {
			t.Errorf("%s: Turkish %+v, French %+v; want %+v, %+v", c.name, sum.Languages["tr"], sum.Languages["fr"], c.tr, c.fr)
		}
	}

	rule := adults()
	single, err := store.CreateManual(ctx, chatID, campaign.Edit{Name: "English", Rule: &rule, SMS: &content.SMS{Text: "Hi {{first_name}}!"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "en"}}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, sum := versionLanguages(t, db, single.VersionID); sum.Languages["en"] != (language.Reach{Total: 3, SMS: 3}) {
		t.Errorf("one English SMS for everyone: %+v; want Ayşe, Mehmet and Léa", sum.Languages)
	}
}
