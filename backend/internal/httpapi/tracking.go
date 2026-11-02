package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"smart-campaign-composer/backend/internal/i18n"
	"smart-campaign-composer/backend/internal/store"
	"smart-campaign-composer/backend/internal/tracking"
)

// readyTimeout bounds the database check of /readyz.
const readyTimeout = 2 * time.Second

// sseWriter sends server-sent events.
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	seq     int
	lang    i18n.Lang // the messages of error, draft.invalid and resync events
}

// startSSE answers with an event stream; ok is false if w cannot stream.
// Each event write must complete within timeout.
func startSSE(w http.ResponseWriter, timeout time.Duration) (*sseWriter, bool) {
	if _, ok := w.(http.Flusher); !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w), timeout: timeout, lang: langOf(w)}, true
}

// send writes one event; an error means the client is gone. A client that
// has not taken the event within the write timeout (it stopped reading)
// counts as gone too, so it cannot hold the handler and its connection
// forever. Data that cannot be encoded is a bug: it is logged and the event
// skipped.
func (s *sseWriter) send(event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		slog.Error("encode event", "type", event, "err", err)
		return nil
	}
	if event == "error" || event == "draft.invalid" || event == "resync" {
		b = localizeJSON(s.lang, b)
	}
	// The deadline is renewed before every write, so a quiet stream (a model
	// thinking for a while) is fine; only a write that does not get through
	// fails. net/http clears it once the response is finished.
	if err := s.rc.SetWriteDeadline(time.Now().Add(s.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	s.seq++
	if _, err := fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", s.seq, event, b); err != nil {
		return err
	}
	return s.rc.Flush()
}

// wait sleeps until the next stream update; false if the client left.
func (s *Server) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.d.Clock.After(s.d.StreamEvery):
		return true
	}
}

// dashboard returns the dashboard as of now.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := tracking.Read(r.Context(), s.d.Pool, s.d.Clock.Now())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// dashboardStream sends the dashboard every StreamEvery until the client
// leaves.
func (s *Server) dashboardStream(w http.ResponseWriter, r *http.Request) {
	out, ok := startSSE(w, s.d.WriteTimeout)
	if !ok {
		internalError(w, errors.New("streaming unsupported"))
		return
	}
	for {
		d, err := tracking.Read(r.Context(), s.d.Pool, s.d.Clock.Now())
		if err != nil {
			s.streamFailed(r, out, err)
			return
		}
		if out.send("dashboard", d) != nil || !s.wait(r.Context()) {
			return
		}
	}
}

// campaignStream sends a campaign's progress every StreamEvery and ends
// once the campaign is over (completed or cancelled).
func (s *Server) campaignStream(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathCampaignID(w, r)
	if !ok {
		return
	}
	c, err := tracking.ReadCampaign(r.Context(), s.d.Pool, id)
	if errors.Is(err, tracking.ErrNotFound) {
		writeError(w, http.StatusNotFound, "campaign_not_found", "kampanya bulunamadı")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	out, ok := startSSE(w, s.d.WriteTimeout)
	if !ok {
		internalError(w, errors.New("streaming unsupported"))
		return
	}
	for {
		if out.send("campaign", c) != nil || c.Status == "completed" || c.Status == "cancelled" || !s.wait(r.Context()) {
			return
		}
		if c, err = tracking.ReadCampaign(r.Context(), s.d.Pool, id); err != nil {
			s.streamFailed(r, out, err)
			return
		}
	}
}

// streamFailed ends a stream whose read failed, telling the client unless
// it is already gone.
func (s *Server) streamFailed(r *http.Request, out *sseWriter, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Error("tracking stream", "path", r.URL.Path, "err", err)
	_ = out.send("error", map[string]string{"code": "unavailable", "message": "durum okunamadı; sayfayı yenileyin"})
}

// readyz reports whether the API can serve: the database answers and its
// migration record lists every migration of this build (schema: the
// recorded version, the build's latest, and what is pending). It also names
// the LLM serving the chat, with the reason when it is the mock, so a demo
// never passes the mock off as Claude.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	out := map[string]any{"llm": s.d.LLM}
	if s.d.Router.Provider != "" {
		out["router"] = s.d.Router
	}
	schema, err := store.ReadSchema(ctx, s.d.Pool)
	if err == nil {
		out["schema"] = schema
	}
	switch {
	case err != nil:
		slog.Warn("readyz: database", "err", err)
		out["status"], out["db"] = "unavailable", "unreachable"
	case !schema.Current():
		out["status"], out["db"] = "unavailable", "not_migrated"
	default:
		out["status"], out["db"] = "ready", "ok"
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, out)
}
