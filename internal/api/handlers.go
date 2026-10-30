package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/version"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

// ---------------------------------------------------------------------------
// healthz
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctx(r)
	defer cancel()

	resp := map[string]any{
		"status":  "ok",
		"version": version.Get(),
		"time":    s.now().UTC().Format(time.RFC3339Nano),
		"db":      "ok",
	}
	if err := s.cfg.Store.Ping(ctx); err != nil {
		resp["status"], resp["db"] = "degraded", "error: "+err.Error()
		writeJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// orders
// ---------------------------------------------------------------------------

type createOrderRequest struct {
	ProductID     string `json:"product_id"`
	ProductName   string `json:"product_name"`
	CatalogPrice  int64  `json:"catalog_price_minor"`
	Currency      string `json:"currency"`
	Method        string `json:"method"`
	UserID        string `json:"user_id"`
	CustomerEmail string `json:"customer_email"`
	CustomerPhone string `json:"customer_phone"`
	TTLSeconds    int64  `json:"ttl_seconds"`
}

func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var req createOrderRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ProductID == "" {
		writeErr(w, http.StatusBadRequest, "missing_product_id")
		return
	}
	if req.CatalogPrice <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_price", "catalog_price_minor must be positive")
		return
	}
	method := types.Method(req.Method)
	if req.Method == "" {
		method = types.MethodEasypaisa
	}
	if !KnownMethod(string(method)) {
		writeErr(w, http.StatusBadRequest, "unknown_method", string(method))
		return
	}
	if !s.methodEnabled(method) {
		writeErr(w, http.StatusBadRequest, "method_disabled", string(method))
		return
	}
	ttl := s.cfg.OrderTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > 24*time.Hour {
			ttl = 24 * time.Hour
		}
	}

	ctx, cancel := ctx(r)
	defer cancel()

	ord, err := s.cfg.Store.CreateOrder(ctx, store.CreateOrderInput{
		ProductID:     req.ProductID,
		ProductName:   req.ProductName,
		CatalogPrice:  req.CatalogPrice,
		Currency:      req.Currency,
		Method:        method,
		UserID:        req.UserID,
		CustomerEmail: req.CustomerEmail,
		CustomerPhone: req.CustomerPhone,
		TTL:           ttl,
	})
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) || strings.Contains(err.Error(), "window exhausted") {
			writeErr(w, http.StatusConflict, "no_unique_amount", err.Error())
			return
		}
		s.log.Error("create order", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "could not create order")
		return
	}
	s.log.Info("order created", "id", ord.ID, "amount", ord.Amount, "method", ord.Method)
	writeJSON(w, http.StatusCreated, ord)
}

func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := ctx(r)
	defer cancel()

	ord, err := s.cfg.Store.Order(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	events, err := s.cfg.Store.Events(ctx, ord.ID)
	if err != nil {
		events = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"order":                             ord,
		"events":                            events,
		"wallet_discount_bps":               s.cfg.Wallet.DiscountBPS(),
		"wallet_price_after_discount_minor": ord.CatalogPrice - s.cfg.Wallet.Discount(ord.CatalogPrice),
	})
}

type orderCreditRequest struct {
	UserID string `json:"user_id"`
}

func (s *Server) handleClaimOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req orderCreditRequest
	// An empty body is valid: default user.
	// An empty body is valid (default wallet user); anything else must parse.
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	res, err := s.cfg.Wallet.PayOrder(ctx, req.UserID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, wallet.ErrInsufficientFunds):
		writeErr(w, http.StatusPaymentRequired, "insufficient_funds", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, "claim_failed", err.Error())
		return
	}
	ord, _ := s.cfg.Store.Order(ctx, id)
	res.Order = ord
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req orderCreditRequest
	// An empty body is valid (default wallet user); anything else must parse.
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	before, err := s.cfg.Store.Order(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	ord, err := s.cfg.Wallet.CancelOrder(ctx, s.cfg.Store, req.UserID, before.ID, "customer", "cancelled by customer")
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoTransition):
			writeErr(w, http.StatusConflict, "not_cancellable", err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	refunded, err := s.refunded(ctx, before.ID)
	if err != nil {
		refunded = false
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": ord, "refunded": refunded})
}

// refunded reports whether the ledger holds a refund row for this order.
// Cancelling an unpaid order is not an error, so the response says so instead
// of implying money moved.
func (s *Server) refunded(ctx context.Context, orderID string) (bool, error) {
	rows, err := s.cfg.Store.DB().QueryContext(ctx,
		`SELECT 1 FROM wallet_ledger WHERE kind = ? AND order_id = ? LIMIT 1`, wallet.KindRefund, orderID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// ---------------------------------------------------------------------------
// webhooks
// ---------------------------------------------------------------------------

// notify fires the optional outbound notification for a state change. It never
// blocks the HTTP handler: a webhook sender that is down must not turn a
// successful payment into a 5xx that the provider then retries.
func (s *Server) notify(event string, o *types.Order) {
	if s.cfg.Notify == nil || o == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.cfg.Notify(ctx, event, o)
	}()
}

type webhookResponse struct {
	Matched  bool   `json:"matched"`
	OrderID  string `json:"order_id,omitempty"`
	Reason   string `json:"reason"`
	Credited bool   `json:"credited"`
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("method")
	method := types.Method(name)
	if !KnownMethod(name) {
		writeErr(w, http.StatusBadRequest, "unknown_method", name)
		return
	}
	if !s.methodEnabled(method) {
		writeErr(w, http.StatusBadRequest, "method_disabled", name)
		return
	}
	if s.cfg.Adapters == nil {
		writeErr(w, http.StatusBadRequest, "no_adapter", "no adapter registry wired for "+name)
		return
	}
	ad := s.cfg.Adapters.Get(method)
	if ad == nil {
		writeErr(w, http.StatusBadRequest, "no_adapter", "no adapter registered for "+name)
		return
	}

	notice, err := ad.VerifyWebhook(r)
	if err != nil {
		// Signature failures are 401 and never reach the matcher.
		writeErr(w, http.StatusUnauthorized, "invalid_signature", err.Error())
		return
	}
	if notice == nil {
		writeErr(w, http.StatusBadRequest, "empty_notice")
		return
	}
	if notice.Method == "" {
		notice.Method = method
	}
	if notice.Method != method {
		writeErr(w, http.StatusBadRequest, "method_mismatch", string(notice.Method))
		return
	}

	ctx, cancel := ctx(r)
	defer cancel()

	out, err := s.cfg.Matcher.Process(ctx, notice)
	if err != nil {
		s.log.Error("webhook process", "method", name, "txn", notice.TxnRef, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "could not process payment")
		return
	}

	resp := webhookResponse{Matched: out.Matched, OrderID: out.OrderID, Reason: out.Reason, Credited: out.Matched}
	if out.Matched && out.OrderID != "" {
		// Verified orders are settled: the seller fulfils from the verified
		// queue, and the transition to fulfilled is its own audited step.
		if _, err := s.cfg.Store.TransitionOrder(ctx, out.OrderID, types.StatusVerified, types.StatusFulfilled, "fulfiller", "delivered after verified payment"); err != nil &&
			!errors.Is(err, store.ErrNoTransition) {
			s.log.Error("fulfil", "order", out.OrderID, "err", err)
		}
		if full, err := s.cfg.Store.Order(ctx, out.OrderID); err == nil {
			s.notify(outboundEvent(types.StatusFulfilled), full)
		}
	}
	s.log.Info("webhook", "method", name, "txn", notice.TxnRef, "amount", notice.Amount,
		"matched", out.Matched, "order", out.OrderID, "reason", out.Reason)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// wallet
// ---------------------------------------------------------------------------

func (s *Server) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("user_id")
	ctx, cancel := ctx(r)
	defer cancel()

	bal, err := s.cfg.Wallet.Balance(ctx, uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 25)
	entries, err := s.cfg.Wallet.History(ctx, uid, limit)
	if err != nil {
		entries = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":         bal.UserID,
		"currency":        bal.Currency,
		"available_minor": bal.Available,
		"discount_bps":    bal.DiscountBPS,
		"entries":         entries,
	})
}

type topupRequest struct {
	UserID         string `json:"user_id"`
	AmountMinor    int64  `json:"amount_minor"`
	IdempotencyKey string `json:"idempotency_key"`
	Memo           string `json:"memo"`
}

func (s *Server) handleTopup(w http.ResponseWriter, r *http.Request) {
	var req topupRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.AmountMinor <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_amount", "amount_minor must be positive")
		return
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		writeErr(w, http.StatusBadRequest, "missing_idempotency_key",
			"idempotency_key is required so retries cannot double-credit")
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	bal, err := s.cfg.Wallet.Topup(ctx, req.UserID, req.AmountMinor, req.IdempotencyKey, req.Memo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bal)
}

// ---------------------------------------------------------------------------
// notices / admin
// ---------------------------------------------------------------------------

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	notices, err := s.cfg.Store.ListNotices(ctx, r.URL.Query().Get("status"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": notices, "count": len(notices)})
}

type approveRequest struct {
	OrderID string `json:"order_id"`
	TxnRef  string `json:"txn_ref"`
	Memo    string `json:"memo"`
}

func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req approveRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.OrderID) == "" {
		writeErr(w, http.StatusBadRequest, "missing_order_id")
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	ord, err := s.cfg.Store.Order(ctx, req.OrderID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out, err := s.cfg.Matcher.Approve(ctx, ord, req.TxnRef, "admin")
	if err != nil {
		writeErr(w, http.StatusConflict, "approve_failed", err.Error())
		return
	}
	if !out.Matched {
		writeErr(w, http.StatusConflict, "not_approvable", out.Reason)
		return
	}
	if _, err := s.cfg.Store.TransitionOrder(ctx, ord.ID, types.StatusVerified, types.StatusFulfilled, "admin", "approved and delivered"); err != nil &&
		!errors.Is(err, store.ErrNoTransition) && !errors.Is(err, store.ErrTransitionInvalid) {
		s.log.Error("approve fulfil", "order", ord.ID, "err", err)
	}
	final, err := s.cfg.Store.Order(ctx, ord.ID)
	if err != nil {
		final = ord
	} else {
		s.notify(outboundEvent(types.StatusFulfilled), final)
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": final, "outcome": out})
}

func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	// The store list does not filter by status; filter here for triage.
	orders, err := s.cfg.Store.ListOrders(ctx, "", limit*4)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if status := r.URL.Query().Get("status"); status != "" {
		filtered := orders[:0]
		for _, o := range orders {
			if string(o.Status) == status {
				filtered = append(filtered, o)
			}
		}
		orders = filtered
	}
	if len(orders) > limit {
		orders = orders[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "count": len(orders)})
}

// ---------------------------------------------------------------------------
// tiny helpers
// ---------------------------------------------------------------------------

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// match.Reason* re-exported for clients that switch on them.
var _ = []string{
	match.ReasonMatched, match.ReasonDuplicateTxn, match.ReasonAmbiguous,
	match.ReasonExpired, match.ReasonNoOrder,
}

// outboundEvent maps an order status to the event name shipped to integrators.
func outboundEvent(st types.OrderStatus) string {
	switch st {
	case types.StatusFulfilled:
		return "order.fulfilled"
	case types.StatusReview:
		return "order.review"
	case types.StatusExpired:
		return "order.expired"
	case types.StatusCancelled:
		return "order.cancelled"
	case types.StatusVerified:
		return "order.verified"
	default:
		return "order." + string(st)
	}
}
