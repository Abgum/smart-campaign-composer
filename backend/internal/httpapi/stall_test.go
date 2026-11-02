package httpapi_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/httpapi"
	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/mock"
)

// stallConn is a server-side connection whose writes can be made to block
// the way writes to a client that has stopped reading block once the kernel
// buffers are full: a blocked write returns only when its write deadline
// passes or the connection is closed.
type stallConn struct {
	net.Conn
	mu        sync.Mutex
	stalled   bool
	deadline  time.Time
	changed   chan struct{} // capacity 1: stalled or deadline changed
	closed    chan struct{}
	closeOnce sync.Once
}

func newStallConn(c net.Conn) *stallConn {
	return &stallConn{Conn: c, changed: make(chan struct{}, 1), closed: make(chan struct{})}
}

// stall makes every later write block: the client reads no more.
func (c *stallConn) stall() {
	c.mu.Lock()
	c.stalled = true
	c.mu.Unlock()
	c.poke()
}

func (c *stallConn) poke() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *stallConn) Write(p []byte) (int, error) {
	for {
		c.mu.Lock()
		stalled, deadline := c.stalled, c.deadline
		c.mu.Unlock()
		if !stalled {
			return c.Conn.Write(p)
		}
		var expired <-chan time.Time
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			expired = time.After(left)
		}
		select {
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		case <-c.changed:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	c.poke()
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	c.poke()
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// stallListener wraps every accepted connection in a stallConn.
type stallListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := newStallConn(c)
	l.mu.Lock()
	l.conns = append(l.conns, sc)
	l.mu.Unlock()
	return sc, nil
}

// closeAll closes every connection, which frees writes still blocked.
func (l *stallListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
}

type connKey struct{}

// stallHarness serves the API on stallable connections and watches one
// request: it hands out that request's connection and reports when its
// handler returns.
type stallHarness struct {
	ln       *stallListener
	watch    func(*http.Request) bool
	conn     chan *stallConn
	returned chan struct{}
}

func newStallEnv(t *testing.T, main llm.Client, watch func(*http.Request) bool, adjust func(*httpapi.Deps)) (*env, *stallHarness) {
	t.Helper()
	h := &stallHarness{watch: watch, conn: make(chan *stallConn, 1), returned: make(chan struct{})}
	e := newEnvOn(t, main, adjust, h.wrap, func(srv *httptest.Server) {
		h.ln = &stallListener{Listener: srv.Listener}
		srv.Listener = h.ln
		srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, c)
		}
	})
	t.Cleanup(h.ln.closeAll) // runs before the server closes: frees a handler still blocked
	return e, h
}

func (h *stallHarness) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.watch(r) {
			next.ServeHTTP(w, r)
			return
		}
		h.conn <- r.Context().Value(connKey{}).(*stallConn)
		defer close(h.returned)
		next.ServeHTTP(w, r)
	})
}

// handlerReturns fails the test unless the watched handler returns soon.
func (h *stallHarness) handlerReturns(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.returned:
	case <-time.After(3 * time.Second):
		t.Fatalf("the %s handler is still blocked writing to a client that stopped reading", what)
	}
}

// A client that stops reading must not hold a stream's handler (and its
// connection) forever: every event write has a deadline, after which the
// handler gives up on the client and returns.
func TestDashboardStream_ClientStopsReading_HandlerReturns(t *testing.T) {
	e, h := newStallEnv(t, mock.NewScripted(),
		func(r *http.Request) bool { return r.URL.Path == "/api/dashboard/stream" },
		func(d *httpapi.Deps) { d.StreamEvery, d.WriteTimeout = 20*time.Millisecond, 200*time.Millisecond })
	resp, err := http.Get(e.srv.URL + "/api/dashboard/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if first := readEvents(t, resp.Body, func(sseEvent) bool { return true }); len(first) != 1 || first[0].Type != "dashboard" {
		t.Fatalf("first event = %+v", first)
	}
	(<-h.conn).stall()
	h.handlerReturns(t, "dashboard stream")
}

// The same holds for a turn's stream; the turn itself runs on, and its
// result is stored for the client to fetch.
func TestTurnStream_ClientStopsReading_HandlerReturns(t *testing.T) {
	text := strings.Repeat("Sepetinde ürün kaldı. ", 12)
	e, h := newStallEnv(t, mock.NewScripted(mock.Call{Text: text, ChunkDelay: 10 * time.Millisecond}),
		func(r *http.Request) bool {
			return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/turns")
		},
		func(d *httpapi.Deps) { d.WriteTimeout = 200 * time.Millisecond })
	chatID := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	turnID := turnIDOf(t, readEvents(t, resp.Body, func(ev sseEvent) bool { return ev.Type == "turn.started" }))
	(<-h.conn).stall()
	h.handlerReturns(t, "turn stream")

	deadline := time.Now().Add(5 * time.Second)
	for {
		res := e.getTurn(t, turnID)
		if res.Status != "running" {
			if res.Status != "completed" || res.AssistantMessage == nil || res.AssistantMessage.Text != text {
				t.Fatalf("result = %+v", res)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn did not finish after its stream gave up on the client")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The write deadline bounds a write, not the time between events: a stream
// that stays quiet longer than WriteTimeout (a model thinking) still
// delivers what comes next, because the deadline is renewed before each
// write.
func TestTurnStream_QuietLongerThanWriteTimeout_StillDelivers(t *testing.T) {
	const text = "Merhaba, yavaş bir yanıt."
	e := newEnvWith(t, mock.NewScripted(mock.Call{Text: text, ChunkDelay: 150 * time.Millisecond}),
		func(d *httpapi.Deps) { d.WriteTimeout = 50 * time.Millisecond }, nil)
	chatID := e.createChat(t)
	resp := e.postTurn(t, context.Background(), chatID, uuid(), "merhaba")
	defer resp.Body.Close()
	events := readEvents(t, resp.Body, nil)
	if len(events) == 0 || events[len(events)-1].Type != "done" || previewText(events) != text {
		t.Fatalf("got %d events, preview %q; want the whole answer and done", len(events), previewText(events))
	}
}
