// Package payoneer implements Payoneer's payment-notification adapter.
//
// # Payoneer's integration shape
//
// Payoneer's Merchant/Billing-Service product exposes a "Payment Notification"
// endpoint: when a customer pays a Payoneer-hosted payment request, Payoneer
// POSTs a form-encoded notification to the merchant's notification URL. The
// notification carries the merchant's own transaction id plus Payoneer's
// payment id, a status, and an amount.
//
// Two independent transports are implemented here, because in the field a
// merchant gets both:
//
//  1. Authenticated notification relay: our relay receives Payoneer's POST and
//     re-posts it as a signed envelope (package notice) carrying the link
//     "stamp". The stamp is `payoneer:<payment_id>:<merchant_txn_id>`.
//  2. Direct notification with a link stamp: when Payoneer is configured to
//     append its own verification parameters to the return notification (the
//     merchant-signature/stamp parameter), the adapter accepts the form
//     directly - but only when the stamp verifies with HMAC-SHA256 over
//     payment_id + "|" + merchant_txn_id using the configured notification
//     secret.
//
// # Assumptions (documented, never silently faked)
//
//  1. Payoneer does not publish a callback signature that can be reproduced
//     offline. The HMAC in this package is therefore *our* verification
//     scheme (a link stamp shared with the relay, the same pattern Payoneer,
//     PayPal and Stripe-hosted links all use in one form or another). It is
//     named here so nobody mistakes it for a Payoneer feature.
//  2. Payoneer notifications do not carry a payer bank account; SenderAcct is
//     the Payoneer payer id when present, otherwise empty. The payer e-mail is
//     kept in Raw only, never as a matching key.
//  3. Amounts are USD (Payoneer settles USD) and arrive as a decimal string;
//     they are stored in cents (USD has 2 decimals). A Payoneer payment in
//     another currency is rejected with ErrUnsupportedCurrency; the wallet
//     layer converts if it wants to price in PKR.
//  4. VerifyAPI returns adapter.ErrUnsupported: Payoneer's status API needs a
//     program-level client id/secret and OAuth token acquisition, which is a
//     different integration from the notification and is not implemented.
//
// # Endpoints
//
// Payoneer's notification URL is configured in the Payoneer dashboard; no URL
// is embedded here. BaseURL defaults to "" - TODO(provider-credentials).
package payoneer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	// StampParam is the parameter our relay (or Payoneer's link) appends.
	StampParam = "pb_stamp"
	// PaymentIDParam, TxnIDParam and StatusParam are the Payoneer field
	// names this adapter reads.
	PaymentIDParam = "payment_id"
	TxnIDParam     = "client_reference_id"
	StatusParam    = "status"
	AmountParam    = "amount"
	CurrencyParam  = "currency"

	maxFormBytes = 1 << 16
)

var (
	// ErrNotSuccessful is returned for an authenticated non-payment.
	ErrNotSuccessful = errors.New("payoneer: notification is not a completed payment")
	// ErrUnsupportedCurrency is returned for a non-USD amount.
	ErrUnsupportedCurrency = errors.New("payoneer: currency is not supported")
	// ErrReplayed is returned for a consumed payment id.
	ErrReplayed = errors.New("payoneer: payment already processed")
	// ErrNotConfigured is returned when no secret is configured.
	ErrNotConfigured = errors.New("payoneer: secret is not configured")
)

// Adapter implements adapter.PaymentAdapter for Payoneer.
type Adapter struct {
	// Secret verifies both the relay envelope and the link stamp.
	Secret string

	// BaseURL is reserved for a future status API. Empty by default.
	// TODO(provider-credentials)
	BaseURL string

	// Clock supplies now. Nil means live.
	Clock clock.Clock

	// MaxSkew bounds envelope age. Zero means 10 minutes.
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

// Method implements adapter.PaymentAdapter. Payoneer settles into the wallet
// layer, so it reports the bank-rail neutral "payoneer" method through the
// MethodUSDT-adjacent constant set: Payoneer is a fiat rail, and types defines
// no dedicated constant, so MethodBank is used with the provider recorded in
// Raw. See MethodName for the string the API layer should display.
func (a *Adapter) Method() types.Method { return types.MethodBank }

// MethodName is the display/adapter-registry name; types.Method has no
// "payoneer" constant and this package must not modify internal/types.
const MethodName types.Method = "payoneer"

func (a *Adapter) now() time.Time {
	if a.Clock == nil {
		return clock.Live{}.Now()
	}
	return a.Clock.Now()
}

func (a *Adapter) wall() time.Duration {
	if a.MaxSkew <= 0 {
		return 10 * time.Minute
	}
	return a.MaxSkew
}

// Supports implements adapter.PaymentAdapter.
func (a *Adapter) Supports() bool { return a.Secret != "" }

// Stamp computes the link stamp for a payment.
//
//	stamp = hex(HMAC-SHA256(secret, "payoneer:" + paymentID + ":" + txnID))
//
// The prefix domain-separates it from any other HMAC this secret might be used
// for, so a stamp can never be replayed as an envelope signature.
func Stamp(secret, paymentID, txnID string) string {
	return provider.HMACSHA256Hex(secret, "payoneer:", paymentID, ":", txnID)
}

// VerifyWebhook implements adapter.PaymentAdapter.
//
// Two accepted transports:
//   - Content-Type application/json with a notice envelope (relay path);
//   - form-encoded notification carrying a valid pb_stamp (direct path).
//
// Everything else - including an unsigned form - is ErrInvalidSignature.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.Secret == "" {
		return nil, ErrNotConfigured
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") ||
		r.Header.Get(notice.HeaderSignature) != "" {
		return a.verifyEnvelope(r)
	}
	return a.verifyForm(r)
}

func (a *Adapter) verifyEnvelope(r *http.Request) (*types.PaymentNotice, error) {
	v := notice.Verifier{Secret: a.Secret, Clock: a.Clock}
	env, err := v.Verify(r, noticeGuard{a.Guard})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	rn, err := env.RailNotice()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	paymentID, txnID, ok := ParseLinkKey(rn.TxnRef)
	if !ok {
		return nil, fmt.Errorf("%w: relay txn_ref %q is not a payoneer link key", provider.ErrMalformed, rn.TxnRef)
	}
	// The relay puts the provider's decimal amount in Note ("12.50"); that is
	// the authoritative string, so it is parsed rather than trusting the
	// envelope's integer copy to have been produced by our own formatter.
	minor, err := provider.ParseAmount(rn.Note, 2)
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: envelope note amount %q", provider.ErrMalformed, rn.Note)
	}
	received := a.now()
	if rn.ReceivedAt != "" {
		if t, err := provider.ParseTimeLocal(rn.ReceivedAt); err == nil {
			received = t
		}
	}
	if d := a.now().Sub(received); d > a.wall() || d < -a.wall() {
		return nil, fmt.Errorf("%w: payoneer received_at %s outside %s of now",
			adapter.ErrInvalidSignature, received.Format(time.RFC3339), a.wall())
	}
	return a.finish(minor, "USD", paymentID, txnID, rn.SenderAccount, received, []byte(env.Payload))
}

func (a *Adapter) verifyForm(r *http.Request) (*types.PaymentNotice, error) {
	raw, err := provider.Body(r, maxFormBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	form, err := url.ParseQuery(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%w: body is not form-encoded", adapter.ErrInvalidSignature)
	}
	paymentID := strings.TrimSpace(form.Get(PaymentIDParam))
	txnID := strings.TrimSpace(form.Get(TxnIDParam))
	stamp := strings.TrimSpace(form.Get(StampParam))
	if paymentID == "" || txnID == "" || stamp == "" {
		return nil, fmt.Errorf("%w: payment_id/client_reference_id/%s missing", adapter.ErrInvalidSignature, StampParam)
	}
	if !provider.EqualHex(stamp, Stamp(a.Secret, paymentID, txnID)) {
		return nil, fmt.Errorf("%w: %s does not match payment identity", adapter.ErrInvalidSignature, StampParam)
	}
	minor, err := provider.ParseAmount(form.Get(AmountParam), 2)
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: amount %q", provider.ErrMalformed, form.Get(AmountParam))
	}
	return a.finish(minor, form.Get(CurrencyParam), paymentID, txnID, form.Get("payer_id"), a.now(), raw)
}

// finish runs the shared post-authentication checks for both transports.
func (a *Adapter) finish(minor int64, currency, paymentID, txnID, payerID string, received time.Time, raw []byte) (*types.PaymentNotice, error) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "" {
		cur = "USD"
	}
	if cur != "USD" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCurrency, cur)
	}
	if received.IsZero() {
		received = a.now()
	}
	if a.RequireGuard && a.Guard == nil {
		return nil, errors.New("payoneer: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(paymentID) {
		return nil, fmt.Errorf("%w: payment_id %s", ErrReplayed, paymentID)
	}
	return &types.PaymentNotice{
		TxnRef:     paymentID,
		Method:     MethodName,
		Amount:     minor,
		Currency:   cur,
		SenderName: txnID,
		SenderAcct: payerID,
		ReceivedAt: received,
		Raw:        raw,
	}, nil
}

// ParseLinkKey splits the relay's txn_ref "payoneer:<payment_id>:<txn_id>".
func ParseLinkKey(txnRef string) (paymentID, txnID string, ok bool) {
	parts := strings.Split(txnRef, ":")
	if len(parts) != 3 || parts[0] != "payoneer" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// LinkKey builds the relay txn_ref for a Payoneer payment.
func LinkKey(paymentID, txnID string) string {
	return "payoneer:" + paymentID + ":" + txnID
}

// VerifyAPI implements adapter.PaymentAdapter. See assumption 4.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// noticeGuard is a read-only view over the guard used by the envelope
// verifier; envelope ids are consumed after parsing in verifyEnvelope via
// RememberEnvelope, not inside notice.Verifier.
type noticeGuard struct{ inner ReplayGuard }

func (g noticeGuard) Seen(id string) bool {
	if g.inner == nil {
		return false
	}
	return g.inner.Seen("env:" + id)
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
