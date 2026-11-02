package turns

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"smart-campaign-composer/backend/internal/campaign"
	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/router"
	"smart-campaign-composer/backend/internal/i18n"
	"smart-campaign-composer/backend/internal/llm"
)

const (
	heartbeatEvery        = 5 * time.Second
	heartbeatQueryTimeout = 3 * time.Second
	finalizeTimeout       = 10 * time.Second
)

// heartbeatContext bounds one heartbeat write: detached from the request
// (the turn outlives a disconnect) but never able to hang on a stuck database.
func heartbeatContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), heartbeatQueryTimeout)
}

// Execute runs a begun turn to completion and persists its outcome. It must
// run detached from the client connection: a disconnect does not cancel the
// turn, and the stored result is what a reconnecting client reads.
//
// Steps: load state → route (router model, checked in Go) → announce the
// route → either answer a clarification directly, or run the main loop
// within the route's write boundary → persist → done. The turn beats from
// its first step to its last write, so however long a step takes (a slow
// router call included) the reaper never takes a live turn for an orphan.
func (r *Runner) Execute(ctx context.Context, turnID string, sink agentloop.Sink) {
	stop := r.heartbeat(ctx, turnID)
	defer stop()
	main := r.d.LLM.Info()
	var (
		res       agentloop.Result
		view      *RouteView
		routerRec *llm.CallRecord
	)
	st, err := r.load(ctx, turnID)
	lang := st.Lang // Turkish when the turn could not be read
	if err != nil {
		res = agentloop.Result{Outcome: agentloop.OutcomeFailed, ErrorCode: "internal_error", Err: err}
	} else {
		var route router.Route
		route, routerRec, err = router.Run(ctx, r.d.Router, routerInput(st))
		switch {
		case err != nil:
			// Routing failed: nothing is guessed and nothing is written.
			res = agentloop.Result{Outcome: agentloop.OutcomeFailed, ErrorCode: llm.ErrorCode(err), Err: err}
		case route.Intent == router.IntentClarify:
			v := routeView(st, route)
			view = &v
			sink.Emit(agentloop.Event{Type: EventRoute, Data: v})
			// The question is the answer; the main model is not called.
			res = agentloop.Result{Text: route.Question, Outcome: agentloop.OutcomeOK}
			sink.Emit(agentloop.Event{Type: agentloop.EventAssistantDelta, Data: map[string]any{"text": route.Question}})
			sink.Emit(agentloop.Event{Type: agentloop.EventTargetAmbiguous, Data: map[string]any{"candidates": route.Candidates}})
		default:
			v := routeView(st, route)
			view = &v
			sink.Emit(agentloop.Event{Type: EventRoute, Data: v})
			p := r.build(st, route)
			var drafts agentloop.DraftSaver = r.d.Campaigns.Drafts(campaign.Author{
				ChatID: st.ChatID, TurnID: turnID, Source: "llm", Provider: main.Provider, Model: main.Model,
			})
			if r.d.WrapDrafts != nil {
				drafts = r.d.WrapDrafts(drafts)
			}
			res = agentloop.Run(ctx, agentloop.Deps{
				LLM: r.d.LLM, Store: drafts, Audience: audienceCounter{r}, Catalog: r.d.Catalog,
			}, p.Input, sink)
		}
	}
	if routerRec != nil {
		res.Calls = append([]llm.CallRecord{*routerRec}, res.Calls...)
	}

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()
	if err := r.finalize(fctx, turnID, res, view, lang); err != nil {
		// Nothing was stored, so "done" would announce a result that GET
		// cannot return. The turn stays running until the reaper ends it;
		// the client falls back to polling.
		slog.Error("finalize turn", "turn_id", turnID, "err", err)
		r.d.Metrics.Turn("persist_failed")
		sink.Emit(agentloop.Event{Type: EventError, Data: map[string]any{
			"code": "persist_failed", "message": "Tur sonucu kaydedilemedi; durum kısa süre içinde güncellenecek."}})
		return
	}
	r.record(res, view)
	if res.ErrorCode != "" {
		sink.Emit(agentloop.Event{Type: EventError, Data: map[string]any{"code": res.ErrorCode, "message": reason(res.ErrorCode, r.voice())}})
	}
	sink.Emit(agentloop.Event{Type: EventDone, Data: map[string]any{"turn_id": turnID, "outcome": res.Outcome}})
}

// record counts a finished, stored turn in the metrics: its outcome, its
// model calls, its rejected drafts and repairs, and whether it asked which
// campaign was meant.
func (r *Runner) record(res agentloop.Result, view *RouteView) {
	m := r.d.Metrics
	m.Turn(res.Outcome)
	for _, c := range res.Calls {
		outcome := "ok"
		if c.ErrorCode != "" {
			outcome = c.ErrorCode
		}
		m.LLMRequest(c.Kind, r.infoFor(c.Kind).Provider, outcome)
	}
	for _, reason := range res.Rejections {
		m.DraftRejected(reason)
	}
	m.LLMRepairs(res.Repairs)
	if view != nil && view.Intent == router.IntentClarify {
		m.TargetAmbiguous()
	}
}

// infoFor returns the provider and model that produced a call or message.
func (r *Runner) infoFor(kind string) llm.Info {
	if kind == "router" && r.d.Router != nil {
		return r.d.Router.Info()
	}
	return r.d.LLM.Info()
}

// finalize writes the assistant message, the turn outcome, the route and the
// call audit in one transaction.
func (r *Runner) finalize(ctx context.Context, turnID string, res agentloop.Result, view *RouteView, lang i18n.Lang) error {
	tx, err := r.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	status := "completed"
	if res.Outcome != agentloop.OutcomeOK {
		status = "failed"
	}
	var routeJSON []byte
	if view != nil {
		if routeJSON, err = json.Marshal(view); err != nil {
			return err
		}
	}
	var chatID string
	err = tx.QueryRow(ctx, `
		UPDATE turns SET status = $2, outcome = $3, error_code = nullif($4, ''), route = $5,
		                 finished_at = now(), heartbeat_at = now()
		WHERE id = $1 AND status = 'running' RETURNING chat_id::text`,
		turnID, status, res.Outcome, res.ErrorCode, routeJSON).Scan(&chatID)
	if err != nil {
		return fmt.Errorf("update turn: %w", err)
	}
	author := r.infoFor("main")
	if view != nil && view.Intent == router.IntentClarify && view.Source == router.SourceRouter {
		author = r.infoFor("router")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO messages (chat_id, turn_id, role, text, outcome, llm_provider, llm_model)
		VALUES ($1, $2, 'assistant', $3, $4, $5, $6)`,
		chatID, turnID, assistantText(res, r.voice(), lang), res.Outcome, author.Provider, author.Model); err != nil {
		return fmt.Errorf("insert assistant message: %w", err)
	}
	for _, c := range res.Calls {
		info := r.infoFor(c.Kind)
		if _, err := tx.Exec(ctx, `
			INSERT INTO llm_calls (turn_id, kind, iteration, provider, model, stop_reason, input_tokens, output_tokens, latency_ms, error_code)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, nullif($10, ''))`,
			turnID, c.Kind, c.Iteration, info.Provider, info.Model, string(c.StopReason), c.Usage.InputTokens,
			c.Usage.OutputTokens, c.Latency.Milliseconds(), c.ErrorCode); err != nil {
			return fmt.Errorf("insert llm call: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE chats SET updated_at = now() WHERE id = $1`, chatID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// assistantText is what the transcript keeps for the turn. A partial turn
// keeps the completed iterations' text plus a Go-written note of what was
// saved; text from the failed iteration is never stored. Go's note is in the
// interface language the turn was started from; the model's text is in the
// language of the message.
func assistantText(res agentloop.Result, v voice, lang i18n.Lang) string {
	why := reason(res.ErrorCode, v)
	if lang == i18n.EN {
		why = i18n.Translate(i18n.EN, why)
	}
	switch res.Outcome {
	case agentloop.OutcomeOK:
		return res.Text
	case agentloop.OutcomePartial:
		notes := make([]string, 0, len(res.Saved))
		for _, s := range res.Saved {
			n := fmt.Sprintf("Kampanya %d v%d kaydedildi", s.Ordinal, s.Version)
			if lang == i18n.EN {
				n = fmt.Sprintf("Campaign %d v%d saved", s.Ordinal, s.Version)
			}
			if s.ChangeSummary != "" {
				n += " (" + s.ChangeSummary + ")"
			}
			notes = append(notes, n)
		}
		tail := v.genitive + " yanıtı tamamlanamadı: " + why + "."
		if lang == i18n.EN {
			tail = v.genitiveEN + " answer could not be completed: " + why + "."
		}
		note := strings.Join(notes, "; ") + ". " + tail
		if res.Text == "" {
			return note
		}
		return res.Text + "\n" + note
	}
	if lang == i18n.EN {
		return "No answer could be produced: " + why + "."
	}
	return "Yanıt üretilemedi: " + why + "."
}

// voice names the model in Go's notes; the demo model is never called
// Claude.
type voice struct{ name, genitive, genitiveEN string }

func (r *Runner) voice() voice {
	switch r.d.LLM.Info().Provider {
	case "anthropic":
		return voice{"Claude", "Claude'un", "Claude's"}
	case "mock":
		return voice{"Mock model", "Mock modelin", "The mock model's"}
	}
	return voice{"Model", "Modelin", "The model's"}
}

// reason is the user-facing explanation of an error code.
func reason(code string, v voice) string {
	switch code {
	case "llm_unavailable":
		return v.name + " geçici olarak yanıt vermiyor"
	case "llm_rate_limited":
		return v.name + " istek sınırına takıldı"
	case "llm_auth":
		return v.name + " anahtarı geçersiz"
	case "llm_bad_request":
		return "uygulamanın model API isteği geçersiz"
	case "refusal":
		return v.name + " bu isteği yanıtlamadı"
	case "max_tokens":
		return "yanıt uzunluk sınırında kesildi"
	case "repair_exhausted":
		return "taslak üç denemede doğrulanamadı"
	case "iteration_limit":
		return "tur adım sınırına ulaştı"
	case "db_error":
		return "kayıt sırasında veritabanı hatası oluştu"
	case "cancelled":
		return "istek iptal edildi"
	case "interrupted":
		return "sunucu tur sırasında yeniden başladı"
	}
	return "beklenmeyen bir hata oluştu"
}

// heartbeat keeps heartbeat_at fresh while the turn runs so the reaper can
// tell a live turn from one orphaned by a crash.
func (r *Runner) heartbeat(ctx context.Context, turnID string) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-r.d.Clock.After(heartbeatEvery):
				hctx, cancel := heartbeatContext(ctx)
				_, err := r.d.Pool.Exec(hctx, `UPDATE turns SET heartbeat_at = now() WHERE id = $1 AND status = 'running'`, turnID)
				cancel()
				if err != nil {
					slog.Warn("turn heartbeat", "turn_id", turnID, "err", err)
				}
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
