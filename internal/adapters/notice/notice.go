// Package notice implements the notice feed every rail adapter speaks:
// a signed, timestamped, replay-safe envelope carrying one payment fact.
//
// This is the transport half of the verification model. Every adapter in
// internal/adapters produces a Notice; the API layer verifies the envelope
// signature first and the rail-specific signature second (rails always win on
// detail, the envelope on transport). An unsigned inbound message has no
// timestamp, no replay protection and no idempotency key, so a replayed or
// forged copy is indistinguishable from a real payment; the envelope exists
// to give every notice all four properties before it reaches the matching
// engine.
//
// # Envelope
//
// A signed notice is a JSON object with four values chosen by the sender:
//
//	{"id": "...", "method": "bank", "issued_at": 1761700000, "payload": {...}}
//
// where payload is the rail normal form (see RailNotice). The transport
// signature is computed over the exact bytes inside payload:
//
//	HMAC-SHA256(secret, "v1." + id + "." + method + "." + issued_at + "." + payloadBytes)
//
// and travels in the X-Paisabridge-Signature header as "v1=<hex>".
//
// # Security properties
//
//   - the signature covers the id, the rail and the timestamp, not just the
//     payload, so an attacker cannot retarget a valid notice at a different
//     rail or replay it under a different id;
//   - issued_at is checked against the clock and rejected outside Window,
//     which bounds the value of a stolen notice;
//   - headers are limited (MaxHeaderBytes), body size is bounded
//     (MaxBodyBytes) and the JSON is decoded with UseNumber so a 64-bit
//     integer can never be silently rounded through float64;
//   - the Admitter seam provides the "seen once" guarantee the API layer
//     needs. The adapter cannot know about the store, so it calls back into
//     a caller-supplied admitter *after* signature and timestamp checks pass.
//
// # Assumptions
//
// The envelope is our own protocol, not a provider format: no payment
// provider publishes it. It is the contract between the notice feed
// (mail/SMS parser, Binance Pay webhook relay) and this gateway, and it is
// documented here precisely because it is invented rather than specified.
package notice

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// HeaderSignature carries "v1=<hex>".
const HeaderSignature = "X-Paisabridge-Signature"

// HeaderSignaturePlain carries the bare hex digest, for relays that cannot
// express the "v1=" prefix (some no-code HTTP clients).
const HeaderSignaturePlain = "X-Paisabridge-Digest"

const (
	// MaxBodyBytes caps the raw request body. A bank alert e-mail with a
	// quoted thread or a Binance Pay callback is well under 64 KiB.
	MaxBodyBytes = 1 << 16

	// MaxHeaderBytes caps a single header value accepted by clientHeader.
	MaxHeaderBytes = 8 << 10

	// MaxClockSkew is the default acceptance window either side of "now".
	// Five minutes covers a relay's normal delivery jitter after a mail or
	// SMS hiccup, and is small enough that a captured signature is stale
	// almost immediately.
	MaxClockSkew = 5 * time.Minute

	// schemeV1 is the only signature scheme accepted today. It is part of
	// the signed material, so introducing v2 cannot be confused with v1.
	schemeV1 = "v1"
)

// RailNotice is the rail normal form inside the envelope. Its field set is
// deliberately a superset of types.PaymentNotice: sender metadata and the
// provider's own reference are kept for audit, and the amount is carried in
// minor units exactly as the provider stated it.
type RailNotice struct {
	// TxnRef is the provider-side transaction reference. It is the
	// idempotency key at the rail; the envelope id is the idempotency key at
	// our boundary. Both are required.
	TxnRef string `json:"txn_ref"`
	// Amount is minor units at the currency's exponent: paisa for PKR,
	// micro-USDT for USDT. Never a float.
	Amount int64 `json:"amount_minor"`
	// Currency is the ISO code as the provider wrote it, upper-cased.
	Currency string `json:"currency"`
	// SenderName / SenderAccount are free text from the provider.
	SenderName    string `json:"sender_name,omitempty"`
	SenderAccount string `json:"sender_account,omitempty"`
	// ReceivedAt is the provider's own timestamp, unix seconds plus the zone
	// offset it was reported in. Stored as a string to keep the JSON exact.
	ReceivedAt string `json:"received_at,omitempty"`
	// Note is a short human-readable memo (bank narrative, Binance remark).
	Note string `json:"note,omitempty"`
}

// Envelope is a signed notice as it arrives on the wire.
type Envelope struct {
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	IssuedAt int64           `json:"issued_at"`
	Payload  json.RawMessage `json:"payload"`
}

type wireEnvelope struct {
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	IssuedAt json.Number     `json:"issued_at"`
	Payload  json.RawMessage `json:"payload"`
}

func (w *wireEnvelope) toEnvelope() (Envelope, error) {
	if w.ID == "" {
		return Envelope{}, errors.New("notice: envelope id is required")
	}
	if w.Method == "" {
		return Envelope{}, errors.New("notice: envelope method is required")
	}
	secs, err := strconv.ParseInt(w.IssuedAt.String(), 10, 64)
	if err != nil {
		return Envelope{}, fmt.Errorf("notice: envelope issued_at %q is not an integer: %w", w.IssuedAt, err)
	}
	if len(w.Payload) == 0 || !json.Valid(w.Payload) {
		return Envelope{}, errors.New("notice: envelope payload is missing or not JSON")
	}
	if trimmed := bytes.TrimSpace(w.Payload); len(trimmed) == 0 || trimmed[0] != '{' {
		return Envelope{}, errors.New("notice: envelope payload must be a JSON object")
	}
	// json.Valid accepts a complete value followed by nothing; re-check that
	// the payload is a single object and not an object with trailing junk.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(w.Payload, &probe); err != nil {
		return Envelope{}, fmt.Errorf("notice: envelope payload: %w", err)
	}
	return Envelope{ID: w.ID, Method: w.Method, IssuedAt: secs, Payload: w.Payload}, nil
}

// RailNotice decodes the payload of a verified envelope. The caller must
// have verified the envelope first; this function performs no authentication.
func (e Envelope) RailNotice() (*RailNotice, error) {
	return decodeRailNotice(e.Payload)
}

func decodeRailNotice(raw []byte) (*RailNotice, error) {
	rn := &RailNotice{}
	if err := decodeJSON(raw, rn); err != nil {
		return nil, fmt.Errorf("notice: payload: %w", err)
	}
	rn.TxnRef = strings.TrimSpace(rn.TxnRef)
	rn.Currency = strings.ToUpper(strings.TrimSpace(rn.Currency))
	rn.SenderName = strings.TrimSpace(rn.SenderName)
	rn.SenderAccount = strings.TrimSpace(rn.SenderAccount)
	if rn.TxnRef == "" {
		return nil, errors.New("notice: payload txn_ref is required")
	}
	if rn.Amount <= 0 {
		return nil, fmt.Errorf("notice: payload amount_minor must be positive, got %d", rn.Amount)
	}
	if rn.Currency == "" {
		return nil, errors.New("notice: payload currency is required")
	}
	return rn, nil
}

// ToPaymentNotice normalises a decoded rail notice into the shared type.
// receivedAt is used whenever the provider did not supply a parseable one,
// so a downstream matching decision always has a defensible timestamp.
func (r *RailNotice) ToPaymentNotice(m types.Method, raw []byte, receivedAt time.Time) *types.PaymentNotice {
	return &types.PaymentNotice{
		TxnRef:     r.TxnRef,
		Method:     m,
		Amount:     r.Amount,
		Currency:   r.Currency,
		SenderName: r.SenderName,
		SenderAcct: r.SenderAccount,
		ReceivedAt: receivedAt,
		Raw:        raw,
	}
}

// Sign returns the "v1=<hex>" value for an envelope. Exported so operators
// and tests can mint notices with the same code path that verifies them.
func Sign(secret string, id string, method string, issuedAt int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signingMaterial(schemeV1, id, method, issuedAt, payload))
	return schemeV1 + "=" + hex.EncodeToString(mac.Sum(nil))
}

func signingMaterial(scheme, id, method string, issuedAt int64, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(scheme)
	buf.WriteByte('.')
	buf.WriteString(id)
	buf.WriteByte('.')
	buf.WriteString(strings.ToLower(method))
	buf.WriteByte('.')
	buf.WriteString(strconv.FormatInt(issuedAt, 10))
	buf.WriteByte('.')
	buf.Write(payload)
	return buf.Bytes()
}

// Verifier checks envelope signatures and freshness.
type Verifier struct {
	// Secret is the shared HMAC key of the notice feed. It is never sent to
	// a provider; each provider gets its own rail verifier.
	Secret string
	// Window is the accepted absolute skew between issued_at and the clock.
	// Zero means MaxClockSkew.
	Window time.Duration
	// Clock is the time source. Nil means live PKT.
	Clock clock.Clock
}

func (v Verifier) now() time.Time {
	if v.Clock == nil {
		return clock.Live{}.Now()
	}
	return v.Clock.Now()
}

func (v Verifier) window() time.Duration {
	if v.Window <= 0 {
		return MaxClockSkew
	}
	return v.Window
}

// SignatureFromRequest extracts the transport signature, preferring the
// prefixed header and falling back to the bare digest header. It returns the
// bare hex digest with any "v1=" prefix removed.
func SignatureFromRequest(r *http.Request) (string, error) {
	for _, h := range []string{HeaderSignature, HeaderSignaturePlain} {
		raw := r.Header.Get(h)
		if raw == "" {
			continue
		}
		if len(raw) > MaxHeaderBytes {
			return "", fmt.Errorf("%w: signature header too large", ErrBadNotice)
		}
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, schemeV1+"=") {
			return strings.TrimPrefix(raw, schemeV1+"="), nil
		}
		return raw, nil
	}
	return "", nil
}

// ErrBadNotice wraps malformed-but-authentic-looking notices. It is
// distinct from adapter.ErrInvalidSignature so callers can tell "someone is
// forging" from "our relay is misconfigured". The adapters map both to
// ErrInvalidSignature on the public API; this error only appears in logs and
// in direct unit tests.
var ErrBadNotice = errors.New("notice: malformed envelope")

// ErrStale is returned when issued_at falls outside the acceptance window.
// It is a security rejection (replay or clock drift), never a soft warning.
var ErrStale = errors.New("notice: stale or future-dated envelope")

// ReplayGuard reports whether a notice id has been seen before. Implemented
// by the store layer; called only after the signature and the timestamp have
// passed, so an unauthenticated attacker can never write into the store.
type ReplayGuard interface {
	// Seen returns true when id was already consumed. Implementations must
	// be safe for concurrent use and must be durable: an in-memory map is
	// reset by a restart, which reopens the window.
	Seen(id string) bool
}

// ErrReplayed is returned for a notice id the ReplayGuard already consumed.
var ErrReplayed = errors.New("notice: replayed envelope id")

// Verify checks the transport signature, the freshness window and (when a
// guard is configured) replay state, in that order. It returns the decoded
// envelope on success.
func (v Verifier) Verify(r *http.Request, guard ReplayGuard) (*Envelope, error) {
	if v.Secret == "" {
		return nil, errors.New("notice: verifier secret is not configured")
	}
	if r == nil {
		return nil, ErrBadNotice
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("notice: read body: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrBadNotice, MaxBodyBytes)
	}
	env, err := Decode(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadNotice, err)
	}
	got, err := SignatureFromRequest(r)
	if err != nil {
		return nil, err
	}
	if got == "" {
		return nil, ErrBadNotice
	}
	want := strings.ToLower(strings.TrimPrefix(Sign(v.Secret, env.ID, env.Method, env.IssuedAt, env.Payload), schemeV1+"="))
	gotDigest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(got), schemeV1+"="))
	if !hmac.Equal([]byte(want), []byte(gotDigest)) {
		// Constant-time compare of hex strings: both sides are the same
		// length when the signature is well-formed, and hmac.Equal does not
		// short-circuit on the first differing byte.
		return nil, fmt.Errorf("%w: transport signature mismatch", ErrBadNotice)
	}
	now := v.now()
	issued := time.Unix(env.IssuedAt, 0)
	if d := now.Sub(issued); d > v.window() || d < -v.window() {
		return nil, fmt.Errorf("%w: issued_at %s, now %s, window %s",
			ErrStale, issued.Format(time.RFC3339), now.Format(time.RFC3339), v.window())
	}
	if guard != nil && guard.Seen(env.ID) {
		return nil, ErrReplayed
	}
	return env, nil
}

// Decode reads an envelope without verifying it. It exists for tests and
// for tooling that mints notices; production code must call Verifier.Verify.
func Decode(r io.Reader) (*Envelope, error) {
	var wire wireEnvelope
	dec := json.NewDecoder(io.LimitReader(r, MaxBodyBytes))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	// A second token must be EOF: trailing bytes mean a relay mangled the
	// body, and guessing which half is authoritative is not acceptable for a
	// payment fact.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("decode envelope: trailing data after the JSON object")
	}
	env, err := wire.toEnvelope()
	if err != nil {
		return nil, err
	}
	return &env, nil
}

// Encode mints a signed envelope. Used by tests, by the operator CLI that
// fabricates test notices, and by the notice relay when it re-signs.
func Encode(secret, id string, method types.Method, issuedAt time.Time, rn RailNotice) ([]byte, string, error) {
	payload, err := json.Marshal(rn)
	if err != nil {
		return nil, "", fmt.Errorf("notice: encode payload: %w", err)
	}
	secs := issuedAt.Unix()
	out, err := json.Marshal(Envelope{
		ID:       id,
		Method:   string(method),
		IssuedAt: secs,
		Payload:  payload,
	})
	if err != nil {
		return nil, "", fmt.Errorf("notice: encode envelope: %w", err)
	}
	return out, Sign(secret, id, string(method), secs, payload), nil
}

// decodeJSON decodes with UseNumber and rejects unknown fields so a typo in
// a relay template ("amount" instead of "amount_minor") fails loudly
// instead of arriving as a zero amount.
func decodeJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// A second decode must hit EOF: trailing garbage means a relay mangled
	// the body and we should not guess which half is authoritative.
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}
