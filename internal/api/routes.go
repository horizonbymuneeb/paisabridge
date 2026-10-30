// Package api is the paisabridge HTTP surface. It uses only the standard
// library's Go 1.22 method patterns (net/http.ServeMux) — no router
// dependency, no middleware framework.
//
// # HTTP surface
//
// Public (no auth):
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

	// Notify, when set, is called after an order reaches a customer-visible
	// state (fulfilled, review, expired, cancelled). It runs in a goroutine so
	// a slow integrator endpoint can never delay a webhook acknowledgement.
	// Errors are the implementation's concern; the API only logs them.
	Notify func(ctx context.Context, event string, order *types.Order)
}

// Server holds the handlers. One Server == one *http.ServeMux.
type Server struct {
	cfg Config
	mux *http.ServeMux
	log *slog.Logger
	now func() time.Time
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
	s.routes()
	return s, nil
}

// ServeHTTP routes the request. Kept thin: routes() owns the pattern table.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Handler returns the mux directly (for httptest.NewServer).
func (s *Server) Handler() http.Handler { return s.mux }

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

type apiError struct {
	Code   int    `json:"-"`
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string, detail ...string) {
	e := apiError{Code: code, Error: msg}
	if len(detail) > 0 {
		e.Detail = detail[0]
	}
	writeJSON(w, code, e)
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
