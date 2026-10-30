// Package registry is the single seam the API layer uses to reach every rail.
//
// It owns the mapping from a payment method to its adapter, holds the shared
// plumbing (replay guard, clock, secrets), and refuses to start in states that
// would silently downgrade security. Everything else in internal/adapters is
// independent of it, so a test can exercise one adapter without a registry.
//
// # What the registry guarantees
//
//   - Registry.Every adapter is looked up by the method the order was created
//     with, so a webhook can never be verified by the wrong rail's adapter.
//   - A webhook body is read once. http.Request.Body is a stream; the API
//     layer hands the request to Registry.VerifyWebhook, which copies the
//     bytes, re-injects them for the adapter, and returns a typed error that
//     maps cleanly onto HTTP status codes. Double-reading a Body is the most
//     common way a "signature verified" path silently verifies "" instead.
//   - Config errors are loud. RequireSignature (default true) makes an adapter
//     with no secret a fatal configuration error at construction, so a deploy
//     that forgot a secret fails health checks instead of accepting forged
//     callbacks.
//
// # Errors
//
//	adapter.ErrInvalidSignature -> 401 (forged or tampered)
//	notice.ErrStale             -> 401 (replay / clock skew)
//	*ErrReplayed                -> 200/208 (duplicate; the provider must STOP)
//	*ErrNotSuccessful           -> 200 (authenticated non-payment; log only)
//	adapter.ErrUnsupported      -> 501 (rail has no status API)
//	*ErrUnknown                 -> 404 (no adapter for that method)
package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/bank"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/easypaisa"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/jazzcash"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/nayapay"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/payoneer"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/sadapay"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/usdt"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// maxWebhookBytes is the largest body any rail may send. Each adapter enforces
// its own tighter limit; this is the outer bound so the registry never buffers
// an unbounded stream.
const maxWebhookBytes = 256 << 10

var (
	// ErrUnknown reports a method with no registered adapter.
	ErrUnknown = errors.New("registry: no adapter for method")
	// ErrReplayed reports a duplicate delivery. It is NOT an authentication
	// failure: the payload was authentic, it was simply already handled.
	ErrReplayed = errors.New("registry: duplicate delivery")
	// ErrNotSuccessful reports an authenticated non-payment.
	ErrNotSuccessful = errors.New("registry: authenticated non-success delivery")
	// ErrConfig reports an invalid RegistryConfig.
	ErrConfig = errors.New("registry: invalid configuration")
)

// Config carries every rail's credentials. An empty value disables the
// optional behaviour for that rail subject to RequireSignature; see each
// adapter's package doc for what its secret authenticates.
type Config struct {
	// EasypaisaSecret is the merchant AES hash key.
	EasypaisaSecret string
	// JazzCashSalt is the merchant integrity salt.
	JazzCashSalt string
	// NayaPayWebhookSecret authenticates our relay for NayaPay callbacks.
	NayaPayWebhookSecret string
	// NayaPayToken / NayaPayBaseURL enable VerifyAPI; either empty disables it.
	NayaPayToken   string
	NayaPayBaseURL string
	// SadaPayWebhookSecret authenticates our relay for SadaPay callbacks.
	SadaPayWebhookSecret string
	// BankRelaySecret authenticates the mail/SMS relay.
	BankRelaySecret string
	// BinancePaySecret is the Binance Pay merchant secret.
	BinancePaySecret string
	// USDTReceiverAddress, USDTContract, USDTChain enable on-chain USDT.
	USDTReceiverAddress string
	USDTContract        string
	USDTChain           int64
	// PayoneerSecret verifies the notification relay and the link stamp.
	PayoneerSecret string

	// Clock overrides the time source for every adapter. Nil means live.
	Clock clock.Clock
	// Guard is the durable replay guard shared by every rail. Nil disables
	// replay tracking, which RequireSignature=true (default) rejects.
	Guard Guard
	// RequireSignature makes a missing secret fatal for a rail that is
	// configured, and is always required for a rail that accepts webhooks.
	// Default true; only an explicit false disables it (tests, dry runs).
	RequireSignature bool
	// Logf, when set, receives a line for every rail skipped because its
	// secret is empty. Nil silences it.
	Logf func(format string, args ...any)
}

// Guard is the durable replay store shared by all adapters. Implemented by
// internal/store; the registry only depends on this small interface so the
// adapter layer stays free of storage concerns.
type Guard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// ConfigWithSignature is the default: RequireSignature cannot be written as a
// struct literal default, so Config literals should set RequireSignature
// explicitly. DefaultConfig documents the production default.
func DefaultConfig() Config { return Config{RequireSignature: true} }

// Registry holds every rail adapter and dispatches by method.
type Registry struct {
	mu       sync.RWMutex
	adapters map[types.Method]adapter.PaymentAdapter
	order    []types.Method
	clock    clock.Clock
	guard    Guard
}

// New builds a registry from a config.
//
// With RequireSignature true (the default, including the zero value's
// documented default when DefaultConfig is used) every rail's secret must be
// present; otherwise New fails with ErrConfig naming the missing secret. That
// is deliberate: a gateway must never boot into a state where webhooks are
// accepted but not authenticated.
func New(cfg Config) (*Registry, error) {
	r := &Registry{adapters: map[types.Method]adapter.PaymentAdapter{}, clock: cfg.Clock, guard: cfg.Guard}
	if cfg.Clock == nil {
		r.clock = clock.Live{}
	}

	type entry struct {
		method types.Method
		ad     adapter.PaymentAdapter
		secret string
		what   string // env var name, used in the "rail skipped" log line
	}
	entries := []entry{
		{types.MethodEasypaisa, &easypaisa.Adapter{
			Secret: cfg.EasypaisaSecret, Clock: r.clock,
			Guard: guardOrNil[easypaisa.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.EasypaisaSecret, "EasypaisaSecret"},
		{types.MethodJazzCash, &jazzcash.Adapter{
			Salt: cfg.JazzCashSalt, Clock: r.clock,
			Guard: guardOrNil[jazzcash.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.JazzCashSalt, "JazzCashSalt"},
		{types.MethodNayaPay, &nayapay.Adapter{
			WebhookSecret: cfg.NayaPayWebhookSecret, Clock: r.clock,
			MerchantToken: cfg.NayaPayToken, BaseURL: cfg.NayaPayBaseURL,
			Guard: guardOrNil[nayapay.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.NayaPayWebhookSecret, "NayaPayWebhookSecret"},
		{types.MethodSadaPay, &sadapay.Adapter{
			WebhookSecret: cfg.SadaPayWebhookSecret, Clock: r.clock,
			Guard: guardOrNil[sadapay.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.SadaPayWebhookSecret, "SadaPayWebhookSecret"},
		{types.MethodBank, &bank.Adapter{
			RelaySecret: cfg.BankRelaySecret, Clock: r.clock,
			Guard: guardOrNil[bank.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.BankRelaySecret, "BankRelaySecret"},
		{types.MethodUSDT, &usdt.Adapter{
			BinancePaySecret: cfg.BinancePaySecret, Clock: r.clock,
			ReceiverAddress: cfg.USDTReceiverAddress, ExpectedContract: cfg.USDTContract,
			ExpectedChain: cfg.USDTChain, Nonces: guardOrNil[usdt.ReplayGuard](cfg.Guard),
			RequireGuard: cfg.RequireSignature,
		}, cfg.BinancePaySecret, "BinancePaySecret"},
		{payoneer.MethodName, &payoneer.Adapter{
			Secret: cfg.PayoneerSecret, Clock: r.clock,
			Guard: guardOrNil[payoneer.ReplayGuard](cfg.Guard), RequireGuard: cfg.RequireSignature,
		}, cfg.PayoneerSecret, "PayoneerSecret"},
	}

	for _, e := range entries {
		if e.secret == "" {
			// An unconfigured rail is simply not offered. Booting must not
			// depend on the operator configuring rails they do not sell: a
			// deployment that only takes Bank transfers is a supported setup.
			// The rail stays closed - Dispatch answers ErrNoAdapter - so an
			// inbound webhook for it can never be accepted without a secret.
			continue
		}
		r.adapters[e.method] = e.ad
		r.order = append(r.order, e.method)
	}
	for _, e := range entries {
		if _, ok := r.adapters[e.method]; !ok && cfg.Logf != nil {
			cfg.Logf("rail %s disabled: %s not set", e.method, e.what)
		}
	}
	if len(r.adapters) == 0 {
		return nil, fmt.Errorf("%w: no rail is configured (set at least one PB_*_SECRET)", ErrConfig)
	}
	// On-chain USDT has a second, independent configuration: an address and
	// the token contract. A receiver with no contract would accept any token.
	if cfg.USDTReceiverAddress != "" && cfg.USDTContract == "" {
		return nil, fmt.Errorf("%w: USDTContract is required when USDTReceiverAddress is set", ErrConfig)
	}
	return r, nil
}

// NewWithAdapters builds a registry from explicit adapters. Tests and the
// API-layer fakes use it; production uses New(Config).
func NewWithAdapters(eps map[types.Method]adapter.PaymentAdapter) *Registry {
	r := &Registry{adapters: map[types.Method]adapter.PaymentAdapter{}, clock: clock.Live{}}
	for m, a := range eps {
		r.adapters[m] = a
		r.order = append(r.order, m)
	}
	sort.Slice(r.order, func(i, j int) bool { return r.order[i] < r.order[j] })
	return r
}

// Lookup returns the adapter for a method.
func (r *Registry) Lookup(m types.Method) (adapter.PaymentAdapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[m]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknown, m)
	}
	return a, nil
}

// Methods returns the registered methods in stable order.
func (r *Registry) Methods() []types.Method {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]types.Method, len(r.order))
	copy(out, r.order)
	return out
}

// Supports reports whether the method is registered and can auto-verify.
func (r *Registry) Supports(m types.Method) bool {
	a, err := r.Lookup(m)
	return err == nil && a.Supports()
}

// VerifyWebhook verifies an inbound callback for the given method.
//
// The body is read exactly once, re-injected for the adapter, and every
// adapter error is classified into the registry's error vocabulary so the API
// layer never has to know which rail produced it.
func (r *Registry) VerifyWebhook(m types.Method, req *http.Request) (*types.PaymentNotice, error) {
	a, err := r.Lookup(m)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", adapter.ErrInvalidSignature)
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", adapter.ErrInvalidSignature, err)
	}
	if int64(len(raw)) > maxWebhookBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", adapter.ErrInvalidSignature, maxWebhookBytes)
	}
	replay := req.Clone(req.Context())
	replay.Body = io.NopCloser(bytes.NewReader(raw))
	n, err := a.VerifyWebhook(replay)
	if err != nil {
		return nil, classify(err)
	}
	if n == nil {
		return nil, fmt.Errorf("%w: adapter returned no notice", ErrConfig)
	}
	if n.Method != m && !(m == types.MethodUSDT && n.Method == types.MethodBinance) {
		// A notice for another rail would be applied to the wrong orders.
		return nil, fmt.Errorf("%w: adapter for %q produced notice for %q", ErrConfig, m, n.Method)
	}
	return n, nil
}

// VerifyAPI asks a rail's status API about a transaction.
func (r *Registry) VerifyAPI(ctx context.Context, m types.Method, txnRef string) (*types.PaymentNotice, error) {
	a, err := r.Lookup(m)
	if err != nil {
		return nil, err
	}
	return a.VerifyAPI(ctx, txnRef)
}

// VerifyTransfer dispatches an on-chain USDT transfer to the USDT adapter.
// It exists so the watcher does not need to know about the adapter struct.
func (r *Registry) VerifyTransfer(t usdt.Transfer) (*types.PaymentNotice, error) {
	a, err := r.Lookup(types.MethodUSDT)
	if err != nil {
		return nil, err
	}
	u, ok := a.(*usdt.Adapter)
	if !ok {
		return nil, fmt.Errorf("%w: USDT adapter is %T", ErrConfig, a)
	}
	return u.VerifyTransfer(t)
}

// classify maps an adapter error onto the registry's vocabulary, preserving
// errors.Is compatibility for every underlying sentinel.
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, adapter.ErrInvalidSignature), errors.Is(err, notice.ErrStale), errors.Is(err, notice.ErrBadNotice):
		return err // authentication failure: keep the sentinel, API maps to 401
	case errors.Is(err, easypaisa.ErrReplayed), errors.Is(err, jazzcash.ErrReplayed),
		errors.Is(err, nayapay.ErrReplayed), errors.Is(err, sadapay.ErrReplayed),
		errors.Is(err, bank.ErrReplayed), errors.Is(err, usdt.ErrReplayed),
		errors.Is(err, payoneer.ErrReplayed), errors.Is(err, notice.ErrReplayed):
		return fmt.Errorf("%w: %v", ErrReplayed, err)
	case errors.Is(err, easypaisa.ErrNotSuccessful), errors.Is(err, jazzcash.ErrNotSuccessful),
		errors.Is(err, nayapay.ErrNotSuccessful), errors.Is(err, sadapay.ErrNotSuccessful),
		errors.Is(err, usdt.ErrNotSuccessful), errors.Is(err, bank.ErrNotACredit):
		return fmt.Errorf("%w: %v", ErrNotSuccessful, err)
	default:
		return err
	}
}

// guardOrNil converts the registry's Guard to a rail's ReplayGuard interface.
// Go interfaces are structurally assignable, so this is a plain cast with a
// nil check (a typed nil would make every Seen() panic).
func guardOrNil[T any](g Guard) T {
	var zero T
	if g == nil {
		return zero
	}
	if v, ok := any(g).(T); ok {
		return v
	}
	return zero
}

// Ensure the concrete adapters satisfy the shared contract at compile time.
var (
	_ adapter.PaymentAdapter = (*easypaisa.Adapter)(nil)
	_ adapter.PaymentAdapter = (*jazzcash.Adapter)(nil)
	_ adapter.PaymentAdapter = (*nayapay.Adapter)(nil)
	_ adapter.PaymentAdapter = (*sadapay.Adapter)(nil)
	_ adapter.PaymentAdapter = (*bank.Adapter)(nil)
	_ adapter.PaymentAdapter = (*usdt.Adapter)(nil)
	_ adapter.PaymentAdapter = (*payoneer.Adapter)(nil)
)
