package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// TestBotLongPollingRoundTrip drives the real polling loop against a fake
// Telegram Bot API, the way it runs in production: getUpdates returns one
// message, the bot answers with sendMessage, then the context is cancelled.
// This is the test that would catch a Run() wiring bug that unit-testing
// handle() alone cannot.
func TestBotLongPollingRoundTrip(t *testing.T) {
	var (
		mu    sync.Mutex
		sent  []string
		polls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n == 1 {
				_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":7,"message":{"message_id":1,"text":"/order PB-ABC123","chat":{"id":99,"type":"private"}}}]}`))
				return
			}
			// Later polls: hold the connection open like real long polling.
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			mu.Lock()
			sent = append(sent, r.Form.Get("text"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	st := &fakeStore{orders: map[string]*types.Order{
		"PB-ABC123": {Ref: "PB-ABC123", ProductName: "ChatGPT Plus", Amount: 149700, Currency: "PKR", Status: types.StatusFulfilled},
	}}
	b, err := New(Config{Token: "T", APIBase: srv.URL, CheckoutURL: "https://shop.test/checkout", PollTimeout: 300 * time.Millisecond}, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(sent)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("bot never replied; polling loop did not deliver the update")
	}
	if !strings.Contains(sent[0], "fulfilled") || !strings.Contains(sent[0], "149700") {
		t.Fatalf("unexpected reply: %q", sent[0])
	}
}

// TestRunStopsOnContextCancel: Run must return when the context is cancelled,
// otherwise graceful shutdown hangs.
func TestRunStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
	}))
	defer srv.Close()
	b, _ := New(Config{Token: "T", APIBase: srv.URL, PollTimeout: 200 * time.Millisecond}, &fakeStore{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}
