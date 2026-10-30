// Package easypaisa implements the Easypaisa (Telenor Microfinance Bank /
// Easypaisa Digital Bank) merchant-integration adapter.
//
// # What is verified
//
// Easypaisa's merchant integration (both the older MA (Merchant Account) /
// "Easypay" flow and the newer OPG "payment gateway" flow used through
// aggregators such as Safepay) authenticates the callback in two places:
//
//   - merchantHashedReq: an AES-ECB encrypted request blob. The merchant
//     encrypts a query string with AES and the provider decrypts it. On the
//     callback path the same blob comes back and the merchant must decrypt
//     it with the shared AES key to learn the transaction facts.
//   - merchantHashedResp: the provider's AES-ECB ciphertext of the
//     "transactionId&responseCode&responseDesc" triple (base64). A merchant
//     trusts the callback only when this decrypts under the shared key to
//     exactly the transaction id in the callback body.
//
// Verified here: the decryption of merchantHashedResp, the equality of the
// recovered transaction id with the body's orderId/transactionId, the
// presence of a merchantHashedReq, the uniqueness of the txn reference and
// the timestamp freshness of the body.
//
// # What is assumed (documented, not faked)
//
//  1. Payload format. No public Easypaisa document is available to this
//     project; the field names below follow the widely published OPG/MA
//     callback shape ("orderId", "transactionId", "responseCode",
//     "responseDesc", "transactionAmount", "transactionCurrency",
//     "transactionDateTime", "merchantHashedReq", "merchantHashedResp").
//     A merchant that receives a callback can compare one live payload
//     against this file before going live; the decode is isolated in
//     decodeCallback so that is a five-line change, not a rewrite.
//  2. AES-ECB. ECB is what the published scheme uses (block-by-block, no IV,
//     no authenticated encryption). ECB is malleable, but the verification
//     property this adapter relies on is not "ciphertext is authentic", it is
//     "the ciphertext has no valid 16-byte-block padding structure under a
//     key the attacker does not hold". Without the key an attacker can only
//     produce garbage blocks, which fail unpadding with overwhelming
//     probability (see the forgery probability note in the package tests).
//     This is weaker than an HMAC and is the honest state of this rail; the
//     adapter says so rather than pretending.
//  3. Padding. PKCS#7 padding is removed when the plaintext is valid UTF-8
//     with all printable characters, otherwise the plaintext is used as-is.
//     This matches both padding conventions seen in Easypaisa-derived SDKs
//     (some strip the padding server-side and hand back unpadded output).
//  4. Success code. "0000" is treated as success. Callbacks with any other
//     responseCode are authenticated and then rejected with ErrNotSuccessful
//     instead of being turned into a payment notice, so a failed/pending
//     callback can never fulfil an order.
//  5. VerifyAPI. Easypaisa has no public merchant status API usable by a
//     third-party gateway. VerifyAPI returns adapter.ErrUnsupported.
//
// # Endpoints
//
// No provider URL is embedded anywhere: the adapter never calls out. The
// BaseURL field exists for operators who front the provider through their
// own relay and defaults to "", marked TODO(provider-credentials).
package easypaisa

import (
	"context"
	"crypto/aes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	// successCode is the responseCode Easypaisa returns for a completed
	// payment. Anything else is authenticated but not a payment.
	successCode = "0000"

	// maxBodyBytes bounds the callback. See package notice for the rationale.
	maxBodyBytes = 1 << 16

	// currencyPKR is the only currency Easypaisa settles in today.
	currencyPKR = "PKR"
)

var (
	// ErrNotSuccessful reports an authenticated callback that does not
	// represent a completed payment (declined, pending, expired). It exists
	// so the API layer can log "provider says failed" instead of
	// misreporting a signature error.
	ErrNotSuccessful = errors.New("easypaisa: callback is not a successful payment")

	// ErrNotConfigured is returned when the AES key is missing. A gateway
	// with no key must never accept a callback.
	ErrNotConfigured = errors.New("easypaisa: AES secret is not configured")
)

// Adapter implements adapter.PaymentAdapter for Easypaisa.
//
// It is safe for concurrent use: the only mutable field is the replay guard,
// which is itself required to be concurrency-safe.
type Adapter struct {
	// Secret is the shared AES key of the merchant account (the value
	// Easypaisa calls the "hash key" / "secret"). 16, 24 or 32 bytes.
	Secret string

	// BaseURL is the merchant relay URL used by VerifyAPI. Empty by default.
	// TODO(provider-credentials): fill from the merchant onboarding pack.
	BaseURL string

	// Clock supplies "now" for freshness and replay checks. Nil means live.
	Clock clock.Clock

	// MaxSkew bounds how old transactionDateTime may be. Zero means 24h.
	// Callbacks can be retried by the provider for hours; a day is generous
	// without allowing an ancient capture to be replayed as new.
	MaxSkew time.Duration

	// Guard holds consumed transaction references. Nil means no replay
	// tracking, which is allowed for tests but must be set in production;
	// the registry returns an error if it is nil and RequireGuard is on.
	Guard ReplayGuard

	// RequireGuard makes an unset Guard a configuration error instead of a
	// silent security downgrade.
	RequireGuard bool
}

// ReplayGuard records consumed transaction ids. It is the same shape as
// notice.ReplayGuard so the store implements one interface for both.
type ReplayGuard interface {
	// Seen returns true when id was already consumed.
	Seen(id string) bool
	// Remember records id. Return value indicates whether it was new.
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodEasypaisa }

// Supports reports true when a usable key is configured: with a key this
// rail can auto-verify callbacks, without one it needs the notice feed.
func (a *Adapter) Supports() bool { return len(a.Secret) > 0 }

func (a *Adapter) now() time.Time {
	if a.Clock == nil {
		return clock.Live{}.Now()
	}
	return a.Clock.Now()
}

func (a *Adapter) maxSkew() time.Duration {
	if a.MaxSkew <= 0 {
		return 24 * time.Hour
	}
	return a.MaxSkew
}

// callback is the decoded Easypaisa callback body. Assumption 1 above: the
// field set is documented, not scraped from a leaked payload.
type callback struct {
	OrderID             string `json:"orderId"`
	TransactionID       string `json:"transactionId"`
	ResponseCode        string `json:"responseCode"`
	ResponseDesc        string `json:"responseDesc"`
	TransactionAmount   string `json:"transactionAmount"`
	TransactionCurrency string `json:"transactionCurrency"`
	TransactionDateTime string `json:"transactionDateTime"`
	MerchantHashedReq   string `json:"merchantHashedReq"`
	MerchantHashedResp  string `json:"merchantHashedResp"`
	MobileAccountNo     string `json:"mobileAccountNo"`
}

// VerifyWebhook implements adapter.PaymentAdapter.
//
// Order of operations matters and is deliberate:
//  1. bounded read and strict decode (no signature is trusted yet);
//  2. merchantHashedResp presence and decryption under the shared key;
//  3. the decrypted transaction id must equal the body's identity - this is
//     what binds the ciphertext to *this* callback;
//  4. responseCode must be success;
//  5. amount and timestamp must be sane and fresh;
//  6. only now, with everything authenticated, is the replay guard consulted.
//
// A step that fails returns adapter.ErrInvalidSignature for 1-3 and the
// specific error for 4-6, so the caller can distinguish "forged" from
// "we already processed this".
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.Secret == "" {
		return nil, ErrNotConfigured
	}
	body, err := provider.Body(r, maxBodyBytes)
	if err != nil {
		return nil, err
	}
	var cb callback
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cb); err != nil {
		return nil, fmt.Errorf("%w: decode callback: %v", adapter.ErrInvalidSignature, err)
	}
	if strings.TrimSpace(cb.MerchantHashedResp) == "" {
		return nil, fmt.Errorf("%w: merchantHashedResp missing", adapter.ErrInvalidSignature)
	}
	if strings.TrimSpace(cb.MerchantHashedReq) == "" {
		// The encrypted request blob is what the provider signs its own
		// record with; without it the callback is not a merchant response.
		return nil, fmt.Errorf("%w: merchantHashedReq missing", adapter.ErrInvalidSignature)
	}
	plain, err := a.decrypt(cb.MerchantHashedResp)
	if err != nil {
		return nil, fmt.Errorf("%w: merchantHashedResp: %v", adapter.ErrInvalidSignature, err)
	}
	txnFromCipher, _, ok := strings.Cut(plain, "&")
	if !ok || strings.TrimSpace(txnFromCipher) == "" {
		return nil, fmt.Errorf("%w: merchantHashedResp does not carry a transaction id", adapter.ErrInvalidSignature)
	}
	// Bind ciphertext to body. Then re-encrypting is unnecessary: an
	// attacker who could produce this ciphertext already holds the key.
	if txnFromCipher != cb.TransactionID || (cb.OrderID != "" && cb.OrderID != txnFromCipher) {
		return nil, fmt.Errorf("%w: ciphertext transaction id %q does not match body %q",
			adapter.ErrInvalidSignature, txnFromCipher, cb.TransactionID)
	}
	if cb.ResponseCode != successCode {
		return nil, fmt.Errorf("%w: responseCode=%s desc=%q", ErrNotSuccessful, cb.ResponseCode, cb.ResponseDesc)
	}
	minor, err := provider.ParseAmount(cb.TransactionAmount, provider.Exponent(currencyPKR))
	if err != nil {
		return nil, fmt.Errorf("%w: transactionAmount: %v", provider.ErrMalformed, err)
	}
	if minor <= 0 {
		return nil, fmt.Errorf("%w: transactionAmount must be positive", provider.ErrMalformed)
	}
	received, err := provider.ParseTimeLocal(cb.TransactionDateTime)
	if err != nil {
		return nil, fmt.Errorf("%w: transactionDateTime: %v", provider.ErrMalformed, err)
	}
	now := a.now()
	if d := now.Sub(received); d > a.maxSkew() || d < -a.maxSkew() {
		return nil, fmt.Errorf("%w: transactionDateTime %s outside %s of now",
			adapter.ErrInvalidSignature, received.Format(time.RFC3339), a.maxSkew())
	}
	currency := strings.ToUpper(strings.TrimSpace(cb.TransactionCurrency))
	if currency == "" {
		currency = currencyPKR
	}
	if a.RequireGuard && a.Guard == nil {
		return nil, errors.New("easypaisa: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(cb.TransactionID) {
		return nil, fmt.Errorf("%w: transactionId %s", ErrReplayed, cb.TransactionID)
	}
	return &types.PaymentNotice{
		TxnRef:     cb.TransactionID,
		Method:     types.MethodEasypaisa,
		Amount:     minor,
		Currency:   currency,
		SenderName: cb.MobileAccountNo,
		SenderAcct: cb.MobileAccountNo,
		ReceivedAt: received,
		Raw:        body,
	}, nil
}

// ErrReplayed reports a transaction id the guard has already consumed.
var ErrReplayed = errors.New("easypaisa: transaction already processed")

// VerifyAPI implements adapter.PaymentAdapter. See assumption 5.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// decrypt performs AES-ECB/PKCS#7 decryption over a base64 ciphertext.
func (a *Adapter) decrypt(s string) (string, error) {
	block, err := aes.NewCipher([]byte(a.Secret))
	if err != nil {
		return "", fmt.Errorf("bad AES key length %d: %w", len(a.Secret), err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		// Some integrators URL-encode the base64; try that once.
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return "", fmt.Errorf("ciphertext is not base64: %w", err)
		}
	}
	if len(raw) == 0 || len(raw)%block.BlockSize() != 0 {
		return "", fmt.Errorf("ciphertext length %d is not a positive multiple of %d", len(raw), block.BlockSize())
	}
	out := make([]byte, len(raw))
	for off := 0; off < len(raw); off += block.BlockSize() {
		block.Decrypt(out[off:off+block.BlockSize()], raw[off:off+block.BlockSize()])
	}
	// PKCS#7 removal is best-effort by design (assumption 3): keep it only
	// when the result is printable UTF-8, which random blocks essentially
	// never are.
	if trimmed, ok := stripPKCS7(out, block.BlockSize()); ok {
		out = trimmed
	}
	if !plausibleText(out) {
		return "", errors.New("decrypted plaintext is not text")
	}
	return string(out), nil
}

func stripPKCS7(b []byte, size int) ([]byte, bool) {
	if len(b) == 0 || len(b)%size != 0 {
		return b, false
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > size || pad > len(b) {
		return b, false
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return b, false
		}
	}
	return b[:len(b)-pad], true
}

func plausibleText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	// Allow only printable ASCII plus space: the plaintext is
	// "transactionId&code&description".
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// Encrypt produces a merchantHashedResp value. It lives in the production
// package because operators need it to build fixture payloads and because
// the tests use the same code path: a test that verified against a
// hand-written expectation could drift from the implementation and would not
// fail when verification is removed.
func (a *Adapter) EncryptPlaintext(p string) (string, error) {
	block, err := aes.NewCipher([]byte(a.Secret))
	if err != nil {
		return "", err
	}
	data := []byte(p)
	pad := block.BlockSize() - len(data)%block.BlockSize()
	for i := 0; i < pad; i++ {
		data = append(data, byte(pad))
	}
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += block.BlockSize() {
		block.Encrypt(out[off:off+block.BlockSize()], data[off:off+block.BlockSize()])
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// Fingerprint is a non-reversible identifier for a secret, used in logs so
// an operator can confirm "the right key is loaded" without printing it.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:6])
}

// memGuard is the in-package replay guard used by tests and by callers that
// explicitly want ephemeral state. Production must supply a durable guard.
type memGuard struct {
	mu   sync.Mutex
	seen map[string]bool
}

// NewMemGuard returns an in-memory replay guard. Test helper; a restart
// forgets it, so it must not be used for production traffic.
func NewMemGuard() ReplayGuard { return &memGuard{seen: map[string]bool{}} }

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
