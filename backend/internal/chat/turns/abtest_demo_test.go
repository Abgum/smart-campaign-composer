package turns_test

import (
	"context"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/testutil"
)

func currentABTest(t *testing.T, db *testutil.DB, campaignID int64) (int64, *abtest.Test) {
	t.Helper()
	var (
		id int64
		ab *abtest.Test
	)
	if err := db.Pool.QueryRow(context.Background(), `
		SELECT v.id, v.ab_test FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id WHERE c.id = $1`,
		campaignID).Scan(&id, &ab); err != nil {
		t.Fatal(err)
	}
	return id, ab
}

// An A/B test played by the demo model through real turns: a new campaign
// with a B side, then, on a campaign in each recipient's language, a B for
// every language. Go checks each B; approval gives every recipient an arm.
func TestDemo_ABTest(t *testing.T) {
	db := testutil.NewDB(t)
	addLanguageUsers(t, db)
	addCartLeaver(t, db, "Ayşe", ptr("tr"))
	runner, chat := newDemoRunner(t, db), newChat(t, db)

	res := runFocused(t, runner, chat, "Sepette ürün bırakanlara A/B testi hazırla #scenario:ab_test", nil)
	if len(res.CampaignRefs) != 1 {
		t.Fatalf("turn = %+v", res)
	}
	id := res.CampaignRefs[0].CampaignID
	versionID, ab := currentABTest(t, db, id)
	if ab == nil || len(ab.Variants) != 1 || ab.Variants[0].Language != "tr" || ab.Variants[0].SMS == nil {
		t.Fatalf("B side = %+v; want one Turkish B", ab)
	}
	if m := res.AssistantMessage.Text; !strings.Contains(m, "A/B") {
		t.Fatalf("reply = %q; want it to name the A/B test", m)
	}

	focus := id
	res = runFocused(t, runner, chat, "Herkese kendi dilinde gönder #scenario:multilingual", &focus)
	res = runFocused(t, runner, chat, "Buna da A/B testi ekle #scenario:ab_test", &focus)
	versionID, ab = currentABTest(t, db, id)
	if ab == nil || len(ab.Variants) != 3 {
		t.Fatalf("B side after languages = %+v; want a B for each of the three languages", ab)
	}
	if _, err := approve(t, db, id, versionID); err == nil {
		// Groups wait for a decision (Canada, Germany): nothing is queued yet.
		t.Fatal("approval went through with recipients waiting for a language decision")
	}
	res = runFocused(t, runner, chat, "Bekleyenlerin hepsine İngilizce gönder #scenario:pending_to_english", &focus)
	versionID, ab = currentABTest(t, db, id)
	if ab == nil || len(ab.Variants) != 3 {
		t.Fatalf("a later change dropped the A/B test: %+v", ab)
	}
	r, err := approve(t, db, id, versionID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(r.Variants) == 0 {
		t.Fatalf("approval = %+v; want counts per arm", r)
	}
	if n := count(t, db, `SELECT count(*) FROM deliveries WHERE campaign_id = $1 AND ab_variant IS NULL`, id); n != 0 {
		t.Fatalf("%d deliveries have no arm", n)
	}
}

// "A/B testini kaldır", played by the demo model: it makes the separate
// removal call, which takes the test off and leaves the content as it was.
// On a campaign without a test it says so and writes nothing.
func TestDemo_ABTest_Remove(t *testing.T) {
	db := testutil.NewDB(t)
	addCartLeaver(t, db, "Ayşe", ptr("tr"))
	runner, chat := newDemoRunner(t, db), newChat(t, db)
	res := runFocused(t, runner, chat, "Sepette ürün bırakanlara A/B testi hazırla #scenario:ab_test", nil)
	if len(res.CampaignRefs) != 1 {
		t.Fatalf("turn = %+v", res)
	}
	id := res.CampaignRefs[0].CampaignID
	before := currentVersion(t, db, id)

	focus := id
	res = runFocused(t, runner, chat, "A/B testini kaldır #scenario:remove_ab_test", &focus)
	if res.Outcome != "ok" || len(res.CampaignRefs) != 1 {
		t.Fatalf("turn = %+v", res)
	}
	if _, ab := currentABTest(t, db, id); ab != nil {
		t.Fatalf("B side = %+v; want the test removed", ab)
	}
	after := currentVersion(t, db, id)
	if after.Version != before.Version+1 || string(after.SMS) != string(before.SMS) || string(after.Email) != string(before.Email) {
		t.Fatalf("version %d = %+v; want the next version with the same content", after.Version, after)
	}
	if m := res.AssistantMessage.Text; !strings.Contains(m, "A/B testi kaldırıldı") {
		t.Fatalf("reply = %q; want it to say the test was removed", m)
	}

	res = runFocused(t, runner, chat, "A/B testini kaldır #scenario:remove_ab_test", &focus)
	if len(res.CampaignRefs) != 0 || currentVersion(t, db, id).Version != after.Version ||
		!strings.Contains(res.AssistantMessage.Text, "A/B testi yok") {
		t.Fatalf("turn = %+v; want an answer that there is no test, and nothing written", res)
	}
}
