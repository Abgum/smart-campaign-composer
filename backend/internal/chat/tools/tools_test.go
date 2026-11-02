package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/llm"
)

// The model reads the tool in English: its description and every schema
// description.
func TestSaveDraftTool_English(t *testing.T) {
	tool := SaveDraftTool()
	schema, _ := json.Marshal(tool.InputSchema)
	if s := tool.Description + string(schema); strings.ContainsAny(s, "çğıöşüÇĞİÖŞÜ") || !strings.Contains(tool.Description, "Saves a campaign draft") {
		t.Fatalf("tool is not in English: %s\n%s", tool.Description, schema)
	}
}

func TestSaveDraftTool_Schema(t *testing.T) {
	tool := SaveDraftTool()
	if tool.Name != SaveDraftName || !tool.Strict || tool.EagerInputStreaming || tool.Description == "" {
		t.Fatalf("tool = %+v", tool)
	}
	s := tool.InputSchema
	if s["type"] != "object" || s["additionalProperties"] != false {
		t.Fatalf("schema root = %v", s)
	}
	req, _ := s["required"].([]string)
	for _, f := range []string{"name", "segment_rule", "change_summary", "sms", "email", "content_language"} {
		if !slices.Contains(req, f) {
			t.Errorf("%s must be required, got %v", f, req)
		}
	}
	// Strict mode writes properties in schema order, and an optional field the
	// model passes over can never be added later: with an optional
	// base_version, changes of existing campaigns were refused. Both are
	// required, written first; null means a new campaign.
	if len(req) < 2 || req[0] != "campaign_id" || req[1] != "base_version" {
		t.Errorf("required must start with campaign_id, base_version (the order the model writes them), got %v", req)
	}
	props, _ := s["properties"].(map[string]any)
	for _, f := range []string{"campaign_id", "base_version"} {
		if !slices.Contains(req, f) {
			t.Errorf("%s must be required (null for a new campaign), got %v", f, req)
		}
		types := map[any]bool{}
		prop, _ := props[f].(map[string]any)
		branches, _ := prop["anyOf"].([]any)
		for _, b := range branches {
			types[b.(map[string]any)["type"]] = true
		}
		if len(branches) != 2 || !types["integer"] || !types["null"] {
			t.Errorf("%s must be an integer or null, got %v", f, prop)
		}
	}
	// Every nested object must be closed too, or strict mode accepts extra keys.
	var walk func(path string, node map[string]any)
	walk = func(path string, node map[string]any) {
		if node["type"] == "object" && node["additionalProperties"] != false {
			t.Errorf("%s: object without additionalProperties:false", path)
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for k, v := range props {
				walk(path+"."+k, v.(map[string]any))
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(path+"[]", items)
		}
		if branches, ok := node["anyOf"].([]any); ok {
			for i, branch := range branches {
				walk(path+".anyOf["+string(rune('0'+i))+"]", branch.(map[string]any))
			}
		}
	}
	walk("$", s)
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("schema must be JSON-serializable: %v", err)
	}
	schema, _ := json.Marshal(s)
	if strings.Contains(string(schema), `"admin1"`) || strings.Contains(string(schema), "ISO 3166-2") {
		t.Fatal("new draft tool must not offer administrative-region targeting")
	}
}

func TestSaveDraftTool_RequiredConditionFields(t *testing.T) {
	tool := SaveDraftTool()
	props := tool.InputSchema["properties"].(map[string]any)
	conditions := props["segment_rule"].(map[string]any)["properties"].(map[string]any)["conditions"].(map[string]any)
	condition := conditions["items"].(map[string]any)
	if _, branched := condition["anyOf"]; branched {
		t.Fatal("condition schema must stay flat enough for Claude grammar compilation")
	}
	required := condition["required"].([]string)
	for _, field := range []string{"kind", "field", "operator", "values", "places", "event_type", "within_days"} {
		if !slices.Contains(required, field) {
			t.Errorf("condition must require %s (use an empty value when irrelevant)", field)
		}
	}
	place := condition["properties"].(map[string]any)["places"].(map[string]any)["items"].(map[string]any)
	for _, field := range []string{"level", "name", "code", "geoname_id", "country"} {
		if !slices.Contains(place["required"].([]string), field) {
			t.Errorf("place must require %s (use an empty value when unknown)", field)
		}
	}
	if RemoveABTestTool().EagerInputStreaming {
		t.Fatal("strict remove_ab_test tool must use schema-validated streaming")
	}
}

// Claude limits the total number of optional parameters across strict tools.
// Keep this check on the exact schemas sent together in a main-model call.
func TestStrictTools_OptionalParametersWithinClaudeLimit(t *testing.T) {
	var count func(map[string]any) int
	count = func(node map[string]any) int {
		total := 0
		if props, ok := node["properties"].(map[string]any); ok {
			required, _ := node["required"].([]string)
			for name, value := range props {
				if !slices.Contains(required, name) {
					total++
				}
				total += count(value.(map[string]any))
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			total += count(items)
		}
		if branches, ok := node["anyOf"].([]any); ok {
			for _, branch := range branches {
				total += count(branch.(map[string]any))
			}
		}
		return total
	}

	tools := []llm.Tool{SaveDraftTool(), RemoveABTestTool()}
	total := 0
	for _, tool := range tools {
		if tool.Strict {
			total += count(tool.InputSchema)
		}
	}
	if total > 24 {
		t.Fatalf("strict tool schemas have %d optional parameters; Claude allows at most 24", total)
	}
}

func TestParseSaveDraft(t *testing.T) {
	in, err := ParseSaveDraft([]byte(`{
		"campaign_id": 12, "base_version": 3, "name": "Sepet",
		"segment_rule": {"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
		"sms": {"text": "Merhaba {{first_name}}"},
		"change_summary": "SMS kısaltıldı"}`))
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if in.CampaignID == nil || *in.CampaignID != 12 || in.BaseVersion == nil || *in.BaseVersion != 3 ||
		in.SMS == nil || in.Email != nil || in.SegmentRule.Conditions[0].NotFollowedBy != "purchase" {
		t.Fatalf("parsed = %+v", in)
	}
	created, err := ParseSaveDraft([]byte(`{"campaign_id": null, "base_version": null, "name": "Sepet",
		"segment_rule": {"match":"all","conditions":[]}, "sms": null, "email": null, "content_language": "tr", "change_summary": "ilk"}`))
	if err != nil || created.CampaignID != nil || created.BaseVersion != nil {
		t.Fatalf("null campaign_id and base_version must mean a new campaign: %+v, %v", created, err)
	}

	bad := map[string]string{
		"unknown top-level key": `{"name":"x","segment_rule":{"match":"all","conditions":[]},"change_summary":"","approve":true}`,
		"unknown nested key":    `{"name":"x","segment_rule":{"match":"all","conditions":[],"sql":"1"},"change_summary":""}`,
		"trailing data":         `{"name":"x","segment_rule":{"match":"all","conditions":[]},"change_summary":""} {}`,
		"truncated":             `{"name":"x","segment_rule":{"match":"al`,
		"trailing comma":        `{"name":"x",}`,
	}
	for name, raw := range bad {
		if _, err := ParseSaveDraft([]byte(raw)); err == nil {
			t.Errorf("%s: expected strict parse error", name)
		}
	}
}
