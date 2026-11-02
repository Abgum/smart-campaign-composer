package campaign_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/testutil"
)

// The A side of these drafts is one Turkish content on both channels; keepB
// is its B side.
var (
	keepSMS   = &content.SMS{Text: "Merhaba {{first_name}}, sepetin seni bekliyor!"}
	keepEmail = &content.Email{Subject: "Sepetin seni bekliyor", Body: "Ürünlerin hâlâ sepetinde."}
	keepB     = &abtest.Test{Variants: []language.Variant{{Language: "tr",
		SMS:   &content.SMS{Text: "Son gün {{first_name}}! Sepetin seni bekliyor."},
		Email: &content.Email{Subject: "Son gün: sepetin seni bekliyor", Body: "Ürünlerin hâlâ sepetinde."}}}}
)

func turkish() *language.Settings {
	return &language.Settings{Mode: language.ModeSingle, Language: "tr"}
}

// abCampaign writes version 1 of an A/B draft the way a chat turn does and
// returns the turn's writer with it.
func abCampaign(t *testing.T, db *testutil.DB) (*campaign.DraftWriter, campaign.Saved) {
	t.Helper()
	chatID, turnID := newChatTurn(t, db)
	w := campaign.NewStore(db.Pool).Drafts(campaign.Author{ChatID: chatID, TurnID: turnID, Source: "llm", Provider: "mock", Model: "demo"})
	s, err := w.SaveDraft(context.Background(), campaign.DraftCmd{Name: "Sepet A/B", Rule: istanbul(), SMS: keepSMS, Email: keepEmail,
		Audience: segment.Audience{Total: 3}, ChangeSummary: "ilk", Localization: turkish(), ABTest: keepB})
	if err != nil {
		t.Fatal(err)
	}
	return w, s
}

// revision changes s's A side and sends no A/B test.
func revision(s campaign.Saved, sms *content.SMS, email *content.Email, loc *language.Settings) campaign.DraftCmd {
	return campaign.DraftCmd{CampaignID: &s.CampaignID, BaseVersion: &s.Version, Name: "Sepet A/B", Rule: istanbul(),
		SMS: sms, Email: email, Audience: segment.Audience{Total: 3}, ChangeSummary: "değişti", Localization: loc}
}

func abTestOf(t *testing.T, db *testutil.DB, versionID int64) *abtest.Test {
	t.Helper()
	return scalar[*abtest.Test](t, db, `SELECT ab_test FROM campaign_versions WHERE id = $1`, versionID)
}

// A revision that sends no A/B test keeps the base version's B side: a
// draft that leaves the test out never removes it.
func TestSaveDraft_RevisionWithoutABTestKeepsIt(t *testing.T) {
	db := testutil.NewDB(t)
	w, v1 := abCampaign(t, db)
	v2, err := w.SaveDraft(context.Background(), revision(v1, &content.SMS{Text: "Merhaba {{first_name}}, sepetini unutma!"}, keepEmail, turkish()))
	if err != nil {
		t.Fatal(err)
	}
	if got := abTestOf(t, db, v2.VersionID); !reflect.DeepEqual(got, keepB) {
		t.Fatalf("v2 ab_test = %+v; want the base version's B side kept", got)
	}
	if !v2.ABTestKept {
		t.Error("Saved.ABTestKept = false; the writer must tell that it kept the test")
	}
}

// The kept B side is checked against the revision's A contents under the
// row lock: when it no longer fits, nothing is written and the error says
// why, so the model can send an updated B.
func TestSaveDraft_KeptABTestMustFitTheNewA(t *testing.T) {
	cases := []struct {
		name  string
		sms   *content.SMS
		email *content.Email
		loc   *language.Settings
		code  string
	}{
		{"A drops a channel", keepSMS, nil, turkish(), "channel_mismatch"},
		{"A changes language", keepSMS, keepEmail, &language.Settings{Mode: language.ModeSingle, Language: "en"}, "missing_variant"},
		{"A becomes B", keepB.Variants[0].SMS, keepB.Variants[0].Email, turkish(), "same_as_a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := testutil.NewDB(t)
			w, v1 := abCampaign(t, db)
			_, err := w.SaveDraft(context.Background(), revision(v1, c.sms, c.email, c.loc))
			var out *campaign.OutdatedABTestError
			if !errors.As(err, &out) || !slices.ContainsFunc(out.Issues, func(is campaign.Issue) bool { return is.Code == c.code }) {
				t.Fatalf("err = %v; want the kept B refused with %s", err, c.code)
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM campaign_versions WHERE campaign_id = $1`, v1.CampaignID); n != 1 {
				t.Fatalf("%d versions; a refused revision must write nothing", n)
			}
		})
	}
}

// Only an explicit drop (the editor's A/B switch turned off) takes the test
// off a revision.
func TestSaveDraft_DropABTest(t *testing.T) {
	db := testutil.NewDB(t)
	w, v1 := abCampaign(t, db)
	cmd := revision(v1, keepSMS, keepEmail, turkish())
	cmd.DropABTest = true
	v2, err := w.SaveDraft(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := abTestOf(t, db, v2.VersionID); got != nil || v2.ABTestKept {
		t.Fatalf("v2 ab_test = %+v (kept %v); want none", got, v2.ABTestKept)
	}
}

// RemoveABTest is a turn's one way to remove an A/B test: a new version of
// the draft that is its base without the B side, written by the turn.
func TestRemoveABTest(t *testing.T) {
	db := testutil.NewDB(t)
	ctx := context.Background()
	w, v1 := abCampaign(t, db)
	r, err := w.RemoveABTest(ctx, v1.CampaignID, v1.Version, "A/B testi kaldırıldı")
	if err != nil {
		t.Fatal(err)
	}
	if r.CampaignID != v1.CampaignID || r.Version != 2 || r.Created || r.Audience.Total != 3 {
		t.Fatalf("removed = %+v", r)
	}
	if got := abTestOf(t, db, r.VersionID); got != nil {
		t.Fatalf("v2 ab_test = %+v; want none", got)
	}
	unchanged := scalar[bool](t, db, `
		SELECT a.name = b.name AND a.segment_rule = b.segment_rule AND a.sms = b.sms AND a.email = b.email
		   AND a.audience = b.audience AND a.localization = b.localization
		   AND a.language_summary IS NOT DISTINCT FROM b.language_summary
		FROM campaign_versions a, campaign_versions b WHERE a.id = $1 AND b.id = $2`, v1.VersionID, r.VersionID)
	if !unchanged {
		t.Fatal("the removal changed more than the A/B test")
	}
	written := scalar[string](t, db, `
		SELECT v.source || '|' || v.change_summary || '|' || (c.current_version_id = v.id) || '|' ||
		       (SELECT action FROM turn_campaign_refs WHERE version_id = v.id)
		FROM campaign_versions v JOIN campaigns c ON c.id = v.campaign_id WHERE v.id = $1`, r.VersionID)
	if written != "llm|A/B testi kaldırıldı|true|revised" {
		t.Fatalf("version = %s; want the turn's current version", written)
	}

	if _, err := w.RemoveABTest(ctx, v1.CampaignID, r.Version, "yine"); !errors.Is(err, campaign.ErrNoABTest) {
		t.Fatalf("removing again: err = %v, want ErrNoABTest", err)
	}
	var stale *campaign.StaleError
	if _, err := w.RemoveABTest(ctx, v1.CampaignID, v1.Version, "eski"); !errors.As(err, &stale) || stale.CurrentVersion != 2 {
		t.Fatalf("from an old version: err = %v, want stale (current 2)", err)
	}
}
