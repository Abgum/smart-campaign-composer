// Package contextbuilder builds the prompt for one chat turn from database
// state. It is a projection, not a growing transcript: earlier turns are
// replayed as plain text (no thinking or tool blocks), and campaign state
// comes from the current versions. Because no earlier thinking block is ever
// replayed, rebuilding the context each turn cannot break the API's
// history-binding check for thinking blocks.
package contextbuilder

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/abtest"
	"smart-campaign-composer/backend/internal/chat/tools"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/segment"
)

// Limits of the projection.
const (
	WindowTurns         = 6
	HistoryTokenBudget  = 6000
	MaxIndexedCampaigns = 20
	MaxFullTargets      = 3
	MaxTokens           = 16000
)

// Ref notes a campaign version a turn produced.
type Ref struct {
	Ordinal       int
	Version       int
	Action        string // created | revised
	ChangeSummary string
}

// HistoryTurn is one finished turn, oldest first.
type HistoryTurn struct {
	UserText      string
	AssistantText string
	Outcome       string // ok | partial | failed
	Refs          []Ref
}

// Version is a campaign version as shown to the model. A per-recipient
// version has its content in Localization's variants; Languages is how its
// audience split by language when it was saved, waiting groups included.
type Version struct {
	Version      int                `json:"version"`
	Source       string             `json:"source"`
	SegmentRule  segment.Rule       `json:"segment_rule"`
	SMS          *content.SMS       `json:"sms,omitempty"`
	Email        *content.Email     `json:"email,omitempty"`
	Audience     segment.Audience   `json:"audience"`
	Localization *language.Settings `json:"localization,omitempty"`
	Languages    *language.Summary  `json:"languages,omitempty"`
	ABTest       *abtest.Test       `json:"ab_test,omitempty"` // the B side of an A/B test
}

// CampaignView is a campaign of the chat.
type CampaignView struct {
	ID        int64
	Ordinal   int
	Name      string
	Status    string
	Version   int
	Source    string
	Summary   string
	UpdatedAt time.Time
	Current   *Version
}

// Targets is the verified routing decision: the turn's write boundary.
type Targets struct {
	Intent      string  // create | edit | clarify | chat
	Source      string  // router | fallback | empty_chat
	Allowed     []int64 // may be written
	Overflow    []int64 // named by the user beyond the cap; not writable
	References  []int64 // shown in full, read-only (e.g. the source of a copy)
	Candidates  []int64 // offered when asking
	AllowCreate bool
}

// Input of one build.
type Input struct {
	History   []HistoryTurn
	Campaigns []CampaignView
	Targets   Targets
	UserText  string
	Today     time.Time
}

// State is the JSON inside <chat_state>.
type State struct {
	Today           string          `json:"today"`
	Campaigns       []IndexEntry    `json:"campaigns"`
	Target          TargetState     `json:"target"`
	TargetCampaigns []TargetVersion `json:"target_campaigns"`
}

// IndexEntry is one line of the campaign index.
type IndexEntry struct {
	Ref     string `json:"ref"`
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Version int    `json:"version"`
	Source  string `json:"source"`
	Summary string `json:"summary"`
}

// TargetState tells the model what it may write.
type TargetState struct {
	Mode               string   `json:"mode"`
	Intent             string   `json:"intent,omitempty"`
	AllowedCampaignIDs []int64  `json:"allowed_campaign_ids"`
	Ambiguous          bool     `json:"ambiguous"`
	Candidates         []string `json:"candidates,omitempty"`
	AllowCreate        bool     `json:"allow_create"`
	Overflow           []string `json:"overflow,omitempty"`
}

// OverflowTargets returns resolved targets beyond MaxFullTargets. They are
// reported in chat_state and rejected on write instead of being dropped.
func OverflowTargets(in Input) []int64 {
	allowed := AllowedTargets(in)
	byID := campaignsByID(in.Campaigns)
	var out []int64
	for _, id := range append(slices.Clone(in.Targets.Allowed), in.Targets.Overflow...) {
		if c, ok := byID[id]; ok && c.Current != nil && !slices.Contains(allowed, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// TargetVersion is the full current version of a target campaign.
type TargetVersion struct {
	ID       int64  `json:"id"`
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Version
}

// EstimateTokens is a rough token estimate (≈3 characters per token).
func EstimateTokens(s string) int { return (utf8.RuneCountInString(s) + 2) / 3 }

// Build returns the request for one turn.
func Build(in Input) llm.Request {
	req := llm.Request{
		System:    SystemPrompt(),
		Tools:     []llm.Tool{tools.SaveDraftTool(), tools.RemoveABTestTool()},
		MaxTokens: MaxTokens,
	}
	for _, t := range window(in.History) {
		req.Messages = append(req.Messages,
			textMessage(llm.RoleUser, t.user),
			textMessage(llm.RoleAssistant, t.assistant))
	}
	state, _ := json.Marshal(buildState(in))
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "<chat_state>\n" + string(state) + "\n</chat_state>"},
		{Type: llm.BlockText, Text: in.UserText},
	}})
	return req
}

// AllowedTargets is the set of campaigns the turn may write: the routed
// targets that exist and are shown in full, capped at MaxFullTargets.
func AllowedTargets(in Input) []int64 {
	byID := campaignsByID(in.Campaigns)
	allowed := []int64{}
	for _, id := range in.Targets.Allowed {
		if c, ok := byID[id]; ok && c.Current != nil && len(allowed) < MaxFullTargets {
			allowed = append(allowed, id)
		}
	}
	return allowed
}

type renderedTurn struct{ user, assistant string }

// window keeps the last WindowTurns turns that matter (failed turns with no
// side effects carry nothing) and drops the oldest until the budget fits.
func window(history []HistoryTurn) []renderedTurn {
	var turns []renderedTurn
	for _, h := range history {
		if h.Outcome == "failed" && len(h.Refs) == 0 {
			continue
		}
		turns = append(turns, renderedTurn{user: h.UserText, assistant: assistantText(h)})
	}
	if len(turns) > WindowTurns {
		turns = turns[len(turns)-WindowTurns:]
	}
	cost := func(ts []renderedTurn) int {
		n := 0
		for _, t := range ts {
			n += EstimateTokens(t.user) + EstimateTokens(t.assistant)
		}
		return n
	}
	for len(turns) > 0 && cost(turns) > HistoryTokenBudget {
		turns = turns[1:]
	}
	return turns
}

func assistantText(h HistoryTurn) string {
	parts := []string{}
	if strings.TrimSpace(h.AssistantText) != "" {
		parts = append(parts, h.AssistantText)
	}
	for _, r := range h.Refs {
		verb := "saved"
		if r.Action == "created" {
			verb = "created"
		}
		note := fmt.Sprintf("[Kampanya %d → v%d %s", r.Ordinal, r.Version, verb)
		if r.ChangeSummary != "" {
			note += ": " + r.ChangeSummary
		}
		parts = append(parts, note+"]")
	}
	if len(parts) == 0 {
		return "(no reply)"
	}
	return strings.Join(parts, "\n")
}

func textMessage(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
}

func campaignsByID(cs []CampaignView) map[int64]CampaignView {
	m := make(map[int64]CampaignView, len(cs))
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func ref(ordinal int) string { return fmt.Sprintf("Kampanya %d", ordinal) }

func buildState(in Input) State {
	byID := campaignsByID(in.Campaigns)

	recent := slices.Clone(in.Campaigns)
	slices.SortFunc(recent, func(a, b CampaignView) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	if len(recent) > MaxIndexedCampaigns {
		recent = recent[:MaxIndexedCampaigns]
	}
	slices.SortFunc(recent, func(a, b CampaignView) int { return a.Ordinal - b.Ordinal })
	index := make([]IndexEntry, 0, len(recent))
	for _, c := range recent {
		index = append(index, IndexEntry{Ref: ref(c.Ordinal), ID: c.ID, Name: c.Name, Status: c.Status,
			Version: c.Version, Source: c.Source, Summary: c.Summary})
	}

	allowed := AllowedTargets(in)
	ts := TargetState{Mode: in.Targets.Source, Intent: in.Targets.Intent, AllowedCampaignIDs: allowed,
		Ambiguous: in.Targets.Intent == "clarify", AllowCreate: in.Targets.AllowCreate}
	for _, id := range OverflowTargets(in) {
		ts.Overflow = append(ts.Overflow, ref(byID[id].Ordinal))
	}
	for _, id := range in.Targets.Candidates {
		if c, ok := byID[id]; ok {
			ts.Candidates = append(ts.Candidates, ref(c.Ordinal))
		}
	}
	targets := make([]TargetVersion, 0, len(allowed))
	for _, id := range allowed {
		c := byID[id]
		targets = append(targets, TargetVersion{ID: c.ID, Ref: ref(c.Ordinal), Name: c.Name, Status: c.Status, Version: *c.Current})
	}
	for _, id := range in.Targets.References {
		c, ok := byID[id]
		if !ok || c.Current == nil || slices.Contains(allowed, id) || len(targets) >= MaxFullTargets {
			continue
		}
		targets = append(targets, TargetVersion{ID: c.ID, Ref: ref(c.Ordinal), Name: c.Name, Status: c.Status,
			ReadOnly: true, Version: *c.Current})
	}
	return State{Today: in.Today.Format("2006-01-02"), Campaigns: index, Target: ts, TargetCampaigns: targets}
}
