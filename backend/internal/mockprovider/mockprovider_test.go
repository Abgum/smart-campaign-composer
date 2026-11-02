package mockprovider_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/mockprovider"
)

const (
	key   = "8a2e4f60-1c3b-4d5e-9f70-112233445566"
	phone = "+905550000001"
)

// open starts a provider logging to path, or to a fresh file when path is "".
func open(t *testing.T, path string, f mockprovider.Faults) *mockprovider.Provider {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "provider.log")
	}
	p, err := mockprovider.Open(path, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// send posts an SMS and returns the status and the message id; status 0
// means no answer came.
func send(t *testing.T, c *http.Client, url, key, to, text string) (int, string, http.Header) {
	t.Helper()
	b, err := json.Marshal(map[string]string{"channel": "sms", "to": to, "body": text})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", nil
	}
	defer resp.Body.Close()
	var out struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.ID, resp.Header
}

func TestSameKey_ReplaysTheAcceptance(t *testing.T) {
	p := open(t, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := srv.Client()

	status, id, _ := send(t, c, srv.URL, key, phone, "Merhaba")
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("first request: %d %q, want 202 with an id", status, id)
	}
	status, again, _ := send(t, c, srv.URL, key, phone, "Merhaba")
	if status != http.StatusOK || again != id {
		t.Fatalf("same key again: %d %q, want 200 %q", status, again, id)
	}
	if n := p.AcceptedFor(phone, "sms", "", "Merhaba"); n != 1 {
		t.Fatalf("acceptances = %d, want 1", n)
	}

	resp, err := c.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st mockprovider.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Requests != 2 || st.Accepted != 1 || st.ByKey[key] != 1 {
		t.Fatalf("GET /stats = %+v, err %v", st, err)
	}
}

func TestSameKeyDifferentPayload_Rejected(t *testing.T) {
	p := open(t, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	if status, _, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba"); status != http.StatusAccepted {
		t.Fatalf("first request: %d", status)
	}
	if status, _, _ := send(t, srv.Client(), srv.URL, key, phone, "Başka bir mesaj"); status != http.StatusUnprocessableEntity {
		t.Fatalf("same key, other payload: %d, want 422", status)
	}
	if st := p.Stats(); st.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", st.Accepted)
	}
}

func TestMissingKey_Rejected(t *testing.T) {
	p := open(t, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	if status, _, _ := send(t, srv.Client(), srv.URL, "", phone, "Merhaba"); status != http.StatusBadRequest {
		t.Fatalf("no Idempotency-Key: %d, want 400", status)
	}
	if st := p.Stats(); st.Accepted != 0 {
		t.Fatalf("accepted = %d, want 0", st.Accepted)
	}
}

// Faults are played per key: rate limited (not accepted), then accepted
// with the answer lost, then the acceptance replayed.
func TestScript_RejectThenDropThenReplay(t *testing.T) {
	p := open(t, "", mockprovider.Script{key: {mockprovider.Reject429, mockprovider.AcceptThenDrop}})
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, _, h := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if status != http.StatusTooManyRequests || h.Get("Retry-After") == "" {
		t.Fatalf("first request: %d, want 429 with Retry-After", status)
	}
	if n := p.AcceptedFor(phone, "sms", "", "Merhaba"); n != 0 {
		t.Fatalf("accepted after 429: %d", n)
	}

	short := &http.Client{Timeout: 200 * time.Millisecond}
	if status, _, _ := send(t, short, srv.URL, key, phone, "Merhaba"); status != 0 {
		t.Fatalf("second request answered with %d; its answer must be lost", status)
	}
	if n := p.AcceptedFor(phone, "sms", "", "Merhaba"); n != 1 {
		t.Fatalf("accepted after the lost answer: %d, want 1", n)
	}

	status, id, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if status != http.StatusOK || id == "" {
		t.Fatalf("third request: %d %q, want 200 replaying the acceptance", status, id)
	}
	if n := p.AcceptedFor(phone, "sms", "", "Merhaba"); n != 1 {
		t.Fatalf("accepted = %d, want 1", n)
	}
}

func TestDelay_AcceptsAndAnswersAfterConfiguredDuration(t *testing.T) {
	p, err := mockprovider.OpenWithDelay(filepath.Join(t.TempDir(), "provider.log"),
		mockprovider.Script{key: {mockprovider.Delay}}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	start := time.Now()
	status, id, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("response after %v, want at least 50ms", elapsed)
	}
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("delayed request: %d %q, want 202 with an id", status, id)
	}
	if got := p.AcceptedFor(phone, "sms", "", "Merhaba"); got != 1 {
		t.Fatalf("acceptances = %d, want 1", got)
	}
	status, replayID, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if status != http.StatusOK || replayID != id || p.Stats().Accepted != 1 {
		t.Fatalf("replay: status %d id %q acceptances %d", status, replayID, p.Stats().Accepted)
	}
}

func TestDelay_ClientTimeout_DoesNotAcceptTwice(t *testing.T) {
	p, err := mockprovider.OpenWithDelay(filepath.Join(t.TempDir(), "provider.log"),
		mockprovider.Script{key: {mockprovider.Delay}}, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	srv := httptest.NewServer(p)
	defer srv.Close()

	if status, _, _ := send(t, &http.Client{Timeout: 20 * time.Millisecond}, srv.URL, key, phone, "Merhaba"); status != 0 {
		t.Fatalf("timed out request returned %d, want no answer", status)
	}
	if got := p.AcceptedFor(phone, "sms", "", "Merhaba"); got != 1 {
		t.Fatalf("acceptances after timeout = %d, want 1", got)
	}
	if status, id, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba"); status != http.StatusOK || id == "" {
		t.Fatalf("retry: %d %q, want 200 replay", status, id)
	}
	if got := p.Stats().Accepted; got != 1 {
		t.Fatalf("acceptances after retry = %d, want 1", got)
	}
}

// A key the provider already accepted is answered with that acceptance; a
// scripted refusal cannot turn it into "not accepted".
func TestAcceptedKey_ScriptedRefusal_ReplaysAcceptance(t *testing.T) {
	p := open(t, "", mockprovider.Script{key: {mockprovider.Pass, mockprovider.Reject429, mockprovider.Reject400, mockprovider.Fail503}})
	srv := httptest.NewServer(p)
	defer srv.Close()

	status, id, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if status != http.StatusAccepted || id == "" {
		t.Fatalf("first request: %d %q, want 202", status, id)
	}
	for _, refusal := range []string{"429", "400", "503"} {
		status, again, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
		if status != http.StatusOK || again != id {
			t.Fatalf("scripted %s on an accepted key: %d %q, want 200 %q", refusal, status, again, id)
		}
	}
	if st := p.Stats(); st.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", st.Accepted)
	}
}

// Acceptances survive a restart: the answer to the first request is lost,
// the provider restarts, and the retry with the same key gets the first
// acceptance instead of a second one.
func TestRestart_KeepsAcceptances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.log")
	before := open(t, path, mockprovider.Script{key: {mockprovider.AcceptThenDrop}})
	srv := httptest.NewServer(before)
	short := &http.Client{Timeout: 200 * time.Millisecond}
	if status, _, _ := send(t, short, srv.URL, key, phone, "Merhaba"); status != 0 {
		t.Fatalf("first answer must be lost, got %d", status)
	}
	srv.Close()
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after := open(t, path, nil)
	if st := after.Stats(); st.Accepted != 1 || st.ByKey[key] != 1 {
		t.Fatalf("after the restart: %+v, want the earlier acceptance", st)
	}
	srv = httptest.NewServer(after)
	defer srv.Close()
	status, id, _ := send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	if status != http.StatusOK || id == "" {
		t.Fatalf("retry after the restart: %d %q, want 200 replaying the first acceptance", status, id)
	}
	if status, _, _ := send(t, srv.Client(), srv.URL, key, phone, "Başka"); status != http.StatusUnprocessableEntity {
		t.Fatalf("same key, other payload after the restart: %d, want 422", status)
	}
	status, other, _ := send(t, srv.Client(), srv.URL, "1b2c3d4e-0000-4000-8000-000000000003", phone, "Yeni mesaj")
	if status != http.StatusAccepted || other == id {
		t.Fatalf("new key after the restart: %d %q; ids must stay unique (first %q)", status, other, id)
	}
	if n := after.AcceptedFor(phone, "sms", "", "Merhaba"); n != 1 {
		t.Fatalf("acceptances of the first message = %d, want 1", n)
	}
}

// A record cut short by a crash was never synced or answered: it is dropped,
// and later records are appended cleanly.
func TestOpen_DropsTornLastRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.log")
	first := open(t, path, nil)
	srv := httptest.NewServer(first)
	send(t, srv.Client(), srv.URL, key, phone, "Merhaba")
	srv.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"key":"torn","id":"msg_0000`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	second := open(t, path, nil)
	if st := second.Stats(); st.Accepted != 1 {
		t.Fatalf("after the crash: %d acceptances, want 1", st.Accepted)
	}
	srv = httptest.NewServer(second)
	if status, _, _ := send(t, srv.Client(), srv.URL, "2c3d4e5f-0000-4000-8000-000000000004", phone, "Yeni"); status != http.StatusAccepted {
		t.Fatalf("new key: %d", status)
	}
	srv.Close()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	if st := open(t, path, nil).Stats(); st.Accepted != 2 {
		t.Fatalf("after reopening: %d acceptances, want 2", st.Accepted)
	}
}

func TestRandom_DrawsAtRates(t *testing.T) {
	rates := map[mockprovider.Fault]float64{mockprovider.Reject429: 0.1, mockprovider.AcceptThenDrop: 0.05, mockprovider.Delay: 0.05}
	r, err := mockprovider.NewRandom(7, rates)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[mockprovider.Fault]int{}
	for i := range 20000 {
		counts[r.Fault(key, i+1)]++
	}
	if c := counts[mockprovider.Reject429]; c < 1700 || c > 2300 {
		t.Errorf("429 drawn %d times in 20000, want about 2000", c)
	}
	if c := counts[mockprovider.AcceptThenDrop]; c < 800 || c > 1200 {
		t.Errorf("accept_then_drop drawn %d times in 20000, want about 1000", c)
	}
	if c := counts[mockprovider.Delay]; c < 800 || c > 1200 {
		t.Errorf("delay drawn %d times in 20000, want about 1000", c)
	}

	a, _ := mockprovider.NewRandom(9, rates)
	b, _ := mockprovider.NewRandom(9, rates)
	for i := range 200 {
		if fa, fb := a.Fault(key, i), b.Fault(key, i); fa != fb {
			t.Fatalf("draw %d: %q vs %q; the same seed must draw the same faults", i, fa, fb)
		}
	}

	for _, bad := range []map[mockprovider.Fault]float64{
		{mockprovider.Reject429: 0.7, mockprovider.Fail503: 0.5},
		{mockprovider.Reject429: -0.1},
	} {
		if _, err := mockprovider.NewRandom(1, bad); err == nil {
			t.Errorf("rates %v accepted, want an error", bad)
		}
	}
}

// The provider counts acceptances per message (person, channel, normalized
// content), whatever the key: a double send by the system shows up here.
func TestSameMessageUnderTwoKeys_CountedTwice(t *testing.T) {
	p := open(t, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	send(t, srv.Client(), srv.URL, "0d3c6b1a-0000-4000-8000-000000000001", phone, "Merhaba Ayşe")
	send(t, srv.Client(), srv.URL, "0d3c6b1a-0000-4000-8000-000000000002", phone, " Merhaba  Ayşe ")
	if n := p.AcceptedFor(phone, "sms", "", "Merhaba Ayşe"); n != 2 {
		t.Fatalf("acceptances of the same message = %d, want 2", n)
	}
}

// Fault rates come from the environment as "fault=rate,…".
func TestParseRates(t *testing.T) {
	got, err := mockprovider.ParseRates(" reject_429=0.02, accept_then_drop=0.005 ,fail_503=0,delay=0.01 ")
	want := map[mockprovider.Fault]float64{mockprovider.Reject429: 0.02, mockprovider.AcceptThenDrop: 0.005, mockprovider.Fail503: 0, mockprovider.Delay: 0.01}
	if err != nil || len(got) != len(want) {
		t.Fatalf("ParseRates = %v, %v; want %v", got, err, want)
	}
	for f, p := range want {
		if got[f] != p {
			t.Fatalf("rate of %s = %v, want %v", f, got[f], p)
		}
	}
	if got, err := mockprovider.ParseRates(""); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v; want no faults", got, err)
	}
	for _, bad := range []string{"boom=0.1", "reject_429", "reject_429=x", "reject_429=-0.1", "reject_429=0.6,fail_503=0.6",
		"reject_429=0.1,reject_429=0.2", "pass=0.1"} {
		if _, err := mockprovider.ParseRates(bad); err == nil {
			t.Errorf("ParseRates(%q) accepted", bad)
		}
	}
}
