package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"smart-campaign-composer/backend/internal/chat/agentloop"
	"smart-campaign-composer/backend/internal/chat/turns"
)

// The stream of a new turn (POST /api/chats/{id}/turns):
//
//   - turn.started {turn_id} comes first.
//   - Live events (route, assistant.delta, draft.*, target.ambiguous, …)
//     follow in order. They are only a preview. If the client reads so
//     slowly that Deps.StreamBuffer live events pile up, live events stop
//     rather than leave a silent gap: right after the last live event the
//     client gets, the stream sends resync {reason: "lagged", result_url}
//     and the client drops its preview.
//   - The final events, error (if any) and done {turn_id, outcome}, are
//     never dropped. done means the result is stored; the client renders
//     GET result_url, not its preview.
//   - A turn whose result could not be stored has no done: the stream ends
//     with resync {reason: "result_pending"} and the client polls
//     GET result_url until the turn is no longer running.
//   - A stream that breaks off without done or resync is a lost connection;
//     the client polls the same way.
const (
	eventTurnStarted = "turn.started"
	eventResync      = "resync"

	resyncLagged        = "lagged"
	resyncResultPending = "result_pending"
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request, turnID string, sink *streamSink) {
	out, ok := startSSE(w, s.d.WriteTimeout)
	if !ok {
		sink.detach()
		internalError(w, errors.New("streaming unsupported"))
		return
	}
	rawDrafts := make(map[string]string)
	write := func(ev agentloop.Event) bool {
		if ev.Type == agentloop.EventDraftDelta {
			if data, ok := ev.Data.(map[string]any); ok {
				toolID, idOK := data["tool_use_id"].(string)
				chunk, chunkOK := data["partial_json"].(string)
				if idOK && chunkOK {
					rawDrafts[toolID] += chunk
					ev.Data = map[string]any{"tool_use_id": toolID,
						"partial_json": draftPreviewSnapshot(rawDrafts[toolID]), "replace": true}
				}
			}
		}
		public, err := s.publicEvent(r.Context(), ev)
		if err != nil {
			slog.Error("stream campaign ID", "turn_id", turnID, "err", err)
			return false
		}
		return out.send(public.Type, public.Data) == nil
	}
	if !write(agentloop.Event{Type: eventTurnStarted, Data: map[string]string{"turn_id": turnID}}) {
		sink.detach()
		return
	}
	lagNoted := false
	for {
		p := sink.take()
		for _, ev := range p.live {
			if !write(ev) {
				sink.detach()
				return
			}
		}
		if p.lagged && !lagNoted {
			lagNoted = true
			if !write(resync(turnID, resyncLagged)) {
				sink.detach()
				return
			}
		}
		if p.ended {
			done := false
			for _, ev := range p.final {
				if !write(ev) {
					return
				}
				done = done || ev.Type == turns.EventDone
			}
			if !done {
				write(resync(turnID, resyncResultPending))
			}
			return
		}
		select {
		case <-sink.wake:
		case <-r.Context().Done():
			// The client is gone; the turn keeps running in the background.
			sink.detach()
			return
		}
	}
}

// resync tells the client that its preview cannot be trusted and where the
// stored result is.
func resync(turnID, reason string) agentloop.Event {
	msg := "Bağlantı yavaş olduğu için canlı önizleme durduruldu; tur bitince kayıtlı sonuç gösterilecek."
	if reason == resyncResultPending {
		msg = "Tur sonucu henüz kaydedilmedi; kayıtlı sonuç hazır olunca gösterilecek."
	}
	return agentloop.Event{Type: eventResync, Data: map[string]string{
		"turn_id": turnID, "reason": reason, "result_url": "/api/turns/" + turnID, "message": msg,
	}}
}

// streamSink hands a turn's events from the runner to its stream without
// ever blocking the runner. Live events wait in a bounded queue; once a slow
// client lets it fill up, the sink queues no more live events (lagged)
// instead of dropping some and passing later ones. Final events are kept
// apart and never dropped. After detach (the client is gone) nothing is kept.
type streamSink struct {
	mu       sync.Mutex
	wake     chan struct{} // capacity 1: there is something new to take
	limit    int
	live     []agentloop.Event
	lagged   bool
	final    []agentloop.Event
	closed   bool
	detached bool
}

func newStreamSink(limit int) *streamSink {
	return &streamSink{wake: make(chan struct{}, 1), limit: limit}
}

// Emit implements agentloop.Sink.
func (s *streamSink) Emit(ev agentloop.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.detached || s.closed:
		return
	case ev.Type == turns.EventError || ev.Type == turns.EventDone:
		s.final = append(s.final, ev)
	case s.lagged:
		return
	case len(s.live) >= s.limit:
		s.lagged = true
	default:
		s.live = append(s.live, ev)
	}
	s.signal()
}

// close marks the end of the runner's events.
func (s *streamSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.signal()
}

func (s *streamSink) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detached, s.live, s.final = true, nil, nil
}

func (s *streamSink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pending is what the stream writes next, in this order.
type pending struct {
	live   []agentloop.Event
	lagged bool
	final  []agentloop.Event
	ended  bool // the runner is done; final is complete
}

func (s *streamSink) take() pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := pending{live: s.live, lagged: s.lagged, ended: s.closed}
	s.live = nil
	if s.closed {
		p.final, s.final = s.final, nil
	}
	return p
}
