package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/segment"
)

// ---- fakes ------------------------------------------------------------------

type fakeCampaign struct {
	status  string
	version int
	ordinal int
	ab      bool // has an A/B test
}

type fakeStore struct {
	mu        sync.Mutex
	campaigns map[int64]*fakeCampaign
	nextID    int64
	cmds      []campaign.DraftCmd
	removals  []removal
	failWith  error
}

func newStore() *fakeStore {
	return &fakeStore{campaigns: map[int64]*fakeCampaign{
		11: {status: "draft", version: 3, ordinal: 1},
		12: {status: "draft", version: 1, ordinal: 2},
		13: {status: "approved", version: 2, ordinal: 3},
	}, nextID: 100}
}

func (s *fakeStore) SaveDraft(_ context.Context, cmd campaign.DraftCmd) (campaign.Saved, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return campaign.Saved{}, s.failWith
	}
	if cmd.CampaignID == nil {
		s.nextID++
		s.campaigns[s.nextID] = &fakeCampaign{status: "draft", version: 1, ordinal: len(s.campaigns) + 1}
		s.cmds = append(s.cmds, cmd)
		return campaign.Saved{CampaignID: s.nextID, Ordinal: s.campaigns[s.nextID].ordinal, Version: 1, Created: true}, nil
	}
	c, ok := s.campaigns[*cmd.CampaignID]
	switch {
	case !ok:
		return campaign.Saved{}, campaign.ErrNotFound
	case c.status != "draft":
		return campaign.Saved{}, campaign.ErrNotDraft
	case cmd.BaseVersion == nil || *cmd.BaseVersion != c.version:
		return campaign.Saved{}, &campaign.StaleError{CurrentVersion: c.version}
	}
	c.version++
	s.cmds = append(s.cmds, cmd)
	saved := campaign.Saved{CampaignID: *cmd.CampaignID, Ordinal: c.ordinal, Version: c.version}
	// Like the store, a revision without an A/B test keeps the campaign's.
	switch {
	case cmd.ABTest != nil:
		c.ab = true
	case cmd.DropABTest:
		c.ab = false
	default:
		saved.ABTestKept = c.ab
	}
	return saved, nil
}

type fakeCounter struct{ rules []segment.Rule }

// Buckets splits the 312 recipients by language facts: Turkish, English and
// French preferences, no preference in Türkiye, France and Canada, and an
// unsupported German preference in Türkiye and in Germany.
func (f *fakeCounter) Buckets(_ context.Context, _ segment.Rule) ([]language.Bucket, error) {
	pref := func(s string) *string { return &s }
	return []language.Bucket{
		{Preference: pref("tr"), Country: "TR", SMS: true, Email: true, Count: 280},
		{Preference: nil, Country: "TR", SMS: true, Email: true, Count: 18},
		{Preference: pref("en"), Country: "FR", SMS: true, Email: true, Count: 3},
		{Preference: nil, Country: "FR", SMS: true, Email: true, Count: 2},
		{Preference: pref("fr"), Country: "TR", SMS: true, Email: true, Count: 1},
		{Preference: pref("de"), Country: "TR", SMS: true, Email: true, Count: 4},
		{Preference: nil, Country: "CA", SMS: true, Email: true, Count: 2},
		{Preference: pref("de"), Country: "DE", SMS: true, Email: true, Count: 2},
	}, nil
}

func (f *fakeCounter) Count(_ context.Context, r segment.Rule) (segment.Audience, error) {
	f.rules = append(f.rules, r)
	return segment.Audience{Total: 312, SMSReachable: 220, EmailReachable: 250}, nil
}

type recorder struct{ events []Event }

func (r *recorder) Emit(e Event) { r.events = append(r.events, e) }

func (r *recorder) types() []string {
	var out []string
	for _, e := range r.events {
		if len(out) == 0 || out[len(out)-1] != e.Type {
			out = append(out, e.Type)
		}
	}
	return out
}

func (r *recorder) has(typ string) bool {
	for _, e := range r.events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// ---- fixtures ---------------------------------------------------------------

const validNew = `{"name":"İstanbul sepet hatırlatma",
 "segment_rule":{"match":"all","conditions":[
   {"kind":"location","operator":"in","places":[{"level":"city","name":"istanbul","country":"TR"}]},
   {"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "sms":{"text":"Merhaba {{first_name}}, sepetindeki ürünler %30 indirimle seni bekliyor!"},
 "content_language":"tr","change_summary":"İlk taslak"}`

func revise(id int64, base int, sms string) string {
	return `{"campaign_id":` + itoa(id) + `,"base_version":` + itoa(int64(base)) + `,"name":"Sepet",
 "segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "sms":{"text":"` + sms + `"},"content_language":"tr","change_summary":"SMS güncellendi"}`
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

var tooLongSMS = `{"name":"x","segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"purchase","within_days":30}]},
 "sms":{"text":"` + strings.Repeat("a", 172) + `"},"content_language":"tr","change_summary":"ilk"}`

func baseInput(allowed ...int64) Input {
	return Input{
		Request: llm.Request{System: "SYS", MaxTokens: 16000, Messages: []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "<chat_state>{}</chat_state>"}, {Type: llm.BlockText, Text: "brief"}}},
		}},
		Allowed:     allowed,
		Candidates:  []int64{11, 12},
		AllowCreate: true,
	}
}

func TestLoop_CreateNotAllowed_Rejected(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Hangi kampanyanın SMS'ini kısaltayım: Kampanya 1 mi, Kampanya 2 mi?"},
	)
	store := newStore()
	in := baseInput()
	in.AllowCreate = false
	res, rec, _ := run(t, m, store, in)
	if len(store.cmds) != 0 || len(res.Saved) != 0 {
		t.Fatalf("an unclear request must not create a campaign: %+v", store.cmds)
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "create_not_allowed") {
		t.Errorf("feedback = %+v", tr)
	}
	if !rec.has(EventTargetAmbiguous) {
		t.Errorf("UI must offer candidates, events = %v", rec.types())
	}
}

func TestLoop_OverflowTarget_Rejected(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(12, 1, "Kısa")},
		mock.Call{Text: "Bu mesajda en fazla 3 kampanya düzenleyebilirim; Kampanya 4'ü ayrıca isteyin."},
	)
	store := newStore()
	in := baseInput(11)
	in.Overflow = []int64{12}
	_, rec, _ := run(t, m, store, in)
	if len(store.cmds) != 0 {
		t.Fatalf("overflow target must not be written: %+v", store.cmds)
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "target_overflow") {
		t.Errorf("feedback = %+v", tr)
	}
	if rec.has(EventTargetAmbiguous) {
		t.Error("overflow is not ambiguity; candidates must not be offered")
	}
}

func run(t *testing.T, m llm.Client, store *fakeStore, in Input) (Result, *recorder, *fakeCounter) {
	t.Helper()
	rec, counter := &recorder{}, &fakeCounter{}
	res := Run(context.Background(), Deps{LLM: m, Store: store, Audience: counter, Catalog: segment.DefaultCatalog()}, in, rec)
	return res, rec, counter
}

func lastToolResult(t *testing.T, req llm.Request) llm.Block {
	t.Helper()
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Blocks) == 0 || last.Blocks[0].Type != llm.BlockToolResult {
		t.Fatalf("last message is not a tool_result: %+v", last)
	}
	return last.Blocks[0]
}

func reqAt(t *testing.T, m *mock.Scripted, i int) llm.Request {
	t.Helper()
	reqs := m.Requests()
	if len(reqs) <= i {
		t.Fatalf("model received %d requests, want at least %d", len(reqs), i+1)
	}
	return reqs[i]
}

// ---- tests ------------------------------------------------------------------

func TestLoop_ValidDraft(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{Text: "Taslağı hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Kaydettim; 312 kişiye ulaşıyor."},
	)
	store := newStore()
	res, rec, counter := run(t, m, store, baseInput())

	if res.Outcome != OutcomeOK || res.ErrorCode != "" {
		t.Fatalf("outcome = %s/%s", res.Outcome, res.ErrorCode)
	}
	if res.Text != "Taslağı hazırlıyorum.\nKaydettim; 312 kişiye ulaşıyor." {
		t.Errorf("text = %q", res.Text)
	}
	if len(res.Saved) != 1 || !res.Saved[0].Created || res.Saved[0].ChangeSummary != "İlk taslak" {
		t.Fatalf("saved = %+v", res.Saved)
	}
	if got := store.cmds[0].Rule.Conditions[0].Places[0]; got.GeonameID != 745044 || got.Name != "İstanbul" {
		t.Errorf("stored rule must carry the resolved city id, got %+v", got)
	}
	if store.cmds[0].Audience.Total != 312 || len(counter.rules) != 1 {
		t.Errorf("audience not counted before save: %+v", store.cmds[0].Audience)
	}
	if got := strings.Join(rec.types(), ","); got != "iteration.started,assistant.delta,draft.start,draft.delta,draft.saved,iteration.started,assistant.delta" {
		t.Errorf("events = %s", got)
	}
	if m.Calls() != 2 {
		t.Fatalf("calls = %d", m.Calls())
	}
	second := reqAt(t, m, 1)
	if len(second.Messages) != 3 || second.Messages[1].Role != llm.RoleAssistant {
		t.Fatalf("tool round not appended: %+v", second.Messages)
	}
	tr := lastToolResult(t, second)
	if tr.IsError || !strings.Contains(tr.Text, `"total":312`) {
		t.Errorf("tool result = %+v", tr)
	}
	if len(res.Calls) != 2 {
		t.Errorf("call records = %d", len(res.Calls))
	}
}

func TestLoop_RepairThenValid(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLongSMS},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Düzelttim."},
	)
	res, rec, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeOK || len(res.Saved) != 1 || m.Calls() != 3 {
		t.Fatalf("outcome=%s saved=%d calls=%d", res.Outcome, len(res.Saved), m.Calls())
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "too_long") || !strings.Contains(tr.Text, "sms.text") {
		t.Errorf("repair feedback = %+v", tr)
	}
	if !rec.has(EventDraftInvalid) || !rec.has(EventDraftSaved) {
		t.Errorf("events = %v", rec.types())
	}
	if res.Repairs != 1 {
		t.Errorf("repairs = %d", res.Repairs)
	}
}

func TestLoop_InvalidJSONThenValid(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "x",}`},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Tamam."},
	)
	res, _, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeOK {
		t.Fatalf("outcome = %s/%s", res.Outcome, res.ErrorCode)
	}
	second := reqAt(t, m, 1)
	tr := lastToolResult(t, second)
	if !tr.IsError || !strings.Contains(tr.Text, "INVALID_JSON") || !json.Valid([]byte(tr.Text)) {
		t.Errorf("tool result = %+v", tr)
	}
	for _, b := range second.Messages[1].Blocks {
		if b.Type == llm.BlockToolUse && !json.Valid(b.Input) {
			t.Errorf("replayed tool_use input must be valid JSON for the API, got %s", b.Input)
		}
	}
}

func TestLoop_ThreeInvalid(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLongSMS},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLongSMS},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: tooLongSMS},
		mock.Call{Text: "never reached"},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput())
	if res.Outcome != OutcomeFailed || res.ErrorCode != "repair_exhausted" {
		t.Fatalf("outcome = %s/%s", res.Outcome, res.ErrorCode)
	}
	if m.Calls() != 3 || len(store.cmds) != 0 {
		t.Fatalf("calls=%d saves=%d", m.Calls(), len(store.cmds))
	}
}

// A round that saves a draft is no rejected round, even when another call
// in it was rejected. Claude sometimes gives up on a call it cannot finish
// with placeholder values and sends the real one next to it; counting that
// round against the repair budget ended the turn as repair_exhausted right
// after the requested version was saved.
func TestLoop_RoundThatSavesIsNoRejectedRound(t *testing.T) {
	placeholder := `{"campaign_id":11,"name":"x","segment_rule":{"match":"all","conditions":[]},
	 "sms":null,"email":null,"content_language":"tr","change_summary":"placeholder"}`
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: placeholder},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: placeholder},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: placeholder,
			Parallel: []mock.ToolCall{{Name: "save_campaign_draft", Input: revise(11, 3, "Kısa SMS")}}},
		mock.Call{Text: "SMS'i kısalttım."},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput(11))
	if res.Outcome != OutcomeOK || res.ErrorCode != "" || len(res.Saved) != 1 || m.Calls() != 4 {
		t.Fatalf("outcome=%s/%s saved=%d calls=%d; the round that saved the draft must not end the turn",
			res.Outcome, res.ErrorCode, len(res.Saved), m.Calls())
	}
	if res.Text != "SMS'i kısalttım." {
		t.Errorf("text = %q", res.Text)
	}
	want := []string{RejectValidation, RejectValidation, RejectValidation}
	if res.Repairs != 2 || !slices.Equal(res.Rejections, want) {
		t.Errorf("repairs = %d, rejections = %v; only the two rounds that saved nothing are repairs", res.Repairs, res.Rejections)
	}
	results := reqAt(t, m, 3).Messages[len(reqAt(t, m, 3).Messages)-1].Blocks
	if len(results) != 2 || !results[0].IsError || results[1].IsError {
		t.Errorf("the model must get both results of the round: %+v", results)
	}
}

func TestLoop_StaleBaseVersion(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(11, 2, "Kısa SMS")},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(11, 3, "Kısa SMS")},
		mock.Call{Text: "Güncel versiyon üzerine kaydettim."},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput(11))
	if res.Outcome != OutcomeOK || len(res.Saved) != 1 || res.Saved[0].Version != 4 {
		t.Fatalf("outcome=%s saved=%+v", res.Outcome, res.Saved)
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "stale_version") || !strings.Contains(tr.Text, `"current_version":3`) {
		t.Errorf("stale feedback = %+v", tr)
	}
}

func TestLoop_OutsideTargets_Rejected(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(12, 1, "Kısa")},
		mock.Call{Text: "Hangi kampanyayı kastettiniz: Kampanya 1 mi, Kampanya 2 mi?"},
	)
	store := newStore()
	res, rec, _ := run(t, m, store, baseInput(11))
	if len(store.cmds) != 0 {
		t.Fatalf("store must not be called for a campaign outside allowed targets: %+v", store.cmds)
	}
	tr := lastToolResult(t, reqAt(t, m, 1))
	if !tr.IsError || !strings.Contains(tr.Text, "target_not_allowed") {
		t.Errorf("feedback = %+v", tr)
	}
	if !rec.has(EventTargetAmbiguous) {
		t.Errorf("UI must be told to offer candidates, events = %v", rec.types())
	}
	if res.Outcome != OutcomeOK {
		t.Errorf("asking the user is a normal outcome, got %s/%s", res.Outcome, res.ErrorCode)
	}
}

func TestLoop_ApprovedCampaign_Rejected(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: revise(13, 2, "Yeni")},
		mock.Call{Text: "Onaylanmış kampanya değiştirilemez; kopyalayayım mı?"},
	)
	res, _, _ := run(t, m, newStore(), baseInput(13))
	if tr := lastToolResult(t, reqAt(t, m, 1)); !tr.IsError || !strings.Contains(tr.Text, "not_draft") {
		t.Errorf("feedback = %+v", tr)
	}
	if len(res.Saved) != 0 {
		t.Errorf("saved = %+v", res.Saved)
	}
}

func TestLoop_MaxTokensRetryOnce(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "Sep`, Stop: llm.StopMaxTokens},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Tamam."},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput())
	if res.Outcome != OutcomeOK || len(store.cmds) != 1 {
		t.Fatalf("outcome=%s/%s saves=%d", res.Outcome, res.ErrorCode, len(store.cmds))
	}
	first, second := reqAt(t, m, 0), reqAt(t, m, 1)
	if second.MaxTokens <= first.MaxTokens {
		t.Errorf("retry must raise max_tokens: %d → %d", first.MaxTokens, second.MaxTokens)
	}
	if len(second.Messages) != len(first.Messages) {
		t.Errorf("a truncated turn must be re-issued, not appended")
	}

	m = mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "Sep`, Stop: llm.StopMaxTokens},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: `{"name": "Sep`, Stop: llm.StopMaxTokens},
	)
	res, _, _ = run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeFailed || res.ErrorCode != "max_tokens" || m.Calls() != 2 {
		t.Fatalf("second truncation: outcome=%s/%s calls=%d", res.Outcome, res.ErrorCode, m.Calls())
	}
}

func TestLoop_TruncatedIterationIsDiscardedWithoutDiscardingPriorWork(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{Text: "Taslağı kaydediyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Yarım yanıt", ToolName: "save_campaign_draft", ToolInput: `{"name":"Yarım`, Stop: llm.StopMaxTokens},
		mock.Call{Text: "Tamamlandı."},
	)
	res, rec, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeOK || res.Text != "Taslağı kaydediyorum.\nTamamlandı." || len(res.Saved) != 1 {
		t.Fatalf("result = %+v", res)
	}
	var lifecycle []string
	for _, e := range rec.events {
		if e.Type != "iteration.started" && e.Type != "iteration.discarded" {
			continue
		}
		data, ok := e.Data.(map[string]any)
		if !ok {
			t.Fatalf("%s payload = %T", e.Type, e.Data)
		}
		lifecycle = append(lifecycle, e.Type+":"+itoa(int64(data["iteration"].(int))))
	}
	if got := strings.Join(lifecycle, ","); got != "iteration.started:1,iteration.started:2,iteration.discarded:2,iteration.started:3" {
		t.Fatalf("lifecycle = %s", got)
	}
	if !rec.has(EventDraftSaved) {
		t.Fatal("earlier valid draft must remain saved")
	}
}

func TestLoop_SecondTruncationAndStreamErrorDiscardTheirPreviews(t *testing.T) {
	t.Run("second truncation", func(t *testing.T) {
		m := mock.NewScripted(
			mock.Call{Text: "kesik bir", Stop: llm.StopMaxTokens},
			mock.Call{Text: "kesik iki", Stop: llm.StopMaxTokens},
		)
		res, rec, _ := run(t, m, newStore(), baseInput())
		if res.Outcome != OutcomeFailed || res.ErrorCode != "max_tokens" {
			t.Fatalf("result = %+v", res)
		}
		var discarded int
		for _, e := range rec.events {
			if e.Type == "iteration.discarded" {
				discarded++
			}
		}
		if discarded != 2 {
			t.Fatalf("discarded %d truncated attempts, want 2", discarded)
		}
	})
	t.Run("stream error after delta", func(t *testing.T) {
		m := mock.NewScripted(mock.Call{Text: "yarım metin", Err: llm.ErrUnavailable, ErrAfterEvents: 1})
		res, rec, _ := run(t, m, newStore(), baseInput())
		if res.Outcome != OutcomeFailed || !rec.has("iteration.discarded") {
			t.Fatalf("result = %+v events = %v", res, rec.types())
		}
	})
}

func TestLoop_Refusal(t *testing.T) {
	m := mock.NewScripted(mock.Call{Text: "Yardımcı olamam.", Stop: llm.StopRefusal})
	res, _, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeFailed || res.ErrorCode != "refusal" || m.Calls() != 1 {
		t.Fatalf("outcome=%s/%s calls=%d", res.Outcome, res.ErrorCode, m.Calls())
	}
}

func TestLoop_SaveThenLLMError_Partial(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{Text: "Hazırlıyorum.", ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "Kayde", Err: llm.ErrUnavailable, ErrAfterEvents: 1},
	)
	res, _, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomePartial || res.ErrorCode != "llm_unavailable" || len(res.Saved) != 1 {
		t.Fatalf("outcome=%s/%s saved=%d", res.Outcome, res.ErrorCode, len(res.Saved))
	}
	if res.Text != "Hazırlıyorum." {
		t.Errorf("text of the failed iteration must be dropped, got %q", res.Text)
	}
	if !errors.Is(res.Err, llm.ErrUnavailable) {
		t.Errorf("err = %v", res.Err)
	}
}

func TestLoop_LLMErrorBeforeSave_Failed(t *testing.T) {
	m := mock.NewScripted(mock.Call{Err: llm.ErrRateLimited})
	res, _, _ := run(t, m, newStore(), baseInput())
	if res.Outcome != OutcomeFailed || res.ErrorCode != "llm_rate_limited" || res.Text != "" {
		t.Fatalf("outcome=%s/%s text=%q", res.Outcome, res.ErrorCode, res.Text)
	}
}

func TestLoop_DBErrorOnSave(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: validNew},
		mock.Call{Text: "never reached"},
	)
	store := newStore()
	store.failWith = errors.New("connection reset")
	res, rec, _ := run(t, m, store, baseInput())
	if res.Outcome != OutcomeFailed || res.ErrorCode != "db_error" || m.Calls() != 1 {
		t.Fatalf("outcome=%s/%s calls=%d", res.Outcome, res.ErrorCode, m.Calls())
	}
	if rec.has(EventDraftSaved) {
		t.Fatal("draft.saved must not be emitted when the write failed")
	}
}

func locationDraft(place string) string {
	return `{"name":"Paris","segment_rule":{"match":"all","conditions":[
 {"kind":"location","operator":"in","places":[` + place + `]}]},
 "email":{"subject":"Bonjour","body":"Merhaba {{first_name}}"},"content_language":"tr","change_summary":"ilk"}`
}

// Two places named Paris: the user is asked, and Claude may not pick one by
// itself later in the same turn.
func TestLoop_AmbiguousPlace_AsksUser(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: locationDraft(`{"level":"city","name":"Paris"}`)},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: locationDraft(`{"level":"city","name":"Paris","country":"FR"}`)},
		mock.Call{Text: "Hangi Paris'i kastediyorsunuz: Fransa'daki mi, ABD'deki (Teksas) mi?"},
	)
	store := newStore()
	res, rec, _ := run(t, m, store, baseInput())
	if len(store.cmds) != 0 || len(res.Saved) != 0 || res.Outcome != OutcomeOK {
		t.Fatalf("nothing may be saved before the user answers: saved=%+v outcome=%s/%s", store.cmds, res.Outcome, res.ErrorCode)
	}
	first := lastToolResult(t, reqAt(t, m, 1))
	if !first.IsError || !strings.Contains(first.Text, "ambiguous_place") ||
		!strings.Contains(first.Text, "2988507") || !strings.Contains(first.Text, "4717560") {
		t.Errorf("ambiguity feedback = %s", first.Text)
	}
	second := lastToolResult(t, reqAt(t, m, 2))
	if !second.IsError || !strings.Contains(second.Text, "ask_user_first") {
		t.Errorf("a guess after an ambiguity must be refused: %s", second.Text)
	}
	if !rec.has(EventLocationAmbiguous) {
		t.Errorf("UI must get the place candidates, events = %v", rec.types())
	}
}

// ---- languages ----------------------------------------------------------------

func perRecipientDraft(campaignID int64, base int, decisions string) string {
	target := ""
	if campaignID != 0 {
		target = `"campaign_id":` + itoa(campaignID) + `,"base_version":` + itoa(int64(base)) + `,`
	}
	return `{` + target + `"name":"Sepet hatırlatma",
 "segment_rule":{"match":"all","conditions":[{"kind":"event","event_type":"cart_add","within_days":30,"not_followed_by":"purchase"}]},
 "per_recipient":{"variants":[
   {"language":"tr","sms":{"text":"Merhaba {{first_name}}, sepetin seni bekliyor!"}},
   {"language":"en","sms":{"text":"Hi {{first_name}}, your cart is waiting!"}},
   {"language":"fr","sms":{"text":"Bonjour {{first_name}}, votre panier vous attend !"}}],
  "country_languages":[{"country":"TR","language":"tr"},{"country":"FR","language":"fr"}],
  "decisions":[` + decisions + `]},
 "change_summary":"Her alıcıya kendi dilinde"}`
}

// toolPayload decodes the tool result the model got in request i.
func toolPayload(t *testing.T, m *mock.Scripted, i int) (map[string]any, bool) {
	t.Helper()
	tr := lastToolResult(t, reqAt(t, m, i))
	var p map[string]any
	if err := json.Unmarshal([]byte(tr.Text), &p); err != nil {
		t.Fatalf("tool result %q: %v", tr.Text, err)
	}
	return p, tr.IsError
}

// A per-recipient draft is saved with its variants; Go works out each
// recipient's language and tells the model who still waits for a decision.
func TestLoop_PerRecipientDraft_SavedWithLanguageSummary(t *testing.T) {
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: perRecipientDraft(0, 0, "")},
		mock.Call{Text: "4 kişinin tercihi Almanca; 2 kişi Kanada'da ve tercihi yok. Ne yapalım?"},
	)
	store := newStore()
	res, rec, _ := run(t, m, store, baseInput())
	if len(store.cmds) != 1 || len(res.Saved) != 1 {
		t.Fatalf("the draft must be saved even with recipients waiting: %+v / %+v", store.cmds, res.Rejections)
	}
	cmd := store.cmds[0]
	if cmd.SMS != nil || cmd.Email != nil || cmd.Localization == nil || cmd.Localization.Mode != language.ModePerRecipient ||
		len(cmd.Localization.Variants) != 3 || len(cmd.Localization.Countries) != 2 {
		t.Fatalf("stored setup = %+v (sms %v)", cmd.Localization, cmd.SMS)
	}
	sum := cmd.Languages
	// The German preference in Türkiye follows Türkiye's mapping.
	if sum == nil || sum.Languages["tr"].Total != 302 || sum.Languages["en"].Total != 3 || sum.Languages["fr"].Total != 3 {
		t.Fatalf("languages = %+v", sum)
	}
	if want := []language.GroupCount{{Group: "country:CA", Count: 2}, {Group: "country:DE", Count: 2}}; !slices.Equal(sum.Unresolved, want) {
		t.Fatalf("waiting = %+v, want %+v", sum.Unresolved, want)
	}
	p, isErr := toolPayload(t, m, 1)
	if isErr {
		t.Fatal("a saved draft is not a tool error")
	}
	langs, _ := p["languages"].(map[string]any)
	waiting, _ := langs["unresolved"].([]any)
	if len(waiting) != 2 || p["language_note"] == nil || !strings.Contains(p["language_note"].(string), "ask the user") ||
		!strings.Contains(p["language_note"].(string), "in words") {
		t.Fatalf("the model must learn who waits and that it has to ask: %v", p)
	}
	saved := rec.events[slices.IndexFunc(rec.events, func(e Event) bool { return e.Type == EventDraftSaved })]
	if data, _ := saved.Data.(map[string]any); data["languages"] == nil {
		t.Fatalf("draft.saved must carry the language summary: %v", saved.Data)
	}
}

// The model may not decide for a group the user has not been asked about:
// the draft is refused and the model saves it without the decision.
func TestLoop_DecisionForAGroupNotPutToTheUser_Rejected(t *testing.T) {
	decision := `{"group":"country:DE","action":"assign","language":"en"}`
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: perRecipientDraft(11, 3, decision)},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: perRecipientDraft(11, 3, "")},
		mock.Call{Text: "Almanya'daki 2 kişi için ne yapalım?"},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput(11))
	if !slices.Equal(res.Rejections, []string{RejectAskUserFirst}) {
		t.Fatalf("rejections = %v, want ask_user_first", res.Rejections)
	}
	p, isErr := toolPayload(t, m, 1)
	if !isErr || !strings.Contains(mustMarshal(p), "per_recipient.decisions[0].group") {
		t.Fatalf("refusal = %v", p)
	}
	if len(store.cmds) != 1 || len(store.cmds[0].Localization.Decisions) != 0 {
		t.Fatalf("only the draft without the decision may be saved: %+v", store.cmds)
	}
}

// Once the base version left the group waiting, the user has been asked:
// the decision is saved and the group is assigned.
func TestLoop_DecisionForAGroupPutToTheUser_Saved(t *testing.T) {
	decision := `{"group":"country:DE","action":"assign","language":"en"},{"group":"country:CA","action":"exclude"}`
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: perRecipientDraft(11, 3, decision)},
		mock.Call{Text: "Almanya'dakiler İngilizce alacak; Kanada'dakiler gönderimden çıkarıldı."},
	)
	store := newStore()
	in := baseInput(11)
	in.DecisionGroups = map[int64][]string{11: {"country:CA", "country:DE"}}
	res, _, _ := run(t, m, store, in)
	if len(res.Rejections) != 0 || len(store.cmds) != 1 {
		t.Fatalf("rejections %v, saved %d", res.Rejections, len(store.cmds))
	}
	sum := store.cmds[0].Languages
	if sum.Languages["en"].Total != 5 || len(sum.Unresolved) != 0 ||
		!slices.Equal(sum.Excluded, []language.GroupCount{{Group: "country:CA", Count: 2}}) {
		t.Fatalf("summary after the decisions = %+v", sum)
	}
}

// One content for everyone states its language; it must be a supported one.
func TestLoop_SingleDraftStatesItsLanguage(t *testing.T) {
	without := strings.Replace(validNew, `"content_language":"tr",`, "", 1)
	german := strings.Replace(validNew, `"content_language":"tr"`, `"content_language":"de"`, 1)
	english := strings.Replace(validNew, `"content_language":"tr"`, `"content_language":"en"`, 1)
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: without},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: german},
		mock.Call{ToolName: "save_campaign_draft", ToolInput: english},
		mock.Call{Text: "İngilizce taslağı kaydettim."},
	)
	store := newStore()
	res, _, _ := run(t, m, store, baseInput())
	for i, code := range []string{"required", "unsupported_language"} {
		p, isErr := toolPayload(t, m, i+1)
		if s := mustMarshal(p); !isErr || !strings.Contains(s, `"content_language"`) || !strings.Contains(s, code) {
			t.Errorf("round %d: %s, want %s at content_language", i+1, s, code)
		}
	}
	if len(store.cmds) != 1 || res.Outcome != OutcomeOK {
		t.Fatalf("saved %d, outcome %s", len(store.cmds), res.Outcome)
	}
	loc := store.cmds[0].Localization
	if loc == nil || loc.Mode != language.ModeSingle || loc.Language != "en" || store.cmds[0].SMS == nil {
		t.Fatalf("single English draft stored as %+v", loc)
	}
	if sum := store.cmds[0].Languages; sum == nil || sum.Languages["en"].Total != 312 || len(sum.Unresolved) != 0 {
		t.Fatalf("everyone gets the English content: %+v", sum)
	}
}

// A per-recipient draft keeps its content in the variants only.
func TestLoop_PerRecipientDraftWithTopLevelContent_Rejected(t *testing.T) {
	mixed := strings.Replace(perRecipientDraft(0, 0, ""), `"change_summary"`, `"sms":{"text":"Merhaba"},"change_summary"`, 1)
	m := mock.NewScripted(
		mock.Call{ToolName: "save_campaign_draft", ToolInput: mixed},
		mock.Call{Text: "Düzeltiyorum."},
	)
	store := newStore()
	run(t, m, store, baseInput())
	p, isErr := toolPayload(t, m, 1)
	if !isErr || !strings.Contains(mustMarshal(p), "per_recipient_content") || len(store.cmds) != 0 {
		t.Fatalf("mixed content = %v, saved %d", p, len(store.cmds))
	}
}

func mustMarshal(v any) string { b, _ := json.Marshal(v); return string(b) }
