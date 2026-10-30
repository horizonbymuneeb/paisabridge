// Package api is the paisabridge HTTP surface. It uses only the standard
// library's Go 1.22 method patterns (net/http.ServeMux) — no router
// dependency, no middleware framework.
//
// # HTTP surface
//
// Public (no auth):
//
//	GET  /api/readyz
//	     Readiness: 200 {"ready":true,"checks":{"db":"ok"}} ; 503 when the
//	     database is unreachable. Liveness stays on /api/healthz so a probe
//	     can distinguish "restart me" from "take me out of rotation".
//
//	GET  /api/healthz
//	     -> 200 {"status":"ok","time":"...","db":"ok"} ; 503 if the DB is down
//
//	POST /api/orders
//	     Body: {"product_id":"...","product_name":"...","catalog_price_minor":150000,
//	            "currency":"PKR","method":"easypaisa","customer_email":"a@b.c",
//	            "customer_phone":"03001234567","ttl_seconds":1800}
//	     -> 201 Order. The returned amount_minor is the unique payable amount
//	        (catalog price adjusted by internal/unique) and expires_at bounds
//	        the payment window.
//	     -> 409 when the unique-amount window is exhausted for that rail.
//
//	GET  /api/orders/{id}
//	     {id} may be the order id or the short ref.
//	     -> 200 Order+{"events":[...]} ; 404
//
//	POST /api/orders/{id}/claim
//	     "I have paid this from my wallet." Charges the wallet at the prepay
//	     discount, moves the order pending_payment -> verified (the sweeper /
//	     fulfilment step then delivers).
//	     Body (optional): {"user_id":"..."}
//	     -> 200 {"order":..., "balance":..., "price_minor":...,"discount_minor":...,"charged_minor":...}
//	     -> 402 insufficient wallet funds ; 409 already paid (idempotent, no double charge)
//
//	POST /api/orders/{id}/cancel
//	     Cancels an open order and refunds it if it was paid from the wallet.
//	     -> 200 {"order":...,"refunded":true|false} ; 409 terminal state
//
//	POST /api/webhooks/{method}
//	     {method} is a types.Method (easypaisa, jazzcash, nayapay, sadapay,
//	     bank, usdt, binance). The body is handed to the registered adapter,
//	     which MUST verify its signature before returning a notice; the notice
//	     then goes through the matcher.
//	     -> 200 {"matched":bool,"order_id":"...","reason":"matched|duplicate_txn_ref|ambiguous_amount|order_expired|no_open_order|...","credited":bool}
//	     Duplicate deliveries are 200 with reason=duplicate_txn_ref and are
//	     never credited twice.
//	     -> 400 bad payload / unknown method ; 401 bad signature
//
//	GET  /api/wallet?user_id=...
//	     -> 200 {"user_id":"...","currency":"PKR","available_minor":n,"discount_bps":500,"entries":[...]}
//
//	POST /api/wallet/topup
//	     Body: {"user_id":"...","amount_minor":100000,"idempotency_key":"tx-123","memo":"..."}
//	     -> 200 balance. Replaying the same idempotency_key is a 200 no-op.
//	     -> 400 missing/negative amount or missing idempotency key
//
//	GET  /api/notices?status=unmatched&limit=50
//	     Admin-ish view of inbound payments for manual reconciliation.
//
// Admin (header X-Admin-Token: <PB_ADMIN_TOKEN>):
//
//	POST /api/admin/approve
//	     Body: {"order_id":"...","txn_ref":"...","memo":"..."}
//	     Manual approval for payments the matcher refused to auto-apply
//	     (ambiguous amount, late payment, unmatched feed entry). Moves the
//	     order to verified through the same state machine and journal.
//	     -> 200 Order ; 401 bad token ; 404 unknown order ; 409 already verified
//
//	GET  /api/admin/orders?status=review&limit=50
//	     Cross-user order list for triage.
//
// The webhook route is the only one that trusts an external caller, and it
// trusts nothing but the adapter's signature check.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

// AdapterLookup resolves a rail to its adapter implementation. cmd wires the
// concrete registry (internal/adapters.Registry) here; tests inject fakes, so
// the API compiles and is testable before the registry exists.
type AdapterLookup interface {
	Get(m types.Method) adapter.PaymentAdapter
}

// AdapterFunc adapts a function into an AdapterLookup (handy in tests/cmd).
type AdapterFunc func(m types.Method) adapter.PaymentAdapter

// Get implements AdapterLookup.
func (f AdapterFunc) Get(m types.Method) adapter.PaymentAdapter { return f(m) }

// Config configures the HTTP server.
type Config struct {
	// Store is required.
	Store *store.Store
	// Wallet is required for the wallet and claim endpoints.
	Wallet *wallet.Wallet
	// Matcher is required for webhooks.
	Matcher *match.Matcher
	// Adapters may be nil until a rail is registered; webhooks for a method
	// with no adapter return 400 unknown_method.
	Adapters AdapterLookup
	// AdminToken, when non-empty, enables and guards /api/admin/*.
	AdminToken string
	// OrderTTL is the default payment window for new orders.
	OrderTTL time.Duration
	// Logger is optional (defaults to slog.Default()).
	Logger *slog.Logger
	// EnabledMethods filters /api/webhooks/{method}; empty means "anything
	// an adapter claims".
	EnabledMethods []types.Method
	// Now is optional clock injection for tests.
	Now func() time.Time

	// RedeliverDeadLetter re-sends one parked outbound delivery. main wires it to
	// the outbound sender; nil means outbound delivery is not configured and the
	// retry endpoint answers 501.
	RedeliverDeadLetter func(ctx context.Context, id string) error

	// Metrics, when non-nil, receives request, order and payment counters.
	// Nil disables recording entirely.
	Metrics Recorder

	// Notify, when set, is called after an order reaches a customer-visible
	// state (fulfilled, review, expired, cancelled). It runs in a goroutine so
	// a slow integrator endpoint can never delay a webhook acknowledgement.
	// Errors are the implementation's concern; the API only logs them.
	Notify func(ctx context.Context, event string, order *types.Order)

	// RateLimits overrides DefaultRateLimits per route class. A class that is
	// absent keeps its default, so callers only spell out what they tune. Set
	// a class to {PerMinute: -1} to disable limiting for it (single-tenant
	// deployments behind their own proxy sometimes want that).
	//
	// The zero value of Config uses DefaultRateLimits unchanged, which is why
	// existing deployments and tests keep their behaviour without code
	// changes.
	RateLimits map[RateLimitClass]RateLimit
	// TrustProxy makes the server read the client address from
	// X-Forwarded-For instead of the socket peer. Enable it only when a reverse
	// proxy in front of this server sets that header (PB_TRUST_PROXY=1);
	// otherwise any client can pick its own rate-limit bucket.
	TrustProxy bool
	// IdempotencyRetention is how long a completed order idempotency key is
	// remembered. Zero means DefaultIdempotencyRetention (24h). A key older
	// than this may create a second order.
	IdempotencyRetention time.Duration
}

// Server holds the handlers. One Server == one *http.ServeMux.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	h       http.Handler // mux wrapped in the rate limiter
	log     *slog.Logger
	now     func() time.Time
	limiter *limiter
}

// New builds a Server with all routes registered.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("api: store is required")
	}
	if cfg.Wallet == nil {
		return nil, errors.New("api: wallet is required")
	}
	if cfg.Matcher == nil {
		return nil, errors.New("api: matcher is required")
	}
	if cfg.OrderTTL <= 0 {
		cfg.OrderTTL = 30 * time.Minute
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	// Rate-limit classes are fixed at construction: merging the caller's
	// overrides over the defaults here means a request never has to decide
	// what its limit is, and a bad map cannot change behaviour mid-flight.
	limits := make(map[RateLimitClass]RateLimit, len(DefaultRateLimits))
	for k, v := range DefaultRateLimits {
		limits[k] = v
	}
	for k, v := range cfg.RateLimits {
		limits[k] = v
	}
	if cfg.IdempotencyRetention <= 0 {
		cfg.IdempotencyRetention = store.DefaultIdempotencyRetention
	}
	s.cfg = cfg
	s.limiter = newLimiter(limits, s.now)
	s.routes()
	s.h = s.rateLimit(s.mux)
	return s, nil
}

// ServeHTTP routes the request. Kept thin: routes() owns the pattern table.
// The rate limiter sits outside the mux, so it sees every path before any
// handler (and before any authentication work) runs.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.h.ServeHTTP(w, r)
}

// Handler returns the rate-limited handler (for httptest.NewServer and
// cmd/paisabridge, which wraps it in request logging).
func (s *Server) Handler() http.Handler { return s.h }

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

// apiError is the wire shape of every error this package writes, and it is the
// shape docs/API.md promises:
//
//	{"error": "human readable message", "code": "machine_code"}
//
// `code` is the machine token (e.g. "not_found", "rate_limited") and `error`
// is the human sentence; `detail`, when present, is extra machine-friendly
// context (the offending value, the store's message). The HTTP status stays
// readable through the response line, so it is not repeated in the body.
//
// The field order matters to readers: error first, code second, exactly as
// documented.
type apiError struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr writes an error response in the documented envelope. status is the
// HTTP code; code is the machine-readable token clients switch on (see the
// table in docs/API.md). Optional detail strings are joined into `detail`.
func writeErr(w http.ResponseWriter, status int, code string, detail ...string) {
	e := apiError{Error: humanError(code), Code: code}
	if len(detail) > 0 {
		e.Detail = strings.Join(detail, "; ")
	}
	writeJSON(w, status, e)
}

// errorMessages maps every machine code this package emits to the sentence put
// in `error`. Keeping it in one place means a client developer can read the
// whole vocabulary at once, and the same condition always reads the same way.
var errorMessages = map[string]string{
	"invalid_json":            "request body is not valid JSON for this endpoint",
	"invalid_request":         "request is missing a required field or has an invalid value",
	"missing_product_id":      "product_id is required",
	"invalid_price":           "catalog_price_minor must be a positive integer amount in minor units",
	"unknown_method":          "method is not one of the supported rails",
	"method_disabled":         "method is not enabled on this deployment",
	"no_unique_amount":        "no unique amount is left for this rail; retry in a few minutes",
	"not_found":               "no such order",
	"unauthorized":            "bad or missing admin token",
	"admin_disabled":          "admin endpoints are disabled because no admin token is configured",
	"insufficient_funds":      "wallet balance does not cover this order",
	"claim_failed":            "the order cannot be paid from the wallet in its current state",
	"not_cancellable":         "the order is already in a terminal state",
	"invalid_amount":          "amount_minor must be a positive integer amount in minor units",
	"missing_idempotency_key": "idempotency_key is required so retries cannot double-credit",
	"missing_order_id":        "order_id is required",
	"approve_failed":          "the order could not be approved",
	"not_approvable":          "the order is not in a state that manual approval can move forward",
	"no_adapter":              "no adapter is registered for this rail",
	"method_mismatch":         "the payment notice names a different rail than the URL",
	"empty_notice":            "the adapter returned no payment notice",
	"invalid_signature":       "the provider signature did not verify",
	"request_cancelled":       "the client went away before the request finished",
	"internal":                "internal error; the request may be retried",
	"idempotency_in_flight":   "another request with this idempotency_key is still being processed",
	"idempotency_conflict":    "this idempotency_key was already used with different request content",
	rateLimitedCode:           "too many requests; retry after the number of seconds in Retry-After",
}

// humanError returns the sentence for code, falling back to the code itself so
// a newly added code can never produce an empty message.
func humanError(code string) string {
	if msg, ok := errorMessages[code]; ok {
		return msg
	}
	return code
}

// decode reads a JSON body with a size cap and rejects unknown fields so a
// typo in a client (`amount` instead of `amount_minor`) fails loudly instead of
// silently charging nothing.
func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		writeErr(w, http.StatusNotFound, "admin_disabled")
		return false
	}
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if got != s.cfg.AdminToken {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// ctx returns a request-scoped context bounded by the request lifetime.
func ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}
