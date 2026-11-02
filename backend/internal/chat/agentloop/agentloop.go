// Package agentloop runs one chat turn against the model: stream, validate
// tool input strictly in Go, save valid drafts, feed errors back for repair.
//
// Rules:
//   - Tool input is parsed from the raw streamed JSON, strictly. A tolerant
//     parse could turn truncated input into a "valid" object.
//   - stop_reason is checked before any tool runs: a refusal or a truncated
//     (max_tokens) message never writes anything.
//   - Each draft is written in its own transaction by the store; a later
//     failure never rolls back a version the user has already seen.
//   - Text of an iteration that failed mid-stream is dropped.
package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/tools"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/geo"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/segment"
)

// Event types sent to the client while a turn runs.
const (
	EventAssistantDelta  = "assistant.delta"
	EventDraftStart      = "draft.start"
	EventDraftDelta      = "draft.delta"
	EventDraftSaved      = "draft.saved"
	EventDraftInvalid    = "draft.invalid"
	EventTargetAmbiguous = "target.ambiguous"
	// Iteration lifecycle lets the client discard text and draft previews from
	// an incomplete model call while preserving earlier completed calls.
	EventIterationStarted   = "iteration.started"
	EventIterationDiscarded = "iteration.discarded"
	// EventLocationAmbiguous carries place candidates when a place name in a
	// draft matches several places; the user must choose.
	EventLocationAmbiguous = "location.ambiguous"
)

// Limits of one turn.
const (
	MaxIterations     = 4
	MaxRejectedRounds = 3
	MaxTokensCeiling  = 32000
)

// Event is one live event for the client.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Sink receives live events. Implementations must not block the loop.
type Sink interface{ Emit(Event) }

// DraftSaver writes campaign versions for the current chat and turn.
type DraftSaver interface {
	SaveDraft(ctx context.Context, cmd campaign.DraftCmd) (campaign.Saved, error)
	RemoveABTest(ctx context.Context, campaignID int64, baseVersion int, changeSummary string) (campaign.Removed, error)
}

// AudienceCounter counts a validated rule's audience, and splits it by the
// facts a recipient's language is worked out from.
type AudienceCounter interface {
	Count(ctx context.Context, r segment.Rule) (segment.Audience, error)
	Buckets(ctx context.Context, r segment.Rule) ([]language.Bucket, error)
}

// Deps of the loop.
type Deps struct {
	LLM      llm.Client
	Store    DraftSaver
	Audience AudienceCounter
	Catalog  segment.Catalog
}

// Input of one turn.
type Input struct {
	Request    llm.Request
	Allowed    []int64 // campaigns this turn may write
	Candidates []int64 // offered to the user when a write targets something else
	// Overflow are campaigns the user named beyond the per-turn cap.
	Overflow []int64
	// AllowCreate tells whether a new campaign may be created.
	AllowCreate bool
	// DecisionGroups are, per campaign, the language groups the user has
	// been asked about: the ones its current version left waiting or
	// decided. A decision for any other group is refused.
	DecisionGroups map[int64][]string
}

// Outcomes of a turn.
const (
	OutcomeOK      = "ok"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
)

// Reasons a draft (a save_campaign_draft call) is rejected.
const (
	RejectInvalidJSON  = "invalid_json"
	RejectValidation   = "validation"    // rule or content checks
	RejectTarget       = "target"        // not a campaign this turn may write
	RejectStaleVersion = "stale_version" // the campaign changed meanwhile
	RejectAskUserFirst = "ask_user_first"
	RejectUnknownTool  = "unknown_tool"
)

// Result of a turn.
type Result struct {
	Text      string
	Saved     []campaign.Saved
	Outcome   string
	ErrorCode string
	Err       error
	// Rejections holds the reason of every rejected draft, in order. Repairs
	// counts the rejected rounds (rounds that saved nothing) sent back to the
	// model to repair: the round that exhausts the repair budget ends the
	// turn and is no repair.
	Rejections []string
	Repairs    int
	Calls      []llm.CallRecord
}

// Issue is a validation finding returned to the model and the UI.
type Issue struct {
	Path       string      `json:"path"`
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Candidates []geo.Place `json:"candidates,omitempty"`
}

// Run executes the turn.
func Run(ctx context.Context, d Deps, in Input, sink Sink) Result {
	l := &loop{d: d, in: in, sink: sink}
	return l.run(ctx)
}

type loop struct {
	d     Deps
	in    Input
	sink  Sink
	res   Result
	texts []string
	// mustAsk is set when a place name was ambiguous: the user has to choose,
	// so no draft may be saved for the rest of the turn.
	mustAsk bool
}

func (l *loop) run(ctx context.Context) Result {
	req := cloneRequest(l.in.Request)
	retriedTruncation := false
	rejected := 0

	for iter := 1; iter <= MaxIterations; iter++ {
		text, end, err := l.call(ctx, iter, req)
		if err != nil {
			l.discard(iter)
			return l.fail(llm.ErrorCode(err), err)
		}
		switch end.StopReason {
		case llm.StopRefusal:
			l.discard(iter)
			return l.fail("refusal", errors.New("model refused"))
		case llm.StopMaxTokens:
			l.discard(iter)
			if retriedTruncation {
				return l.fail("max_tokens", errors.New("response truncated twice"))
			}
			retriedTruncation = true
			req.MaxTokens = min(req.MaxTokens*2, MaxTokensCeiling)
			continue // re-issue the same request; nothing from it is used
		}
		if text != "" {
			l.texts = append(l.texts, text)
		}

		var uses []llm.Block
		for _, b := range end.Content {
			if b.Type == llm.BlockToolUse {
				uses = append(uses, b)
			}
		}
		if len(uses) == 0 {
			return l.finish()
		}

		results := make([]llm.Block, 0, len(uses))
		anyRejected := false
		savedBefore := len(l.res.Saved)
		for _, use := range uses {
			result, rejectedCall, err := l.handleTool(ctx, use)
			if err != nil {
				return l.fail("db_error", err)
			}
			anyRejected = anyRejected || rejectedCall
			results = append(results, result)
		}
		// A round that saved something is no rejected round: a call refused
		// next to the save (often one the model gave up on) costs no repair.
		if anyRejected && len(l.res.Saved) == savedBefore {
			rejected++
			if rejected >= MaxRejectedRounds {
				return l.fail("repair_exhausted", errors.New("tool input rejected three times"))
			}
			l.res.Repairs++
		}
		req.Messages = append(req.Messages,
			llm.Message{Role: llm.RoleAssistant, Blocks: replayable(end.Content)},
			llm.Message{Role: llm.RoleUser, Blocks: results})
	}
	return l.fail("iteration_limit", errors.New("turn exceeded iteration limit"))
}

func (l *loop) discard(iter int) {
	l.sink.Emit(Event{Type: EventIterationDiscarded, Data: map[string]any{"iteration": iter}})
}

// cloneRequest returns a copy whose message slice can be appended to.
func cloneRequest(r llm.Request) llm.Request {
	r.Messages = slices.Clone(r.Messages)
	return r
}

// call streams one model call, forwarding live events. It returns the text
// of the call and its MessageEnd; a stream error drops the text.
func (l *loop) call(ctx context.Context, iter int, req llm.Request) (string, llm.MessageEnd, error) {
	l.sink.Emit(Event{Type: EventIterationStarted, Data: map[string]any{"iteration": iter}})
	start := time.Now()
	rec := llm.CallRecord{Kind: "main", Iteration: iter}
	defer func() {
		rec.Latency = time.Since(start)
		l.res.Calls = append(l.res.Calls, rec)
	}()

	s, err := l.d.LLM.Stream(ctx, req)
	if err != nil {
		rec.ErrorCode = llm.ErrorCode(err)
		return "", llm.MessageEnd{}, err
	}
	defer s.Close()

	var text strings.Builder
	var end *llm.MessageEnd
	for s.Next() {
		switch e := s.Event().(type) {
		case llm.TextDelta:
			text.WriteString(e.Text)
			l.sink.Emit(Event{Type: EventAssistantDelta, Data: map[string]any{"text": e.Text}})
		case llm.ToolUseStart:
			l.sink.Emit(Event{Type: EventDraftStart, Data: map[string]any{"tool_use_id": e.ID}})
		case llm.ToolInputDelta:
			l.sink.Emit(Event{Type: EventDraftDelta, Data: map[string]any{"tool_use_id": e.ID, "partial_json": e.PartialJSON}})
		case llm.MessageEnd:
			end = &e
		}
	}
	if err := s.Err(); err != nil {
		rec.ErrorCode = llm.ErrorCode(err)
		return "", llm.MessageEnd{}, err
	}
	if end == nil {
		err := &llm.Error{Kind: llm.KindStreamInterrupted, Err: errors.New("stream ended without message end")}
		rec.ErrorCode = llm.ErrorCode(err)
		return "", llm.MessageEnd{}, err
	}
	rec.StopReason, rec.Usage = end.StopReason, end.Usage
	return text.String(), *end, nil
}

// handleTool validates and runs one tool call. It returns the tool_result,
// whether the call was rejected, and a non-nil error only for infrastructure
// failures that must end the turn.
func (l *loop) handleTool(ctx context.Context, use llm.Block) (llm.Block, bool, error) {
	switch use.ToolName {
	case tools.SaveDraftName:
		return l.saveDraft(ctx, use)
	case tools.RemoveABTestName:
		return l.removeABTest(ctx, use)
	}
	return l.reject(use, RejectUnknownTool, map[string]any{"errors": []Issue{{Path: "", Code: "unknown_tool",
		Message: "yalnızca " + tools.SaveDraftName + ", " + tools.RemoveABTestName + " kullanılabilir"}}})
}

// reject records a rejected call and returns its error tool_result.
func (l *loop) reject(use llm.Block, reason string, payload any) (llm.Block, bool, error) {
	l.res.Rejections = append(l.res.Rejections, reason)
	return toolResult(use.ToolUseID, payload, true), true, nil
}

func (l *loop) saveDraft(ctx context.Context, use llm.Block) (llm.Block, bool, error) {
	in, err := tools.ParseSaveDraft(use.Input)
	if err != nil {
		l.emitInvalid(use.ToolUseID, []Issue{{Code: "invalid_json", Message: err.Error()}})
		return l.reject(use, RejectInvalidJSON, map[string]string{"INVALID_JSON": string(use.Input)})
	}
	if reason, issues := l.refuseWrite(in.CampaignID); reason != "" {
		return l.reject(use, reason, map[string]any{"errors": issues})
	}

	rule, warnings, issues := l.validate(in)
	if len(issues) > 0 {
		l.emitInvalid(use.ToolUseID, issues)
		for _, is := range issues {
			if is.Code == "ambiguous_place" {
				l.mustAsk = true
				l.sink.Emit(Event{Type: EventLocationAmbiguous, Data: map[string]any{
					"tool_use_id": use.ToolUseID, "path": is.Path, "candidates": is.Candidates}})
			}
		}
		return l.reject(use, RejectValidation, map[string]any{"errors": issues})
	}
	if early := l.unaskedDecisions(in); len(early) > 0 {
		l.emitInvalid(use.ToolUseID, early)
		return l.reject(use, RejectAskUserFirst, map[string]any{"errors": early})
	}

	audience, err := l.d.Audience.Count(ctx, rule)
	if err != nil {
		return llm.Block{}, false, fmt.Errorf("count audience: %w", err)
	}
	buckets, err := l.d.Audience.Buckets(ctx, rule)
	if err != nil {
		return llm.Block{}, false, fmt.Errorf("language buckets: %w", err)
	}
	settings := in.Settings()
	languages := language.Summarize(settings, in.SMS, in.Email, buckets)
	// A change that leaves ab_test out keeps the campaign's test: the store
	// carries it over, or refuses the draft when it no longer fits.
	saved, err := l.d.Store.SaveDraft(ctx, campaign.DraftCmd{
		CampaignID: in.CampaignID, BaseVersion: in.BaseVersion, Name: strings.TrimSpace(in.Name),
		Rule: rule, SMS: in.SMS, Email: in.Email, Audience: audience, ChangeSummary: in.ChangeSummary,
		Localization: &settings, Languages: &languages, ABTest: in.ABTest,
	})
	if reason, payload := l.refused(use.ToolUseID, err); reason != "" {
		return l.reject(use, reason, payload)
	}
	if err != nil {
		return llm.Block{}, false, err
	}

	saved.ChangeSummary = in.ChangeSummary
	l.res.Saved = append(l.res.Saved, saved)
	l.sink.Emit(Event{Type: EventDraftSaved, Data: map[string]any{
		"tool_use_id": use.ToolUseID, "campaign_id": saved.CampaignID, "ordinal": saved.Ordinal,
		"version": saved.Version, "created": saved.Created, "audience": audience, "languages": languages, "warnings": warnings,
	}})
	result := map[string]any{
		"saved": map[string]any{"campaign_id": saved.CampaignID, "ref": fmt.Sprintf("Kampanya %d", saved.Ordinal),
			"version": saved.Version, "created": saved.Created},
		"audience":  audience,
		"languages": languages,
		"warnings":  warnings,
	}
	if len(languages.Unresolved) > 0 {
		// For the model only (the UI shows the groups itself), so in English
		// like the rest of the instructions.
		result["language_note"] = "Some recipients wait for a language decision (languages.unresolved); approval stays locked until " +
			"every group is decided. variant:<language>: if the user wants everyone in their own language, add that variant and " +
			"save again. country:<country>: ask the user, group by group, whether to assign the group " +
			"to one of the supported languages (" + strings.Join(language.Supported, ", ") + ") or to leave it out; " +
			"do not decide yourself. A decision for a group the user was not asked about is refused. " +
			"When you write to the user, name the languages and countries in words, not as codes or group keys."
	}
	if saved.ABTestKept {
		result["ab_test_note"] = abTestKeptNote
	}
	return toolResult(use.ToolUseID, result, false), false, nil
}

// removeABTest runs remove_ab_test: the campaign's next version without its
// A/B test, everything else as it was. A draft saved without ab_test keeps
// the test, so this separate call is the one way a turn removes it; the
// model makes it only when the user asks.
func (l *loop) removeABTest(ctx context.Context, use llm.Block) (llm.Block, bool, error) {
	in, err := tools.ParseRemoveABTest(use.Input)
	if err != nil {
		l.emitInvalid(use.ToolUseID, []Issue{{Code: "invalid_json", Message: err.Error()}})
		return l.reject(use, RejectInvalidJSON, map[string]string{"INVALID_JSON": string(use.Input)})
	}
	if reason, issues := l.refuseWrite(&in.CampaignID); reason != "" {
		return l.reject(use, reason, map[string]any{"errors": issues})
	}
	removed, err := l.d.Store.RemoveABTest(ctx, in.CampaignID, in.BaseVersion, in.ChangeSummary)
	if reason, payload := l.refused(use.ToolUseID, err); reason != "" {
		return l.reject(use, reason, payload)
	}
	if err != nil {
		return llm.Block{}, false, err
	}

	saved := removed.Saved
	l.res.Saved = append(l.res.Saved, saved)
	l.sink.Emit(Event{Type: EventDraftSaved, Data: map[string]any{
		"tool_use_id": use.ToolUseID, "campaign_id": saved.CampaignID, "ordinal": saved.Ordinal,
		"version": saved.Version, "created": false, "audience": removed.Audience, "languages": removed.Languages,
	}})
	return toolResult(use.ToolUseID, map[string]any{
		"saved": map[string]any{"campaign_id": saved.CampaignID, "ref": fmt.Sprintf("Kampanya %d", saved.Ordinal),
			"version": saved.Version, "created": false},
		"audience":        removed.Audience,
		"ab_test_removed": true,
	}, false), false, nil
}

// Notes for the model about the A/B test of a draft that left ab_test out,
// in English like the rest of its instructions.
const (
	abTestKeptNote = "ab_test was not sent, so the campaign's A/B test was kept with its B side unchanged. " +
		"Only remove_ab_test removes it, and only when the user asks for that."
	abTestOutdatedNote = "This campaign has an A/B test. A save without ab_test keeps it, but the kept B side does not fit " +
		"the new A contents (see errors). Save again with ab_test holding an updated B for every A content. " +
		"Only remove_ab_test removes the test, and only when the user explicitly asked for that."
)

// refuseWrite tells why a call may not write campaign id (nil: a new
// campaign) in this turn, with the issues for the model; "" when it may.
func (l *loop) refuseWrite(id *int64) (string, []Issue) {
	switch {
	case l.mustAsk:
		return RejectAskUserFirst, []Issue{{Path: "segment_rule", Code: "ask_user_first",
			Message: "Bir yer adı belirsiz; önce kullanıcıya hangi yeri kastettiğini sor. Bu mesajda kayıt yapılamaz."}}
	case id == nil && !l.in.AllowCreate:
		l.sink.Emit(Event{Type: EventTargetAmbiguous, Data: map[string]any{"candidates": l.in.Candidates}})
		return RejectTarget, []Issue{{Path: "campaign_id", Code: "create_not_allowed",
			Message: "Bu istekte yeni kampanya oluşturulamaz: kullanıcı yeni kampanya istemedi. Kaydetme; " +
				"mevcut bir kampanyayı mı kastettiğini, yoksa yeni kampanya mı istediğini sor."}}
	case id != nil && slices.Contains(l.in.Overflow, *id):
		return RejectTarget, []Issue{{Path: "campaign_id", Code: "target_overflow",
			Message: "Bir mesajda en fazla 3 kampanya değiştirilebilir. Bunu kaydetme; kullanıcıya bu kampanyayı ayrı bir mesajda istemesini söyle."}}
	case id != nil && !slices.Contains(l.in.Allowed, *id):
		l.sink.Emit(Event{Type: EventTargetAmbiguous, Data: map[string]any{"candidates": l.in.Candidates}})
		return RejectTarget, []Issue{{Path: "campaign_id", Code: "target_not_allowed",
			Message: "Bu kampanya bu turda hedef değil. Kaydetme; kullanıcıya hangi kampanyayı kastettiğini sor."}}
	}
	return "", nil
}

// refused maps the store's refusal of a write to the rejection reason and
// the model's feedback, and shows the issues in the live preview. The
// reason is "" for any other error.
func (l *loop) refused(toolUseID string, err error) (string, map[string]any) {
	var (
		stale    *campaign.StaleError
		outdated *campaign.OutdatedABTestError
		reason   string
		issues   []Issue
	)
	payload := map[string]any{}
	switch {
	case errors.As(err, &stale):
		reason = RejectStaleVersion
		issues = []Issue{{Path: "base_version", Code: "stale_version",
			Message: fmt.Sprintf("Kampanya bu arada değişti; güncel versiyon %d. Güncel hali koruyarak tekrar kaydet.", stale.CurrentVersion)}}
		payload["current_version"] = stale.CurrentVersion
	case errors.As(err, &outdated):
		reason = RejectValidation
		issues = []Issue{{Path: "ab_test", Code: "ab_test_outdated",
			Message: "ab_test gönderilmediği için kampanyanın A/B testi korunuyor, ama B yeni A içeriğine uymuyor. " +
				"B'yi yeni A'ya göre güncelleyip ab_test ile tekrar kaydet."}}
		for _, is := range outdated.Issues {
			issues = append(issues, Issue{Path: is.Path, Code: is.Code, Message: is.Message})
		}
		payload["ab_test_note"] = abTestOutdatedNote
	case errors.Is(err, campaign.ErrNotDraft):
		reason = RejectTarget
		issues = []Issue{{Path: "campaign_id", Code: "not_draft",
			Message: "Kampanya onaylanmış; değiştirilemez. Kullanıcıya yeni bir kopya oluşturmayı öner."}}
	case errors.Is(err, campaign.ErrNotFound):
		reason = RejectTarget
		issues = []Issue{{Path: "campaign_id", Code: "not_found", Message: "Bu sohbette böyle bir kampanya yok."}}
	case errors.Is(err, campaign.ErrNoABTest):
		reason = RejectValidation
		issues = []Issue{{Path: "campaign_id", Code: "no_ab_test", Message: "Bu kampanyada A/B testi yok; kaldırılacak bir şey yok."}}
	default:
		return "", nil
	}
	l.emitInvalid(toolUseID, issues)
	payload["errors"] = issues
	return reason, payload
}

// unaskedDecisions refuses decisions for language groups the user has not
// been asked about: only the base version's waiting or decided groups may be
// decided, so the model cannot settle a group on its own in the turn it
// first sees it.
func (l *loop) unaskedDecisions(in tools.SaveDraftInput) []Issue {
	if in.PerRecipient == nil {
		return nil
	}
	var known []string
	if in.CampaignID != nil {
		known = l.in.DecisionGroups[*in.CampaignID]
	}
	var issues []Issue
	for i, d := range in.PerRecipient.Decisions {
		if !slices.Contains(known, d.Group) {
			issues = append(issues, Issue{Path: fmt.Sprintf("per_recipient.decisions[%d].group", i), Code: "ask_user_first",
				Message: fmt.Sprintf("%s grubu kullanıcıya henüz sorulmadı. Bu kararı kaydetme; taslağı kararsız kaydedip kullanıcıya sor.", d.Group)})
		}
	}
	return issues
}

func (l *loop) validate(in tools.SaveDraftInput) (segment.Rule, []Issue, []Issue) {
	var issues, warnings []Issue
	if strings.TrimSpace(in.Name) == "" {
		issues = append(issues, Issue{Path: "name", Code: "required", Message: "kampanya adı boş olamaz"})
	}
	if in.CampaignID != nil && in.BaseVersion == nil {
		issues = append(issues, Issue{Path: "base_version", Code: "required", Message: "mevcut kampanyayı değiştirirken base_version gerekli"})
	}
	res := segment.Validate(in.SegmentRule, l.d.Catalog)
	for _, e := range res.Errors {
		issues = append(issues, Issue{Path: "segment_rule." + e.Path, Code: e.Code, Message: e.Message, Candidates: e.Candidates})
	}
	for _, w := range res.Warnings {
		warnings = append(warnings, Issue{Path: "segment_rule." + w.Path, Code: w.Code, Message: w.Message})
	}
	settings := in.Settings()
	if settings.Mode == language.ModePerRecipient {
		for _, f := range []struct {
			path string
			set  bool
		}{{"sms", in.SMS != nil}, {"email", in.Email != nil}, {"content_language", in.ContentLanguage != ""}} {
			if f.set {
				issues = append(issues, Issue{Path: f.path, Code: "per_recipient_content",
					Message: "per_recipient ile içerik yalnız variants içinde olur; sms, email ve content_language gönderme"})
			}
		}
	} else {
		for _, e := range content.ValidateDraft(in.SMS, in.Email) {
			issues = append(issues, Issue{Path: e.Path, Code: e.Code, Message: e.Message})
		}
	}
	for _, e := range language.Validate(settings, l.countryKnown) {
		issues = append(issues, Issue{Path: e.Path, Code: e.Code, Message: e.Message})
	}
	if in.ABTest != nil {
		for _, e := range abtest.Validate(*in.ABTest, abtest.Contents(&settings, in.SMS, in.Email)) {
			issues = append(issues, Issue{Path: e.Path, Code: e.Code, Message: e.Message})
		}
	}
	return res.Normalized, warnings, issues
}

func (l *loop) countryKnown(code string) bool {
	if l.d.Catalog.Geo == nil {
		return false
	}
	_, ok := l.d.Catalog.Geo.Country(code)
	return ok
}

func (l *loop) emitInvalid(toolUseID string, issues []Issue) {
	l.sink.Emit(Event{Type: EventDraftInvalid, Data: map[string]any{"tool_use_id": toolUseID, "errors": issues}})
}

func (l *loop) finish() Result {
	l.res.Text = strings.Join(l.texts, "\n")
	l.res.Outcome = OutcomeOK
	return l.res
}

func (l *loop) fail(code string, err error) Result {
	l.res.Text = strings.Join(l.texts, "\n")
	l.res.ErrorCode, l.res.Err = code, err
	l.res.Outcome = OutcomeFailed
	if len(l.res.Saved) > 0 {
		l.res.Outcome = OutcomePartial
	}
	return l.res
}

// replayable returns the assistant content for the next request. A tool_use
// whose streamed input is not valid JSON cannot be sent back to the API, so
// its input is replaced by {} (the tool_result explains the error).
func replayable(content []llm.Block) []llm.Block {
	out := slices.Clone(content)
	for i, b := range out {
		if b.Type == llm.BlockToolUse && !json.Valid(b.Input) {
			out[i].Input = json.RawMessage("{}")
			out[i].Provider = nil
		}
	}
	return out
}

func toolResult(id string, payload any, isError bool) llm.Block {
	b, _ := json.Marshal(payload)
	return llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Text: string(b), IsError: isError}
}
