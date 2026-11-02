// Package provider sends messages to the SMS/email provider, one attempt per
// call, and classifies every attempt by what it proves about delivery.
//
// Contract with the provider: 2xx means accepted; 408 and 429 mean not
// accepted, try later; other 4xx mean refused. 5xx, and any failure once a
// connection to the provider is up (even a failed write), may hide an
// acceptance; only a failure before any connection proves the message was
// not sent. Every request carries the delivery id as Idempotency-Key, so the
// provider answers a repeated key with its first acceptance instead of
// sending a second message.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Outcome of one send attempt: what it proves about delivery.
type Outcome string

const (
	// Sent: the provider accepted the message.
	Sent Outcome = "sent"
	// NotSentRetryable: the provider certainly did not accept the message
	// (408 or 429, or no connection to it could be made); a later attempt
	// may succeed.
	NotSentRetryable Outcome = "not_sent_retryable"
	// NotSentPermanent: the provider refused the message; retrying will not
	// help.
	NotSentPermanent Outcome = "not_sent_permanent"
	// Ambiguous: the provider may have accepted the message without the
	// answer reaching us (5xx, or a failure after connecting).
	Ambiguous Outcome = "ambiguous"
)

// Classify maps one attempt to its outcome. status is the HTTP status when
// an answer came; err is the transport error otherwise, and connected tells
// whether a connection to the provider had been made before err. Bytes of
// the request may have left as soon as there is a connection, so from then
// on no error proves the message was not sent.
func Classify(status int, err error, connected bool) Outcome {
	if err != nil {
		if connected {
			return Ambiguous
		}
		return NotSentRetryable
	}
	switch {
	case status >= 200 && status < 300:
		return Sent
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		return NotSentRetryable
	case status >= 400 && status < 500:
		return NotSentPermanent
	default:
		// 5xx, and anything outside the contract.
		return Ambiguous
	}
}

// Message is one rendered message, sent exactly as approved.
type Message struct {
	IdempotencyKey string // the delivery id; every attempt reuses it
	Channel        string // sms | email
	To             string // phone number or email address
	Subject        string // email only
	Body           string
}

// Result of one attempt.
type Result struct {
	Outcome    Outcome
	Status     int           // HTTP status; 0 when no answer came
	MessageID  string        // the provider's id when Sent
	RetryAfter time.Duration // the provider's Retry-After, if any
	Err        error         // transport error when no answer came
}

const (
	defaultTimeout = 10 * time.Second
	maxRetryAfter  = time.Hour
)

// Options of a Client.
type Options struct {
	Timeout   time.Duration     // per attempt; 0 means 10s
	Transport http.RoundTripper // nil means a clone of http.DefaultTransport
}

// Client calls the provider's HTTP API.
type Client struct {
	url string
	hc  *http.Client
}

// NewClient returns a client for the provider at baseURL.
func NewClient(baseURL string, o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Transport == nil {
		o.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &Client{url: strings.TrimRight(baseURL, "/") + "/v1/messages", hc: &http.Client{
		Timeout:   o.Timeout,
		Transport: o.Transport,
		// A redirect is not an acceptance; never resend the message elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type request struct {
	Channel string `json:"channel"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// Send makes one attempt; the caller decides from the outcome whether to try
// again. (The transport may itself resend over a stale keep-alive
// connection; the idempotency key makes that harmless.)
func (c *Client) Send(ctx context.Context, m Message) Result {
	payload, err := json.Marshal(request{Channel: m.Channel, To: m.To, Subject: m.Subject, Body: m.Body})
	if err != nil {
		return Result{Outcome: NotSentPermanent, Err: err}
	}
	// connected turns true once the transport holds a connection (new or
	// reused, TLS done) for the request. Before that nothing was sent; after
	// it, even a failed write may have delivered the request.
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return Result{Outcome: NotSentPermanent, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return Result{Outcome: Classify(0, err, connected.Load()), Err: err}
	}
	defer resp.Body.Close()
	res := Result{
		Outcome:    Classify(resp.StatusCode, nil, true),
		Status:     resp.StatusCode,
		RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}
	body := io.LimitReader(resp.Body, 64<<10)
	if res.Outcome == Sent {
		// The status proves the acceptance; an unreadable body only loses the id.
		var out struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(body).Decode(&out)
		res.MessageID = out.ID
	}
	_, _ = io.Copy(io.Discard, body) // lets the connection be reused
	return res
}

// retryAfter parses a Retry-After header (seconds or an HTTP date).
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(min(max(s, 0), int(maxRetryAfter/time.Second))) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter)
	}
	return 0
}
