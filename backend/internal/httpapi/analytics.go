package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"smart-campaign-composer/backend/internal/analytics"
	"smart-campaign-composer/backend/internal/tracking"
)

const (
	defaultTimezone = "Europe/Istanbul"
	chatTitleRunes  = 60
)

// analyticsOverview answers GET /api/analytics: the overview's numbers for
// one slice (range, channel, campaign).
func (s *Server) analyticsOverview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	publicID := q.Get("campaign_id")
	q.Del("campaign_id")
	f, err := analytics.ParseFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if publicID != "" {
		if !uuidRe.MatchString(publicID) {
			writeError(w, http.StatusBadRequest, "invalid_filter", "campaign_id bir UUID olmalı")
			return
		}
		f.CampaignID, err = s.lookupCampaignID(r.Context(), publicID)
		if errors.Is(err, pgx.ErrNoRows) {
			f.CampaignID = -1 // An unknown UUID selects no campaigns.
		} else if err != nil {
			internalError(w, err)
			return
		}
	}
	o, err := analytics.Read(r.Context(), s.d.Pool, s.d.Catalog.Geo, s.d.Location, s.d.Clock.Now(), f)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// listCampaigns answers GET /api/campaigns?id&status&q&limit: campaigns of
// every chat with their breakdown.
func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := tracking.ListFilter{Status: q.Get("status"), Query: q.Get("q")}
	if v := q.Get("id"); v != "" {
		if !uuidRe.MatchString(v) {
			writeError(w, http.StatusBadRequest, "invalid_filter", "id bir UUID olmalı")
			return
		}
		var err error
		f.ID, err = s.lookupCampaignID(r.Context(), v)
		if errors.Is(err, pgx.ErrNoRows) {
			f.ID = -1 // An unknown UUID selects no campaigns.
		} else if err != nil {
			internalError(w, err)
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "limit 1-500 olmalı")
			return
		}
		f.Limit = n
	}
	list, err := tracking.ListCampaigns(r.Context(), s.d.Pool, f)
	if errors.Is(err, tracking.ErrBadFilter) {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": list})
}

type chatSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
}

// listChats answers GET /api/chats: the chats written in, most recent
// first, named after their first campaign or else their first message.
func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "limit 1-100 olmalı")
			return
		}
		limit = n
	}
	rows, err := s.d.Pool.Query(r.Context(), `
		SELECT c.id::text,
		       coalesce((SELECT k.name FROM campaigns k WHERE k.chat_id = c.id ORDER BY k.ordinal LIMIT 1),
		                (SELECT m.text FROM messages m WHERE m.chat_id = c.id AND m.role = 'user' ORDER BY m.id LIMIT 1), ''),
		       c.updated_at
		FROM chats c WHERE EXISTS (SELECT 1 FROM messages m WHERE m.chat_id = c.id)
		   OR EXISTS (SELECT 1 FROM campaigns k WHERE k.chat_id = c.id)
		ORDER BY c.updated_at DESC, c.id LIMIT $1`, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	chats, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (chatSummary, error) {
		var c chatSummary
		err := row.Scan(&c.ID, &c.Title, &c.UpdatedAt)
		if utf8.RuneCountInString(c.Title) > chatTitleRunes {
			c.Title = string([]rune(c.Title)[:chatTitleRunes-1]) + "…"
		}
		return c, err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	if chats == nil {
		chats = []chatSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

const requestRunes = 200

type requestView struct {
	TurnID    string       `json:"turn_id"`
	ChatID    string       `json:"chat_id"`
	Text      string       `json:"text"`
	Status    string       `json:"status"`  // running | completed | failed
	Outcome   string       `json:"outcome"` // ok | partial | failed; empty while running
	Intent    string       `json:"intent"`  // create | edit | clarify | chat; empty if not routed
	Campaigns []requestRef `json:"campaigns"`
	// LLMProvider is who answered (anthropic | mock), from the assistant's
	// message; empty while the turn runs. A mock plays canned drafts, so its
	// result is not a reading of the request.
	LLMProvider string    `json:"llm_provider"`
	CreatedAt   time.Time `json:"created_at"`
}

type requestRef struct {
	CampaignID string `json:"campaign_id"`
	Name       string `json:"name"`
	Ordinal    int    `json:"ordinal"`
	Version    int    `json:"version"`
	Action     string `json:"action"` // created | revised
}

// listRequests answers GET /api/requests?limit: the latest messages
// marketers wrote, newest first, each with what came of it: the campaigns
// its turn created or changed, or the question it asked back.
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	limit := 6
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "limit 1-50 olmalı")
			return
		}
		limit = n
	}
	ctx := r.Context()
	rows, err := s.d.Pool.Query(ctx, `
		SELECT t.id::text, t.chat_id::text, m.text, t.status, coalesce(t.outcome, ''), coalesce(t.route->>'intent', ''),
		       coalesce(a.llm_provider, ''), t.started_at
		FROM turns t JOIN messages m ON m.turn_id = t.id AND m.role = 'user'
		LEFT JOIN messages a ON a.turn_id = t.id AND a.role = 'assistant'
		ORDER BY t.started_at DESC, t.id DESC LIMIT $1`, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (requestView, error) {
		v := requestView{Campaigns: []requestRef{}}
		err := row.Scan(&v.TurnID, &v.ChatID, &v.Text, &v.Status, &v.Outcome, &v.Intent, &v.LLMProvider, &v.CreatedAt)
		if utf8.RuneCountInString(v.Text) > requestRunes {
			v.Text = string([]rune(v.Text)[:requestRunes-1]) + "…"
		}
		return v, err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	if list == nil {
		list = []requestView{}
	}
	at := make(map[string]int, len(list))
	ids := make([]string, len(list))
	for i, v := range list {
		at[v.TurnID], ids[i] = i, v.TurnID
	}
	refs, err := s.d.Pool.Query(ctx, `
		SELECT r.turn_id::text, c.public_id::text, c.name, c.ordinal, v.version, r.action
		FROM turn_campaign_refs r
		JOIN campaigns c ON c.id = r.campaign_id
		JOIN campaign_versions v ON v.id = r.version_id
		WHERE r.turn_id = ANY($1::uuid[]) ORDER BY v.id`, ids)
	if err != nil {
		internalError(w, err)
		return
	}
	_, err = pgx.CollectRows(refs, func(row pgx.CollectableRow) (struct{}, error) {
		var (
			turnID string
			ref    requestRef
		)
		if err := row.Scan(&turnID, &ref.CampaignID, &ref.Name, &ref.Ordinal, &ref.Version, &ref.Action); err != nil {
			return struct{}{}, err
		}
		v := &list[at[turnID]]
		v.Campaigns = append(v.Campaigns, ref)
		return struct{}{}, nil
	})
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": list})
}
