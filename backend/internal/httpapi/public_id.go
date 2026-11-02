package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/turns"
)

// Campaign IDs in HTTP are UUIDs. The numeric primary key is kept inside Go
// and PostgreSQL for existing relationships and the model's write boundary.
func (s *Server) lookupCampaignID(ctx context.Context, publicID string) (int64, error) {
	if !uuidRe.MatchString(publicID) {
		return 0, pgx.ErrNoRows
	}
	var id int64
	err := s.d.Pool.QueryRow(ctx, `SELECT id FROM campaigns WHERE public_id = $1`, publicID).Scan(&id)
	return id, err
}

func (s *Server) pathCampaignID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := s.lookupCampaignID(r.Context(), chi.URLParam(r, "campaignID"))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "campaign_not_found", "kampanya bulunamadı")
	case err != nil:
		internalError(w, err)
	default:
		return id, true
	}
	return 0, false
}

func (s *Server) publicCampaignID(ctx context.Context, id int64) (string, error) {
	var publicID string
	err := s.d.Pool.QueryRow(ctx, `SELECT public_id::text FROM campaigns WHERE id = $1`, id).Scan(&publicID)
	return publicID, err
}

type publicTargetRef struct {
	CampaignID string `json:"campaign_id"`
	Ordinal    int    `json:"ordinal"`
	Name       string `json:"name"`
}

type publicRouteView struct {
	Intent       string            `json:"intent"`
	Source       string            `json:"source"`
	AllowCreate  bool              `json:"allow_create"`
	Targets      []publicTargetRef `json:"targets"`
	Overflow     []publicTargetRef `json:"overflow,omitempty"`
	References   []publicTargetRef `json:"references,omitempty"`
	Candidates   []publicTargetRef `json:"candidates,omitempty"`
	Question     string            `json:"question,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	RejectReason string            `json:"reject_reason,omitempty"`
}

type publicRefView struct {
	CampaignID string `json:"campaign_id"`
	VersionID  int64  `json:"version_id"`
	Version    int    `json:"version"`
	Ordinal    int    `json:"ordinal"`
	Action     string `json:"action"`
}

type publicTurnResult struct {
	TurnID           string             `json:"turn_id"`
	Status           string             `json:"status"`
	Outcome          string             `json:"outcome,omitempty"`
	ErrorCode        string             `json:"error_code,omitempty"`
	AssistantMessage *turns.MessageView `json:"assistant_message,omitempty"`
	CampaignRefs     []publicRefView    `json:"campaign_refs"`
	Route            *publicRouteView   `json:"route,omitempty"`
}

func (s *Server) publicRoute(ctx context.Context, route *turns.RouteView) (*publicRouteView, error) {
	if route == nil {
		return nil, nil
	}
	refs := func(in []turns.TargetRef) ([]publicTargetRef, error) {
		out := make([]publicTargetRef, 0, len(in))
		for _, ref := range in {
			id, err := s.publicCampaignID(ctx, ref.CampaignID)
			if err != nil {
				return nil, err
			}
			out = append(out, publicTargetRef{CampaignID: id, Ordinal: ref.Ordinal, Name: ref.Name})
		}
		return out, nil
	}
	out := &publicRouteView{Intent: route.Intent, Source: route.Source, AllowCreate: route.AllowCreate,
		Question: route.Question, Reason: route.Reason, RejectReason: route.RejectReason}
	var err error
	if out.Targets, err = refs(route.Targets); err != nil {
		return nil, err
	}
	if out.Overflow, err = refs(route.Overflow); err != nil {
		return nil, err
	}
	if out.References, err = refs(route.References); err != nil {
		return nil, err
	}
	if out.Candidates, err = refs(route.Candidates); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) publicTurn(ctx context.Context, res turns.TurnResult) (publicTurnResult, error) {
	out := publicTurnResult{TurnID: res.TurnID, Status: res.Status, Outcome: res.Outcome,
		ErrorCode: res.ErrorCode, AssistantMessage: res.AssistantMessage,
		CampaignRefs: make([]publicRefView, 0, len(res.CampaignRefs))}
	for _, ref := range res.CampaignRefs {
		id, err := s.publicCampaignID(ctx, ref.CampaignID)
		if err != nil {
			return publicTurnResult{}, err
		}
		out.CampaignRefs = append(out.CampaignRefs, publicRefView{CampaignID: id,
			VersionID: ref.VersionID, Version: ref.Version, Ordinal: ref.Ordinal, Action: ref.Action})
	}
	var err error
	out.Route, err = s.publicRoute(ctx, res.Route)
	return out, err
}

func (s *Server) publicEvent(ctx context.Context, ev agentloop.Event) (agentloop.Event, error) {
	switch ev.Type {
	case turns.EventRoute:
		v, ok := ev.Data.(turns.RouteView)
		if !ok {
			return ev, nil
		}
		public, err := s.publicRoute(ctx, &v)
		ev.Data = public
		return ev, err
	case agentloop.EventDraftSaved:
		data, ok := ev.Data.(map[string]any)
		if !ok {
			return ev, nil
		}
		id, ok := data["campaign_id"].(int64)
		if !ok {
			return ev, nil
		}
		public, err := s.publicCampaignID(ctx, id)
		if err != nil {
			return agentloop.Event{}, err
		}
		copy := make(map[string]any, len(data))
		for key, value := range data {
			copy[key] = value
		}
		copy["campaign_id"] = public
		ev.Data = copy
		return ev, nil
	case agentloop.EventTargetAmbiguous:
		data, ok := ev.Data.(map[string]any)
		if !ok {
			return ev, nil
		}
		ids, ok := data["candidates"].([]int64)
		if !ok {
			return ev, nil
		}
		public := make([]string, 0, len(ids))
		for _, id := range ids {
			v, err := s.publicCampaignID(ctx, id)
			if err != nil {
				return agentloop.Event{}, err
			}
			public = append(public, v)
		}
		copy := make(map[string]any, len(data))
		for key, value := range data {
			copy[key] = value
		}
		copy["candidates"] = public
		ev.Data = copy
	}
	return ev, nil
}
