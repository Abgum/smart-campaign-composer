package tools

import (
	"slices"
	"strings"
	"testing"
)

// The tool can carry an A/B test's B side, optional and closed like the
// rest; the parser reads it strictly.
func TestSaveDraftTool_ABTest(t *testing.T) {
	s := SaveDraftTool().InputSchema
	props := s["properties"].(map[string]any)
	ab, ok := props["ab_test"].(map[string]any)
	if !ok {
		t.Fatal("no ab_test property")
	}
	if req, _ := s["required"].([]string); slices.Contains(req, "ab_test") {
		t.Fatal("ab_test must be optional")
	}
	variants := ab["properties"].(map[string]any)["variants"].(map[string]any)
	item := variants["items"].(map[string]any)
	if item["additionalProperties"] != false || item["properties"].(map[string]any)["language"] == nil {
		t.Fatalf("B variant item = %v", item)
	}
	if ab["description"] == "" || ab["description"] == nil {
		t.Fatal("ab_test needs a description for the model")
	}

	in, err := ParseSaveDraft([]byte(`{"name":"x","segment_rule":{"match":"all","conditions":[]},"change_summary":"y",
		"sms":{"text":"A"},"content_language":"tr","ab_test":{"variants":[{"language":"tr","sms":{"text":"B"}}]}}`))
	if err != nil || in.ABTest == nil || len(in.ABTest.Variants) != 1 || in.ABTest.Variants[0].SMS.Text != "B" {
		t.Fatalf("parsed = %+v, err %v", in.ABTest, err)
	}
	if _, err := ParseSaveDraft([]byte(`{"name":"x","segment_rule":{"match":"all","conditions":[]},"change_summary":"y",
		"ab_test":{"variants":[],"split":50}}`)); err == nil {
		t.Fatal("an unknown key inside ab_test must be refused")
	}
}

// remove_ab_test is parsed strictly in Go. Keeping only the complex draft
// tool strict keeps the combined Claude grammar within its compilation limit.
func TestRemoveABTestTool(t *testing.T) {
	tool := RemoveABTestTool()
	if tool.Name != "remove_ab_test" || tool.Strict ||
		!strings.Contains(tool.Description, "only when the user") {
		t.Fatalf("tool = %+v", tool)
	}
	s := tool.InputSchema
	if s["additionalProperties"] != false {
		t.Fatalf("schema = %v; want a closed object", s)
	}
	if req, _ := s["required"].([]string); !slices.Equal(req, []string{"campaign_id", "base_version", "change_summary"}) {
		t.Fatalf("required = %v", s["required"])
	}

	in, err := ParseRemoveABTest([]byte(`{"campaign_id":7,"base_version":3,"change_summary":"A/B testi kaldırıldı"}`))
	if err != nil || in != (RemoveABTestInput{CampaignID: 7, BaseVersion: 3, ChangeSummary: "A/B testi kaldırıldı"}) {
		t.Fatalf("parsed = %+v, err %v", in, err)
	}
	for _, bad := range []string{
		`{"campaign_id":7,"base_version":3,"change_summary":"x","sms":{"text":"A"}}`,
		`{"campaign_id":7,"base_version":3`,
		`{"campaign_id":7,"base_version":3,"change_summary":"x"} {}`,
	} {
		if _, err := ParseRemoveABTest([]byte(bad)); err == nil {
			t.Errorf("parsed %s", bad)
		}
	}

	ab := SaveDraftTool().InputSchema["properties"].(map[string]any)["ab_test"].(map[string]any)
	if d, _ := ab["description"].(string); !strings.Contains(d, "keeps") || !strings.Contains(d, RemoveABTestName) {
		t.Errorf("ab_test description = %q; want it to say a draft without it keeps the test, and name %s", d, RemoveABTestName)
	}
}
