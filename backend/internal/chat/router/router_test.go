package router

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
)

func ptr(v int64) *int64 { return &v }

var campaigns = []Campaign{
	{ID: 11, Ordinal: 1, Name: "İstanbul Sepet Hatırlatma", Status: "draft", Summary: "SMS; 312 kişi"},
	{ID: 12, Ordinal: 2, Name: "İzmir Yaz İndirimi", Status: "draft", Summary: "email; 840 kişi"},
	{ID: 13, Ordinal: 3, Name: "Eski Kampanya", Status: "approved", Summary: "SMS; 90 kişi"},
	{ID: 14, Ordinal: 4, Name: "Bursa", Status: "draft", Summary: "SMS"},
	{ID: 15, Ordinal: 5, Name: "Antalya", Status: "draft", Summary: "SMS"},
}

func input(text string, focus *int64, cs []Campaign) Input {
	return Input{UserText: text, FocusCampaignID: focus, Campaigns: cs}
}

func dec(intent string, ids ...int64) Decision {
	if ids == nil {
		ids = []int64{}
	}
	return Decision{Intent: intent, CampaignIDs: ids, Reason: "test"}
}

// ---- Validate: Go checks ownership, editability and the write boundary --------

func TestValidate_Create(t *testing.T) {
	r := Validate(dec(IntentCreate), input("Ankara için hoş geldin mesajı hazırla", nil, campaigns))
	if r.Intent != IntentCreate || !r.AllowCreate || len(r.Allowed) != 0 || r.RejectReason != "" {
		t.Fatalf("route = %+v", r)
	}
	// A copy: create with a read-only reference, which may be approved.
	r = Validate(dec(IntentCreate, 13), input("Eski kampanyayı Ankara için kopyala", nil, campaigns))
	if r.Intent != IntentCreate || !r.AllowCreate || len(r.Allowed) != 0 || !slices.Equal(r.References, []int64{13}) {
		t.Fatalf("copy route = %+v", r)
	}
}

// Free-form names and descriptions are the router's interpretation. The
// literal #N form has its own safety check below.
func TestValidate_EditAcceptsRouterInterpretation(t *testing.T) {
	r := Validate(dec(IntentEdit, 12), input("yazlık olanın konusunu değiştir", ptr(11), campaigns))
	if r.Intent != IntentEdit || !slices.Equal(r.Allowed, []int64{12}) || r.AllowCreate || r.RejectReason != "" {
		t.Fatalf("route = %+v", r)
	}
}

func TestValidate_HashNumberCannotBecomeInternalID(t *testing.T) {
	cs := []Campaign{
		{ID: 12, Ordinal: 1, Name: "İstanbul", Status: "draft"},
		{ID: 99, Ordinal: 12, Name: "İzmir", Status: "draft"},
	}
	for _, d := range []Decision{dec(IntentEdit, 12), dec(IntentEdit, 99), dec(IntentCreate, 12)} {
		r := Validate(d, input("#12 kampanyasının SMS'ini değiştir", ptr(12), cs))
		if r.Intent != IntentClarify || r.AllowCreate || len(r.Allowed) != 0 || r.RejectReason != "ambiguous_hash_reference" || !strings.Contains(r.Question, "#12") {
			t.Fatalf("decision %+v accepted ambiguous hash reference: %+v", d, r)
		}
	}
	r := Validate(dec(IntentEdit, 12), input("Kampanya #12'nin SMS'ini değiştir", nil, cs))
	if r.Intent != IntentClarify || r.RejectReason != "ambiguous_hash_reference" {
		t.Fatalf("campaign-prefixed hash accepted: %+v", r)
	}
	r = Validate(dec(IntentEdit, 12), input("şu #12 için SMS yaz", nil, cs))
	if r.Intent != IntentClarify || r.RejectReason != "ambiguous_hash_reference" {
		t.Fatalf("hash followed by 'için' accepted as internal id: %+v", r)
	}
	r = Validate(dec(IntentEdit, 12), input("şu #12'nin SMS'ini değiştir", nil, cs))
	if r.Intent != IntentClarify || r.RejectReason != "ambiguous_hash_reference" {
		t.Fatalf("hash with Turkish possessive suffix accepted as internal id: %+v", r)
	}

	// The displayed ordinal has an unambiguous form and remains usable.
	r = Validate(dec(IntentEdit, 99), input("Kampanya 12'nin SMS'ini değiştir", nil, cs))
	if r.Intent != IntentEdit || !slices.Equal(r.Allowed, []int64{99}) {
		t.Fatalf("displayed ordinal rejected: %+v", r)
	}
}

func TestValidate_ContentHashtagDoesNotOverrideCampaignTarget(t *testing.T) {
	cs := []Campaign{
		{ID: 12, Ordinal: 1, Name: "İstanbul", Status: "draft"},
		{ID: 99, Ordinal: 2, Name: "İzmir", Status: "draft"},
	}
	for _, tc := range []struct {
		name  string
		text  string
		focus *int64
		id    int64
	}{
		{"explicit ordinal", "Kampanya 2 için SMS'e #30 indirim kodu ekle", nil, 99},
		{"selected tab", "SMS'e #30 indirim ekle", ptr(12), 12},
		{"same numeric value as selected tab", "SMS'e #12 indirim ekle", ptr(12), 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(dec(IntentEdit, tc.id), input(tc.text, tc.focus, cs))
			if r.Intent != IntentEdit || !slices.Equal(r.Allowed, []int64{tc.id}) || r.RejectReason != "" {
				t.Fatalf("content hashtag blocked verified target: %+v", r)
			}
		})
	}
}

func TestValidate_EditRejections(t *testing.T) {
	cases := []struct {
		name       string
		d          Decision
		wantReject string
		wantInQ    []string
	}{
		{"id from another chat", dec(IntentEdit, 99), "unknown_campaign", []string{"Kampanya 1"}},
		{"no target", dec(IntentEdit), "edit_without_target", []string{"Kampanya 1"}},
		{"approved campaign", dec(IntentEdit, 13), "not_editable", []string{"Kampanya 3", "onaylanmış", "kopya"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(tc.d, input("x", nil, campaigns))
			if r.Intent != IntentClarify || r.RejectReason != tc.wantReject || r.AllowCreate || len(r.Allowed) != 0 {
				t.Fatalf("route = %+v", r)
			}
			for _, want := range tc.wantInQ {
				if !strings.Contains(r.Question, want) {
					t.Errorf("question %q lacks %q", r.Question, want)
				}
			}
		})
	}
}

func TestValidate_EditOverflow(t *testing.T) {
	r := Validate(dec(IntentEdit, 11, 12, 14, 15), input("hepsine emoji ekle", nil, campaigns))
	if r.Intent != IntentEdit || !slices.Equal(r.Allowed, []int64{11, 12, 14}) || !slices.Equal(r.Overflow, []int64{15}) {
		t.Fatalf("route = %+v", r)
	}
}

func TestValidate_Clarify(t *testing.T) {
	d := dec(IntentClarify, 11, 12, 99)
	d.Question = "Hangi kampanyanın SMS'ini kısaltayım?"
	r := Validate(d, input("SMS'i kısalt", nil, campaigns))
	if r.Intent != IntentClarify || r.Question != d.Question || !slices.Equal(r.Candidates, []int64{11, 12}) {
		t.Fatalf("route = %+v (unknown candidate must be dropped)", r)
	}
	r = Validate(dec(IntentClarify), input("SMS'i kısalt", nil, campaigns))
	if !slices.Equal(r.Candidates, []int64{11, 12, 14, 15}) {
		t.Fatalf("default candidates are the drafts, got %v", r.Candidates)
	}
	for _, want := range []string{"Kampanya 1", "Kampanya 2", "yeni"} {
		if !strings.Contains(r.Question, want) {
			t.Errorf("default question %q lacks %q", r.Question, want)
		}
	}
}

func TestValidate_ChatAndInvalid(t *testing.T) {
	r := Validate(dec(IntentChat), input("Kampanya 1 kaç kişiye gidiyor?", nil, campaigns))
	if r.Intent != IntentChat || r.AllowCreate || len(r.Allowed) != 0 {
		t.Fatalf("chat route must not write: %+v", r)
	}
	r = Validate(dec("delete", 11), input("hepsini sil", nil, campaigns))
	if r.Intent != IntentClarify || r.RejectReason != "invalid_intent" {
		t.Fatalf("route = %+v", r)
	}
}

// ---- Run ----------------------------------------------------------------------

func routeCall(d Decision) mock.Call {
	b, _ := json.Marshal(d)
	return mock.Call{ToolName: ToolName, ToolInput: string(b)}
}

func TestRun_EmptyChat_NoModelCall(t *testing.T) {
	m := mock.NewScripted()
	r, rec, err := Run(context.Background(), m, input("İstanbul sepet hatırlatması", nil, nil))
	if err != nil || r.Intent != IntentCreate || !r.AllowCreate || r.Source != SourceEmptyChat || rec != nil || m.Calls() != 0 {
		t.Fatalf("route=%+v record=%v err=%v calls=%d", r, rec, err, m.Calls())
	}
}

func TestRun_RouterDecides(t *testing.T) {
	m := mock.NewScripted(routeCall(dec(IntentCreate)))
	in := input("Ankara için hoş geldin mesajı hazırla", ptr(11), campaigns)
	in.History = []Turn{{User: "İstanbul sepet kampanyası hazırla", Assistant: "Kaydettim."}}
	r, rec, err := Run(context.Background(), m, in)
	if err != nil || r.Intent != IntentCreate || !r.AllowCreate || r.Source != SourceRouter {
		t.Fatalf("route = %+v err=%v", r, err)
	}
	if rec == nil || rec.Kind != "router" || rec.ErrorCode != "" {
		t.Fatalf("call record = %+v", rec)
	}

	req := m.Requests()[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != ToolName || !req.Tools[0].Strict {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if _, ok := req.Tools[0].InputSchema["properties"].(map[string]any)["basis"]; ok {
		t.Error("the router schema must not carry a basis field")
	}
	if req.System != Request(input("x", nil, nil)).System {
		t.Error("router system prompt must be static")
	}
	body := req.Messages[len(req.Messages)-1].Blocks[0].Text
	for _, want := range []string{`"selected_tab":11`, `"ref":"Kampanya 2"`, "İzmir Yaz İndirimi", `"status":"approved"`,
		"İstanbul sepet kampanyası hazırla", "Ankara için hoş geldin mesajı hazırla"} {
		if !strings.Contains(body, want) {
			t.Errorf("routing input lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "explicit") {
		t.Errorf("routing input must not carry Go text-parsing hints:\n%s", body)
	}
}

func TestRequest_HashNumberDoesNotMeanInternalCampaignID(t *testing.T) {
	req := Request(input("#12 kampanyasının SMS'ini değiştir", nil, []Campaign{
		{ID: 12, Ordinal: 1, Name: "İstanbul", Status: "draft"},
		{ID: 99, Ordinal: 12, Name: "İzmir", Status: "draft"},
	}))
	if !strings.Contains(req.System, "#12") || !strings.Contains(req.System, "is not an internal id") ||
		!strings.Contains(req.System, "ask which campaign") {
		t.Fatalf("router prompt must ask about ambiguous #N instead of mapping it to an internal id: %s", req.System)
	}
	if !strings.Contains(req.Messages[0].Blocks[0].Text, `"ref":"Kampanya 12"`) {
		t.Fatal("router must see the user-facing ordinal")
	}
}

// The router reads English; the question it asks the marketer is in the
// language of the marketer's message.
func TestRequest_EnglishPromptQuestionInTheUsersLanguage(t *testing.T) {
	req := Request(input("selam", nil, campaigns))
	if !strings.HasPrefix(req.System, "Decide what the latest user message") ||
		!strings.Contains(req.System, "short question in the language of the user's message") {
		t.Fatalf("router prompt must be English and ask in the user's language: %s", req.System)
	}
	for _, old := range []string{"Görevin", "Hedefi belirlerken", "Kampanya yazmıyorsun"} {
		if strings.Contains(req.System, old) {
			t.Errorf("router prompt still has Turkish %q", old)
		}
	}
	if d := req.Tools[0].Description; strings.ContainsAny(d, "çğıöşüÇĞİÖŞÜ") {
		t.Errorf("tool description is Turkish: %q", d)
	}
}

// A failed model call fails the turn (no side effects); it is not ambiguity.
func TestRun_ModelError(t *testing.T) {
	m := mock.NewScripted(mock.Call{Err: llm.ErrUnavailable})
	_, rec, err := Run(context.Background(), m, input("SMS'i kısalt", nil, campaigns))
	if !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if rec == nil || rec.Kind != "router" || rec.ErrorCode != "llm_unavailable" {
		t.Fatalf("call record = %+v", rec)
	}
}

// A model that answers but gives no usable decision leads to a question.
func TestRun_NoUsableDecision_Clarifies(t *testing.T) {
	cases := map[string]mock.Call{
		"no tool call":    {Text: "Bence yeni kampanya."},
		"invalid json":    {ToolName: ToolName, ToolInput: `{"intent":"create",}`},
		"unknown field":   {ToolName: ToolName, ToolInput: `{"intent":"create","campaign_ids":[],"reason":"x","approve":true}`},
		"wrong tool name": {ToolName: "save_campaign_draft", ToolInput: `{}`},
		"refusal":         {ToolName: ToolName, ToolInput: `{"intent":"create","campaign_ids":[],"reason":"x"}`, Stop: llm.StopRefusal},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			m := mock.NewScripted(call)
			r, rec, err := Run(context.Background(), m, input("SMS'i kısalt", nil, campaigns))
			if err != nil || r.Intent != IntentClarify || r.Source != SourceFallback || r.AllowCreate || r.Question == "" {
				t.Fatalf("route = %+v err=%v", r, err)
			}
			if rec == nil || rec.ErrorCode == "" {
				t.Fatalf("the unusable call must be audited: %+v", rec)
			}
		})
	}
}

func TestRun_NoRouterConfigured_Clarifies(t *testing.T) {
	r, rec, err := Run(context.Background(), nil, input("SMS'i kısalt", ptr(11), campaigns))
	if err != nil || rec != nil || r.Intent != IntentClarify || r.AllowCreate || len(r.Allowed) != 0 {
		t.Fatalf("without a router Go must not guess a target: route=%+v err=%v", r, err)
	}
}

// Go's own questions (a #N reference, no usable question from the model)
// are written in the language of the interface the request came from;
// Turkish when it is not known.
func TestFallbackQuestions_FollowTheInterfaceLanguage(t *testing.T) {
	cs := []Campaign{{ID: 12, Ordinal: 1, Name: "Sepet", Status: "draft"}, {ID: 13, Ordinal: 2, Name: "Bülten", Status: "draft"}}
	en := input("#12", nil, cs)
	en.Lang = "en"
	r := Validate(dec(IntentEdit, 12), en)
	if r.Question != "Which campaign do you mean by #12? Please write its name or the Campaign N number shown on the screen." {
		t.Fatalf("hash question in English = %q", r.Question)
	}
	en.UserText = "change it"
	r = Validate(Decision{Intent: IntentClarify, Reason: "test"}, en)
	if r.Question != "Which campaign do you mean: Campaign 1 (Sepet), Campaign 2 (Bülten)? Or do you want a new campaign?" {
		t.Fatalf("default question in English = %q", r.Question)
	}
	r = Validate(Decision{Intent: IntentClarify, Reason: "test"}, input("değiştir", nil, cs))
	if !strings.HasPrefix(r.Question, "Hangi kampanyayı kastediyorsunuz: Kampanya 1 (Sepet)") {
		t.Fatalf("default question in Turkish = %q", r.Question)
	}
}
