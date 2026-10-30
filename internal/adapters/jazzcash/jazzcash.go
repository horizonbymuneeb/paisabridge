// Package jazzcash implements the JazzCash (Jazz / Mobilink Microfinance
// Bank) Hosted Checkout return-callback adapter.
//
// # The published scheme
//
// JazzCash's Hosted Checkout integration page specifies pp_SecureHash roughly
// as follows (paraphrased to avoid misquoting exact prose we cannot verify
// offline):
//
//	pp_SecureHash is an HMAC SHA-256 of the post parameters, taken in
//	ascending order, excluding pp_SecureHash and pp_SecureHashType, keyed
//	with the merchant Integrity Salt.
//
// Merchants and community SDKs disagree on one detail the text leaves open:
// whether the hashed material is "k1=v1&k2=v2&..." or just the concatenated
// values "v1v2...". This adapter implements both and records which one
// matched, so an operator can pin the right one (Adapter.Mode) after the
// first live callback. Both schemes are over the same field set, so the
// widening does not weaken the requirement for the salt.
//
// Ordering rules, stated so they are auditable and testable:
//
//   - pg_ fields come first, then pp_ fields (the order the published
//     PHP/Java samples build the hash in, and the single most common
//     integration bug);
//   - within a group, keys sort ascending by their lower-cased form;
//   - pp_SecureHash and pp_SecureHashType are excluded;
//   - a parameter that is present but empty still contributes ("").
//
// # Assumptions (documented, never silently faked)
//
//  1. The HMAC key is the merchant Integrity Salt verbatim, not a hash of it.
//  2. Both joining conventions are accepted (see above). This is bounded
//     widening, not a signature bypass: the forger still needs the salt.
//  3. pp_SecureHashType is accepted when absent or equal (case-insensitively)
//     to "HMACSHA256". Any other value - notably "HMACSHA1", which older
//     JazzCash documentation mentions - is rejected rather than downgraded.
//  4. pp_ResponseCode "000" is the completed-payment code.
//  5. pp_Amount is already in minor units (paisa): the Hosted Checkout
//     samples send pp_Amount=1100 for Rs 11.00, i.e. no decimal point.
//  6. VerifyAPI returns adapter.ErrUnsupported. The MPGS status inquiry needs
//     merchant ID + password + salt and an XML envelope; it is a separate
//     integration this adapter does not pretend to implement.
//
// # Endpoints
//
// None are embedded by design. JazzCash posts the callback to the merchant's
// return URL; this adapter never dials out. BaseURL is reserved for a future
// status inquiry and defaults to "" - TODO(provider-credentials).
package jazzcash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	successCode  = "000"
	maxFormBytes = 1 << 16

	// SchemeKV signs "k1=v1&k2=v2&...".
	SchemeKV = "hmac-sha256:sorted-key-value-amp"
	// SchemeValues signs "v1v2..." with no separator.
	SchemeValues = "hmac-sha256:sorted-values-concat"

	// Auto tries SchemeKV first, then SchemeValues.
	Auto = "auto"
)

var (
	// ErrNotSuccessful is returned for an authenticated non-payment.
	ErrNotSuccessful = errors.New("jazzcash: callback is not a successful payment")
	// ErrReplayed is returned for a consumed pp_TxnRefNo.
	ErrReplayed = errors.New("jazzcash: transaction already processed")
	// ErrNotConfigured is returned when no integrity salt is present.
	ErrNotConfigured = errors.New("jazzcash: integrity salt is not configured")
)

// Verified is the full outcome of a successful verification: the notice plus
// the signature convention that matched, which operators need for the audit
// trail described in assumption 2.
type Verified struct {
	Notice *types.PaymentNotice
	Scheme string
}

// Adapter implements adapter.PaymentAdapter for JazzCash hosted checkout.
type Adapter struct {
	// Salt is the merchant integrity salt (HMAC key).
	Salt string

	// Mode is Auto, SchemeKV or SchemeValues. Empty means Auto.
	Mode string

	// BaseURL is reserved for a status inquiry. Empty by default.
	// TODO(provider-credentials)
	BaseURL string

	// Clock supplies now for freshness checks. Nil means live.
	Clock clock.Clock

	// MaxSkew bounds how old pp_TxnDateTime may be. Zero means 24h.
	MaxSkew time.Duration

	// Guard holds consumed pp_TxnRefNo values.
	Guard ReplayGuard

	// RequireGuard turns a nil Guard into a configuration error.
	RequireGuard bool
}

// ReplayGuard records consumed transaction references.
type ReplayGuard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodJazzCash }

// Supports implements adapter.PaymentAdapter.
func (a *Adapter) Supports() bool { return a.Salt != "" }

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

// VerifyWebhook implements adapter.PaymentAdapter.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	v, err := a.VerifyForm(r)
	if err != nil {
		return nil, err
	}
	return v.Notice, nil
}

// VerifyForm is VerifyWebhook that also reports which convention matched.
//
// Order of operations: decode; verify the HMAC over the exact submitted field
// set; refuse a hash type we do not implement; then - only for authenticated
// callbacks - check the provider's success code, the amount, the timestamp,
// and finally replay state. Nothing touches the replay guard before the
// signature and freshness checks pass, so unauthenticated traffic cannot
// write into the store.
func (a *Adapter) VerifyForm(r *http.Request) (*Verified, error) {
	if a.Salt == "" {
		return nil, ErrNotConfigured
	}
	raw, err := provider.Body(r, maxFormBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	form, err := parseForm(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	submitted := form.Get("pp_SecureHash")
	if submitted == "" {
		return nil, fmt.Errorf("%w: pp_SecureHash missing", adapter.ErrInvalidSignature)
	}
	if ht := form.Get("pp_SecureHashType"); ht != "" && !strings.EqualFold(ht, "HMACSHA256") {
		return nil, fmt.Errorf("%w: unsupported pp_SecureHashType %q", adapter.ErrInvalidSignature, ht)
	}

	schemes := []string{SchemeKV, SchemeValues}
	switch a.Mode {
	case "", Auto:
	case SchemeKV, SchemeValues:
		schemes = []string{a.Mode}
	default:
		return nil, fmt.Errorf("jazzcash: unknown Mode %q", a.Mode)
	}
	joined := signingMaterial(form)
	matched := ""
	for _, s := range schemes {
		if provider.EqualHex(submitted, provider.HMACSHA256Hex(a.Salt, joined[s])) {
			matched = s
			break
		}
	}
	if matched == "" {
		return nil, fmt.Errorf("%w: pp_SecureHash matches neither supported convention", adapter.ErrInvalidSignature)
	}

	if code := form.Get("pp_ResponseCode"); code != successCode {
		return nil, fmt.Errorf("%w: pp_ResponseCode=%s pp_ResponseMessage=%q",
			ErrNotSuccessful, code, form.Get("pp_ResponseMessage"))
	}
	txnRef := strings.TrimSpace(form.Get("pp_TxnRefNo"))
	if txnRef == "" {
		return nil, fmt.Errorf("%w: pp_TxnRefNo missing", adapter.ErrInvalidSignature)
	}
	// pp_Amount is paisa in the documented form ("149700"), but some
	// integrators post "1497.00". A decimal point anywhere other than
	// trailing ".00" is ambiguous at the paisa level, so the presence of a
	// dot switches to the rupee-and-paisa interpretation.
	amountStr := strings.TrimSpace(form.Get("pp_Amount"))
	exp := int32(0)
	if strings.Contains(amountStr, ".") {
		exp = 2
	}
	minor, err := provider.ParseAmount(amountStr, exp)
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: pp_Amount %q", provider.ErrMalformed, amountStr)
	}
	// The hosted-checkout field is pp_TxnCurrency; pp_Currency is accepted
	// too because relays sometimes rename it on the way through.
	currency := form.Get("pp_TxnCurrency")
	if currency == "" {
		currency = form.Get("pp_Currency")
	}
	if c := strings.ToUpper(strings.TrimSpace(currency)); c != "" && c != "PKR" {
		return nil, fmt.Errorf("%w: unexpected currency %q", provider.ErrMalformed, c)
	}
	received, err := provider.ParseTimeLocal(form.Get("pp_TxnDateTime"))
	if err != nil {
		// Compact "20061102120231" stamp: unambiguous ordering, accepted.
		t, cerr := time.ParseInLocation("20060102150405", form.Get("pp_TxnDateTime"), clock.PKT)
		if cerr != nil {
			return nil, fmt.Errorf("%w: pp_TxnDateTime %q", provider.ErrMalformed, form.Get("pp_TxnDateTime"))
		}
		received = t
	}
	now := a.now()
	if d := now.Sub(received); d > a.maxSkew() || d < -a.maxSkew() {
		return nil, fmt.Errorf("%w: pp_TxnDateTime %s outside %s of now",
			adapter.ErrInvalidSignature, received.Format(time.RFC3339), a.maxSkew())
	}
	if a.RequireGuard && a.Guard == nil {
		return nil, errors.New("jazzcash: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(txnRef) {
		return nil, fmt.Errorf("%w: pp_TxnRefNo %s", ErrReplayed, txnRef)
	}
	return &Verified{
		Scheme: matched,
		Notice: &types.PaymentNotice{
			TxnRef:     txnRef,
			Method:     types.MethodJazzCash,
			Amount:     minor,
			Currency:   "PKR",
			SenderName: form.Get("pp_BillReference"),
			SenderAcct: form.Get("pp_MobileNumber"),
			ReceivedAt: received,
			Raw:        raw,
		},
	}, nil
}

// VerifyAPI implements adapter.PaymentAdapter. See assumption 6.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// SigningMaterial computes, for every supported convention, the exact string
// fed to HMAC-SHA256 for a given form. Exported so tests, fixtures and the
// operator docs can all reference one definition.
func SigningMaterial(form url.Values) map[string]string {
	return signingMaterial(form)
}

// signingMaterial implements the ordering rules from the package doc.
func signingMaterial(form url.Values) map[string]string {
	keys := make([]string, 0, len(form))
	for k := range form {
		lk := strings.ToLower(k)
		if lk == "pp_securehash" || lk == "pp_securehashtype" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		li, lj := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		pi, pj := strings.HasPrefix(li, "pg_"), strings.HasPrefix(lj, "pg_")
		if pi != pj {
			return pi
		}
		return li < lj
	})
	kv := make([]string, 0, len(keys))
	vals := make([]string, 0, len(keys))
	for _, k := range keys {
		v := form.Get(k)
		kv = append(kv, k+"="+v)
		vals = append(vals, v)
	}
	return map[string]string{
		SchemeKV:     strings.Join(kv, "&"),
		SchemeValues: strings.Join(vals, ""),
	}
}

// Sign returns the pp_SecureHash value for a form under the given convention.
// It is the same code path VerifyForm compares against, so a fixture built
// with Sign() cannot drift away from the verifier.
func Sign(salt, scheme string, form url.Values) string {
	return provider.HMACSHA256Hex(salt, signingMaterial(form)[scheme])
}

// parseForm accepts both form-encoded bodies and query strings (a relay may
// convert the POST into a GET redirect).
func parseForm(raw []byte) (url.Values, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, errors.New("empty body")
	}
	if !strings.Contains(s, "=") {
		return nil, errors.New("body is not form-encoded")
	}
	return url.ParseQuery(s)
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
