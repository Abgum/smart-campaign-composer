// Package router decides what a chat message asks for (create, edit,
// clarify, chat) and which campaigns it targets, before the main model runs.
//
// The language understanding is Claude's: it reads campaign names, numbers,
// the selected tab and the recent conversation, and proposes a decision
// through the strict route_request tool. Go only recognizes the literal #N
// form so a database id cannot be confused with a displayed campaign number.
// Go checks what is checkable: proposed ids belong to this chat, edit targets
// are editable drafts, and the per-turn cap. A decision that fails those
// checks becomes a clarifying question. The verified route is the write
// boundary the agent loop enforces.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/llm"
)

// ToolName is the router's only tool.
const ToolName = "route_request"

// Intents.
const (
	IntentCreate  = "create"
	IntentEdit    = "edit"
	IntentClarify = "clarify"
	IntentChat    = "chat"
)

// Sources of a route.
const (
	SourceRouter    = "router"
	SourceFallback  = "fallback"   // no usable decision from the router
	SourceEmptyChat = "empty_chat" // nothing to route: a first message creates
)

// Limits.
const (
	MaxTargets        = 3
	historyTurns      = 3
	maxAssistantRunes = 300
	maxTokens         = 4000
)

// Campaign is a campaign as the router sees it.
type Campaign struct {
	ID      int64  `json:"id"`
	Ordinal int    `json:"ordinal"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// Turn is a recent exchange.
type Turn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

// Input of a routing decision.
type Input struct {
	UserText        string
	FocusCampaignID *int64
	Campaigns       []Campaign
	History         []Turn // oldest first
	// Lang is the interface language of the request ("en" or Turkish); Go's
	// own questions are written in it.
	Lang string
}

// Decision is what the model proposes.
type Decision struct {
	Intent      string  `json:"intent"`
	CampaignIDs []int64 `json:"campaign_ids"`
	Question    string  `json:"question,omitempty"`
	Reason      string  `json:"reason"`
}

// Route is the verified decision.
type Route struct {
	Intent       string
	Source       string
	Allowed      []int64 // campaigns the turn may write
	Overflow     []int64 // targets beyond MaxTargets: named, not writable
	References   []int64 // shown in full, read-only (e.g. the source of a copy)
	Candidates   []int64 // offered when asking
	AllowCreate  bool
	Question     string
	Reason       string
	RejectReason string
}

type chatIndex struct {
	byID    map[int64]Campaign
	drafts  []int64 // by ordinal
	english bool    // Go's own questions in English (the interface language)
}

func index(in Input) chatIndex {
	ix := chatIndex{byID: make(map[int64]Campaign, len(in.Campaigns)), english: in.Lang == "en"}
	cs := slices.Clone(in.Campaigns)
	slices.SortFunc(cs, func(a, b Campaign) int { return a.Ordinal - b.Ordinal })
	for _, c := range cs {
		ix.byID[c.ID] = c
		if c.Status == "draft" {
			ix.drafts = append(ix.drafts, c.ID)
		}
	}
	return ix
}

// Validate checks a decision against the chat: ids must belong to it, edit
// targets must be drafts, and at most MaxTargets may be written.
func Validate(d Decision, in Input) Route {
	ix := index(in)
	base := Route{Source: SourceRouter, Reason: d.Reason}
	ids := dedupe(d.CampaignIDs)

	if d.Intent == IntentClarify {
		var cands []int64
		for _, id := range ids {
			if _, ok := ix.byID[id]; ok {
				cands = append(cands, id)
			}
		}
		return clarify(base, ix, "", d.Question, cands)
	}
	// #N is not a campaign reference in the UI. Even if N happens to match an
	// internal id or ordinal, a model guess cannot authorize a write to it.
	if ref := ambiguousCampaignHash(in.UserText); ref != "" {
		q := fmt.Sprintf("#%s ile hangi kampanyayı kastediyorsunuz? Lütfen kampanya adını veya ekranda görünen Kampanya N numarasını yazın.", ref)
		if ix.english {
			q = fmt.Sprintf("Which campaign do you mean by #%s? Please write its name or the Campaign N number shown on the screen.", ref)
		}
		return clarify(base, ix, "ambiguous_hash_reference", q, nil)
	}
	for _, id := range ids {
		if _, ok := ix.byID[id]; !ok {
			return clarify(base, ix, "unknown_campaign", "", nil)
		}
	}

	switch d.Intent {
	case IntentCreate:
		base.Intent, base.AllowCreate = IntentCreate, true
		base.References = ids[:min(len(ids), MaxTargets)]
		return base
	case IntentChat:
		base.Intent = IntentChat
		return base
	case IntentEdit:
		if len(ids) == 0 {
			return clarify(base, ix, "edit_without_target", "", nil)
		}
		var locked []string
		for _, id := range ids {
			if c := ix.byID[id]; c.Status != "draft" {
				locked = append(locked, ix.ref(c))
			}
		}
		if len(locked) > 0 {
			q := strings.Join(locked, ", ") + " onaylanmış olduğu için değiştirilemez. Yeni bir kampanya olarak kopyasını oluşturmamı ister misiniz?"
			if ix.english {
				q = strings.Join(locked, ", ") + " is approved and cannot change. Shall I create a copy as a new campaign?"
			}
			return clarify(base, ix, "not_editable", q, nil)
		}
		base.Intent = IntentEdit
		base.Allowed, base.Overflow = split(ids)
		return base
	}
	return clarify(base, ix, "invalid_intent", "", nil)
}

// ambiguousCampaignHash recognizes #N only when it is used as a campaign
// label ("#12 kampanyası", "Kampanya #12", "#12 için") or appears alone.
// Content such as "SMS'e #30 indirim ekle" must not override a target chosen
// from an explicit campaign name, visible ordinal, or selected tab.
func ambiguousCampaignHash(s string) string {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '#' || s[i+1] < '0' || s[i+1] > '9' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if strings.HasPrefix(s[j:], "'") || strings.HasPrefix(s[j:], "’") {
			return s[i+1 : j]
		}
		before := strings.Fields(strings.ToLower(s[:i]))
		after := strings.Fields(strings.ToLower(s[j:]))
		if len(before) == 0 && len(after) == 0 {
			return s[i+1 : j]
		}
		if len(before) > 0 && strings.Trim(before[len(before)-1], " :,") == "kampanya" {
			return s[i+1 : j]
		}
		if len(after) > 0 && strings.HasPrefix(after[0], "kampanya") {
			return s[i+1 : j]
		}
		if len(after) > 0 && after[0] == "için" {
			return s[i+1 : j]
		}
	}
	return ""
}

func clarify(base Route, ix chatIndex, rejectReason, question string, cands []int64) Route {
	r := Route{Intent: IntentClarify, Source: base.Source, Reason: base.Reason, RejectReason: rejectReason, Candidates: cands}
	if len(r.Candidates) == 0 {
		r.Candidates = ix.drafts
	}
	r.Question = strings.TrimSpace(question)
	if r.Question == "" {
		r.Question = defaultQuestion(r.Candidates, ix)
	}
	return r
}

func defaultQuestion(cands []int64, ix chatIndex) string {
	if len(cands) == 0 {
		if ix.english {
			return "Could you say a little more about what you want? Shall we change an existing campaign or prepare a new one?"
		}
		return "Ne yapmak istediğinizi biraz açar mısınız? Mevcut bir kampanyayı mı değiştirelim, yoksa yeni bir kampanya mı hazırlayalım?"
	}
	names := make([]string, len(cands))
	for i, id := range cands {
		names[i] = ix.ref(ix.byID[id])
	}
	if ix.english {
		return "Which campaign do you mean: " + strings.Join(names, ", ") + "? Or do you want a new campaign?"
	}
	return "Hangi kampanyayı kastediyorsunuz: " + strings.Join(names, ", ") + "? Ya da yeni bir kampanya mı istiyorsunuz?"
}

// ref names a campaign as the interface shows it.
func (ix chatIndex) ref(c Campaign) string {
	if ix.english {
		return fmt.Sprintf("Campaign %d (%s)", c.Ordinal, c.Name)
	}
	return fmt.Sprintf("Kampanya %d (%s)", c.Ordinal, c.Name)
}

func split(ids []int64) (allowed, overflow []int64) {
	if len(ids) <= MaxTargets {
		return ids, nil
	}
	return ids[:MaxTargets], ids[MaxTargets:]
}

func dedupe(ids []int64) []int64 {
	out := []int64{}
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// ---- model call ---------------------------------------------------------------

// Run routes a message. An empty chat needs no routing: the first message
// creates. Otherwise the router model decides; a failed model call is
// returned as an error (the turn fails, nothing is guessed), and an answer
// without a usable decision, or a missing router, leads to a question.
func Run(ctx context.Context, c llm.Client, in Input) (Route, *llm.CallRecord, error) {
	if len(in.Campaigns) == 0 {
		return Route{Intent: IntentCreate, Source: SourceEmptyChat, AllowCreate: true}, nil, nil
	}
	if c == nil {
		return clarify(Route{Source: SourceFallback}, index(in), "no_router", "", nil), nil, nil
	}
	rec := &llm.CallRecord{Kind: "router"}
	start := time.Now()
	d, err := decide(ctx, c, Request(in), rec)
	rec.Latency = time.Since(start)
	switch {
	case errors.Is(err, errUnusable):
		return clarify(Route{Source: SourceFallback}, index(in), rec.ErrorCode, "", nil), rec, nil
	case err != nil:
		return Route{}, rec, err
	}
	return Validate(d, in), rec, nil
}

var errUnusable = errors.New("router answered without a usable decision")

func decide(ctx context.Context, c llm.Client, req llm.Request, rec *llm.CallRecord) (Decision, error) {
	s, err := c.Stream(ctx, req)
	if err != nil {
		rec.ErrorCode = llm.ErrorCode(err)
		return Decision{}, err
	}
	defer s.Close()
	var (
		raw   []byte
		found bool
	)
	for s.Next() {
		switch e := s.Event().(type) {
		case llm.ToolUseEnd:
			if e.Name == ToolName && !found {
				raw, found = e.RawInput, true
			}
		case llm.MessageEnd:
			rec.StopReason, rec.Usage = e.StopReason, e.Usage
		}
	}
	if err := s.Err(); err != nil {
		rec.ErrorCode = llm.ErrorCode(err)
		return Decision{}, err
	}
	if !found || rec.StopReason == llm.StopRefusal || rec.StopReason == llm.StopMaxTokens {
		rec.ErrorCode = "no_decision"
		return Decision{}, errUnusable
	}
	d, err := parseDecision(raw)
	if err != nil {
		rec.ErrorCode = "invalid_decision"
		return Decision{}, errUnusable
	}
	return d, nil
}

func parseDecision(raw []byte) (Decision, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Decision
	if err := dec.Decode(&d); err != nil {
		return Decision{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Decision{}, errors.New("trailing data")
	}
	return d, nil
}

// routingInput is the JSON the router reads.
type routingInput struct {
	SelectedTab *int64           `json:"selected_tab"`
	Campaigns   []routedCampaign `json:"campaigns"`
	RecentTurns []Turn           `json:"recent_turns"`
	Message     string           `json:"message"`
}

type routedCampaign struct {
	ID      int64  `json:"id"`
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// Request builds the router call: a static system prompt and tool, and one
// user message with the routing input.
func Request(in Input) llm.Request {
	ri := routingInput{SelectedTab: in.FocusCampaignID, Campaigns: []routedCampaign{}, RecentTurns: []Turn{}, Message: in.UserText}
	cs := slices.Clone(in.Campaigns)
	slices.SortFunc(cs, func(a, b Campaign) int { return a.Ordinal - b.Ordinal })
	for _, c := range cs {
		ri.Campaigns = append(ri.Campaigns, routedCampaign{ID: c.ID, Ref: fmt.Sprintf("Kampanya %d", c.Ordinal),
			Name: c.Name, Status: c.Status, Summary: c.Summary})
	}
	hist := in.History
	if len(hist) > historyTurns {
		hist = hist[len(hist)-historyTurns:]
	}
	for _, h := range hist {
		ri.RecentTurns = append(ri.RecentTurns, Turn{User: h.User, Assistant: truncate(h.Assistant, maxAssistantRunes)})
	}
	body, _ := json.Marshal(ri)
	return llm.Request{
		System:    systemPrompt,
		Tools:     []llm.Tool{tool()},
		MaxTokens: maxTokens,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "<routing_input>\n" + string(body) + "\n</routing_input>"},
		}}},
	}
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func tool() llm.Tool {
	return llm.Tool{
		Name:        ToolName,
		Description: "Reports the intent of the latest user message and the campaigns it targets.",
		Strict:      true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"intent":       map[string]any{"type": "string", "enum": []string{IntentCreate, IntentEdit, IntentClarify, IntentChat}},
				"campaign_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"question":     map[string]any{"type": "string"},
				"reason":       map[string]any{"type": "string"},
			},
			"required":             []string{"intent", "campaign_ids", "reason"},
			"additionalProperties": false,
		},
	}
}

// systemPrompt is in English; marketers mostly write in Turkish, and the
// question the router asks them is in the language of their message. The
// Turkish phrases quoted are how marketers write.
const systemPrompt = `Decide what the latest user message to the campaign assistant asks for and which campaigns it targets. You do not write campaigns; call the route_request tool exactly once.

The input is in <routing_input>: selected_tab (the id of the campaign open in the interface, or null), campaigns (id, ref, name, status, summary), recent_turns and message. Marketers mostly write in Turkish.

intent:
- create: the user wants a new campaign. A request written for a new audience, city or goal that does not overlap the audience or topic of the existing campaigns is create, even without the word "kampanya" ("Ankara için hoş geldin mesajı hazırla"). A request to copy a campaign into a new one is create too; put the source in campaign_ids.
- edit: the user wants to change one or more existing campaigns; campaign_ids is required.
- clarify: the request is a change, but it is unclear which campaign is meant, or whether a new or an existing one. Write a short question in the language of the user's message in the question field; put the possible candidates in campaign_ids.
- chat: a message that changes no campaign (a question, thanks, a request for an explanation).

Choosing the target:
- If the message names a campaign by the ordinal shown in the chat ("Kampanya 2", "2. kampanya"), by its name or by a distinguishing trait ("İzmir olanı", "hoş geldin kampanyası"), choose it. An explicit mention in the message comes before selected_tab. A bare number after # such as "#12" is not an internal id; ask which campaign is meant.
- If the message names no campaign and selected_tab is set, choose it if the request applies to the open tab.
- If the chat has a single draft campaign and the request applies to it, choose it.
- The campaign discussed or changed most recently is not enough on its own. If there are several possible targets and none of the above points to one, choose clarify.
- Use only ids from the campaigns list. If the message mentions a campaign that is not in the list, choose clarify and say so in the question.
- Write a short reason for your decision in the reason field.`
