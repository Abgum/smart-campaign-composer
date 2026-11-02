// Package llm is the provider-neutral contract for one model call. The agent
// loop depends only on this package; the Anthropic SDK lives behind it in
// llm/anthropic and the scripted test double in llm/mock.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Role of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType of a content block.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockOpaque     BlockType = "opaque" // provider-only content, e.g. thinking
)

// Block is one content block. Provider, when set, is the provider-native
// block that produced it; adapters replay it verbatim within a turn so that
// thinking signatures stay valid. It is never persisted.
type Block struct {
	Type      BlockType
	Text      string          // text, or tool_result content
	ToolUseID string          // tool_use id, or the id a tool_result answers
	ToolName  string          // tool_use
	Input     json.RawMessage // tool_use input as received
	IsError   bool            // tool_result
	Provider  any
}

// Message is one conversation message.
type Message struct {
	Role   Role
	Blocks []Block
}

// Tool is a client tool definition. InputSchema is a full JSON Schema object.
type Tool struct {
	Name                string
	Description         string
	InputSchema         map[string]any
	Strict              bool
	EagerInputStreaming bool
}

// Request is one model call.
type Request struct {
	System    string
	Tools     []Tool
	Messages  []Message
	MaxTokens int
}

// StopReason of a finished message.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage of one call.
type Usage struct {
	InputTokens          int64
	OutputTokens         int64
	CacheReadInputTokens int64
}

// Event is emitted by a Stream.
type Event interface{ isEvent() }

// TextDelta is a chunk of assistant text.
type TextDelta struct{ Text string }

// ToolUseStart opens a tool call.
type ToolUseStart struct{ ID, Name string }

// ToolInputDelta is a raw chunk of a tool call's JSON input.
type ToolInputDelta struct{ ID, PartialJSON string }

// ToolUseEnd closes a tool call. RawInput is the concatenated input exactly as
// streamed; it may be invalid or truncated JSON and must be parsed strictly.
type ToolUseEnd struct {
	ID, Name string
	RawInput []byte
}

// MessageEnd closes the message. Content is the full assistant content in
// order, for replay within the same turn.
type MessageEnd struct {
	StopReason StopReason
	Usage      Usage
	Content    []Block
}

func (TextDelta) isEvent()      {}
func (ToolUseStart) isEvent()   {}
func (ToolInputDelta) isEvent() {}
func (ToolUseEnd) isEvent()     {}
func (MessageEnd) isEvent()     {}

// Stream yields the events of one call.
type Stream interface {
	Next() bool
	Event() Event
	Err() error
	Close() error
}

// Info describes the active provider; it is surfaced to the UI.
type Info struct {
	Provider string `json:"provider"` // "anthropic" | "mock"
	Model    string `json:"model"`
	Reason   string `json:"reason,omitempty"`
}

// Client makes model calls.
type Client interface {
	Stream(ctx context.Context, req Request) (Stream, error)
	Info() Info
}

// ErrorKind classifies a failed call.
type ErrorKind string

const (
	KindRateLimited       ErrorKind = "rate_limited"
	KindOverloaded        ErrorKind = "overloaded"
	KindUnavailable       ErrorKind = "unavailable"
	KindBadRequest        ErrorKind = "bad_request"
	KindAuth              ErrorKind = "auth"
	KindTimeout           ErrorKind = "timeout"
	KindStreamInterrupted ErrorKind = "stream_interrupted"
)

// Error is a classified provider error. Match kinds with errors.Is against
// the Err* sentinels.
type Error struct {
	Kind       ErrorKind
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm %s: %v", e.Kind, e.Err)
	}
	return "llm " + string(e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches any *Error of the same kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrRateLimited       = &Error{Kind: KindRateLimited}
	ErrOverloaded        = &Error{Kind: KindOverloaded}
	ErrUnavailable       = &Error{Kind: KindUnavailable}
	ErrBadRequest        = &Error{Kind: KindBadRequest}
	ErrAuth              = &Error{Kind: KindAuth}
	ErrTimeout           = &Error{Kind: KindTimeout}
	ErrStreamInterrupted = &Error{Kind: KindStreamInterrupted}
)

// CallRecord audits one model call.
type CallRecord struct {
	Kind       string // "router" | "main"
	Iteration  int
	StopReason StopReason
	Usage      Usage
	Latency    time.Duration
	ErrorCode  string
}

// ErrorCode maps a call error to the code stored on turns and calls.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrRateLimited):
		return "llm_rate_limited"
	case errors.Is(err, ErrAuth):
		return "llm_auth"
	case errors.Is(err, ErrBadRequest):
		return "llm_bad_request"
	case errors.Is(err, ErrOverloaded), errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrTimeout), errors.Is(err, ErrStreamInterrupted),
		errors.Is(err, context.DeadlineExceeded):
		return "llm_unavailable"
	}
	return "llm_error"
}
