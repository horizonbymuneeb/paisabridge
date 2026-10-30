package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/api"
	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

const testSecret = "test-webhook-secret"
const adminToken = "test-admin-token"

// fakeAdapter is a signed webhook rail for tests. It mirrors what a real
// adapter does: verify HMAC over the raw body, then decode into a notice.
// The adapters teammate's implementations must behave equivalently.
type fakeAdapter struct {
	method types.Method
	secret string
	// failSignature simulates a forged/replayed body.
	failSignature bool
	calls         int
	mu            sync.Mutex
}

func (f *fakeAdapter) Method() types.Method { return f.method }
func (f *fakeAdapter) Supports() bool       { return true }

func (f *fakeAdapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.failSignature {
		return nil, adapter.ErrInvalidSignature
	}
	body, err := adapter.HMACVerifier(f.secret, r)
	if err != nil {
		return nil, err
	}
	var payload struct {
		TxnRef     string `json:"txn_id"`
		Amount     int64  `json:"amount_minor"`
		Currency   string `json:"currency"`
		SenderName string `json:"sender_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.TxnRef == "" {
		return nil, errors.New("missing txn_id")
	}
	return &types.PaymentNotice{
		TxnRef:     payload.TxnRef,
		Method:     f.method,
		Amount:     payload.Amount,
		Currency:   payload.Currency,
		SenderName: payload.SenderName,
	}, nil
}

func (f *fakeAdapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

type registry map[types.Method]adapter.PaymentAdapter

func (r registry) Get(m types.Method) adapter.PaymentAdapter { return r[m] }

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	store   *store.Store
	wallet  *wallet.Wallet
	matcher *match.Matcher
	ep      *fakeAdapter
	jp      *fakeAdapter
	now     func() time.Time
}

// newHarness starts a full HTTP test server over a fresh SQLite file.
// Optional tweaks run after the defaults, so a test can change one Config
// field (a rate limit, the clock) without copying the whole wiring.
func newHarness(t *testing.T, tweaks ...func(*api.Config)) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	w := wallet.New(s)
	m := match.New(s)
	ep := &fakeAdapter{method: types.MethodEasypaisa, secret: testSecret}
	jp := &fakeAdapter{method: types.MethodJazzCash, secret: "jazz-secret"}

	cfg := api.Config{
		Store: s, Wallet: w, Matcher: m,
		Adapters:   registry{types.MethodEasypaisa: ep, types.MethodJazzCash: jp},
		AdminToken: adminToken,
		OrderTTL:   15 * time.Minute,
	}
	for _, fn := range tweaks {
		fn(&cfg)
	}
	apiSrv, err := api.New(cfg)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	ts := httptest.NewServer(apiSrv)
	t.Cleanup(ts.Close)

	return &harness{t: t, srv: ts, store: s, wallet: w, matcher: m, ep: ep, jp: jp}
}

func (h *harness) do(method, path string, body any, headers map[string]string) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func decodeInto(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decode %s: %v", string(b), err)
	}
}

// signedBody computes the HMAC exactly as adapter.HMACVerifier expects.
func signedBody(secret string, payload any) ([]byte, map[string]string) {
	b, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(b)
	return b, map[string]string{"X-Signature": hex.EncodeToString(mac.Sum(nil))}
}

func (h *harness) postSigned(method types.Method, secret string, payload any) (int, []byte) {
	h.t.Helper()
	b, hdrs := signedBody(secret, payload)
	req, err := http.NewRequest("POST", h.srv.URL+"/api/webhooks/"+string(method), bytes.NewReader(b))
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) createOrder(price int64) *types.Order {
	h.t.Helper()
	code, body := h.do("POST", "/api/orders", map[string]any{
		"product_id":          "netflix-1m",
		"product_name":        "Netflix 1 month",
		"catalog_price_minor": price,
		"currency":            "PKR",
		"method":              "easypaisa",
		"customer_email":      "buyer@example.com",
	}, nil)
	if code != http.StatusCreated {
		h.t.Fatalf("create order: %d %s", code, body)
	}
	var o types.Order
	decodeInto(h.t, body, &o)
	return &o
}

func (h *harness) walletBalance(userID string) int64 {
	h.t.Helper()
	code, body := h.do("GET", "/api/wallet?user_id="+userID, nil, nil)
	if code != http.StatusOK {
		h.t.Fatalf("wallet: %d %s", code, body)
	}
	var w struct {
		Available int64 `json:"available_minor"`
	}
	decodeInto(h.t, body, &w)
	return w.Available
}

// ---------------------------------------------------------------------------
// the required end-to-end test
// ---------------------------------------------------------------------------

func TestEndToEndCreateWebhookVerifyFulfil(t *testing.T) {
	h := newHarness(t)

	// 1. Create the order through HTTP.
	ord := h.createOrder(150000)
	if ord.Status != types.StatusPendingPayment {
		t.Fatalf("new order status = %s, want pending_payment", ord.Status)
	}
	if ord.Amount <= 0 {
		t.Fatalf("order has no amount")
	}

	// 2. Top up the buyer's wallet (so we can prove the ledger end to end).
	code, body := h.do("POST", "/api/wallet/topup", map[string]any{
		"user_id": "buyer-1", "amount_minor": 200000, "idempotency_key": "top-1",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("topup: %d %s", code, body)
	}

	// 3. A signed webhook for exactly the unique amount.
	code, body = h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "EP-88231", "amount_minor": ord.Amount, "currency": "PKR", "sender_name": "Ali Raza",
	})
	if code != http.StatusOK {
		t.Fatalf("webhook: %d %s", code, body)
	}
	var hook struct {
		Matched  bool   `json:"matched"`
		OrderID  string `json:"order_id"`
		Reason   string `json:"reason"`
		Credited bool   `json:"credited"`
	}
	decodeInto(t, body, &hook)
	if !hook.Matched || hook.OrderID != ord.ID || hook.Reason != match.ReasonMatched {
		t.Fatalf("webhook outcome = %+v, want matched %s", hook, ord.ID)
	}
	if !hook.Credited {
		t.Fatal("matched webhook reported credited=false")
	}

	// 4. The order must have gone verified -> fulfilled.
	code, body = h.do("GET", "/api/orders/"+ord.ID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("get order: %d %s", code, body)
	}
	var view struct {
		Order  types.Order        `json:"order"`
		Events []store.OrderEvent `json:"events"`
	}
	decodeInto(t, body, &view)
	if view.Order.Status != types.StatusFulfilled {
		t.Fatalf("final status = %s, want fulfilled", view.Order.Status)
	}
	if view.Order.TxRef != "EP-88231" {
		t.Fatalf("tx_ref = %q, want EP-88231", view.Order.TxRef)
	}
	// The journal must show the whole chain.
	var chain []string
	for _, e := range view.Events {
		chain = append(chain, string(e.ToState))
	}
	want := []string{"pending_payment", "verified", "fulfilled"}
	if len(chain) != len(want) {
		t.Fatalf("journal = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("journal = %v, want %v", chain, want)
		}
	}

	// 5. Wallet balance is exactly what was topped up (webhook money does not
	//    touch the wallet — it was paid directly on the rail).
	if got := h.walletBalance("buyer-1"); got != 200000 {
		t.Fatalf("wallet = %d, want 200000", got)
	}
}

func TestDuplicateWebhookDoesNotDoubleCredit(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(80000)

	payload := map[string]any{
		"txn_id": "EP-DUP-1", "amount_minor": ord.Amount, "currency": "PKR", "sender_name": "Sara",
	}
	code, body := h.postSigned(types.MethodEasypaisa, testSecret, payload)
	if code != http.StatusOK {
		t.Fatalf("first webhook: %d %s", code, body)
	}
	var first struct {
		Matched bool `json:"matched"`
	}
	decodeInto(t, body, &first)
	if !first.Matched {
		t.Fatalf("first webhook did not match: %s", body)
	}

	// The provider retries the identical body four times.
	for i := 0; i < 4; i++ {
		code, body = h.postSigned(types.MethodEasypaisa, testSecret, payload)
		if code != http.StatusOK {
			t.Fatalf("retry %d: status %d %s (a replay must be a clean 200)", i, code, body)
		}
		var again struct {
			Matched  bool   `json:"matched"`
			Reason   string `json:"reason"`
			Credited bool   `json:"credited"`
		}
		decodeInto(t, body, &again)
		if again.Matched || again.Credited {
			t.Fatalf("retry %d was credited again: %+v", i, again)
		}
		if again.Reason != match.ReasonDuplicateTxn {
			t.Fatalf("retry %d reason = %q, want %q", i, again.Reason, match.ReasonDuplicateTxn)
		}
	}

	// Exactly one fulfilment event, one notice row, and no wallet movement.
	_, body = h.do("GET", "/api/orders/"+ord.ID, nil, nil)
	var view struct {
		Order  types.Order        `json:"order"`
		Events []store.OrderEvent `json:"events"`
	}
	decodeInto(t, body, &view)
	fulfilled := 0
	for _, e := range view.Events {
		if e.ToState == types.StatusFulfilled {
			fulfilled++
		}
	}
	if fulfilled != 1 {
		t.Fatalf("fulfilled recorded %d times after replays, want 1", fulfilled)
	}
	var notices int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM payment_notices WHERE txn_ref = ?`, "EP-DUP-1").Scan(&notices); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if notices != 1 {
		t.Fatalf("%d notice rows, want 1", notices)
	}
}

func TestExpiredOrderRefunded(t *testing.T) {
	h := newHarness(t)

	// Short-TTL order, paid from the wallet, then abandoned.
	code, body := h.do("POST", "/api/orders", map[string]any{
		"product_id": "short", "catalog_price_minor": 50000, "currency": "PKR",
		"method": "easypaisa", "ttl_seconds": 1,
	}, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var ord types.Order
	decodeInto(t, body, &ord)

	if code, body := h.do("POST", "/api/wallet/topup", map[string]any{
		"user_id": "buyer-2", "amount_minor": 100000, "idempotency_key": "top-2",
	}, nil); code != http.StatusOK {
		t.Fatalf("topup: %d %s", code, body)
	}

	// Claim it from the wallet: 5% off 50000 => 47500 charged, 52500 left.
	code, body = h.do("POST", "/api/orders/"+ord.ID+"/claim", map[string]any{"user_id": "buyer-2"}, nil)
	if code != http.StatusOK {
		t.Fatalf("claim: %d %s", code, body)
	}
	var claim struct {
		ChargedMinor  int64 `json:"charged_minor"`
		DiscountMinor int64 `json:"discount_minor"`
	}
	decodeInto(t, body, &claim)
	if claim.DiscountMinor != 2500 || claim.ChargedMinor != 47500 {
		t.Fatalf("claim discount/charged = %d/%d, want 2500/47500", claim.DiscountMinor, claim.ChargedMinor)
	}
	if got := h.walletBalance("buyer-2"); got != 52500 {
		t.Fatalf("wallet after claim = %d, want 52500", got)
	}

	// Time passes and the delivery window closes: verified -> review -> expired,
	// then the sweeper refunds.
	if _, err := h.store.TransitionOrder(context.Background(), ord.ID, types.StatusVerified, types.StatusReview, "system", "delivery window elapsed"); err != nil {
		t.Fatalf("to review: %v", err)
	}
	if _, err := h.store.TransitionOrder(context.Background(), ord.ID, types.StatusReview, types.StatusExpired, "expiry", "unfulfilled"); err != nil {
		t.Fatalf("to expired: %v", err)
	}
	if _, err := h.wallet.Refund(context.Background(), "buyer-2", ord.ID, "order expired"); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if got := h.walletBalance("buyer-2"); got != 100000 {
		t.Fatalf("wallet after refund = %d, want 100000", got)
	}

	// Refunding again (sweeper re-run / double click) must not add money.
	for i := 0; i < 3; i++ {
		if _, err := h.wallet.Refund(context.Background(), "buyer-2", ord.ID, "order expired"); err != nil {
			t.Fatalf("replay refund: %v", err)
		}
	}
	if got := h.walletBalance("buyer-2"); got != 100000 {
		t.Fatalf("wallet after replayed refunds = %d, want 100000", got)
	}

	// A late webhook for an expired order must not match.
	code, body = h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "EP-LATE-1", "amount_minor": ord.Amount, "currency": "PKR",
	})
	if code != http.StatusOK {
		t.Fatalf("late webhook: %d %s", code, body)
	}
	var late struct {
		Matched bool   `json:"matched"`
		Reason  string `json:"reason"`
	}
	decodeInto(t, body, &late)
	if late.Matched {
		t.Fatal("late webhook matched an expired order")
	}
}

func TestPendingOrderExpirySweep(t *testing.T) {
	// Unpaid order that simply times out: the sweeper expires it, nothing is
	// refunded, and the amount is released for reuse.
	h := newHarness(t)
	code, body := h.do("POST", "/api/orders", map[string]any{
		"product_id": "sweep", "catalog_price_minor": 1000, "currency": "PKR",
		"method": "jazzcash", "ttl_seconds": 1,
	}, nil)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var ord types.Order
	decodeInto(t, body, &ord)

	time.Sleep(1100 * time.Millisecond)
	expired, err := h.wallet.RefundExpired(context.Background(), h.store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	found := false
	for _, o := range expired {
		if o.ID == ord.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("sweeper did not expire %s (expired %d orders)", ord.ID, len(expired))
	}
	_, body = h.do("GET", "/api/orders/"+ord.ID, nil, nil)
	var view struct {
		Order types.Order `json:"order"`
	}
	decodeInto(t, body, &view)
	if view.Order.Status != types.StatusExpired {
		t.Fatalf("status = %s, want expired", view.Order.Status)
	}
}

// ---------------------------------------------------------------------------
// surface / validation tests
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("GET", "/api/healthz", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("healthz: %d %s", code, body)
	}
	var resp map[string]any
	decodeInto(t, body, &resp)
	if resp["status"] != "ok" || resp["db"] != "ok" {
		t.Fatalf("healthz body = %v", resp)
	}
}

func TestWebhookBadSignatureRejected(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(30000)

	code, body := h.postSigned(types.MethodEasypaisa, "wrong-secret", map[string]any{
		"txn_id": "FORGED", "amount_minor": ord.Amount, "currency": "PKR",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("forged webhook: %d %s, want 401", code, body)
	}
	// Unchanged order.
	_, body = h.do("GET", "/api/orders/"+ord.ID, nil, nil)
	var view struct {
		Order types.Order `json:"order"`
	}
	decodeInto(t, body, &view)
	if view.Order.Status != types.StatusPendingPayment {
		t.Fatalf("forged webhook changed order to %s", view.Order.Status)
	}
}

func TestWebhookUnsignedRejected(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(31000)
	code, body := h.do("POST", "/api/webhooks/easypaisa", map[string]any{
		"txn_id": "NOSIG", "amount_minor": ord.Amount, "currency": "PKR",
	}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d %s, want 401", code, body)
	}
}

func TestWebhookUnknownMethod(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do("POST", "/api/webhooks/paytmm", map[string]any{}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown method: %d, want 400", code)
	}
}

func TestMethodNotAllowedUsesGo122Mux(t *testing.T) {
	h := newHarness(t)
	// DELETE on a GET/POST route must be 405, not 404 — that comes from the
	// Go 1.22 method patterns.
	code, _ := h.do("DELETE", "/api/orders", nil, nil)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/orders: %d, want 405", code)
	}
}

func TestOrderNotFound(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do("GET", "/api/orders/does-not-exist", nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing order: %d, want 404", code)
	}
}

func TestCreateOrderValidation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing product", map[string]any{"catalog_price_minor": 100}, http.StatusBadRequest},
		{"zero price", map[string]any{"product_id": "p", "catalog_price_minor": 0}, http.StatusBadRequest},
		{"unknown method", map[string]any{"product_id": "p", "catalog_price_minor": 100, "method": "paytmm"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"product_id": "p", "catalog_price_minor": 100, "amount": 5}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, body := h.do("POST", "/api/orders", tc.body, nil)
		if code != tc.want {
			t.Fatalf("%s: %d %s, want %d", tc.name, code, body, tc.want)
		}
	}
}

func TestTopupIdempotencyOverHTTP(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"user_id": "buyer-3", "amount_minor": 7000, "idempotency_key": "same"}
	for i := 0; i < 3; i++ {
		code, resp := h.do("POST", "/api/wallet/topup", body, nil)
		if code != http.StatusOK {
			t.Fatalf("topup %d: %d %s", i, code, resp)
		}
	}
	if got := h.walletBalance("buyer-3"); got != 7000 {
		t.Fatalf("wallet = %d after 3 identical topups, want 7000", got)
	}

	// Missing idempotency key is rejected outright.
	code, _ := h.do("POST", "/api/wallet/topup", map[string]any{"user_id": "buyer-3", "amount_minor": 10}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("topup without key: %d, want 400", code)
	}
}

func TestClaimInsufficientFundsIs402(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(90000)
	code, body := h.do("POST", "/api/orders/"+ord.ID+"/claim", map[string]any{"user_id": "broke"}, nil)
	if code != http.StatusPaymentRequired {
		t.Fatalf("claim without funds: %d %s, want 402", code, body)
	}
}

func TestClaimTwiceChargesOnce(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(100000)
	if code, body := h.do("POST", "/api/wallet/topup", map[string]any{
		"user_id": "buyer-4", "amount_minor": 200000, "idempotency_key": "t",
	}, nil); code != http.StatusOK {
		t.Fatalf("topup: %d %s", code, body)
	}
	code, body := h.do("POST", "/api/orders/"+ord.ID+"/claim", map[string]any{"user_id": "buyer-4"}, nil)
	if code != http.StatusOK {
		t.Fatalf("claim: %d %s", code, body)
	}
	var res struct {
		ChargedMinor int64 `json:"charged_minor"`
	}
	decodeInto(t, body, &res)
	if res.ChargedMinor != 95000 {
		t.Fatalf("charged = %d, want 95000 (5%% off)", res.ChargedMinor)
	}
	// Double-clicked claim.
	code, body = h.do("POST", "/api/orders/"+ord.ID+"/claim", map[string]any{"user_id": "buyer-4"}, nil)
	if code != http.StatusOK {
		t.Fatalf("second claim: %d %s, want 200", code, body)
	}
	var again struct {
		AlreadyPaid bool `json:"already_paid"`
	}
	decodeInto(t, body, &again)
	if !again.AlreadyPaid {
		t.Fatal("second claim should report already_paid")
	}
	if got := h.walletBalance("buyer-4"); got != 105000 {
		t.Fatalf("wallet = %d, want 105000 (double charge)", got)
	}
}

func TestCancelEndpoint(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(45000)
	code, body := h.do("POST", "/api/orders/"+ord.ID+"/cancel", map[string]any{}, nil)
	if code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, body)
	}
	var res struct {
		Order    types.Order `json:"order"`
		Refunded bool        `json:"refunded"`
	}
	decodeInto(t, body, &res)
	if res.Order.Status != types.StatusCancelled {
		t.Fatalf("status = %s, want cancelled", res.Order.Status)
	}
	if res.Refunded {
		t.Fatal("unpaid order reported as refunded")
	}
	// Cancelling again is a conflict, not a second cancellation.
	code, _ = h.do("POST", "/api/orders/"+ord.ID+"/cancel", map[string]any{}, nil)
	if code != http.StatusConflict {
		t.Fatalf("second cancel: %d, want 409", code)
	}
}

func TestAdminApproveRequiresToken(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(52000)
	code, _ := h.do("POST", "/api/admin/approve", map[string]any{"order_id": ord.ID}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	code, _ = h.do("POST", "/api/admin/approve", map[string]any{"order_id": ord.ID},
		map[string]string{"X-Admin-Token": "wrong"})
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", code)
	}
}

func TestAdminApproveFlow(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(62000)

	// Put it in review the way an ambiguous/late payment would.
	if _, err := h.store.TransitionOrder(context.Background(), ord.ID, types.StatusPendingPayment, types.StatusReview, "matcher", "ambiguous amount"); err != nil {
		t.Fatalf("to review: %v", err)
	}

	code, body := h.do("POST", "/api/admin/approve", map[string]any{
		"order_id": ord.ID, "txn_ref": "MANUAL-1", "memo": "verified on bank statement",
	}, map[string]string{"X-Admin-Token": adminToken})
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	var res struct {
		Order types.Order `json:"order"`
	}
	decodeInto(t, body, &res)
	if res.Order.Status != types.StatusFulfilled {
		t.Fatalf("approved order status = %s, want fulfilled", res.Order.Status)
	}
	if res.Order.TxRef != "MANUAL-1" {
		t.Fatalf("tx_ref = %q", res.Order.TxRef)
	}

	// Second approve is a conflict (already terminal), never a double fulfil.
	code, _ = h.do("POST", "/api/admin/approve", map[string]any{"order_id": ord.ID},
		map[string]string{"X-Admin-Token": adminToken})
	if code != http.StatusConflict {
		t.Fatalf("second approve: %d, want 409", code)
	}
}

func TestAdminOrdersAndNotices(t *testing.T) {
	h := newHarness(t)
	ord := h.createOrder(72000)
	// Produce one unmatched notice.
	if code, body := h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "ORPHAN-1", "amount_minor": 12345, "currency": "PKR",
	}); code != http.StatusOK {
		t.Fatalf("orphan webhook: %d %s", code, body)
	}

	code, body := h.do("GET", "/api/admin/orders?status=pending_payment", nil,
		map[string]string{"X-Admin-Token": adminToken})
	if code != http.StatusOK {
		t.Fatalf("admin orders: %d %s", code, body)
	}
	var list struct {
		Count int `json:"count"`
	}
	decodeInto(t, body, &list)
	if list.Count == 0 {
		t.Fatal("admin orders returned nothing for a fresh pending order")
	}
	_ = ord

	code, body = h.do("GET", "/api/notices?status=unmatched", nil,
		map[string]string{"X-Admin-Token": adminToken})
	if code != http.StatusOK {
		t.Fatalf("notices: %d %s", code, body)
	}
	var notices struct {
		Notices []map[string]any `json:"notices"`
	}
	decodeInto(t, body, &notices)
	if len(notices.Notices) == 0 {
		t.Fatal("orphan payment was not recorded as an unmatched notice")
	}
}

func TestAmbiguousAmountRequiresAdmin(t *testing.T) {
	h := newHarness(t)
	// Two open orders on the same rail never share an amount (partial unique
	// index), so the only way an ambiguous payment happens is a stray payment
	// whose amount matches nothing exactly. It must be recorded, unmatched,
	// and approvable by hand.
	ord := h.createOrder(93000)
	if code, body := h.postSigned(types.MethodEasypaisa, testSecret, map[string]any{
		"txn_id": "STRAY-1", "amount_minor": ord.Amount + 7, "currency": "PKR",
	}); code != http.StatusOK {
		t.Fatalf("stray webhook: %d %s", code, body)
	}
	code, body := h.do("GET", "/api/notices?status=unmatched", nil,
		map[string]string{"X-Admin-Token": adminToken})
	if code != http.StatusOK {
		t.Fatalf("notices: %d %s", code, body)
	}
	var notices struct {
		Notices []struct {
			TxnRef string `json:"txn_ref"`
			Status string `json:"status"`
		} `json:"notices"`
	}
	decodeInto(t, body, &notices)
	found := false
	for _, n := range notices.Notices {
		if n.TxnRef == "STRAY-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stray payment not in unmatched notices: %s", body)
	}
}

// TestReadyzSeparatesReadinessFromLiveness: readiness must fail when the store
// is unreachable while liveness keeps reporting process health, so an
// orchestrator restarts on one signal and de-rotates on the other.
func TestReadyzSeparatesReadinessFromLiveness(t *testing.T) {
	h := newHarness(t)

	code, body := h.do("GET", "/api/readyz", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("readyz code = %d, want 200", code)
	}
	var ready struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	decodeInto(t, body, &ready)
	if !ready.Ready || ready.Checks["db"] != "ok" {
		t.Fatalf("readyz body = %+v, want ready with db ok", ready)
	}

	// Take the database away: readiness must report 503, liveness must stay 200.
	// That difference is the whole point of having two probes.
	if err := h.store.DB().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	code, body = h.do("GET", "/api/readyz", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with closed db = %d %s, want 503", code, body)
	}
	code, body = h.do("GET", "/api/healthz", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("healthz with closed db = %d %s, want 200 (liveness must not fail)", code, body)
	}
	if !strings.Contains(string(body), "degraded") {
		t.Fatalf("healthz should report the degraded db: %s", body)
	}
}
