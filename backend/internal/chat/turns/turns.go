// Package turns runs chat turns: it records the request, builds the context
// from the database, runs the agent loop and persists the outcome.
package turns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/clock"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/metrics"
	"smart-campaign-composer/backend/internal/segment"
)

// Turn-level events. EventRoute announces the verified routing decision
// (intent and targets) before anything else streams. EventError and
// EventDone are final: Execute emits them last, after every live event, and
// EventDone only once the result is stored.
const (
	EventRoute = "route"
	EventError = "error"
	EventDone  = "done"
)

// Errors of Begin and Result.
var (
	ErrTurnNotFound        = errors.New("turn not found")
	ErrChatNotFound        = errors.New("chat not found")
	ErrIdempotencyMismatch = errors.New("client_turn_id was already used with a different request")
	ErrTurnInProgress      = errors.New("another turn is running in this chat")
)

// ReapStale ends running turns whose heartbeat is older than staleAfter
// (their process died). They become failed/interrupted, or partial when a
// draft had already been saved, and the chat is unlocked.
func (r *Runner) ReapStale(ctx context.Context, staleAfter time.Duration) (int, error) {
	tx, err := r.d.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	rows, err := tx.Query(ctx, `
		UPDATE turns t SET status = 'failed', error_code = 'interrupted', finished_at = now(),
		       outcome = CASE WHEN EXISTS (SELECT 1 FROM turn_campaign_refs r WHERE r.turn_id = t.id)
		                      THEN 'partial' ELSE 'failed' END
		WHERE t.status = 'running' AND t.heartbeat_at < now() - make_interval(secs => $1)
		RETURNING t.id::text, t.chat_id::text, t.outcome`, staleAfter.Seconds())
	if err != nil {
		return 0, err
	}
	type reaped struct{ turnID, chatID, outcome string }
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (reaped, error) {
		var x reaped
		err := row.Scan(&x.turnID, &x.chatID, &x.outcome)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	for _, x := range list {
		text := "Yanıt üretilemedi: " + reason("interrupted", r.voice()) + "."
		if x.outcome == "partial" {
			var notes []string
			refRows, err := tx.Query(ctx, `
				SELECT c.ordinal, v.version FROM turn_campaign_refs r
				JOIN campaigns c ON c.id = r.campaign_id JOIN campaign_versions v ON v.id = r.version_id
				WHERE r.turn_id = $1 ORDER BY v.id`, x.turnID)
			if err != nil {
				return 0, err
			}
			notes, err = pgx.CollectRows(refRows, func(row pgx.CollectableRow) (string, error) {
				var ordinal, version int
				err := row.Scan(&ordinal, &version)
				return fmt.Sprintf("Kampanya %d v%d kaydedildi", ordinal, version), err
			})
			if err != nil {
				return 0, err
			}
			text = strings.Join(notes, "; ") + ". Yanıt tamamlanamadı: " + reason("interrupted", r.voice()) + "."
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO messages (chat_id, turn_id, role, text, outcome) VALUES ($1, $2, 'assistant', $3, $4)
			ON CONFLICT (turn_id, role) DO NOTHING`, x.chatID, x.turnID, text, x.outcome); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(list), nil
}

// Deps of the runner.
type Deps struct {
	Pool       *pgxpool.Pool
	LLM        llm.Client
	Router     llm.Client // nil = deterministic fallback routing
	Campaigns  *campaign.Store
	Catalog    segment.Catalog
	Clock      clock.Clock
	WrapDrafts func(agentloop.DraftSaver) agentloop.DraftSaver // test hook
	Metrics    *metrics.Metrics                                // may be nil
}

// Runner runs turns.
type Runner struct{ d Deps }

// NewRunner returns a runner.
func NewRunner(d Deps) *Runner { return &Runner{d: d} }

// BeginRequest starts a turn.
type BeginRequest struct {
	ChatID          string
	ClientTurnID    string
	Text            string
	FocusCampaignID *int64
	// Lang is the interface language of the request ("tr", "en"); Go's own
	// notes in the transcript are written in it. Empty means Turkish.
	Lang string
}

// Begun reports the turn a request maps to.
type Begun struct {
	TurnID string
	New    bool
	Status string
}

// MessageView is the stored assistant message.
type MessageView struct {
	Text        string `json:"text"`
	Outcome     string `json:"outcome"`
	LLMProvider string `json:"llm_provider"`
	LLMModel    string `json:"llm_model"`
}

// RefView is a campaign version produced by the turn.
type RefView struct {
	CampaignID int64  `json:"campaign_id"`
	VersionID  int64  `json:"version_id"`
	Version    int    `json:"version"`
	Ordinal    int    `json:"ordinal"`
	Action     string `json:"action"`
}

// TurnResult is the stored outcome of a turn.
type TurnResult struct {
	TurnID           string       `json:"turn_id"`
	Status           string       `json:"status"`
	Outcome          string       `json:"outcome,omitempty"`
	ErrorCode        string       `json:"error_code,omitempty"`
	AssistantMessage *MessageView `json:"assistant_message,omitempty"`
	CampaignRefs     []RefView    `json:"campaign_refs"`
	Route            *RouteView   `json:"route,omitempty"`
}

// Begin records a turn and its user message. The (chat, client_turn_id)
// pair is the idempotency key: a repeated request returns the existing turn
// (New=false) and never starts a second execution; the same key with a
// different request is ErrIdempotencyMismatch. A chat runs one turn at a
// time (ErrTurnInProgress).
func (r *Runner) Begin(ctx context.Context, req BeginRequest) (Begun, error) {
	tx, err := r.d.Pool.Begin(ctx)
	if err != nil {
		return Begun{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	hash := requestHash(req)
	var turnID string
	err = tx.QueryRow(ctx, `
		INSERT INTO turns (chat_id, client_turn_id, request_hash, status, focus_campaign_id, lang)
		VALUES ($1, $2, $3, 'running', $4, nullif($5, ''))
		ON CONFLICT (chat_id, client_turn_id) DO NOTHING
		RETURNING id::text`,
		req.ChatID, req.ClientTurnID, hash, req.FocusCampaignID, req.Lang).Scan(&turnID)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Same key seen before (possibly by a concurrent request that just
		// committed): report that turn instead of starting another.
		var (
			existing Begun
			oldHash  string
		)
		if err := r.d.Pool.QueryRow(ctx, `
			SELECT id::text, status, request_hash FROM turns WHERE chat_id = $1 AND client_turn_id = $2`,
			req.ChatID, req.ClientTurnID).Scan(&existing.TurnID, &existing.Status, &oldHash); err != nil {
			return Begun{}, err
		}
		if oldHash != hash {
			return Begun{}, ErrIdempotencyMismatch
		}
		return existing, nil
	case errors.As(err, &pgErr) && pgErr.ConstraintName == "ux_turns_one_running_per_chat":
		return Begun{}, ErrTurnInProgress
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return Begun{}, ErrChatNotFound
	case err != nil:
		return Begun{}, fmt.Errorf("insert turn: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO messages (chat_id, turn_id, role, text) VALUES ($1, $2, 'user', $3)`,
		req.ChatID, turnID, req.Text); err != nil {
		return Begun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Begun{}, err
	}
	return Begun{TurnID: turnID, New: true, Status: "running"}, nil
}

// requestHash fingerprints what the client sent under a client_turn_id.
func requestHash(req BeginRequest) string {
	h := sha256.New()
	h.Write([]byte(req.Text))
	h.Write([]byte{0})
	if req.FocusCampaignID != nil {
		h.Write([]byte(strconv.FormatInt(*req.FocusCampaignID, 10)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Result returns the stored outcome of a turn.
func (r *Runner) Result(ctx context.Context, turnID string) (TurnResult, error) {
	var (
		res                            TurnResult
		text, outcome, provider, model *string
		route                          []byte
	)
	err := r.d.Pool.QueryRow(ctx, `
		SELECT t.id::text, t.status, coalesce(t.outcome, ''), coalesce(t.error_code, ''), t.route,
		       a.text, a.outcome, a.llm_provider, a.llm_model
		FROM turns t LEFT JOIN messages a ON a.turn_id = t.id AND a.role = 'assistant'
		WHERE t.id = $1`, turnID).Scan(&res.TurnID, &res.Status, &res.Outcome, &res.ErrorCode, &route,
		&text, &outcome, &provider, &model)
	if errors.Is(err, pgx.ErrNoRows) {
		return TurnResult{}, ErrTurnNotFound
	}
	if err != nil {
		return TurnResult{}, err
	}
	if route != nil {
		res.Route = &RouteView{}
		if err := json.Unmarshal(route, res.Route); err != nil {
			return TurnResult{}, fmt.Errorf("decode route: %w", err)
		}
	}
	if text != nil {
		res.AssistantMessage = &MessageView{Text: *text, Outcome: deref(outcome), LLMProvider: deref(provider), LLMModel: deref(model)}
	}
	rows, err := r.d.Pool.Query(ctx, `
		SELECT r.campaign_id, r.version_id, v.version, c.ordinal, r.action
		FROM turn_campaign_refs r
		JOIN campaign_versions v ON v.id = r.version_id
		JOIN campaigns c ON c.id = r.campaign_id
		WHERE r.turn_id = $1 ORDER BY v.id`, turnID)
	if err != nil {
		return TurnResult{}, err
	}
	res.CampaignRefs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RefView, error) {
		var v RefView
		err := row.Scan(&v.CampaignID, &v.VersionID, &v.Version, &v.Ordinal, &v.Action)
		return v, err
	})
	if err != nil {
		return TurnResult{}, err
	}
	if res.CampaignRefs == nil {
		res.CampaignRefs = []RefView{}
	}
	return res, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
