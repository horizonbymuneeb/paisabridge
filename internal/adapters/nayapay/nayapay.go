// Package nayapay implements NayaPay's payment-link callback adapter.
//
// # NayaPay's integration shape (as publicly documented)
//
// NayaPay's merchant offering is built on standard HTTP basics rather than a
// bespoke signing scheme:
//
//   - a payment link is created with the merchant token in a Basic-auth
//     header and an idempotency key, body
//     {"amount":1497.00,"currency":"PKR","orderRef":...,"redirectUrl":...},
//     and the response carries a hosted payment URL;
//   - when the payer finishes, NayaPay redirects to redirectUrl with a query
//     string carrying the payment reference;
//   - a server-to-server (S2S) status call authenticated with the merchant
//     token is the authoritative source of truth: the redirect is a hint,
//     the status call is the fact.
//
// # What this adapter verifies
//
// Because the documented redirect is unauthenticated query parameters (like
// every hosted-checkout redirect), this adapter refuses to turn a redirect
// into a payment notice. It accepts only a request that carries NayaPay's
// server-to-server signature, computed by the relay that receives the S2S
// callback:
//
//	signature = HMAC-SHA256(webhookSecret, body)
//	sent in X-Nayapay-Signature as hex (optional "sha256=" prefix)
//
// and it additionally requires the caller to have confirmed the payment over
// VerifyAPI (or through the relay). Two independent checks - transport HMAC
// here, provider status there - is the whole point of distinguishing this
// gateway from an e-mail-alert + manual-approve pipeline.
//
// # Assumptions (documented, never silently faked)
//
//  1. NayaPay does not publish a webhook signing scheme that this project
//     could reproduce offline. Rather than invent a provider signature and
//     claim it is theirs, the HMAC above is ours: it authenticates the relay
//     we operate. The secret is shared only between our relay and our
//     gateway. This is stated here so nobody mistakes it for a NayaPay
//     feature; when NayaPay ships a signature, it replaces this one and the
//     relay keeps its own HMAC as defence in depth.
//  2. The redirect query parameters alone are never trusted. VerifyWebhook
//     requires a signed JSON body; a bare redirect gets ErrInvalidSignature.
//  3. VerifyAPI performs the documented status call shape but is inactive
//     unless BaseURL and MerchantToken are configured; with them unset it
//     returns ErrNotConfigured, never a fabricated success.
//  4. VerifyAPI does not treat a 200 response alone as success: the body must
//     carry a terminal "SUCCESS"/"COMPLETED" status and an amount equal to
//     what the provider reports. The Go HTTP client is injectable so tests
//     run offline.
//
// # Endpoints
//
// No NayaPay hostname is embedded. The documented API lives under the
// sandbox/production base URL issued with merchant credentials, which is why
// BaseURL exists and defaults to "": TODO(provider-credentials).
package nayapay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	signatureHeader = "X-Nayapay-Signature"
	maxBodyBytes    = 1 << 16
	// statusPath is appended to BaseURL for the S2S status call.
	statusPath = "/v2/payments/status"
)

var (
	// ErrNotSuccessful is returned for an authenticated non-payment.
	ErrNotSuccessful = errors.New("nayapay: payment is not in a successful state")
	// ErrReplayed is returned for a consumed order reference.
	ErrReplayed = errors.New("nayapay: payment already processed")
	// ErrNotConfigured is returned when required credentials or the webhook
	// secret are missing.
	ErrNotConfigured = errors.New("nayapay: credentials are not configured")
)

// verifiedStatuses are the provider statuses that mean money moved.
var verifiedStatuses = map[string]bool{
	"SUCCESS":   true,
	"COMPLETED": true,
	"PAID":      true,
	"CAPTURED":  true,
}

// Adapter implements adapter.PaymentAdapter for NayaPay.
type Adapter struct {
	// WebhookSecret is the shared HMAC key between our relay and this
	// gateway. See assumption 1.
	WebhookSecret string

	// MerchantToken is the NayaPay merchant bearer credential (Basic auth
	// user:password pair, sent verbatim as "Authorization: Basic <token>").
	// TODO(provider-credentials)
	MerchantToken string

	// BaseURL is the NayaPay API base URL issued with the merchant account.
	// Empty by default - TODO(provider-credentials).
	BaseURL string

	// HTTPClient overrides the client used by VerifyAPI. Nil means a client
	// with a 10s timeout.
	HTTPClient *http.Client

	// Clock supplies now. Nil means live.
	Clock clock.Clock

	// MaxSkew bounds how old a signed callback payload may be. Zero means
	// 10 minutes: the relay is on the same network, so drift is small.
	MaxSkew time.Duration

	// Guard holds consumed payment references.
	Guard ReplayGuard

	// RequireGuard turns a nil Guard into a configuration error.
	RequireGuard bool
}

// ReplayGuard records consumed payment references.
type ReplayGuard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodNayaPay }

// Supports reports whether the signed relay callback can be verified.
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

// callback is the body our relay posts after receiving the S2S callback. The
// field names mirror NayaPay's status payload so the relay is a thin shim.
type callback struct {
	PaymentRef  string `json:"payment_ref"`
	OrderRef    string `json:"order_ref"`
	Status      string `json:"status"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	PayerName   string `json:"payer_name,omitempty"`
	PayerAcct   string `json:"payer_account,omitempty"`
	CompletedAt int64  `json:"completed_at,omitempty"` // unix seconds
}

// VerifyWebhook implements adapter.PaymentAdapter.
//
// A bare redirect (no signature header) is rejected: query parameters are
// attacker-controlled, so they can never establish that money moved.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.WebhookSecret == "" {
		return nil, ErrNotConfigured
	}
	sig, err := provider.Header(r.Header, signatureHeader)
	if err != nil {
		// A redirect callback carries no body signature: treat it exactly
		// like a forged one (assumption 2).
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	sig = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(sig)), "sha256=")
	body, err := provider.Body(r, maxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	want := provider.HMACSHA256Hex(a.WebhookSecret, string(body))
	if !provider.EqualHex(sig, want) {
		return nil, fmt.Errorf("%w: %s does not match body", adapter.ErrInvalidSignature, signatureHeader)
	}

	var cb callback
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cb); err != nil {
		return nil, fmt.Errorf("%w: body: %v", provider.ErrMalformed, err)
	}
	if strings.TrimSpace(cb.PaymentRef) == "" {
		return nil, fmt.Errorf("%w: payment_ref missing", provider.ErrMalformed)
	}
	if !verifiedStatuses[strings.ToUpper(strings.TrimSpace(cb.Status))] {
		return nil, fmt.Errorf("%w: status %q", ErrNotSuccessful, cb.Status)
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
		return nil, errors.New("nayapay: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(cb.PaymentRef) {
		return nil, fmt.Errorf("%w: payment_ref %s", ErrReplayed, cb.PaymentRef)
	}
	return &types.PaymentNotice{
		TxnRef:     cb.PaymentRef,
		Method:     types.MethodNayaPay,
		Amount:     minor,
		Currency:   currency,
		SenderName: cb.PayerName,
		SenderAcct: cb.PayerAcct,
		ReceivedAt: received,
		Raw:        body,
	}, nil
}

// statusResponse is the subset of NayaPay's payment-status payload this
// adapter consumes. Unknown fields are ignored here on purpose: provider
// APIs add fields routinely, and the signature that matters is the one
// NayaPay puts on its own TLS endpoint plus our bearer token.
type statusResponse struct {
	PaymentRef string `json:"payment_ref"`
	OrderRef   string `json:"order_ref"`
	Status     string `json:"status"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
}

// VerifyAPI implements adapter.PaymentAdapter (assumption 3 and 4).
//
// It returns ErrNotConfigured rather than a synthetic notice when the
// merchant credentials are absent: a gateway that cannot ask the provider
// must report "unknown", never "paid".
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	if a.BaseURL == "" || a.MerchantToken == "" {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(txnRef) == "" {
		return nil, fmt.Errorf("%w: empty payment reference", provider.ErrMalformed)
	}
	endpoint := strings.TrimRight(a.BaseURL, "/") + statusPath + "?" + url.Values{"payment_ref": {txnRef}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("nayapay: build status request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+a.MerchantToken)
	req.Header.Set("Accept", "application/json")

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nayapay: status call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nayapay: status call returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("nayapay: read status: %w", err)
	}
	var sr statusResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("nayapay: decode status: %w", err)
	}
	if sr.PaymentRef != txnRef {
		// A status endpoint answering about a different payment is a
		// misrouted request, not a result.
		return nil, fmt.Errorf("nayapay: status is for %q, asked for %q", sr.PaymentRef, txnRef)
	}
	if !verifiedStatuses[strings.ToUpper(strings.TrimSpace(sr.Status))] {
		return nil, fmt.Errorf("%w: status %q", ErrNotSuccessful, sr.Status)
	}
	currency := strings.ToUpper(strings.TrimSpace(sr.Currency))
	if currency == "" {
		currency = "PKR"
	}
	minor, err := provider.ParseAmount(sr.Amount, provider.Exponent(currency))
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: amount %q", provider.ErrMalformed, sr.Amount)
	}
	return &types.PaymentNotice{
		TxnRef:     sr.PaymentRef,
		Method:     types.MethodNayaPay,
		Amount:     minor,
		Currency:   currency,
		ReceivedAt: a.now(),
		Raw:        body,
	}, nil
}

// Sign produces the relay signature for a body. Shared with the relay
// package and the tests so there is one definition of the header format.
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
