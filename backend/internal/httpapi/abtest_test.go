package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"smart-campaign-composer/backend/internal/llm/mock"
)

// A/B over the API: a manual draft carries its B side, an edit keeps it
// unless it sends one (null removes it), a broken B is refused with a
// translated issue, and the approval, its preview and the campaign list
// split their numbers by arm.
func TestCampaignEndpoints_ABTest(t *testing.T) {
	e := newEnv(t, mock.NewScripted())
	ctx := context.Background()
	for i := range 12 {
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO users (first_name, last_name, phone, country_code, admin1_code, city_geoname_id,
			birth_date, consent_sms) VALUES ($1,'Yılmaz',$2,'TR','TR-34',745044,'1990-01-01',true)`,
			fmt.Sprintf("Kişi%d", i), fmt.Sprintf("+90555%07d", i)); err != nil {
			t.Fatal(err)
		}
	}
	single := map[string]any{"mode": "single", "language": "tr"}
	b := func(text string) map[string]any {
		return map[string]any{"variants": []map[string]any{{"language": "tr", "sms": map[string]string{"text": text}}}}
	}
	code, body := e.do(t, http.MethodPost, "/api/campaigns", map[string]any{"name": "A/B",
		"segment_rule": map[string]any{"match": "all", "conditions": []map[string]any{{"kind": "location", "operator": "in",
			"places": []map[string]any{{"level": "city", "geoname_id": 745044, "country": "TR"}}}}},
		"sms": map[string]string{"text": "A {{first_name}}"}, "localization": single, "ab_test": b("B {{first_name}}")})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	base := "/api/campaigns/" + body["campaign_id"].(string)
	v1 := int64(body["version_id"].(float64))
	abOf := func() map[string]any {
		_, got := e.do(t, http.MethodGet, base, nil)
		cur, _ := got["current_version"].(map[string]any)
		ab, _ := cur["ab_test"].(map[string]any)
		return ab
	}
	if ab := abOf(); ab == nil || len(ab["variants"].([]any)) != 1 {
		t.Fatalf("current version ab_test = %v", ab)
	}

	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{"base_version_id": v1, "name": "A/B",
		"sms": map[string]string{"text": "A2 {{first_name}}"}, "localization": single})
	if code != http.StatusCreated || abOf() == nil {
		t.Fatalf("edit without ab_test: %d %v; the A/B setup must stay", code, body)
	}
	v2 := int64(body["version_id"].(float64))

	code, body = e.doLang(t, http.MethodPost, base+"/versions", map[string]any{"base_version_id": v2, "name": "A/B",
		"sms": map[string]string{"text": "A2 {{first_name}}"}, "localization": single, "ab_test": b("A2 {{first_name}}")}, "en")
	issues, _ := body["issues"].([]any)
	var first map[string]any
	if len(issues) > 0 {
		first, _ = issues[0].(map[string]any)
	}
	if code != http.StatusUnprocessableEntity || first["code"] != "same_as_a" ||
		first["message"] != "the B variant is the same as A; at least one text must differ" {
		t.Fatalf("B equal to A: %d %v", code, body)
	}

	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{"base_version_id": v2, "name": "A/B",
		"sms": map[string]string{"text": "A2 {{first_name}}"}, "localization": single, "ab_test": nil})
	if code != http.StatusCreated || abOf() != nil {
		t.Fatalf("edit with ab_test null: %d %v; the A/B setup must go", code, body)
	}
	v3 := int64(body["version_id"].(float64))
	code, body = e.do(t, http.MethodPost, base+"/versions", map[string]any{"base_version_id": v3, "name": "A/B",
		"sms": map[string]string{"text": "A2 {{first_name}}"}, "localization": single, "ab_test": b("B2 {{first_name}}")})
	if code != http.StatusCreated {
		t.Fatalf("set again: %d %v", code, body)
	}
	v4 := int64(body["version_id"].(float64))

	code, body = e.do(t, http.MethodGet, base+"/approval-preview?version_id="+strconv.FormatInt(v4, 10), nil)
	variants, _ := body["variants"].(map[string]any)
	arms := map[string]bool{}
	for _, s := range body["samples"].([]any) {
		arms[fmt.Sprint(s.(map[string]any)["variant"])] = true
	}
	if code != http.StatusOK || len(variants) != 2 || !arms["A"] || !arms["B"] {
		t.Fatalf("preview: %d variants %v samples %v", code, variants, body["samples"])
	}

	code, body = e.do(t, http.MethodPost, base+"/approve", map[string]any{"version_id": v4})
	variants, _ = body["variants"].(map[string]any)
	total := 0
	for _, arm := range []string{"A", "B"} {
		sms, _ := variants[arm].(map[string]any)["sms"].(map[string]any)
		total += int(sms["selected"].(float64))
	}
	if code != http.StatusOK || total != 12 {
		t.Fatalf("approve: %d %v; want 12 selected across both arms", code, body)
	}
	code, body = e.do(t, http.MethodGet, "/api/campaigns?id="+base[len("/api/campaigns/"):], nil)
	list, _ := body["campaigns"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["variants"] == nil {
		t.Fatalf("campaign list: %d %v; want the per-arm numbers", code, body)
	}
}
