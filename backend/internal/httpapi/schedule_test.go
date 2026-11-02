package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
)

// The approval takes an optional sending time: a past one is refused with a
// translated issue; a future one is answered, and kept, in UTC.
func TestApproveCampaign_Scheduled(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	ctx := context.Background()
	chatID := e.createChat(t)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO users (first_name, last_name, phone, country_code, admin1_code, city_geoname_id,
		birth_date, consent_sms) VALUES ('Ayşe','Yılmaz','+905550000001','TR','TR-34',745044,'1990-01-01',true)`); err != nil {
		t.Fatal(err)
	}
	saved, err := campaign.NewStore(e.db.Pool).Drafts(campaign.Author{ChatID: chatID, Source: "user"}).SaveDraft(ctx, campaign.DraftCmd{
		Name: "İstanbul", SMS: &content.SMS{Text: "Merhaba {{first_name}}"}, ChangeSummary: "ilk",
		Rule: segment.Rule{Match: "all", Conditions: []segment.Condition{{Kind: "location", Operator: "in",
			Places: []segment.PlaceRef{{Level: "city", GeonameID: 745044, Country: "TR", Name: "İstanbul"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	public := publicCampaignID(t, e, saved.CampaignID)
	base := "/api/campaigns/" + public

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	code, body := e.doLang(t, http.MethodPost, base+"/approve", map[string]any{"version_id": saved.VersionID, "scheduled_at": past}, "en")
	issues, _ := body["issues"].([]any)
	var first map[string]any
	if len(issues) == 1 {
		first, _ = issues[0].(map[string]any)
	}
	if code != http.StatusUnprocessableEntity || first["code"] != "schedule_in_past" || first["message"] != "the sending time must be in the future" {
		t.Fatalf("approve in the past: %d %v", code, body)
	}

	sendAt := time.Now().Add(48 * time.Hour).In(time.FixedZone("TRT", 3*60*60)).Truncate(time.Second)
	want := sendAt.UTC().Format(time.RFC3339)
	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": saved.VersionID, "scheduled_at": sendAt.Format(time.RFC3339)})
	if code != http.StatusOK || body["status"] != "approved" || body["scheduled_at"] != want {
		t.Fatalf("approve scheduled: %d %v; want scheduled_at %s", code, body, want)
	}
	if code, body := e.do(t, http.MethodGet, base, nil); code != http.StatusOK || body["scheduled_at"] != want {
		t.Fatalf("campaign: %d scheduled_at=%v; want %s", code, body["scheduled_at"], want)
	}
	code, body = e.do(t, http.MethodGet, "/api/campaigns?id="+public, nil)
	list, _ := body["campaigns"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["scheduled_at"] != want {
		t.Fatalf("campaign list: %d %v; want scheduled_at %s", code, body, want)
	}
	code, body = e.do(t, http.MethodPost, base+"/cancel", nil)
	if code != http.StatusOK || body["status"] != "cancelled" {
		t.Fatalf("cancel scheduled: %d %v", code, body)
	}
	if code, body := e.do(t, http.MethodGet, base, nil); code != http.StatusOK || body["status"] != "cancelled" {
		t.Fatalf("campaign after cancel: %d %v", code, body)
	}
	var remaining int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, saved.CampaignID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("deliveries after cancel = %d, err = %v", remaining, err)
	}
}
