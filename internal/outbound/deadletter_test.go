package outbound

// Dead-letter tests. They pin the three outcomes a caller has to be able to
// tell apart: delivered, refused-permanently (never parked), and exhausted
// (parked exactly once, with the body that would actually be re-signed).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memSink is an in-memory Sink. It is the same idempotency contract as
// store.PutDeadLetter: one row per delivery id, newest error wins.
type memSink struct {
	mu    sync.Mutex
	rows  map[string]*DeadLetter
	order []string
	err   error
	puts  int
}

func newMemSink() *memSink { return &memSink{rows: map[string]*DeadLetter{}} }

func (m *memSink) Put(ctx context.Context, dl *DeadLetter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.err != nil {
		return m.err
	}
	cp := *dl
	cp.Body = append([]byte(nil), dl.Body...)
	if prev, ok := m.rows[dl.DeliveryID]; ok {
		// Same contract as store.PutDeadLetter: newest error wins, the
		// attempt count never goes backwards.
		cp.Attempts = prev.Attempts
		if dl.Attempts > cp.Attempts {
			cp.Attempts = dl.Attempts
		}
	} else {
		m.order = append(m.order, dl.DeliveryID)
	}
	m.rows[dl.DeliveryID] = &cp
	return nil
}

func (m *memSink) get(id string) *DeadLetter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[id]
}

func (m *memSink) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// TestExhaustedDeliveryIsParkedExactlyOnce is the core dead-letter contract: a
// receiver that only ever answers 500 gets every attempt, then one parked row
// holding the last body and the last error.
func TestExhaustedDeliveryIsParkedExactlyOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	const secret = "park-secret"
	sink := newMemSink()
	var results []string
	sender, err := New(Config{
		URL: srv.URL, Secret: secret, MaxAttempts: 3,
		Backoff: func(int) time.Duration { return time.Millisecond },
		Metrics: func(event, result string) { results = append(results, event+"/"+result) },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	sender.UseDeadLetter(sink, nil)

	err = sender.Notify(context.Background(), EventFulfilled, "dlv-500", map[string]any{"id": "ord_500", "status": "fulfilled"})
	if err == nil {
		t.Fatal("a fully failed delivery must return an error")
	}
	var derr *DeliveryError
	if !errors.As(err, &derr) {
		t.Fatalf("want *DeliveryError, got %T (%v)", err, err)
	}
	if !derr.DeadLettered {
		t.Fatalf("delivery should be parked, got %+v", derr)
	}
	if derr.SinkErr != nil {
		t.Fatalf("unexpected sink error: %v", derr.SinkErr)
	}
	if derr.Attempts != 3 {
		t.Fatalf("want 3 attempts recorded, got %d", derr.Attempts)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("want 3 http calls, got %d", got)
	}
	if n := sink.len(); n != 1 {
		t.Fatalf("want exactly one dead-letter row, got %d", n)
	}
	row := sink.get("dlv-500")
	if row == nil {
		t.Fatal("row missing")
	}
	if row.Event != EventFulfilled {
		t.Fatalf("event: got %q", row.Event)
	}
	if row.OrderID != "ord_500" {
		t.Fatalf("order id: got %q", row.OrderID)
	}
	if !strings.Contains(row.LastError, "500") {
		t.Fatalf("last error should name the status, got %q", row.LastError)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts: got %d", row.Attempts)
	}
	// The stored body must be the last thing that was actually POSTed: it is
	// what a redelivery re-signs, so it has to carry the final attempt number.
	var p Payload
	if err := json.Unmarshal(row.Body, &p); err != nil {
		t.Fatalf("stored body is not a payload: %v", err)
	}
	if p.Attempt != 3 || p.DeliveryID != "dlv-500" || p.Event != EventFulfilled {
		t.Fatalf("stored body is not the final attempt: %+v", p)
	}
	if wants := []string{"order.fulfilled/failed"}; !equalStrings(results, wants) {
		t.Fatalf("metrics results: got %v want %v", results, wants)
	}
}

// TestPermanentRejectionIsNotParked: a 4xx is an answer, not an outage. Parking
// it would put a permanently undeliverable event in the operator's queue.
func TestPermanentRejectionIsNotParked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	sink := newMemSink()
	var results []string
	s := fastSender(t, srv.URL, "s", 5)
	s.cfg.Metrics = func(event, result string) { results = append(results, result) }
	s.UseDeadLetter(sink, nil)

	err := s.Notify(context.Background(), EventExpired, "dlv-422", map[string]any{"id": "o"})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
	if sink.len() != 0 {
		t.Fatalf("a 4xx must not be parked, got %d rows", sink.len())
	}
	if len(results) != 1 || results[0] != "permanent" {
		t.Fatalf("metrics result: got %v", results)
	}
}

// TestSinkFailureIsReported: if parking itself fails, the caller must be able to
// see that the delivery is lost. Silently returning "failed" would be a lie.
func TestSinkFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	sink := newMemSink()
	sink.err = errors.New("disk full")
	s := fastSender(t, srv.URL, "s", 2)
	s.UseDeadLetter(sink, nil)

	err := s.Notify(context.Background(), EventFulfilled, "dlv-down", map[string]any{"id": "o"})
	var derr *DeliveryError
	if !errors.As(err, &derr) {
		t.Fatalf("want *DeliveryError, got %v", err)
	}
	if derr.DeadLettered {
		t.Fatal("sink failed, must not claim the delivery is parked")
	}
	if derr.SinkErr == nil || !strings.Contains(derr.SinkErr.Error(), "disk full") {
		t.Fatalf("sink error should be preserved, got %v", derr.SinkErr)
	}
	if !strings.Contains(err.Error(), "could not be dead-lettered") {
		t.Fatalf("error text should say the delivery was lost: %v", err)
	}
}

// TestSuccessIsCountedOnce: "delivered" is first attempt, "retried" is later,
// and a success never parks anything.
func TestSuccessIsCountedOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newMemSink()
	var results []string
	s := fastSender(t, srv.URL, "s", 4)
	s.cfg.Metrics = func(event, result string) { results = append(results, result) }
	s.UseDeadLetter(sink, nil)

	if err := s.Notify(context.Background(), EventFulfilled, "dlv-ok", map[string]any{"id": "o"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if sink.len() != 0 {
		t.Fatalf("success must not park, got %d rows", sink.len())
	}
	if len(results) != 1 || results[0] != "retried" {
		t.Fatalf("want one 'retried' result, got %v", results)
	}
}

// TestRedeliverReSignsStoredBody: the whole point of storing the bytes is that a
// retry produces a signature the receiver still accepts for the body it already
// has. This asserts byte-for-byte: same body content apart from the attempt
// counter, same delivery id, and a valid signature.
func TestRedeliverReSignsStoredBody(t *testing.T) {
	const secret = "redeliver-secret"
	var (
		mu       sync.Mutex
		gotBody  []byte
		gotSig   string
		gotID    string
		gotEvent string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotSig = r.Header.Get(HeaderSignature)
		gotID = r.Header.Get(HeaderDelivery)
		gotEvent = r.Header.Get(HeaderEvent)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender, err := New(Config{URL: srv.URL, Secret: secret, MaxAttempts: 1,
		Backoff: func(int) time.Duration { return time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	// What a dead-letter row from an earlier outage holds.
	original, _ := json.Marshal(Payload{Event: EventFulfilled, DeliveryID: "dlv-re", Order: json.RawMessage(`{"id":"ord_9","status":"fulfilled"}`), Attempt: 5})
	dl := &DeadLetter{DeliveryID: "dlv-re", Event: EventFulfilled, OrderID: "ord_9", Attempts: 5, Body: original}

	if err := sender.Redeliver(context.Background(), dl); err != nil {
		t.Fatalf("redeliver: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotID != "dlv-re" {
		t.Fatalf("delivery id must be stable across a redelivery, got %q", gotID)
	}
	if gotEvent != EventFulfilled {
		t.Fatalf("event header: got %q", gotEvent)
	}
	if gotSig != Sign(secret, gotBody) {
		t.Fatal("signature does not verify over the body that was sent")
	}
	var p Payload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("body is not a payload: %v", err)
	}
	if p.Attempt != 6 {
		t.Fatalf("attempt should advance to 6, got %d", p.Attempt)
	}
	if string(p.Order) != `{"id":"ord_9","status":"fulfilled"}` {
		t.Fatalf("order json changed: %s", p.Order)
	}
}

// TestRedeliverWithoutURLFails: a sender with no URL cannot redeliver, and must
// say so instead of pretending an event went out.
func TestRedeliverWithoutURLFails(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	dl := &DeadLetter{DeliveryID: "d", Event: EventFulfilled, Body: []byte(`{"event":"order.fulfilled","order":{"id":"x"}}`)}
	if err := s.Redeliver(context.Background(), dl); err == nil {
		t.Fatal("want an error when no outbound URL is configured")
	}
}

// TestZeroValueSenderIsSafe keeps the nil-path honest: a *Sender with no sink
// must behave exactly like before dead-lettering existed.
func TestZeroValueSenderIsSafe(t *testing.T) {
	s := fastSender(t, "http://127.0.0.1:1/nope", "s", 1)
	if s.HasDeadLetter() {
		t.Fatal("no sink installed")
	}
	err := s.Notify(context.Background(), EventFulfilled, "d", map[string]any{"id": "o"})
	var derr *DeliveryError
	if !errors.As(err, &derr) || derr.DeadLettered {
		t.Fatalf("want an un-parked DeliveryError, got %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
