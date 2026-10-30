package api_test

//
// The limiter's own bucket logic is unit-tested in ratelimit_test.go (package
// api, with a fake clock). These tests go through the real HTTP server
// instead, so they prove the middleware is wired into the route table and that
// the 429 body matches the documented error envelope.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/horizonbymuneeb/paisabridge/internal/api"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

func TestRateLimitWritePathReturns429(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 10; i++ {
		code, body := h.do("POST", "/api/orders", map[string]any{
			"product_id":          "netflix-1m",
			"catalog_price_minor": int64(100000 + i*1000),
			"currency":            "PKR",
			"method":              "easypaisa",
		}, nil)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i+1, code, body)
		}
	}

	req, err := http.NewRequest("POST", h.srv.URL+"/api/orders", strings.NewReader(`{"product_id":"x","catalog_price_minor":1000}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q, want a positive number of seconds", ra)
	}
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if e.Code != "rate_limited" || e.Error == "" {
		t.Fatalf("429 envelope = %+v, want code=rate_limited with a message", e)
	}
}

// TestRateLimitClassesAreIndependent proves a caller hammering one class does
// not lock itself out of another: a webhook flood must not stop the status
// endpoint a checkout page is polling.
func TestRateLimitClassesAreIndependent(t *testing.T) {
	h := newHarness(t, func(c *api.Config) {
		c.RateLimits = map[api.RateLimitClass]api.RateLimit{
			api.RateLimitWebhook: {PerMinute: 1, Burst: 1},
		}
	})
	ord := h.createOrder(30000)

	// Webhook bucket: one allowed, then refused. The second delivery is for an
	// amount that matches nothing, so the only thing refusing it is the limit.
	code, body := h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "RL-1", "amount_minor": ord.Amount + 1, "currency": "PKR",
	})
	if code != http.StatusOK {
		t.Fatalf("first webhook: %d %s", code, body)
	}
	code, body = h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "RL-2", "amount_minor": ord.Amount + 2, "currency": "PKR",
	})
	if code != http.StatusTooManyRequests {
		t.Fatalf("second webhook = %d %s, want 429 with a 1/min budget", code, body)
	}

	// The order-read class is untouched.
	code, body = h.do("GET", "/api/orders/"+ord.ID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET order after webhook flood = %d %s, want 200", code, body)
	}
}
