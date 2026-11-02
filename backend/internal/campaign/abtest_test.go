package campaign_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// person adds an İstanbul recipient with consent and contact on both
// channels and the given recorded language ("" for none).
func person(t *testing.T, db *testutil.DB, first, lang string) int64 {
	t.Helper()
	var pref *string
	if lang != "" {
		pref = &lang
	}
	n := randDigits(7)
	return scalar[int64](t, db, `
		INSERT INTO users (first_name, last_name, email, phone, country_code, admin1_code, city_geoname_id,
		                   birth_date, consent_sms, consent_email, language)
		VALUES ($1, 'Yılmaz', $2, $3, 'TR', 'TR-34', 745044, '1990-01-01', true, true, $4) RETURNING id`,
		first, "kisi"+n+"@example.com", "+90533"+n, pref)
}

func saveAB(t *testing.T, db *testutil.DB, cmd campaign.DraftCmd) campaign.Saved {
	t.Helper()
	chatID, _ := newChatTurn(t, db)
	if cmd.Name == "" {
		cmd.Name = "A/B"
	}
	cmd.Rule, cmd.ChangeSummary = istanbul(), "ilk"
	s, err := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type sentRow struct {
	user               int64
	channel, arm, lang string
	subject, body      string
}

func deliveries(t *testing.T, db *testutil.DB, campaignID int64) []sentRow {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), `
		SELECT user_id, channel, coalesce(ab_variant, ''), coalesce(language, ''), coalesce(subject, ''), body
		FROM deliveries WHERE campaign_id = $1 ORDER BY user_id, channel`, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sentRow
	for rows.Next() {
		var r sentRow
		if err := rows.Scan(&r.user, &r.channel, &r.arm, &r.lang, &r.subject, &r.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// Every recipient gets one arm, the same on SMS and email: the one
// abtest.Arm gives, with that arm's content. The approval counts and keeps
// each arm's numbers.
func TestApprove_ABTest_OneArmPerRecipientOnBothChannels(t *testing.T) {
	db := testutil.NewDB(t)
	users := map[int64]string{}
	for i := range 16 {
		first := fmt.Sprintf("Kişi%d", i)
		users[person(t, db, first, "")] = first
	}
	s := saveAB(t, db, campaign.DraftCmd{
		SMS: &content.SMS{Text: "A {{first_name}}"}, Email: &content.Email{Subject: "A konu", Body: "A gövde"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest: &abtest.Test{Variants: []language.Variant{{Language: "tr",
			SMS: &content.SMS{Text: "B {{first_name}}"}, Email: &content.Email{Subject: "B konu", Body: "B gövde"}}}},
	})

	res, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	byUser := map[int64]string{}
	rows := deliveries(t, db, s.CampaignID)
	if len(rows) != 2*len(users) {
		t.Fatalf("deliveries = %d, want %d", len(rows), 2*len(users))
	}
	for _, r := range rows {
		arm := abtest.Arm(s.CampaignID, r.user)
		if r.arm != arm {
			t.Fatalf("user %d %s got arm %q, want %q", r.user, r.channel, r.arm, arm)
		}
		if prev, ok := byUser[r.user]; ok && prev != r.arm {
			t.Fatalf("user %d got %s on one channel and %s on the other", r.user, prev, r.arm)
		}
		byUser[r.user] = r.arm
		wantBody, wantSubject := arm+" "+users[r.user], ""
		if r.channel == "email" {
			wantBody, wantSubject = arm+" gövde", arm+" konu"
		}
		if r.body != wantBody || r.subject != wantSubject {
			t.Fatalf("user %d %s (%s) = %q / %q, want %q / %q", r.user, r.channel, arm, r.subject, r.body, wantSubject, wantBody)
		}
	}
	for _, arm := range byUser {
		want[arm]++
	}
	for _, arm := range []string{abtest.A, abtest.B} {
		for _, ch := range []string{"sms", "email"} {
			if got := res.Variants[arm][ch]; got != (campaign.ChannelCounts{Selected: want[arm]}) {
				t.Errorf("variants[%s][%s] = %+v, want %d selected", arm, ch, got, want[arm])
			}
		}
	}

	again, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyApproved || fmt.Sprint(again.Variants) != fmt.Sprint(res.Variants) {
		t.Fatalf("approve again variants = %v, want the stored %v", again.Variants, res.Variants)
	}
}

// With content per language, the language comes first and the arm picks A
// or B within it: a Turkish reader in B gets the Turkish B.
func TestApprove_ABTest_WithLanguages(t *testing.T) {
	db := testutil.NewDB(t)
	for i := range 8 {
		person(t, db, fmt.Sprintf("Ayşe%d", i), "tr")
		person(t, db, fmt.Sprintf("Anne%d", i), "en")
	}
	s := saveAB(t, db, campaign.DraftCmd{
		Localization: &language.Settings{Mode: language.ModePerRecipient, Variants: []language.Variant{
			{Language: "tr", SMS: &content.SMS{Text: "TR-A"}}, {Language: "en", SMS: &content.SMS{Text: "EN-A"}}}},
		ABTest: &abtest.Test{Variants: []language.Variant{
			{Language: "tr", SMS: &content.SMS{Text: "TR-B"}}, {Language: "en", SMS: &content.SMS{Text: "EN-B"}}}},
	})

	res, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	perArm := map[string]int{}
	for _, r := range deliveries(t, db, s.CampaignID) {
		want := strings.ToUpper(r.lang) + "-" + abtest.Arm(s.CampaignID, r.user)
		if r.body != want || r.arm != abtest.Arm(s.CampaignID, r.user) {
			t.Fatalf("user %d (%s, %s) got %q, want %q", r.user, r.lang, r.arm, r.body, want)
		}
		perArm[r.arm]++
	}
	if res.Languages["tr"]["sms"].Selected != 8 || res.Languages["en"]["sms"].Selected != 8 {
		t.Fatalf("languages = %v, want 8 each", res.Languages)
	}
	for _, arm := range []string{abtest.A, abtest.B} {
		if res.Variants[arm]["sms"].Selected != perArm[arm] {
			t.Fatalf("variants = %v, want %v", res.Variants, perArm)
		}
	}
}

// The channel limit holds for B's rendered messages too: a B over the
// limit stops the approval and points at B.
func TestApprove_ABTest_RenderedLimitInB(t *testing.T) {
	db := testutil.NewDB(t)
	long := strings.Repeat("Ç", 30)
	var ids []int64
	for range 12 {
		ids = append(ids, person(t, db, long, ""))
	}
	s := saveAB(t, db, campaign.DraftCmd{
		SMS:          &content.SMS{Text: "Merhaba {{first_name}}"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest: &abtest.Test{Variants: []language.Variant{{Language: "tr",
			SMS: &content.SMS{Text: strings.Repeat("x", 140) + " {{first_name}}"}}}},
	})
	inB := 0
	for _, id := range ids {
		if abtest.Arm(s.CampaignID, id) == abtest.B {
			inB++
		}
	}
	if inB == 0 {
		t.Fatal("setup: nobody in B")
	}
	_, err := approve(t, db, s)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) != 1 || ve.Issues[0].Path != "ab_test.variants[0].sms.text" || ve.Issues[0].Code != "too_long" {
		t.Fatalf("err = %v, want one too_long issue on B's SMS", err)
	}
	if !strings.HasPrefix(ve.Issues[0].Message, fmt.Sprintf("%d alıcıda", inB)) {
		t.Fatalf("message = %q, want the %d recipients in B counted", ve.Issues[0].Message, inB)
	}
}

// Content dedupe works per arm: recipients whose arm repeats a message they
// already got are left out; the other arm's message still goes.
func TestApprove_ABTest_DuplicatesCountedPerArm(t *testing.T) {
	db := testutil.NewDB(t)
	var ids []int64
	for i := range 16 {
		ids = append(ids, person(t, db, fmt.Sprintf("Kişi%d", i), ""))
	}
	first := saveAB(t, db, campaign.DraftCmd{SMS: &content.SMS{Text: "Ortak metin"}})
	if _, err := approve(t, db, first); err != nil {
		t.Fatal(err)
	}
	s := saveAB(t, db, campaign.DraftCmd{
		SMS:          &content.SMS{Text: "Ortak metin"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest:       &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "Yeni metin"}}}},
	})
	nA := 0
	for _, id := range ids {
		if abtest.Arm(s.CampaignID, id) == abtest.A {
			nA++
		}
	}
	res, err := approve(t, db, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Variants[abtest.A]["sms"]; got != (campaign.ChannelCounts{ExcludedDuplicate: nA}) {
		t.Errorf("A = %+v, want all %d left out as duplicates", got, nA)
	}
	if got := res.Variants[abtest.B]["sms"]; got != (campaign.ChannelCounts{Selected: len(ids) - nA}) {
		t.Errorf("B = %+v, want %d selected", got, len(ids)-nA)
	}
	if got := res.Channels["sms"]; got != (campaign.ChannelCounts{Selected: len(ids) - nA, ExcludedDuplicate: nA}) {
		t.Errorf("sms = %+v", got)
	}
}

// The preview splits the counts by arm and shows a message of each arm.
func TestPreviewApproval_ABTest(t *testing.T) {
	db := testutil.NewDB(t)
	var ids []int64
	for i := range 16 {
		ids = append(ids, person(t, db, fmt.Sprintf("Kişi%d", i), ""))
	}
	s := saveAB(t, db, campaign.DraftCmd{
		SMS:          &content.SMS{Text: "A {{first_name}}"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest:       &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "B {{first_name}}"}}}},
	})
	p, err := campaign.NewStore(db.Pool).PreviewApproval(context.Background(), s.CampaignID, s.VersionID, segment.DefaultCatalog(), approveNow)
	if err != nil {
		t.Fatal(err)
	}
	nA := 0
	for _, id := range ids {
		if abtest.Arm(s.CampaignID, id) == abtest.A {
			nA++
		}
	}
	if p.Variants[abtest.A]["sms"].Selected != nA || p.Variants[abtest.B]["sms"].Selected != len(ids)-nA {
		t.Fatalf("preview variants = %v, want A %d, B %d", p.Variants, nA, len(ids)-nA)
	}
	arms := map[string]string{}
	for _, sm := range p.Samples {
		arms[sm.Variant] = sm.Body
	}
	if !strings.HasPrefix(arms[abtest.A], "A ") || !strings.HasPrefix(arms[abtest.B], "B ") {
		t.Fatalf("samples = %+v, want one per arm", p.Samples)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("a preview wrote %d deliveries", n)
	}
}

// Approval checks B again: a B that no longer mirrors A stops it.
func TestApprove_ABTest_RevalidatesB(t *testing.T) {
	db := testutil.NewDB(t)
	person(t, db, "Ayşe", "")
	s := saveAB(t, db, campaign.DraftCmd{
		SMS: &content.SMS{Text: "A"}, Email: &content.Email{Subject: "A", Body: "A"},
		Localization: &language.Settings{Mode: language.ModeSingle, Language: "tr"},
		ABTest:       &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "B"}}}},
	})
	_, err := approve(t, db, s)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || len(ve.Issues) != 1 || ve.Issues[0].Code != "channel_mismatch" {
		t.Fatalf("err = %v, want channel_mismatch", err)
	}
}

// A manual edit keeps the A/B setup unless it sends one; it can set,
// replace or remove it, and a broken B is refused.
func TestEditVersion_ABTest(t *testing.T) {
	db := testutil.NewDB(t)
	person(t, db, "Ayşe", "")
	cat := segment.DefaultCatalog()
	store := campaign.NewStore(db.Pool)
	ctx := context.Background()
	chatID, _ := newChatTurn(t, db)
	rule := istanbul()
	b := &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "B"}}}}
	single := &language.Settings{Mode: language.ModeSingle, Language: "tr"}
	v1, err := store.CreateManual(ctx, chatID, campaign.Edit{Name: "AB", Rule: &rule, SMS: &content.SMS{Text: "A"},
		Localization: single, ABTest: b, ABTestSet: true}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	hasAB := func(versionID int64) bool {
		return scalar[bool](t, db, `SELECT ab_test IS NOT NULL FROM campaign_versions WHERE id = $1`, versionID)
	}
	if !hasAB(v1.VersionID) {
		t.Fatal("created without its A/B setup")
	}
	v2, err := store.EditVersionWithRule(ctx, v1.CampaignID, v1.VersionID, campaign.Edit{Name: "AB", SMS: &content.SMS{Text: "A2"}}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	if !hasAB(v2.VersionID) {
		t.Fatal("an edit that sent no A/B setup dropped it")
	}
	_, err = store.EditVersionWithRule(ctx, v1.CampaignID, v2.VersionID, campaign.Edit{Name: "AB", SMS: &content.SMS{Text: "A2"},
		ABTest: &abtest.Test{Variants: []language.Variant{{Language: "tr", SMS: &content.SMS{Text: "A2"}}}}, ABTestSet: true}, cat, approveNow)
	var ve *campaign.ValidationError
	if !errors.As(err, &ve) || ve.Issues[0].Code != "same_as_a" {
		t.Fatalf("B equal to A: err = %v, want same_as_a", err)
	}
	v3, err := store.EditVersionWithRule(ctx, v1.CampaignID, v2.VersionID, campaign.Edit{Name: "AB", SMS: &content.SMS{Text: "A2"}, ABTestSet: true}, cat, approveNow)
	if err != nil {
		t.Fatal(err)
	}
	if hasAB(v3.VersionID) {
		t.Fatal("an edit that removed the A/B setup kept it")
	}
}
