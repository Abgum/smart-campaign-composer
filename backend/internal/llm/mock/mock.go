// Package mock provides llm.Client implementations that need no API key:
// Scripted for tests and Demo for running the app without Claude.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/llm"
)

// Call scripts one model call.
type Call struct {
	Text      string
	ToolName  string
	ToolInput string // raw JSON as the model would stream it; may be invalid
	// Parallel are further tool calls in the same message, after ToolName's:
	// Claude may call tools in parallel.
	Parallel []ToolCall
	// Stop defaults to tool_use when a tool is scripted, end_turn otherwise.
	Stop llm.StopReason
	// Err fails the call: immediately, or after ErrAfterEvents events.
	Err            *llm.Error
	ErrAfterEvents int
	// ChunkDelay slows streaming down (demo of disconnects, cancellation tests).
	ChunkDelay time.Duration
}

// ToolCall is one more tool call of a scripted message.
type ToolCall struct{ Name, Input string }

// Scripted plays calls in order and records the requests it received.
type Scripted struct {
	mu       sync.Mutex
	calls    []Call
	n        int
	requests []llm.Request
	info     llm.Info
}

// NewScripted returns a client that answers the n-th call with calls[n].
func NewScripted(calls ...Call) *Scripted {
	return &Scripted{calls: calls, info: llm.Info{Provider: "mock", Model: "mock-scripted"}}
}

// Info implements llm.Client.
func (m *Scripted) Info() llm.Info { return m.info }

// Stream implements llm.Client.
func (m *Scripted) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.n >= len(m.calls) {
		m.n++
		return nil, fmt.Errorf("mock: script exhausted (call %d of %d)", m.n, len(m.calls))
	}
	c := m.calls[m.n]
	m.n++
	return Play(ctx, c, fmt.Sprintf("toolu_mock_%d", m.n)), nil
}

// Calls returns how many calls were made.
func (m *Scripted) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

// Requests returns the requests received so far.
func (m *Scripted) Requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Request(nil), m.requests...)
}

// Play turns a scripted call into a stream. Text and tool input are emitted
// in small rune-aligned chunks, like the real API.
func Play(ctx context.Context, c Call, toolID string) llm.Stream {
	var events []llm.Event
	var content []llm.Block
	if c.Text != "" {
		for _, ch := range chunks(c.Text, 6) {
			events = append(events, llm.TextDelta{Text: ch})
		}
		content = append(content, llm.Block{Type: llm.BlockText, Text: c.Text})
	}
	stop := c.Stop
	var calls []ToolCall
	if c.ToolName != "" {
		calls = append(calls, ToolCall{Name: c.ToolName, Input: c.ToolInput})
	}
	for i, t := range append(calls, c.Parallel...) {
		id := toolID
		if i > 0 {
			id = fmt.Sprintf("%s_%d", toolID, i+1)
		}
		events = append(events, llm.ToolUseStart{ID: id, Name: t.Name})
		for _, ch := range chunks(t.Input, 12) {
			events = append(events, llm.ToolInputDelta{ID: id, PartialJSON: ch})
		}
		raw := []byte(t.Input)
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		events = append(events, llm.ToolUseEnd{ID: id, Name: t.Name, RawInput: raw})
		content = append(content, llm.Block{Type: llm.BlockToolUse, ToolUseID: id, ToolName: t.Name, Input: json.RawMessage(raw)})
		if stop == "" {
			stop = llm.StopToolUse
		}
	}
	if stop == "" {
		stop = llm.StopEndTurn
	}
	s := &stream{ctx: ctx, delay: c.ChunkDelay}
	if c.Err != nil {
		s.events = events[:min(c.ErrAfterEvents, len(events))]
		s.failWith = c.Err
		return s
	}
	s.events = append(events, llm.MessageEnd{
		StopReason: stop,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: int64(utf8.RuneCountInString(c.Text + c.ToolInput))},
		Content:    content,
	})
	return s
}

func chunks(s string, size int) []string {
	var out []string
	for len(s) > 0 {
		i, n := 0, 0
		for i < len(s) && n < size {
			_, w := utf8.DecodeRuneInString(s[i:])
			i += w
			n++
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return out
}

type stream struct {
	ctx      context.Context
	delay    time.Duration
	events   []llm.Event
	cur      llm.Event
	failWith error
	err      error
}

func (s *stream) Next() bool {
	if s.err != nil {
		return false
	}
	if len(s.events) == 0 {
		s.err = s.failWith
		return false
	}
	if s.delay > 0 {
		select {
		case <-s.ctx.Done():
			s.err = s.ctx.Err()
			return false
		case <-time.After(s.delay):
		}
	} else if err := s.ctx.Err(); err != nil {
		s.err = err
		return false
	}
	s.cur, s.events = s.events[0], s.events[1:]
	return true
}

func (s *stream) Event() llm.Event { return s.cur }
func (s *stream) Err() error       { return s.err }
func (s *stream) Close() error     { return nil }
