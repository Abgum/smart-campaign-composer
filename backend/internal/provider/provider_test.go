package provider_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/mockprovider"
	"smart-campaign-composer/backend/internal/provider"
)

var msg = provider.Message{
	IdempotencyKey: "5f1d7c2a-9b0e-4c1a-8f3e-2d6b9a4c7e10",
	Channel:        "sms",
	To:             "+905550000001",
	Body:           "Merhaba Ayşe, sepetin seni bekliyor!",
}

// Only an answer, or an error before any connection to the provider, is
// definite. Once connected, even a failed write may hide an acceptance.
func TestClassify_Outcomes(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	broken := &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	cases := []struct {
		name      string
		status    int
		err       error
		connected bool
		want      provider.Outcome
	}{
		{"200 accepted", 200, nil, true, provider.Sent},
		{"202 accepted", 202, nil, true, provider.Sent},
		{"429 rate limited", 429, nil, true, provider.NotSentRetryable},
		{"408 request timeout", 408, nil, true, provider.NotSentRetryable},
		{"400 rejected", 400, nil, true, provider.NotSentPermanent},
		{"401 unauthorized", 401, nil, true, provider.NotSentPermanent},
		{"403 forbidden", 403, nil, true, provider.NotSentPermanent},
		{"422 invalid", 422, nil, true, provider.NotSentPermanent},
		{"500", 500, nil, true, provider.Ambiguous},
		{"502", 502, nil, true, provider.Ambiguous},
		{"503", 503, nil, true, provider.Ambiguous},
		{"504", 504, nil, true, provider.Ambiguous},
		{"connection refused", 0, refused, false, provider.NotSentRetryable},
		{"timeout before a connection", 0, context.DeadlineExceeded, false, provider.NotSentRetryable},
		{"write error after connecting", 0, broken, true, provider.Ambiguous},
		{"timeout after connecting", 0, context.DeadlineExceeded, true, provider.Ambiguous},
		{"EOF after connecting", 0, io.EOF, true, provider.Ambiguous},
		{"unexpected EOF after connecting", 0, io.ErrUnexpectedEOF, true, provider.Ambiguous},
		{"connection reset after connecting", 0, reset, true, provider.Ambiguous},
	}
	for _, c := range cases {
		if got := provider.Classify(c.status, c.err, c.connected); got != c.want {
			t.Errorf("%s: Classify(%d, %v, connected=%v) = %q, want %q", c.name, c.status, c.err, c.connected, got, c.want)
		}
	}
}

type captured struct {
	method, path, key string
	body              map[string]string
}

func TestSend_Accepted(t *testing.T) {
	reqs := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := captured{method: r.Method, path: r.URL.Path, key: r.Header.Get("Idempotency-Key")}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		reqs <- c
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"msg_1"}`)
	}))
	defer srv.Close()

	res := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second}).Send(context.Background(), msg)
	if res.Outcome != provider.Sent || res.Status != http.StatusAccepted || res.MessageID != "msg_1" || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	select {
	case c := <-reqs:
		if c.method != http.MethodPost || c.path != "/v1/messages" || c.key != msg.IdempotencyKey ||
			c.body["channel"] != msg.Channel || c.body["to"] != msg.To || c.body["body"] != msg.Body {
			t.Fatalf("request = %+v", c)
		}
	default:
		t.Fatal("the provider received no request")
	}
}

func TestSend_ConnectionRefused_NotSentRetryable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	res := provider.NewClient("http://"+addr, provider.Options{Timeout: time.Second}).Send(context.Background(), msg)
	if res.Outcome != provider.NotSentRetryable || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestSend_TimeoutBeforeConnect_NotSentRetryable(t *testing.T) {
	stuckDial := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := provider.NewClient("http://provider.test", provider.Options{Timeout: 100 * time.Millisecond, Transport: stuckDial})
	if res := c.Send(context.Background(), msg); res.Outcome != provider.NotSentRetryable || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
}

// lyingConn delivers every write but reports it as failed, like a write
// whose bytes had already left when the error surfaced.
type lyingConn struct{ net.Conn }

func (c lyingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil {
		err = syscall.EPIPE
	}
	return n, err
}

// The client's write fails after connecting, yet the request reached the
// provider and was accepted: a write error proves nothing about delivery.
func TestSend_WriteErrorAfterConnect_Ambiguous(t *testing.T) {
	mp, err := mockprovider.Open(filepath.Join(t.TempDir(), "provider.log"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mp.Close()
	srv := httptest.NewServer(mp)
	defer srv.Close()
	lying := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return lyingConn{c}, nil
	}}

	res := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second, Transport: lying}).Send(context.Background(), msg)
	if res.Outcome != provider.Ambiguous || res.Err == nil {
		t.Fatalf("result = %+v, want ambiguous", res)
	}
	deadline := time.Now().Add(2 * time.Second)
	for mp.AcceptedFor(msg.To, msg.Channel, "", msg.Body) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the provider never got the request, so the test proves nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The first bytes of the request are already on the wire when a write
// fails, while the request is still being written: nothing proves the
// provider did not get or act on it.
func TestSend_WriteFailsMidRequest_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	smallWrites := &http.Transport{
		WriteBufferSize: 16, // the request goes out in small writes; the first one fails
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return lyingConn{c}, nil
		},
	}
	res := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second, Transport: smallWrites}).Send(context.Background(), msg)
	if res.Outcome != provider.Ambiguous || res.Err == nil {
		t.Fatalf("result = %+v, want ambiguous", res)
	}
}

// The provider closes the connection right after accepting it. The client
// cannot tell whether its request was read, so the attempt is ambiguous.
func TestSend_ClosedRightAfterConnect_Ambiguous(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	res := provider.NewClient("http://"+ln.Addr().String(), provider.Options{Timeout: time.Second}).Send(context.Background(), msg)
	if res.Outcome != provider.Ambiguous || res.Err == nil {
		t.Fatalf("result = %+v, want ambiguous", res)
	}
}

// The provider read the whole request and never answered: it may have
// accepted the message.
func TestSend_TimeoutAfterWrite_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()
	res := provider.NewClient(srv.URL, provider.Options{Timeout: 200 * time.Millisecond}).Send(context.Background(), msg)
	if res.Outcome != provider.Ambiguous || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestSend_ConnectionDroppedAfterWrite_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	res := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second}).Send(context.Background(), msg)
	if res.Outcome != provider.Ambiguous || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestSend_StatusOutcomes(t *testing.T) {
	cases := []struct {
		status     int
		retryAfter string
		want       provider.Outcome
		wantWait   time.Duration
	}{
		{http.StatusTooManyRequests, "7", provider.NotSentRetryable, 7 * time.Second},
		{http.StatusUnprocessableEntity, "", provider.NotSentPermanent, 0},
		{http.StatusServiceUnavailable, "", provider.Ambiguous, 0},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
		}))
		res := provider.NewClient(srv.URL, provider.Options{Timeout: time.Second}).Send(context.Background(), msg)
		srv.Close()
		if res.Outcome != c.want || res.Status != c.status || res.RetryAfter != c.wantWait {
			t.Errorf("%d: result = %+v, want %s (wait %v)", c.status, res, c.want, c.wantWait)
		}
	}
}
