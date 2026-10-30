package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastSender(t *testing.T, url, secret string, maxAttempts int) *Sender {
	t.Helper()
	s, err := New(Config{
		URL: url, Secret: secret, MaxAttempts: maxAttempts,
		Backoff: func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

// TestDeliveryIsSignedAndIdempotent pins the two things a receiver relies on:
// the signature covers the exact body, and retries reuse the delivery id.
func TestDeliveryIsSignedAndIdempotent(t *testing.T) {
	const secret = "s3cret"
	var (
		calls  int32
		ids    []string
		sigs   []string
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		ids = append(ids, r.Header.Get(HeaderDelivery))
		sigs = append(sigs, r.Header.Get(HeaderSignature))
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		if n < 3 { // fail twice, then accept
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := fastSender(t, srv.URL, secret, 5)
	if err := s.Notify(context.Background(), EventFulfilled, "dlv-1", map[string]any{"id": "ord_1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts, got %d", calls)
	}
	for i, id := range ids {
		if id != "dlv-1" {
			t.Fatalf("attempt %d used delivery id %q (must be stable)", i, id)
		}
		if sigs[i] != Sign(secret, bodies[i]) {
			t.Fatalf("attempt %d signature does not verify over the body", i)
		}
	}
	var p Payload
	if err := json.Unmarshal(bodies[len(bodies)-1], &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != EventFulfilled || p.DeliveryID != "dlv-1" {
		t.Fatalf("payload wrong: %+v", p)
	}
}

// TestPermanentRejectionIsNotRetried: a 4xx means the receiver will never
// accept it, so retrying would spam the integrator.
func TestPermanentRejectionIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s := fastSender(t, srv.URL, "s", 5)
	err := s.Notify(context.Background(), EventExpired, "d2", map[string]any{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("4xx must not be retried, got %d calls", calls)
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s := fastSender(t, srv.URL, "s", 3)
	if err := s.Notify(context.Background(), EventCreated, "d3", map[string]any{}); err != nil {
		t.Fatalf("429 must retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestDisabledSenderIsNoop(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled() {
		t.Fatal("empty URL must be disabled")
	}
	if err := s.Notify(context.Background(), EventCreated, "x", nil); err != nil {
		t.Fatalf("disabled sender must not error: %v", err)
	}
}

func TestURLWithoutSecretRejected(t *testing.T) {
	if _, err := New(Config{URL: "https://example.test/h"}); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("want secret requirement, got %v", err)
	}
}
