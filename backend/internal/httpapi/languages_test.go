package httpapi_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"smart-campaign-composer/backend/internal/llm/mock"
)

// A campaign that sends each recipient their language, over HTTP: created
// by hand with its variants, its detail shows the setup and the language
// split; the approval preview counts per language and channel and writes
// nothing; approving with a recipient waiting answers 409 with the group;
// a decision saved as a new version unlocks the approval.
func TestCampaignEndpoints_Languages(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	ctx := context.Background()
	chatID := e.createChat(t)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO users (first_name, last_name, phone, country_code, admin1_code, city_geoname_id,
		language, birth_date, consent_sms) VALUES
		('Ayşe','Yılmaz','+905550000001','TR','TR-34',745044,'tr','1990-01-01',true),
		('Anna','Müller','+905550000002','DE','DE-BE',2950159,'de','1990-01-01',true)`); err != nil {
		t.Fatal(err)
	}
	variants := []map[string]any{
		{"language": "tr", "sms": map[string]string{"text": "Merhaba {{first_name}}"}},
		{"language": "en", "sms": map[string]string{"text": "Hi {{first_name}}"}},
	}
	setup := func(decisions ...map[string]any) map[string]any {
		return map[string]any{"mode": "per_recipient", "variants": variants,
			"country_languages": []map[string]string{{"country": "TR", "language": "tr"}}, "decisions": decisions}
	}
	code, body := e.do(t, http.MethodPost, "/api/campaigns", map[string]any{"chat_id": chatID, "name": "Diller",
		"segment_rule": map[string]any{"match": "all", "conditions": []map[string]any{
			{"kind": "attribute", "field": "age", "operator": "gte", "values": []string{"18"}}}},
		"localization": setup()})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	base := "/api/campaigns/" + body["campaign_id"].(string)
	v1 := int64(body["version_id"].(float64))

	code, body = e.do(t, http.MethodGet, base, nil)
	cur, _ := body["current_version"].(map[string]any)
	loc, _ := cur["localization"].(map[string]any)
	sum, _ := cur["language_summary"].(map[string]any)
	waiting, _ := sum["unresolved"].([]any)
	if code != http.StatusOK || loc["mode"] != "per_recipient" || len(waiting) != 1 {
		t.Fatalf("detail: %d %v", code, cur)
	}

	code, body = e.do(t, http.MethodGet, base+"/approval-preview?version_id="+strconv.FormatInt(v1, 10), nil)
	tr, _ := body["languages"].(map[string]any)["tr"].(map[string]any)
	sms, _ := tr["sms"].(map[string]any)
	if code != http.StatusOK || sms["selected"] != float64(1) || len(body["unresolved"].([]any)) != 1 || len(body["samples"].([]any)) != 1 {
		t.Fatalf("preview: %d %v", code, body)
	}

	code, body = e.doLang(t, http.MethodPost, base+"/approve", map[string]any{"version_id": v1}, "en")
	groups, _ := body["groups"].([]any)
	if code != http.StatusConflict || body["error"] != "language_decision_required" || len(groups) != 1 ||
		body["message"] != "sending has not started: some recipients need a language decision" {
		t.Fatalf("approve with a recipient waiting: %d %v", code, body)
	}
	var queued int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued %d (%v)", queued, err)
	}

	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{"base_version_id": v1, "name": "Diller",
		"localization": setup(map[string]any{"group": "country:DE", "action": "assign", "language": "en"})})
	if code != http.StatusCreated {
		t.Fatalf("decision: %d %v", code, body)
	}
	v2 := int64(body["version_id"].(float64))
	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": v2})
	en, _ := body["languages"].(map[string]any)["en"].(map[string]any)
	if code != http.StatusOK || en["sms"].(map[string]any)["selected"] != float64(1) {
		t.Fatalf("approve after the decision: %d %v", code, body)
	}
}
