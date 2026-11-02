package turns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/contextbuilder"
	"smart-campaign-composer/backend/internal/chat/router"
	"smart-campaign-composer/backend/internal/content"
	"smart-campaign-composer/backend/internal/i18n"
	"smart-campaign-composer/backend/internal/language"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/segment"
)

// historyLimit bounds how many finished turns are loaded; the context
// builder keeps fewer.
const historyLimit = 20

// state is everything a turn is decided and built from.
type state struct {
	ChatID   string
	TurnID   string
	UserText string
	Lang     i18n.Lang // the interface language the turn was started from
	Focus    *int64
	Views    []contextbuilder.CampaignView
	History  []contextbuilder.HistoryTurn
}

// Prepared is the context of a turn.
type Prepared struct {
	ChatID  string
	Route   router.Route
	Request llm.Request
	Input   agentloop.Input
}

// Prepare loads the chat state, routes the message and builds the request.
// Execute runs the same steps itself so it can audit and announce the route.
func (r *Runner) Prepare(ctx context.Context, turnID string) (Prepared, error) {
	st, err := r.load(ctx, turnID)
	if err != nil {
		return Prepared{}, err
	}
	route, _, err := router.Run(ctx, r.d.Router, routerInput(st))
	if err != nil {
		return Prepared{}, err
	}
	return r.build(st, route), nil
}

func (r *Runner) load(ctx context.Context, turnID string) (state, error) {
	st := state{TurnID: turnID}
	var lang *string
	err := r.d.Pool.QueryRow(ctx, `
		SELECT t.chat_id::text, t.focus_campaign_id, m.text, t.lang
		FROM turns t JOIN messages m ON m.turn_id = t.id AND m.role = 'user'
		WHERE t.id = $1`, turnID).Scan(&st.ChatID, &st.Focus, &st.UserText, &lang)
	st.Lang = i18n.TR
	if lang != nil && *lang == string(i18n.EN) {
		st.Lang = i18n.EN
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return state{}, ErrTurnNotFound
	}
	if err != nil {
		return state{}, err
	}
	if st.Views, err = r.loadCampaigns(ctx, st.ChatID); err != nil {
		return state{}, fmt.Errorf("load campaigns: %w", err)
	}
	if st.History, err = r.loadHistory(ctx, st.ChatID, turnID); err != nil {
		return state{}, fmt.Errorf("load history: %w", err)
	}
	return st, nil
}

func routerInput(st state) router.Input {
	in := router.Input{UserText: st.UserText, FocusCampaignID: st.Focus, Lang: string(st.Lang)}
	for _, v := range st.Views {
		in.Campaigns = append(in.Campaigns, router.Campaign{ID: v.ID, Ordinal: v.Ordinal, Name: v.Name, Status: v.Status, Summary: v.Summary})
	}
	for _, h := range st.History {
		in.History = append(in.History, router.Turn{User: h.UserText, Assistant: h.AssistantText})
	}
	return in
}

// build turns a verified route into the main model's request and the loop's
// write permissions.
func (r *Runner) build(st state, route router.Route) Prepared {
	in := contextbuilder.Input{
		History: st.History, Campaigns: st.Views, UserText: st.UserText, Today: r.d.Clock.Now(),
		Targets: contextbuilder.Targets{
			Intent: route.Intent, Source: route.Source, Allowed: route.Allowed, Overflow: route.Overflow,
			References: route.References, Candidates: route.Candidates, AllowCreate: route.AllowCreate,
		},
	}
	req := contextbuilder.Build(in)
	candidates := route.Candidates
	if len(candidates) == 0 {
		for _, v := range st.Views {
			if v.Status == "draft" {
				candidates = append(candidates, v.ID)
			}
		}
	}
	// A language group may be decided only once its campaign's current
	// version has put it to the user (left it waiting) or decided it.
	groups := map[int64][]string{}
	for _, v := range st.Views {
		if c := v.Current; c != nil && c.Localization != nil && c.Languages != nil {
			groups[v.ID] = language.KnownGroups(*c.Localization, *c.Languages)
		}
	}
	return Prepared{
		ChatID:  st.ChatID,
		Route:   route,
		Request: req,
		Input: agentloop.Input{
			Request: req, Allowed: contextbuilder.AllowedTargets(in), Candidates: candidates,
			Overflow: contextbuilder.OverflowTargets(in), AllowCreate: route.AllowCreate, DecisionGroups: groups,
		},
	}
}

// RouteView is the routing decision as the user sees it: what the turn will
// do and which campaigns it targets, by number and name.
type RouteView struct {
	Intent       string      `json:"intent"`
	Source       string      `json:"source"`
	AllowCreate  bool        `json:"allow_create"`
	Targets      []TargetRef `json:"targets"`
	Overflow     []TargetRef `json:"overflow,omitempty"`
	References   []TargetRef `json:"references,omitempty"`
	Candidates   []TargetRef `json:"candidates,omitempty"`
	Question     string      `json:"question,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	RejectReason string      `json:"reject_reason,omitempty"`
}

// TargetRef names a campaign for display.
type TargetRef struct {
	CampaignID int64  `json:"campaign_id"`
	Ordinal    int    `json:"ordinal"`
	Name       string `json:"name"`
}

func routeView(st state, route router.Route) RouteView {
	byID := make(map[int64]contextbuilder.CampaignView, len(st.Views))
	for _, v := range st.Views {
		byID[v.ID] = v
	}
	refs := func(ids []int64) []TargetRef {
		out := []TargetRef{}
		for _, id := range ids {
			if v, ok := byID[id]; ok {
				out = append(out, TargetRef{CampaignID: id, Ordinal: v.Ordinal, Name: v.Name})
			}
		}
		return out
	}
	return RouteView{
		Intent: route.Intent, Source: route.Source, AllowCreate: route.AllowCreate,
		Targets: refs(route.Allowed), Overflow: refs(route.Overflow), References: refs(route.References),
		Candidates: refs(route.Candidates), Question: route.Question, Reason: route.Reason, RejectReason: route.RejectReason,
	}
}

func (r *Runner) loadCampaigns(ctx context.Context, chatID string) ([]contextbuilder.CampaignView, error) {
	rows, err := r.d.Pool.Query(ctx, `
		SELECT c.id, c.ordinal, c.name, c.status, c.updated_at,
		       v.version, v.source, v.segment_rule, v.sms, v.email, v.audience, v.localization, v.language_summary, v.ab_test
		FROM campaigns c JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.chat_id = $1 ORDER BY c.ordinal`, chatID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (contextbuilder.CampaignView, error) {
		var (
			c       contextbuilder.CampaignView
			v       contextbuilder.Version
			updated time.Time
			sms     *content.SMS
			email   *content.Email
			loc     *language.Settings
			langs   *language.Summary
		)
		if err := row.Scan(&c.ID, &c.Ordinal, &c.Name, &c.Status, &updated,
			&v.Version, &v.Source, &v.SegmentRule, &sms, &email, &v.Audience, &loc, &langs, &v.ABTest); err != nil {
			return c, err
		}
		v.SMS, v.Email, v.Localization, v.Languages = sms, email, loc, langs
		c.UpdatedAt, c.Version, c.Source, c.Current = updated, v.Version, v.Source, &v
		c.Summary = summary(v)
		return c, nil
	})
}

func summary(v contextbuilder.Version) string {
	sms, email := v.SMS != nil, v.Email != nil
	var langs []string
	if loc := v.Localization; loc != nil && loc.Mode == language.ModePerRecipient {
		for _, vr := range loc.Variants {
			langs = append(langs, vr.Language)
		}
		if len(loc.Variants) > 0 {
			sms, email = loc.Variants[0].SMS != nil, loc.Variants[0].Email != nil
		}
	}
	var channels []string
	if sms {
		channels = append(channels, "SMS")
	}
	if email {
		channels = append(channels, "email")
	}
	s := fmt.Sprintf("%s; %d kişi; %d koşul", strings.Join(channels, "+"), v.Audience.Total, len(v.SegmentRule.Conditions))
	if len(langs) > 0 {
		s += "; diller " + strings.Join(langs, "/")
	}
	return s
}

func (r *Runner) loadHistory(ctx context.Context, chatID, currentTurnID string) ([]contextbuilder.HistoryTurn, error) {
	rows, err := r.d.Pool.Query(ctx, `
		SELECT t.id::text, coalesce(t.outcome, 'failed'), u.text, coalesce(a.text, '')
		FROM turns t
		JOIN messages u ON u.turn_id = t.id AND u.role = 'user'
		LEFT JOIN messages a ON a.turn_id = t.id AND a.role = 'assistant'
		WHERE t.chat_id = $1 AND t.id <> $2 AND t.status <> 'running'
		ORDER BY t.started_at DESC, t.id DESC LIMIT $3`, chatID, currentTurnID, historyLimit)
	if err != nil {
		return nil, err
	}
	type row struct {
		id string
		contextbuilder.HistoryTurn
	}
	turns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var t row
		err := r.Scan(&t.id, &t.Outcome, &t.UserText, &t.AssistantText)
		return t, err
	})
	if err != nil || len(turns) == 0 {
		return nil, err
	}
	slices.Reverse(turns)

	ids := make([]string, len(turns))
	for i, t := range turns {
		ids[i] = t.id
	}
	refRows, err := r.d.Pool.Query(ctx, `
		SELECT r.turn_id::text, c.ordinal, v.version, r.action, v.change_summary
		FROM turn_campaign_refs r
		JOIN campaigns c ON c.id = r.campaign_id
		JOIN campaign_versions v ON v.id = r.version_id
		WHERE r.turn_id = ANY($1::uuid[]) ORDER BY v.id`, ids)
	if err != nil {
		return nil, err
	}
	refs := map[string][]contextbuilder.Ref{}
	_, err = pgx.CollectRows(refRows, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			turnID string
			ref    contextbuilder.Ref
		)
		err := row.Scan(&turnID, &ref.Ordinal, &ref.Version, &ref.Action, &ref.ChangeSummary)
		refs[turnID] = append(refs[turnID], ref)
		return struct{}{}, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]contextbuilder.HistoryTurn, len(turns))
	for i, t := range turns {
		t.Refs = refs[t.id]
		out[i] = t.HistoryTurn
	}
	return out, nil
}

// audienceCounter counts audiences against the users table.
type audienceCounter struct{ r *Runner }

func (c audienceCounter) Count(ctx context.Context, rule segment.Rule) (segment.Audience, error) {
	return segment.Count(ctx, c.r.d.Pool, rule, c.r.d.Catalog, c.r.d.Clock.Now())
}

func (c audienceCounter) Buckets(ctx context.Context, rule segment.Rule) ([]language.Bucket, error) {
	return segment.LanguageBuckets(ctx, c.r.d.Pool, rule, c.r.d.Catalog, c.r.d.Clock.Now())
}
