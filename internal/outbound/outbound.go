// Package outbound forwards order state changes to the integrator's own site.
//
// The gateway already answers the *provider* side (inbound webhooks). This is
// the *merchant* side: when an order becomes fulfilled, reviewed or expired,
// the storefront that created it needs to know. Polling /api/orders works but
// does not scale to a checkout, so every transition is also pushed.
//
// Delivery contract:
//
//	POST <url>
//	X-PB-Event: order.fulfilled
//	X-PB-Signature: hex(HMAC-SHA256(secret, rawBody))
//	X-PB-Delivery: <unique delivery id>
//
// A receiver must verify the signature over the exact bytes it received and
// treat the delivery id as an idempotency key. Retries reuse the same id so a
// lost acknowledgement can never create two fulfilments downstream.
package outbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event names.
const (
	EventCreated   = "order.created"
	EventFulfilled = "order.fulfilled"
	EventReview    = "order.review"
	EventExpired   = "order.expired"
	EventCancelled = "order.cancelled"
	EventVerified  = "order.verified"
)

// Headers set on every delivery.
const (
	HeaderSignature = "X-PB-Signature"
	HeaderEvent     = "X-PB-Event"
	HeaderDelivery  = "X-PB-Delivery"
)

// ErrPermanent marks a response the sender must not retry (4xx).
var ErrPermanent = errors.New("outbound: permanent rejection")

// Payload is the body posted to the integrator.
type Payload struct {
	Event      string          `json:"event"`
	DeliveryID string          `json:"delivery_id"`
	Order      json.RawMessage `json:"order"`
	Attempt    int             `json:"attempt"`
}

// Config configures a sender.
type Config struct {
	URL        string
	Secret     string
	HTTPClient *http.Client
	// MaxAttempts defaults to 5.
	MaxAttempts int
	// Backoff is the delay before attempt n+1; defaults to 1s,2s,4s,8s,16s.
	Backoff func(attempt int) time.Duration
	// Logf receives one line per attempt. Nil silences it.
	Logf func(format string, args ...any)
}

// Sender delivers events with bounded exponential backoff.
type Sender struct {
	cfg    Config
	client *http.Client
}

// New validates the config. A Sender with an empty URL is a no-op sender so
// callers never need a nil check.
func New(cfg Config) (*Sender, error) {
	if cfg.URL == "" {
		return &Sender{cfg: cfg}, nil
	}
	if cfg.Secret == "" {
		return nil, errors.New("outbound: secret is required when a URL is set")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(attempt int) time.Duration {
			return time.Duration(1<<uint(attempt-1)) * time.Second
		}
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &Sender{cfg: cfg, client: c}, nil
}

// Enabled reports whether deliveries are configured.
func (s *Sender) Enabled() bool { return s.cfg.URL != "" }

// Sign computes the header value for a body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Notify delivers one order event. The order value is marshalled by the
// caller-supplied encoder so the gateway does not depend on the store's types.
func (s *Sender) Notify(ctx context.Context, event, deliveryID string, order any) error {
	if !s.Enabled() {
		return nil
	}
	raw, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("outbound: marshal order: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		body, err := json.Marshal(Payload{Event: event, DeliveryID: deliveryID, Order: raw, Attempt: attempt})
		if err != nil {
			return fmt.Errorf("outbound: marshal payload: %w", err)
		}
		lastErr = s.deliver(ctx, event, deliveryID, body, attempt)
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, ErrPermanent) {
			return lastErr
		}
		if attempt == s.cfg.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.Backoff(attempt)):
		}
	}
	return fmt.Errorf("outbound: giving up after %d attempts: %w", s.cfg.MaxAttempts, lastErr)
}

func (s *Sender) deliver(ctx context.Context, event, deliveryID string, body []byte, attempt int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("outbound: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderSignature, Sign(s.cfg.Secret, body))

	resp, err := s.client.Do(req)
	if err != nil {
		if s.cfg.Logf != nil {
			s.cfg.Logf("outbound %s delivery=%s attempt=%d network error: %v", event, deliveryID, attempt, err)
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if s.cfg.Logf != nil {
		s.cfg.Logf("outbound %s delivery=%s attempt=%d status=%d", event, deliveryID, attempt, resp.StatusCode)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrPermanent, resp.StatusCode)
	default:
		return fmt.Errorf("outbound: status %d", resp.StatusCode)
	}
}
