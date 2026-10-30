// Package sadapay implements SadaPay's payment-request callback adapter.
//
// # SadaPay's integration shape
//
// SadaPay's public merchant documentation centres on a payment request that
// is created or refreshed with an API key and returns a hosted payment link
// plus, on completion, a 6-digit confirmation code shown to the payer.
// SadaPay's domestic card/wallet rails deliver money to the merchant account
// and expose no published callback signing scheme (unlike JazzCash's
// pp_SecureHash or Easypaisa's AES hash), so a third-party gateway cannot
// verify a SadaPay callback from the payload alone.
//
// # What this adapter verifies
//
// Two independent facts:
//
//  1. the transport: the S2S callback relay we operate signs the exact body
//     with HMAC-SHA256 and sends X-Sadapay-Signature (hex, optional "sha256="
//     prefix). This authenticates the relay, not SadaPay;
//  2. the rail: a confirmation code (SadaPay's 6-digit reference) plus the
//     provider-side payment id. The notice carries both, and the matching
//     engine refuses to fulfil an order whose expected confirmation code does
//     not equal the one in a signed callback.
//
// # Assumptions (documented, never silently faked)
//
//  1. SadaPay does not publish a webhook signature that can be reproduced
//     offline; the HMAC above is *our* protocol between relay and gateway.
//     It is stated explicitly so it is never mistaken for a SadaPay feature.
//  2. Amounts arrive as a decimal string in PKR ("1497.00") and are parsed
//     without floating point (provider.ParseAmount).
//  3. A status API path is not implemented: SadaPay's API key flow is a
//     payment-request API, not a "give me the status of txn X" API, so
//     VerifyAPI returns adapter.ErrUnsupported rather than guessing an
//     endpoint.
//
// # Endpoints
//
// None embedded. BaseURL defaults to "" - TODO(provider-credentials).
package sadapay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	signatureHeader = "X-Sadapay-Signature"
	maxBodyBytes    = 1 << 16
)

var (
	// ErrNotSuccessful is returned for an authenticated non-completion.
	ErrNotSuccessful = errors.New("sadapay: payment is not completed")
	// ErrReplayed is returned for a consumed payment id.
	ErrReplayed = errors.New("sadapay: payment already processed")
	// ErrNotConfigured is returned when the relay secret is missing.
	ErrNotConfigured = errors.New("sadapay: webhook secret is not configured")
	// ErrBadConfirmationCode is returned when the code is not the 6 digits
	// SadaPay shows to the payer.
	ErrBadConfirmationCode = errors.New("sadapay: confirmation code is not 6 digits")
)

// Adapter implements adapter.PaymentAdapter for SadaPay.
type Adapter struct {
	// WebhookSecret authenticates our relay (assumption 1).
	WebhookSecret string

	// BaseURL is reserved for the payment-request API. Empty by default.
	// TODO(provider-credentials)
	BaseURL string

	// Clock supplies now. Nil means live.
	Clock clock.Clock

	// MaxSkew bounds how old completed_at may be. Zero means 10 minutes.
	MaxSkew time.Duration

	// Guard holds consumed payment ids.
	Guard ReplayGuard

	// RequireGuard turns a nil Guard into a configuration error.
	RequireGuard bool
}

// ReplayGuard records consumed payment ids.
type ReplayGuard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodSadaPay }

// Supports implements adapter.PaymentAdapter.
func (a *Adapter) Supports() bool { return a.WebhookSecret != "" }

func (a *Adapter) now() time.Time {
	if a.Clock == nil {
		return clock.Live{}.Now()
	}
	return a.Clock.Now()
}

func (a *Adapter) maxSkew() time.Duration {
	if a.MaxSkew <= 0 {
		return 10 * time.Minute
	}
	return a.MaxSkew
}

type callback struct {
	PaymentID        string `json:"payment_id"`
	ConfirmationCode string `json:"confirmation_code"`
	Status           string `json:"status"`
	Amount           string `json:"amount"`
	Currency         string `json:"currency"`
	PayerName        string `json:"payer_name,omitempty"`
	PayerPhone       string `json:"payer_phone,omitempty"`
	CompletedAt      int64  `json:"completed_at,omitempty"`
}

// VerifyWebhook implements adapter.PaymentAdapter.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.WebhookSecret == "" {
		return nil, ErrNotConfigured
	}
	sig, err := provider.Header(r.Header, signatureHeader)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	sig = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(sig)), "sha256=")
	body, err := provider.Body(r, maxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	if !provider.EqualHex(sig, provider.HMACSHA256Hex(a.WebhookSecret, string(body))) {
		return nil, fmt.Errorf("%w: %s does not match body", adapter.ErrInvalidSignature, signatureHeader)
	}

	var cb callback
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cb); err != nil {
		return nil, fmt.Errorf("%w: body: %v", provider.ErrMalformed, err)
	}
	cb.PaymentID = strings.TrimSpace(cb.PaymentID)
	if cb.PaymentID == "" {
		return nil, fmt.Errorf("%w: payment_id missing", provider.ErrMalformed)
	}
	if s := strings.ToUpper(strings.TrimSpace(cb.Status)); s != "COMPLETED" && s != "SUCCESS" && s != "PAID" {
		return nil, fmt.Errorf("%w: status %q", ErrNotSuccessful, cb.Status)
	}
	code := strings.TrimSpace(cb.ConfirmationCode)
	if !isSixDigits(code) {
		return nil, fmt.Errorf("%w: %q", ErrBadConfirmationCode, code)
	}
	currency := strings.ToUpper(strings.TrimSpace(cb.Currency))
	if currency == "" {
		currency = "PKR"
	}
	minor, err := provider.ParseAmount(cb.Amount, provider.Exponent(currency))
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: amount %q", provider.ErrMalformed, cb.Amount)
	}
	received := a.now()
	if cb.CompletedAt != 0 {
		t, err := provider.ParseUnixSeconds(cb.CompletedAt)
		if err != nil {
			return nil, fmt.Errorf("%w: completed_at", provider.ErrMalformed)
		}
		if d := a.now().Sub(t); d > a.maxSkew() || d < -a.maxSkew() {
			return nil, fmt.Errorf("%w: completed_at %s outside %s of now",
				adapter.ErrInvalidSignature, t.Format(time.RFC3339), a.maxSkew())
		}
		received = t
	}
	if a.RequireGuard && a.Guard == nil {
		return nil, errors.New("sadapay: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(cb.PaymentID) {
		return nil, fmt.Errorf("%w: payment_id %s", ErrReplayed, cb.PaymentID)
	}
	return &types.PaymentNotice{
		TxnRef:     cb.PaymentID + ":" + code,
		Method:     types.MethodSadaPay,
		Amount:     minor,
		Currency:   currency,
		SenderName: cb.PayerName,
		SenderAcct: cb.PayerPhone,
		ReceivedAt: received,
		Raw:        body,
	}, nil
}

// ConfirmationCodeOf extracts the code from a TxnRef produced by this
// adapter. The matching engine uses it to compare against the order's
// expected code.
func ConfirmationCodeOf(txnRef string) (code string, ok bool) {
	_, code, ok = strings.Cut(txnRef, ":")
	if !ok || !isSixDigits(code) {
		return "", false
	}
	return code, true
}

func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// VerifyAPI implements adapter.PaymentAdapter. See assumption 3.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// Sign produces the relay signature for a body.
func Sign(secret string, body []byte) string {
	return provider.HMACSHA256Hex(secret, string(body))
}

// NewMemGuard returns an in-memory replay guard for tests.
func NewMemGuard() ReplayGuard { return &memGuard{seen: map[string]bool{}} }

type memGuard struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (g *memGuard) Seen(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen[id]
}

func (g *memGuard) Remember(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen[id] {
		return false
	}
	g.seen[id] = true
	return true
}
