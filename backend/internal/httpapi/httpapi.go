// Package httpapi serves the REST and SSE API.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/turns"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/i18n"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/segment"
)

// Limits.
const (
	maxTextRunes        = 4000
	defaultStreamBuffer = 1024
	defaultWriteTimeout = 10 * time.Second
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Deps of the API.
type Deps struct {
	Pool        *pgxpool.Pool
	Runner      *turns.Runner
	Campaigns   *campaign.Store
	Catalog     segment.Catalog
	Clock       clock.Clock
	LLM         llm.Info
	Router      llm.Info
	TurnTimeout time.Duration
	// StreamBuffer is how many live events a turn stream holds for a client
	// that reads slowly; 0 means defaultStreamBuffer.
	StreamBuffer int
	// StreamEvery is how often the dashboard and campaign streams send; 0
	// means a second.
	StreamEvery time.Duration
	// Location sets the local days of the analytics; nil means Europe/Istanbul.
	Location *time.Location
	// WriteTimeout bounds one event write to a streaming client, so that a
	// client that stops reading cannot hold a handler forever; 0 means
	// defaultWriteTimeout.
	WriteTimeout time.Duration
	Metrics      *metrics.Metrics    // may be nil
	Gatherer     prometheus.Gatherer // what /metrics serves; nil: no /metrics
}

// Server is the HTTP API.
type Server struct {
	d   Deps
	mux chi.Router
	wg  sync.WaitGroup // background turn executions
}

// New returns the API server.
func New(d Deps) *Server {
	if d.StreamBuffer <= 0 {
		d.StreamBuffer = defaultStreamBuffer
	}
	if d.StreamEvery <= 0 {
		d.StreamEvery = time.Second
	}
	if d.WriteTimeout <= 0 {
		d.WriteTimeout = defaultWriteTimeout
	}
	if d.Location == nil {
		loc, err := time.LoadLocation(defaultTimezone)
		if err != nil {
			loc = time.UTC
		}
		d.Location = loc
	}
	s := &Server{d: d, mux: chi.NewRouter()}
	s.mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Get("/api/meta", s.meta)
	s.mux.Get("/api/chats", s.listChats)
	s.mux.Post("/api/chats", s.createChat)
	s.mux.Get("/api/chats/{chatID}", s.getChat)
	s.mux.Post("/api/chats/{chatID}/turns", s.postTurn)
	s.mux.Get("/api/turns/{turnID}", s.getTurn)
	s.mux.Get("/api/campaigns", s.listCampaigns)
	s.mux.Post("/api/campaigns", s.createCampaign)
	s.mux.Get("/api/campaign-rules/catalog", s.campaignRuleCatalog)
	s.mux.Get("/api/campaigns/{campaignID}", s.getCampaign)
	s.mux.Post("/api/campaigns/{campaignID}/versions", s.editCampaign)
	s.mux.Post("/api/campaigns/{campaignID}/approve", s.approveCampaign)
	s.mux.Post("/api/campaigns/{campaignID}/cancel", s.cancelCampaign)
	s.mux.Get("/api/campaigns/{campaignID}/approval-preview", s.previewApproval)
	s.mux.Get("/api/campaigns/{campaignID}/stream", s.campaignStream)
	s.mux.Get("/api/analytics", s.analyticsOverview)
	s.mux.Get("/api/requests", s.listRequests)
	s.mux.Get("/api/dashboard", s.dashboard)
	s.mux.Get("/api/dashboard/stream", s.dashboardStream)
	s.mux.Get("/readyz", s.readyz)
	if d.Gatherer != nil {
		s.mux.Method(http.MethodGet, "/metrics", promhttp.HandlerFor(d.Gatherer, promhttp.HandlerOpts{}))
	}
	return s
}

// ServeHTTP answers in the language the request asks for (see i18n.go).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lw := &langWriter{ResponseWriter: w, lang: i18n.FromRequest(r)}
	s.mux.ServeHTTP(lw, r)
	lw.finish()
}

// Wait blocks until background turns have finished (graceful shutdown).
func (s *Server) Wait() { s.wg.Wait() }

// meta names the LLM serving the chat (and the router's, if separate).
// While the mock answers it also lists the demo's scenarios, so the UI can
// offer them.
func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"llm": s.d.LLM}
	if s.d.Router.Provider != "" {
		out["router"] = s.d.Router
	}
	if s.d.LLM.Provider == "mock" {
		lang := i18n.FromRequest(r)
		scenarios := make([]mock.Scenario, len(mock.Scenarios))
		for i, sc := range mock.Scenarios {
			sc.Description = i18n.Translate(lang, sc.Description)
			scenarios[i] = sc
		}
		out["mock_scenarios"] = scenarios
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var id string
	if err := s.d.Pool.QueryRow(r.Context(), `INSERT INTO chats DEFAULT VALUES RETURNING id::text`).Scan(&id); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

type messageView struct {
	TurnID      string    `json:"turn_id"`
	Role        string    `json:"role"`
	Text        string    `json:"text"`
	Outcome     *string   `json:"outcome"`
	LLMProvider *string   `json:"llm_provider"`
	LLMModel    *string   `json:"llm_model"`
	CreatedAt   time.Time `json:"created_at"`
}

type campaignView struct {
	ID        string    `json:"id"`
	Ordinal   int       `json:"ordinal"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Version   *int      `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// getChat returns the transcript, the campaigns and the running turn (if
// any), so a reloaded client can render and resume polling.
func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	ctx, chatID := r.Context(), chi.URLParam(r, "chatID")
	if !uuidRe.MatchString(chatID) {
		writeError(w, http.StatusNotFound, "chat_not_found", "sohbet bulunamadı")
		return
	}
	var exists bool
	if err := s.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chats WHERE id = $1)`, chatID).Scan(&exists); err != nil {
		internalError(w, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "chat_not_found", "sohbet bulunamadı")
		return
	}
	rows, err := s.d.Pool.Query(ctx, `
		SELECT turn_id::text, role, text, outcome, llm_provider, llm_model, created_at
		FROM messages WHERE chat_id = $1 ORDER BY id`, chatID)
	if err != nil {
		internalError(w, err)
		return
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (messageView, error) {
		var m messageView
		err := row.Scan(&m.TurnID, &m.Role, &m.Text, &m.Outcome, &m.LLMProvider, &m.LLMModel, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	rows, err = s.d.Pool.Query(ctx, `
		SELECT c.public_id::text, c.ordinal, c.name, c.status, v.version, c.updated_at
		FROM campaigns c LEFT JOIN campaign_versions v ON v.id = c.current_version_id
		WHERE c.chat_id = $1 ORDER BY c.ordinal`, chatID)
	if err != nil {
		internalError(w, err)
		return
	}
	campaigns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (campaignView, error) {
		var c campaignView
		err := row.Scan(&c.ID, &c.Ordinal, &c.Name, &c.Status, &c.Version, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		internalError(w, err)
		return
	}
	var running *string
	err = s.d.Pool.QueryRow(ctx, `SELECT id::text FROM turns WHERE chat_id = $1 AND status = 'running'`, chatID).Scan(&running)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internalError(w, err)
		return
	}
	if messages == nil {
		messages = []messageView{}
	}
	if campaigns == nil {
		campaigns = []campaignView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": chatID, "messages": messages, "campaigns": campaigns, "running_turn_id": running})
}

type turnRequest struct {
	ClientTurnID    string  `json:"client_turn_id"`
	Text            string  `json:"text"`
	FocusCampaignID *string `json:"focus_campaign_id"`
}

// postTurn starts a turn and streams it, or reports an existing one.
//
// A new turn runs in the background, detached from this request: if the
// client goes away the turn still completes and GET /api/turns/{id} returns
// the stored result. Repeating the same client_turn_id never runs the turn
// again: it answers 202 (still running) or 200 with the stored result.
func (s *Server) postTurn(w http.ResponseWriter, r *http.Request) {
	chatID := chi.URLParam(r, "chatID")
	var req turnRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "istek gövdesi okunamadı")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	switch {
	case !uuidRe.MatchString(req.ClientTurnID):
		writeError(w, http.StatusBadRequest, "invalid_client_turn_id", "client_turn_id bir UUID olmalı")
		return
	case req.Text == "":
		writeError(w, http.StatusBadRequest, "empty_text", "mesaj boş olamaz")
		return
	case utf8.RuneCountInString(req.Text) > maxTextRunes:
		writeError(w, http.StatusBadRequest, "text_too_long", fmt.Sprintf("mesaj en fazla %d karakter olabilir", maxTextRunes))
		return
	case !uuidRe.MatchString(chatID):
		writeError(w, http.StatusNotFound, "chat_not_found", "sohbet bulunamadı")
		return
	}
	var focusID *int64
	if req.FocusCampaignID != nil {
		if !uuidRe.MatchString(*req.FocusCampaignID) {
			writeError(w, http.StatusBadRequest, "invalid_focus_campaign_id", "focus_campaign_id bir UUID olmalı")
			return
		}
		var id int64
		err := s.d.Pool.QueryRow(r.Context(),
			`SELECT id FROM campaigns WHERE public_id = $1 AND chat_id = $2`, *req.FocusCampaignID, chatID).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "campaign_not_found", "kampanya bu sohbette bulunamadı")
			return
		case err != nil:
			internalError(w, err)
			return
		}
		focusID = &id
	}

	begun, err := s.d.Runner.Begin(r.Context(), turns.BeginRequest{
		ChatID: chatID, ClientTurnID: strings.ToLower(req.ClientTurnID), Text: req.Text, FocusCampaignID: focusID,
		Lang: string(langOf(w)),
	})
	switch {
	case errors.Is(err, turns.ErrIdempotencyMismatch):
		writeError(w, http.StatusConflict, "idempotency_mismatch", "bu client_turn_id farklı bir istekle kullanılmış")
		return
	case errors.Is(err, turns.ErrTurnInProgress):
		writeError(w, http.StatusConflict, "turn_in_progress", "bu sohbette çalışan bir istek var; bitmesini bekleyin")
		return
	case errors.Is(err, turns.ErrChatNotFound):
		writeError(w, http.StatusNotFound, "chat_not_found", "sohbet bulunamadı")
		return
	case err != nil:
		internalError(w, err)
		return
	}
	if !begun.New {
		s.d.Metrics.TurnDedup()
		if begun.Status == "running" {
			writeJSON(w, http.StatusAccepted, map[string]string{"turn_id": begun.TurnID, "status": "running"})
			return
		}
		res, err := s.d.Runner.Result(r.Context(), begun.TurnID)
		if err != nil {
			internalError(w, err)
			return
		}
		public, err := s.publicTurn(r.Context(), res)
		if err != nil {
			internalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, public)
		return
	}

	sink := newStreamSink(s.d.StreamBuffer)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer sink.close()
		ctx, cancel := context.WithTimeout(context.Background(), s.d.TurnTimeout)
		defer cancel()
		s.d.Runner.Execute(ctx, begun.TurnID, sink)
	}()
	s.stream(w, r, begun.TurnID, sink)
}

// getTurn returns the stored result of a turn.
func (s *Server) getTurn(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "turnID")
	if !uuidRe.MatchString(id) {
		writeError(w, http.StatusNotFound, "turn_not_found", "istek bulunamadı")
		return
	}
	res, err := s.d.Runner.Result(r.Context(), id)
	if errors.Is(err, turns.ErrTurnNotFound) {
		writeError(w, http.StatusNotFound, "turn_not_found", "istek bulunamadı")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	public, err := s.publicTurn(r.Context(), res)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, public)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write json", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "beklenmeyen bir hata oluştu")
}
