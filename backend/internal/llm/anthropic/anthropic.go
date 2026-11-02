// Package anthropic adapts the official Anthropic Go SDK (v2) to llm.Client.
// It is the only package that imports the SDK.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"

	"smart-campaign-composer/backend/internal/llm"
)

// Config configures the adapter.
type Config struct {
	APIKey     string
	BaseURL    string // empty = SDK default
	Model      string
	Effort     string // low | medium | high | xhigh | max
	MaxRetries int
	Timeout    time.Duration // per request attempt, 0 = SDK default
	HTTPClient *http.Client
}

// Client is an llm.Client backed by the Messages API.
type Client struct {
	cfg Config
	sdk sdk.Client
}

// New returns a client; an API key is required.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic: API key is required")
	}
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(cfg.MaxRetries),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.Timeout))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Client{cfg: cfg, sdk: sdk.NewClient(opts...)}, nil
}

// Info implements llm.Client.
func (c *Client) Info() llm.Info { return llm.Info{Provider: "anthropic", Model: c.cfg.Model} }

// Stream implements llm.Client. The request uses adaptive thinking, a cached
// system prompt and no sampling parameters or beta headers.
func (c *Client) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(c.cfg.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  toMessages(req.Messages),
		Tools:     toTools(req.Tools),
		Thinking:  sdk.ThinkingConfigParamUnion{OfAdaptive: &sdk.ThinkingConfigAdaptiveParam{}},
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System, CacheControl: sdk.NewCacheControlEphemeralParam()}}
	}
	if c.cfg.Effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(c.cfg.Effort)}
	}
	return &stream{
		ctx:   ctx,
		s:     c.sdk.Messages.NewStreaming(ctx, params),
		tools: map[int64]*toolState{},
	}, nil
}

func toTools(tools []llm.Tool) []sdk.ToolUnionParam {
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		ordered, _ := inOrder(t.InputSchema).(map[string]any)
		schema := sdk.ToolInputSchemaParam{Properties: ordered["properties"], ExtraFields: map[string]any{}}
		switch req := t.InputSchema["required"].(type) {
		case []string:
			schema.Required = req
		case []any:
			for _, r := range req {
				if s, ok := r.(string); ok {
					schema.Required = append(schema.Required, s)
				}
			}
		}
		for k, v := range t.InputSchema {
			if k != "type" && k != "properties" && k != "required" {
				schema.ExtraFields[k] = v
			}
		}
		p := sdk.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			p.Description = sdk.String(t.Description)
		}
		if t.Strict {
			p.Strict = sdk.Bool(true)
		}
		if t.EagerInputStreaming {
			p.EagerInputStreaming = sdk.Bool(true)
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &p})
	}
	return out
}

// inOrder returns the schema with every object's properties in the order the
// model should write them. Strict mode writes properties in schema order, and
// a Go map would go out sorted: an optional field could come first only by
// the alphabet, and one sorted before a field already written could no
// longer be added. The order is the object's required list, then its
// optional properties, sorted.
func inOrder(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			out[k] = v
			if k != "properties" {
				out[k] = inOrder(v)
			}
		}
		props, ok := n["properties"].(map[string]any)
		if !ok {
			return out
		}
		o := orderedObject{values: make(map[string]any, len(props))}
		var required []string
		switch r := n["required"].(type) {
		case []string:
			required = r
		case []any:
			for _, v := range r {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		}
		for _, k := range required {
			if _, ok := props[k]; ok && !slices.Contains(o.keys, k) {
				o.keys = append(o.keys, k)
			}
		}
		var optional []string
		for k, v := range props {
			o.values[k] = inOrder(v)
			if !slices.Contains(o.keys, k) {
				optional = append(optional, k)
			}
		}
		slices.Sort(optional)
		o.keys = append(o.keys, optional...)
		out["properties"] = o
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inOrder(v)
		}
		return out
	}
	return node
}

// orderedObject is a JSON object whose keys keep their order.
type orderedObject struct {
	keys   []string
	values map[string]any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(o.values[k])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// toMessages converts domain messages. Blocks that carry a provider-native
// param (assistant content from earlier in this turn) are replayed verbatim
// so thinking signatures stay valid.
func toMessages(msgs []llm.Message) []sdk.MessageParam {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]sdk.ContentBlockParamUnion, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			if p, ok := b.Provider.(sdk.ContentBlockParamUnion); ok {
				blocks = append(blocks, p)
				continue
			}
			switch b.Type {
			case llm.BlockText:
				blocks = append(blocks, sdk.NewTextBlock(b.Text))
			case llm.BlockToolUse:
				blocks = append(blocks, sdk.NewToolUseBlock(b.ToolUseID, b.Input, b.ToolName))
			case llm.BlockToolResult:
				blocks = append(blocks, sdk.NewToolResultBlock(b.ToolUseID, b.Text, b.IsError))
			}
		}
		if m.Role == llm.RoleAssistant {
			out = append(out, sdk.NewAssistantMessage(blocks...))
		} else {
			out = append(out, sdk.NewUserMessage(blocks...))
		}
	}
	return out
}

type toolState struct {
	id, name string
	buf      []byte
}

type stream struct {
	ctx     context.Context
	s       *ssestream.Stream[sdk.MessageStreamEventUnion]
	acc     sdk.Message
	tools   map[int64]*toolState
	pending []llm.Event
	cur     llm.Event
	err     error
	started bool // at least one event was received
	stopped bool // message_stop was received
}

func (s *stream) Event() llm.Event { return s.cur }
func (s *stream) Err() error       { return s.err }
func (s *stream) Close() error     { return s.s.Close() }

func (s *stream) Next() bool {
	for {
		if len(s.pending) > 0 {
			s.cur, s.pending = s.pending[0], s.pending[1:]
			return true
		}
		if s.err != nil || s.stopped {
			return false
		}
		if !s.s.Next() {
			switch err := s.s.Err(); {
			case err != nil:
				s.err = mapError(err, s.started)
			default:
				s.err = &llm.Error{Kind: llm.KindStreamInterrupted, Err: io.ErrUnexpectedEOF}
			}
			return false
		}
		s.started = true
		ev := s.s.Current()
		if err := s.acc.Accumulate(ev); err != nil {
			s.err = &llm.Error{Kind: llm.KindStreamInterrupted, Err: err}
			return false
		}
		s.translate(ev)
	}
}

func (s *stream) translate(ev sdk.MessageStreamEventUnion) {
	switch v := ev.AsAny().(type) {
	case sdk.ContentBlockStartEvent:
		if v.ContentBlock.Type == "tool_use" {
			s.tools[v.Index] = &toolState{id: v.ContentBlock.ID, name: v.ContentBlock.Name}
			s.pending = append(s.pending, llm.ToolUseStart{ID: v.ContentBlock.ID, Name: v.ContentBlock.Name})
		}
	case sdk.ContentBlockDeltaEvent:
		switch v.Delta.Type {
		case "text_delta":
			s.pending = append(s.pending, llm.TextDelta{Text: v.Delta.Text})
		case "input_json_delta":
			if ts := s.tools[v.Index]; ts != nil && v.Delta.PartialJSON != "" {
				ts.buf = append(ts.buf, v.Delta.PartialJSON...)
				s.pending = append(s.pending, llm.ToolInputDelta{ID: ts.id, PartialJSON: v.Delta.PartialJSON})
			}
		}
	case sdk.ContentBlockStopEvent:
		if ts := s.tools[v.Index]; ts != nil {
			raw := ts.buf
			if len(raw) == 0 {
				raw = []byte("{}")
			}
			s.pending = append(s.pending, llm.ToolUseEnd{ID: ts.id, Name: ts.name, RawInput: raw})
		}
	case sdk.MessageStopEvent:
		s.stopped = true
		s.pending = append(s.pending, llm.MessageEnd{
			StopReason: mapStop(s.acc.StopReason),
			Usage: llm.Usage{
				InputTokens:          s.acc.Usage.InputTokens,
				OutputTokens:         s.acc.Usage.OutputTokens,
				CacheReadInputTokens: s.acc.Usage.CacheReadInputTokens,
			},
			Content: contentBlocks(s.acc.Content),
		})
	}
}

func contentBlocks(content []sdk.ContentBlockUnion) []llm.Block {
	out := make([]llm.Block, 0, len(content))
	for _, cb := range content {
		b := llm.Block{Provider: cb.ToParam()}
		switch cb.Type {
		case "text":
			b.Type, b.Text = llm.BlockText, cb.Text
		case "tool_use":
			b.Type, b.ToolUseID, b.ToolName, b.Input = llm.BlockToolUse, cb.ID, cb.Name, json.RawMessage(cb.Input)
		default:
			b.Type = llm.BlockOpaque
		}
		out = append(out, b)
	}
	return out
}

func mapStop(r sdk.StopReason) llm.StopReason {
	switch r {
	case sdk.StopReasonEndTurn:
		return llm.StopEndTurn
	case sdk.StopReasonToolUse:
		return llm.StopToolUse
	case sdk.StopReasonMaxTokens:
		return llm.StopMaxTokens
	case sdk.StopReasonRefusal:
		return llm.StopRefusal
	}
	return llm.StopOther
}

// mapError classifies SDK errors into llm error kinds. started tells whether
// the stream had already delivered events (a broken connection then means an
// interrupted stream rather than an unavailable service).
func mapError(err error, started bool) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var aerr *sdk.Error
	if errors.As(err, &aerr) {
		le := &llm.Error{Status: aerr.StatusCode, Err: err}
		switch code := aerr.StatusCode; {
		case code == http.StatusTooManyRequests:
			le.Kind = llm.KindRateLimited
			if aerr.Response != nil {
				if secs, err := strconv.Atoi(aerr.Response.Header.Get("Retry-After")); err == nil {
					le.RetryAfter = time.Duration(secs) * time.Second
				}
			}
		case code == 529:
			le.Kind = llm.KindOverloaded
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			le.Kind = llm.KindAuth
		case code >= 500:
			le.Kind = llm.KindUnavailable
		case code >= 400:
			le.Kind = llm.KindBadRequest
		default: // error event inside a 200 stream
			if string(aerr.Type()) == "overloaded_error" {
				le.Kind = llm.KindOverloaded
			} else {
				le.Kind = llm.KindStreamInterrupted
			}
		}
		return le
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &llm.Error{Kind: llm.KindTimeout, Err: err}
	}
	if started {
		return &llm.Error{Kind: llm.KindStreamInterrupted, Err: err}
	}
	return &llm.Error{Kind: llm.KindUnavailable, Err: err}
}
