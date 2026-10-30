package outbound

// Dead-letter support.
//
// The sender retries a failed delivery with bounded backoff. When the last
// retry fails there are exactly two honest outcomes: the delivery is parked
// somewhere durable, or the storefront never learns the order happened. This
// file implements the first one. Everything a caller needs is a Sink — an
// interface, so internal/outbound still does not import internal/store and can
// be tested without a database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DeadLetter is one parked delivery. It carries the exact bytes that were
// signed, so a retry re-signs the same body and the receiver's copy of the
// signature still matches.
type DeadLetter struct {
	// DeliveryID is the X-PB-Delivery value: the receiver's idempotency key.
	DeliveryID string
	Event      string
	OrderID    string
	// LastError is the final transport or status error, in prose.
	LastError string
	// Attempts is how many sends failed before the delivery was parked.
	Attempts int
	// Body is the last JSON body POSTed (already containing the attempt
	// number, so it is the body to re-send, not a template).
	Body []byte
	// AttemptedAt is when the final attempt failed (zero = "now").
	AttemptedAt time.Time
}

// Sink persists parked deliveries. Store implements it with a SQLite table
// (store.PutDeadLetter); a test may implement it with a slice.
//
// Put must be idempotent on DeliveryID: the same delivery failing twice parks
// once, with the newest error.
type Sink interface {
	Put(ctx context.Context, dl *DeadLetter) error
}

// DeadLetterCodec encodes bodies for storage. The default JSONCodec stores the
// body verbatim, which is what a retry needs; a deployment that also encrypts
// at rest can plug in its own (see docs/OPERATIONS.md, "Outbound deliveries").
type DeadLetterCodec interface {
	Encode(body []byte) (stored string, err error)
	Decode(stored string) (body []byte, err error)
}

// JSONCodec keeps the body as-is. It is the default because the body is the
// signed JSON a receiver already received over the network; re-serialising or
// wrapping it would make the stored bytes useless for a retry.
type JSONCodec struct{}

// Encode implements DeadLetterCodec.
func (JSONCodec) Encode(body []byte) (string, error) { return string(body), nil }

// Decode implements DeadLetterCodec.
func (JSONCodec) Decode(stored string) ([]byte, error) { return []byte(stored), nil }

// UseDeadLetter installs a sink on an already-built sender. New() takes no
// options (its signature is part of the package's published surface), so the
// wiring in main.go does New(...) then UseDeadLetter(...). A nil sink is a
// no-op: a sender without a sink keeps the old log-only behaviour, which is
// what every existing test expects.
func (s *Sender) UseDeadLetter(sink Sink, codec DeadLetterCodec) {
	if s == nil || sink == nil {
		return
	}
	s.sink = sink
	if codec == nil {
		s.codec = JSONCodec{}
	} else {
		s.codec = codec
	}
}

// HasDeadLetter reports whether a sink is installed (used by main.go to log
// which delivery mode is active at boot).
func (s *Sender) HasDeadLetter() bool { return s != nil && s.sink != nil }

// DeliveryError describes a delivery that was abandoned. It is returned by
// Notify after the sink call, so errors.As can tell a caller whether the event
// is parked (DeadLettered) or the parking itself failed (SinkErr).
type DeliveryError struct {
	Event      string
	DeliveryID string
	Attempts   int
	// Err is the final attempt's error.
	Err error
	// DeadLettered is true when the sink accepted the record.
	DeadLettered bool
	// SinkErr is non-nil when parking failed; the caller must log it, because
	// a delivery that is neither delivered nor parked is lost.
	SinkErr error
}

func (e *DeliveryError) Error() string {
	if e.SinkErr != nil {
		return fmt.Sprintf("outbound: %s delivery=%s abandoned after %d attempts (%v) and could not be dead-lettered: %v",
			e.Event, e.DeliveryID, e.Attempts, e.Err, e.SinkErr)
	}
	if e.DeadLettered {
		return fmt.Sprintf("outbound: %s delivery=%s abandoned after %d attempts (%v); dead-lettered",
			e.Event, e.DeliveryID, e.Attempts, e.Err)
	}
	return fmt.Sprintf("outbound: %s delivery=%s abandoned after %d attempts: %v",
		e.Event, e.DeliveryID, e.Attempts, e.Err)
}

// Unwrap exposes the final attempt's error (ErrPermanent survives, so callers
// can still tell "the receiver refused this" from "the receiver was down").
func (e *DeliveryError) Unwrap() error { return e.Err }

// park writes the failed delivery to the sink. It is called with the body of
// the final attempt, which is the body the receiver last saw.
func (s *Sender) park(ctx context.Context, event, deliveryID string, body []byte, attempts int, cause error) error {
	if s.sink == nil {
		return nil
	}
	stored, err := s.codec.Encode(body)
	if err != nil {
		return fmt.Errorf("outbound: encode dead letter: %w", err)
	}
	dl := &DeadLetter{
		DeliveryID: deliveryID,
		Event:      event,
		OrderID:    orderIDFrom(body),
		LastError:  truncate(cause.Error(), 1024),
		Attempts:   attempts,
		Body:       []byte(stored),
	}
	// A dead-letter write must not hang the caller: the caller is a background
	// goroutine in the API, but a wedged database must still not pin it.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.sink.Put(wctx, dl); err != nil {
		return err
	}
	if s.cfg.Logf != nil {
		s.cfg.Logf("outbound %s delivery=%s parked in dead letter after %d attempts: %v",
			event, deliveryID, attempts, cause)
	}
	return nil
}

// Redeliver re-sends a parked delivery from its stored body. It re-signs the
// body (the signature covers the bytes, so the stored copy is enough), uses the
// original delivery id, and makes exactly one attempt: a retry from the admin
// endpoint is an operator action, not a retry loop, and the operator must see
// the receiver's real answer immediately.
//
// The backoff/attempt budget is deliberately not reused: parking a delivery a
// second time from a manual retry would hide the fact that the receiver is
// still down behind another wait.
func (s *Sender) Redeliver(ctx context.Context, dl *DeadLetter) error {
	if s == nil || s.cfg.URL == "" {
		return errors.New("outbound: no outbound URL configured; cannot redeliver")
	}
	if dl == nil {
		return errors.New("outbound: nil dead letter")
	}
	if len(dl.Body) == 0 {
		return errors.New("outbound: dead letter has no body")
	}
	if dl.DeliveryID == "" {
		return errors.New("outbound: dead letter has no delivery id")
	}
	attempt := dl.Attempts + 1
	body := dl.Body
	if _, ok := s.reusableBody(body); !ok {
		// The stored body is not the payload shape we sign (a hand-edited
		// row, or a body from an older version). Rebuild the envelope around
		// whatever object it holds rather than sending something unparseable.
		var probe struct {
			Event string          `json:"event"`
			Order json.RawMessage `json:"order"`
		}
		_ = json.Unmarshal(body, &probe)
		if len(probe.Order) == 0 {
			return fmt.Errorf("outbound: dead letter body is not a delivery payload")
		}
		var err error
		body, err = json.Marshal(Payload{Event: dl.Event, DeliveryID: dl.DeliveryID, Order: probe.Order, Attempt: attempt})
		if err != nil {
			return fmt.Errorf("outbound: rebuild payload: %w", err)
		}
	} else {
		// Bump the attempt counter in the stored envelope so the receiver can
		// see this is a redelivery, keeping every other byte identical.
		var p Payload
		_ = json.Unmarshal(body, &p)
		p.Attempt = attempt
		body, _ = json.Marshal(p)
	}
	err := s.deliver(ctx, dl.Event, dl.DeliveryID, body, attempt)
	if err != nil && s.cfg.Logf != nil {
		s.cfg.Logf("outbound redeliver %s delivery=%s attempt=%d: %v", dl.Event, dl.DeliveryID, attempt, err)
	}
	return err
}

// reusableBody reports whether the stored bytes decode as a Delivery payload
// (the shape deliver() signs).
func (s *Sender) reusableBody(body []byte) ([]byte, bool) {
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false
	}
	if p.Event == "" && len(p.Order) == 0 {
		return nil, false
	}
	return body, true
}

// orderIDFrom pulls the order id out of a payload body for the denormalised
// order_id column. A body that does not parse yields "" — the row is still
// recorded, because losing a failure because its body was odd is worse.
func orderIDFrom(body []byte) string {
	var p struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	return p.Order.ID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
