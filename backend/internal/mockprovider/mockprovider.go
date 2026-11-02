// Package mockprovider is a fake SMS/email provider for the demo and tests.
// Nothing leaves it: it only records what it accepted.
//
// API: POST /v1/messages {channel, to, subject?, body} with an
// Idempotency-Key header. 202 {id} accepts a message. The same key again is
// answered 200 with the first acceptance when the payload matches, 422 when
// it does not. GET /stats returns the acceptance counters.
//
// Acceptances are appended to a log file and synced before they are
// answered. A restarted provider reloads them, so a retry after a lost
// answer still gets the first acceptance and a key is never accepted twice.
//
// Faults, chosen per key and request, make it behave like a flaky provider.
// Refusals (429, 400, 503 without accepting) only hit keys not accepted yet:
// a key already accepted is answered with its acceptance whatever the
// script says. Lost answers (accept then 503, accept then no answer) can hit
// any request, replays included.
package mockprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"smart-campaign-composer/backend/internal/content"
)

// Fault is how the provider treats one request.
type Fault string

const (
	// Pass accepts the message, or replays an earlier acceptance, and answers.
	Pass Fault = ""
	// Reject429 answers 429 with Retry-After; nothing is accepted.
	Reject429 Fault = "reject_429"
	// Reject400 answers 400; nothing is accepted.
	Reject400 Fault = "reject_400"
	// Fail503 answers 503 without accepting.
	Fail503 Fault = "fail_503"
	// AcceptThen503 accepts the message (or replays), then answers 503.
	AcceptThen503 Fault = "accept_then_503"
	// AcceptThenDrop accepts the message (or replays) and never answers; the
	// caller times out.
	AcceptThenDrop Fault = "accept_then_drop"
	// Delay accepts the message (or replays) and answers after the configured delay.
	Delay Fault = "delay"
)

// dropHold bounds how long an unanswered request is held open.
const dropHold = time.Minute

// Faults picks the fault for the n-th request (1-based) carrying a key.
type Faults interface {
	Fault(key string, n int) Fault
}

// Script plays a list of faults per key; requests beyond the list pass.
type Script map[string][]Fault

// Fault implements Faults.
func (s Script) Fault(key string, n int) Fault {
	if fs := s[key]; n >= 1 && n <= len(fs) {
		return fs[n-1]
	}
	return Pass
}

// Random draws a fault for each request with fixed probabilities (the rest
// pass), for the demo and chaos tests. It is safe for concurrent use.
type Random struct {
	mu    sync.Mutex
	rng   *rand.Rand
	rates []rate
}

type rate struct {
	fault Fault
	p     float64
}

// NewRandom returns faults drawn from a source seeded with seed. The
// probabilities must be non-negative and add up to at most 1.
func NewRandom(seed uint64, rates map[Fault]float64) (*Random, error) {
	r := &Random{rng: rand.New(rand.NewPCG(seed, seed))}
	sum := 0.0
	for f, p := range rates {
		if p < 0 || math.IsNaN(p) {
			return nil, fmt.Errorf("mockprovider: rate of %s is %v", f, p)
		}
		sum += p
		r.rates = append(r.rates, rate{f, p})
	}
	if sum > 1+1e-9 {
		return nil, fmt.Errorf("mockprovider: fault rates add up to %v, more than 1", sum)
	}
	// Map order is random; fix it so that a seed always draws the same faults.
	sort.Slice(r.rates, func(i, j int) bool { return r.rates[i].fault < r.rates[j].fault })
	return r, nil
}

// Fault implements Faults.
func (r *Random) Fault(string, int) Fault {
	r.mu.Lock()
	x := r.rng.Float64()
	r.mu.Unlock()
	for _, rt := range r.rates {
		if x < rt.p {
			return rt.fault
		}
		x -= rt.p
	}
	return Pass
}

// Stats counts message requests since start, and acceptances per
// idempotency key and per message (person, channel, content hash) whatever
// the key: a message sent twice under two keys shows up as 2.
type Stats struct {
	Requests  int            `json:"requests"`
	Accepted  int            `json:"accepted"`
	ByKey     map[string]int `json:"by_key"`
	ByMessage []MessageCount `json:"by_message"`
}

// MessageCount is how often one message was accepted.
type MessageCount struct {
	To          string `json:"to"`
	Channel     string `json:"channel"`
	ContentHash string `json:"content_hash"`
	Accepted    int    `json:"accepted"`
}

type message struct {
	Channel string `json:"channel"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// record is one acceptance in the log.
type record struct {
	Key string `json:"key"`
	ID  string `json:"id"`
	message
}

type messageKey struct{ to, channel, hash string }

type acceptance struct {
	id  string
	msg message
}

// Provider is the fake provider; it is an http.Handler.
type Provider struct {
	faults Faults
	delay  time.Duration

	mu        sync.Mutex
	log       *os.File
	broken    error          // a failed log write: nothing more is accepted
	received  int            // message requests since start, accepted or not
	requests  map[string]int // the same, per key
	accepted  map[string]acceptance
	byMessage map[messageKey]int
	total     int
}

// Open starts a provider whose acceptances live in the log at path, created
// if missing. f may be nil for no faults.
func Open(path string, f Faults) (*Provider, error) {
	return OpenWithDelay(path, f, 0)
}

// OpenWithDelay is Open with a response delay for requests selected by the
// Delay fault. Acceptance is recorded before the response is delayed.
func OpenWithDelay(path string, f Faults, delay time.Duration) (*Provider, error) {
	if delay < 0 {
		return nil, fmt.Errorf("mockprovider: delay must not be negative")
	}
	if f == nil {
		f = Script(nil)
	}
	log, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	p := &Provider{faults: f, delay: delay, log: log, requests: map[string]int{}, accepted: map[string]acceptance{}, byMessage: map[messageKey]int{}}
	if err := p.load(); err != nil {
		log.Close()
		return nil, err
	}
	return p, nil
}

// load replays the log. A last line without its newline was cut short by a
// crash before it was synced and answered, so it never counted as an
// acceptance: it is cut off. Any other unreadable line is an error.
func (p *Provider) load() error {
	data, err := io.ReadAll(p.log)
	if err != nil {
		return err
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	for i, line := range bytes.Split(data[:end], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("mockprovider: log line %d: %w", i+1, err)
		}
		if _, dup := p.accepted[r.Key]; dup {
			return fmt.Errorf("mockprovider: log line %d: key %s accepted twice", i+1, r.Key)
		}
		p.remember(r)
	}
	if end < len(data) {
		return p.log.Truncate(int64(end))
	}
	return nil
}

func (p *Provider) remember(r record) {
	p.total++
	p.accepted[r.Key] = acceptance{id: r.ID, msg: r.message}
	p.byMessage[messageKey{r.To, r.Channel, content.Hash(r.Channel, r.Subject, r.Body)}]++
}

// Close closes the log.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.Close()
}

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.mu.Lock()
		p.received++
		p.mu.Unlock()
		p.send(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		writeJSON(w, http.StatusOK, p.Stats())
	default:
		http.NotFound(w, r)
	}
}

func (p *Provider) send(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key")
		return
	}
	var m message
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || (m.Channel != "sms" && m.Channel != "email") || m.To == "" || m.Body == "" {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}

	fault, known := p.begin(key)
	if !known {
		// A provider refuses before it accepts. A key it already accepted
		// is answered with that acceptance, whatever the script says.
		switch fault {
		case Reject429:
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		case Reject400:
			writeError(w, http.StatusBadRequest, "rejected")
			return
		case Fail503:
			writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
	}
	id, replay, ok, err := p.accept(key, m)
	switch {
	case err != nil:
		slog.Error("mockprovider: record acceptance", "err", err)
		writeError(w, http.StatusInternalServerError, "storage_failed")
		return
	case !ok:
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused")
		return
	}
	switch fault {
	case AcceptThenDrop:
		drop(w, r)
		return
	case AcceptThen503:
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	case Delay:
		if p.delay > 0 {
			timer := time.NewTimer(p.delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
	}
	status := http.StatusAccepted
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]string{"id": id, "status": "accepted"})
}

// begin counts the request and returns its fault and whether its key was
// already accepted.
func (p *Provider) begin(key string) (Fault, bool) {
	p.mu.Lock()
	p.requests[key]++
	n := p.requests[key]
	_, known := p.accepted[key]
	p.mu.Unlock()
	return p.faults.Fault(key, n), known
}

// accept records the message under key, durably, or finds the key's
// earlier acceptance; ok is false when key was accepted with another
// payload.
func (p *Provider) accept(key string, m message) (id string, replay, ok bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a, seen := p.accepted[key]; seen {
		return a.id, true, a.msg == m, nil
	}
	if p.broken != nil {
		return "", false, false, p.broken
	}
	r := record{Key: key, ID: fmt.Sprintf("msg_%06d", p.total+1), message: m}
	line, err := json.Marshal(r)
	if err != nil {
		return "", false, false, err
	}
	// The acceptance exists once it is on disk; only then is it answered.
	if _, err := p.log.Write(append(line, '\n')); err != nil {
		p.broken = err
		return "", false, false, err
	}
	if err := p.log.Sync(); err != nil {
		p.broken = err
		return "", false, false, err
	}
	p.remember(r)
	return r.ID, false, true, nil
}

// drop holds the request without an answer until the caller gives up, then
// closes the connection (returning normally would send an empty 200).
func drop(w http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(dropHold):
	}
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		conn.Close()
	}
}

// Stats returns the counters.
func (p *Provider) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Stats{Requests: p.received, Accepted: p.total, ByKey: make(map[string]int, len(p.accepted)),
		ByMessage: make([]MessageCount, 0, len(p.byMessage))}
	for k := range p.accepted {
		st.ByKey[k] = 1
	}
	for k, n := range p.byMessage {
		st.ByMessage = append(st.ByMessage, MessageCount{To: k.to, Channel: k.channel, ContentHash: k.hash, Accepted: n})
	}
	sort.Slice(st.ByMessage, func(i, j int) bool {
		a, b := st.ByMessage[i], st.ByMessage[j]
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Channel != b.Channel {
			return a.Channel < b.Channel
		}
		return a.ContentHash < b.ContentHash
	})
	return st
}

// AcceptedFor returns how often a message (person, channel, content) was
// accepted.
func (p *Provider) AcceptedFor(to, channel, subject, body string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byMessage[messageKey{to, channel, content.Hash(channel, subject, body)}]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("mockprovider: write json", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// faults are the faults ParseRates knows; requests left over pass.
var faults = []Fault{Reject429, Reject400, Fail503, AcceptThen503, AcceptThenDrop, Delay}

// ParseRates reads fault rates written as "fault=rate,…" (for example
// "reject_429=0.02,accept_then_drop=0.005"); an empty string means none.
// Each fault is known and given once, each rate is between 0 and 1, and the
// rates add up to at most 1.
func ParseRates(s string) (map[Fault]float64, error) {
	rates := map[Fault]float64{}
	sum := 0.0
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		f := Fault(strings.TrimSpace(name))
		switch {
		case !ok:
			return nil, fmt.Errorf("mockprovider: %q: want fault=rate", part)
		case !slices.Contains(faults, f):
			return nil, fmt.Errorf("mockprovider: unknown fault %q (known: %v)", f, faults)
		}
		if _, seen := rates[f]; seen {
			return nil, fmt.Errorf("mockprovider: fault %s given twice", f)
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil || math.IsNaN(p) || p < 0 || p > 1 {
			return nil, fmt.Errorf("mockprovider: rate of %s must be between 0 and 1, got %q", f, val)
		}
		rates[f] = p
		sum += p
	}
	if sum > 1+1e-9 {
		return nil, fmt.Errorf("mockprovider: fault rates add up to %v, more than 1", sum)
	}
	return rates, nil
}
