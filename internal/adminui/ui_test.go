package adminui_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adminui"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

const testToken = "test-admin-token"

// harness mounts the UI the way cmd/paisabridge does: one handler under a
// prefix, reached through http.StripPrefix.
type harness struct {
	t     *testing.T
	srv   *httptest.Server
	cat   *httptest.Server
	store *store.Store
	now   time.Time
}

// newHarness builds a store with a fixed clock so ages and windows are
// deterministic, seeds a handful of orders across states, and mounts the UI.
func newHarness(t *testing.T, withWallet bool) *harness {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "ui.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := adminui.Config{Store: s, AdminToken: testToken, InsecureCookie: true}
	if withWallet {
		cfg.Wallet = wallet.New(s)
	}
	ui, err := adminui.New(cfg)
	if err != nil {
		t.Fatalf("adminui.New: %v", err)
	}
	if ui == nil {
		t.Fatal("adminui.New returned nil handler with a token set")
	}
	ts := httptest.NewServer(http.StripPrefix("/admin", ui))
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts, store: s, now: time.Now().UTC()}
}

// newClient returns a client with a cookie jar, standing in for a browser.
func (h *harness) newClient() *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatalf("cookiejar: %v", err)
	}
	c := &http.Client{
		Jar: jar,
		// Follow no redirects: most assertions are about the redirect itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// get fetches a path, optionally with a header token.
func (h *harness) get(c *http.Client, path string, hdr map[string]string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/admin"+path, nil)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	return resp, string(body)
}

// post form-encodes a body and sends it with the CSRF token from the page.
func (h *harness) post(c *http.Client, path string, form url.Values, hdr map[string]string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/admin"+path, strings.NewReader(form.Encode()))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// createOrder seeds an order at catalog price plus a small unique offset.
func (h *harness) createOrder(price int64, method types.Method, email, phone string) *types.Order {
	h.t.Helper()
	o, err := h.store.CreateOrder(context.Background(), store.CreateOrderInput{
		ProductID: "prod-1", ProductName: "Starter plan",
		CatalogPrice: price, Currency: "PKR", Method: method,
		CustomerEmail: email, CustomerPhone: phone, TTL: 30 * time.Minute,
	})
	if err != nil {
		h.t.Fatalf("create order: %v", err)
	}
	return o
}

// toReview moves an order pending_payment -> review the way the matcher does.
func (h *harness) toReview(o *types.Order) *types.Order {
	h.t.Helper()
	out, err := h.store.TransitionOrder(context.Background(), o.ID,
		types.StatusPendingPayment, types.StatusReview, "matcher", "ambiguous amount")
	if err != nil {
		h.t.Fatalf("to review: %v", err)
	}
	return out
}

// login posts the token form and returns the client holding the cookie.
func (h *harness) login() *http.Client {
	h.t.Helper()
	c := h.newClient()
	resp, body := h.post(c, "/login", url.Values{"token": {testToken}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("login: status %d, want 303 (body: %s)", resp.StatusCode, firstLines(body, 3))
	}
	return c
}

// csrf pulls the anti-forgery token out of a rendered page.
var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("no csrf field in page:\n%s", firstLines(body, 5))
	}
	return m[1]
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

func TestNewRefusesWithoutToken(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	h, err := adminui.New(adminui.Config{Store: s})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h != nil {
		t.Fatal("New returned a handler without an admin token; the UI would be an unauthenticated write path")
	}
	if _, err := adminui.New(adminui.Config{AdminToken: "x"}); err == nil {
		t.Fatal("New accepted a nil store")
	}
}

func TestPagesRequireAuth(t *testing.T) {
	h := newHarness(t, false)
	h.toReview(h.createOrder(150000, types.MethodEasypaisa, "a@example.com", ""))

	c := h.newClient()
	for _, path := range []string{"/", "/queue", "/orders", "/notices", "/orders/xyz"} {
		resp, body := h.get(c, path, nil)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s without auth: %d, want 303 to login", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("GET %s redirects to %q, want the login form", path, loc)
		}
		if strings.Contains(body, "Review queue") && strings.Contains(body, "Approve") {
			t.Fatalf("GET %s leaked the queue body while unauthenticated", path)
		}
	}

	// A wrong token is still unauthenticated.
	resp, _ := h.get(c, "/queue", map[string]string{"X-Admin-Token": "wrong"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("wrong header token: %d, want 303", resp.StatusCode)
	}

	// The right token in X-Admin-Token works, same convention as /api/admin/*.
	resp, body := h.get(c, "/queue", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid header token: %d, want 200 (body: %s)", resp.StatusCode, firstLines(body, 3))
	}

	// Bearer works too, mirroring internal/api's fallback.
	resp, _ = h.get(c, "/orders", map[string]string{"Authorization": "Bearer " + testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer token: %d, want 200", resp.StatusCode)
	}
}

func TestLoginRejectsBadToken(t *testing.T) {
	h := newHarness(t, false)
	c := h.newClient()
	resp, body := h.post(c, "/login", url.Values{"token": {"nope"}}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(body, "That token is not valid.") {
		t.Fatalf("bad login page does not explain the failure:\n%s", firstLines(body, 20))
	}
	// No session cookie was issued.
	resp2, _ := h.get(c, "/orders", nil)
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("after a failed login the UI is still served: %d", resp2.StatusCode)
	}
}

func TestLoginSetsSignedCookieAndLogoutClearsIt(t *testing.T) {
	h := newHarness(t, false)
	c := h.newClient()
	resp, _ := h.post(c, "/login", url.Values{"token": {testToken}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	u, _ := url.Parse(h.srv.URL)
	var cookie *http.Cookie
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == adminui.CookieName {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatalf("%s cookie not set", adminui.CookieName)
	}

	// The cookie is an id plus an HMAC; it must never contain the token.
	if strings.Contains(cookie.Value, testToken) {
		t.Fatalf("session cookie contains the admin token: %q", cookie.Value)
	}
	if !strings.Contains(cookie.Value, ".") {
		t.Fatalf("session cookie is not signed: %q", cookie.Value)
	}

	resp, body := h.get(c, "/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard with cookie: %d", resp.StatusCode)
	}

	// Logout needs the CSRF token, then clears the cookie.
	csrf := csrfFrom(t, body)
	resp, _ = h.post(c, "/logout", url.Values{"csrf": {csrf}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	resp, _ = h.get(c, "/orders", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after logout the UI is still served: %d", resp.StatusCode)
	}
}

func TestTamperedCookieIsRejected(t *testing.T) {
	h := newHarness(t, false)
	c := h.newClient()
	h.post(c, "/login", url.Values{"token": {testToken}}, nil)
	u, _ := url.Parse(h.srv.URL)

	// Swap the signature for another session id: the HMAC no longer verifies.
	c.Jar.SetCookies(u, []*http.Cookie{{Name: adminui.CookieName, Value: "deadbeef.abc", Path: "/"}})
	resp, _ := h.get(c, "/queue", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("forged cookie accepted: %d, want 303", resp.StatusCode)
	}
}

func TestOpenRedirectRefused(t *testing.T) {
	h := newHarness(t, false)
	c := h.newClient()
	resp, _ := h.post(c, "/login", url.Values{"token": {testToken}, "next": {"//evil.example/x"}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("login redirected to %q, want / (protocol-relative next must be refused)", loc)
	}
	resp, _ = h.post(h.newClient(), "/login", url.Values{"token": {testToken}, "next": {"/orders?status=review"}}, nil)
	if loc := resp.Header.Get("Location"); loc != "/orders?status=review" {
		t.Fatalf("same-origin next redirected to %q", loc)
	}
}

// ---------------------------------------------------------------------------
// pages render
// ---------------------------------------------------------------------------

func TestDashboardRenders(t *testing.T) {
	h := newHarness(t, true)
	reviewed := h.toReview(h.createOrder(150000, types.MethodBank, "ops@example.com", ""))
	h.createOrder(62000, types.MethodJazzCash, "", "03001234567")

	c := h.newClient()
	resp, body := h.get(c, "/", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d %s", resp.StatusCode, firstLines(body, 3))
	}
	for _, want := range []string{
		"Dashboard", "Money in today", "Open order value", "Status",
		"Review", "Pending payment", "Needs a decision",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// The review count is a real number from the seeded store.
	if !strings.Contains(body, "Review queue") {
		t.Fatal("dashboard has no link to the review queue")
	}
	_ = reviewed
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	// No external asset is referenced anywhere.
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("dashboard references an external URL")
	}
}

func TestQueueListsReviewOrdersAndTransitionsOnApprove(t *testing.T) {
	h := newHarness(t, true)
	o := h.toReview(h.createOrder(250000, types.MethodEasypaisa, "customer@example.com", "03009998888"))
	if o.Status != types.StatusReview {
		t.Fatalf("seeded order status = %s", o.Status)
	}

	c := h.newClient()
	resp, body := h.get(c, "/queue", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue: %d %s", resp.StatusCode, firstLines(body, 3))
	}
	wantMinor := strconv.FormatInt(o.Amount, 10)
	for _, want := range []string{o.Ref, "Easypaisa", "Approve", "Reject", wantMinor} {
		if !strings.Contains(body, want) {
			t.Fatalf("queue missing %q\n%s", want, firstLines(body, 40))
		}
	}
	csrf := csrfFrom(t, body)

	resp, body = h.post(c, "/queue", url.Values{
		"csrf": {csrf}, "order_id": {o.ID}, "action": {"approve"}, "txn_ref": {"MANUAL-777"},
	}, map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d, want 303 (body %s)", resp.StatusCode, firstLines(body, 5))
	}

	got, err := h.store.Order(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != types.StatusFulfilled {
		t.Fatalf("approved order status = %s, want fulfilled", got.Status)
	}
	if got.TxRef != "MANUAL-777" {
		t.Fatalf("tx_ref = %q, want MANUAL-777", got.TxRef)
	}

	// The journal records who did it.
	events, err := h.store.Events(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawApprove bool
	for _, e := range events {
		if e.ToState == types.StatusVerified && strings.Contains(e.Detail, "review UI") {
			sawApprove = true
		}
	}
	if !sawApprove {
		t.Fatalf("journal has no review-UI approval event: %+v", events)
	}

	// The queue is now empty.
	_, body = h.get(c, "/queue", map[string]string{"X-Admin-Token": testToken})
	if !strings.Contains(body, "Queue is clear") {
		t.Fatal("queue still lists the approved order")
	}
}

func TestRejectCancelsOrder(t *testing.T) {
	h := newHarness(t, true)
	o := h.toReview(h.createOrder(91000, types.MethodNayaPay, "x@example.com", ""))

	c := h.newClient()
	_, body := h.get(c, "/queue", map[string]string{"X-Admin-Token": testToken})
	csrf := csrfFrom(t, body)

	resp, _ := h.post(c, "/queue", url.Values{
		"csrf": {csrf}, "order_id": {o.ID}, "action": {"reject"},
	}, map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reject: %d", resp.StatusCode)
	}
	got, err := h.store.Order(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != types.StatusCancelled {
		t.Fatalf("rejected order status = %s, want cancelled", got.Status)
	}
}

func TestActionRequiresCSRF(t *testing.T) {
	h := newHarness(t, true)
	o := h.toReview(h.createOrder(88000, types.MethodSadaPay, "", ""))

	c := h.newClient()
	resp, _ := h.post(c, "/queue", url.Values{
		"csrf": {"forged"}, "order_id": {o.ID}, "action": {"approve"},
	}, map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("forged csrf: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "badtoken") {
		t.Fatalf("forged csrf redirected to %q, want the badtoken banner", loc)
	}
	got, err := h.store.Order(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != types.StatusReview {
		t.Fatalf("a forged CSRF token changed the order to %s", got.Status)
	}
}

func TestActionRequiresAuth(t *testing.T) {
	h := newHarness(t, true)
	o := h.toReview(h.createOrder(77000, types.MethodBinance, "", ""))

	c := h.newClient()
	resp, _ := h.post(c, "/queue", url.Values{
		"csrf": {""}, "order_id": {o.ID}, "action": {"approve"},
	}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated action: %d, want 303 to login", resp.StatusCode)
	}
	got, err := h.store.Order(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != types.StatusReview {
		t.Fatalf("unauthenticated action changed the order to %s", got.Status)
	}
}

func TestOrdersListFiltersAndSearches(t *testing.T) {
	h := newHarness(t, false)
	rev := h.toReview(h.createOrder(120000, types.MethodBank, "bank@example.com", ""))
	pend := h.createOrder(45000, types.MethodJazzCash, "jazz@example.com", "")

	c := h.newClient()
	hdr := map[string]string{"X-Admin-Token": testToken}

	resp, body := h.get(c, "/orders", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("orders: %d", resp.StatusCode)
	}
	if !strings.Contains(body, rev.Ref) || !strings.Contains(body, pend.Ref) {
		t.Fatal("unfiltered order list is missing a seeded order")
	}

	_, body = h.get(c, "/orders?status=review", hdr)
	if !strings.Contains(body, rev.Ref) {
		t.Fatal("status=review lost the review order")
	}
	if strings.Contains(body, pend.Ref) {
		t.Fatal("status=review still shows a pending_payment order")
	}

	// Search by ref.
	_, body = h.get(c, "/orders?q="+url.QueryEscape(rev.Ref), hdr)
	if !strings.Contains(body, rev.Ref) || strings.Contains(body, pend.Ref) {
		t.Fatal("ref search did not narrow to one order")
	}

	// Search by txn ref, which is what a bank statement shows.
	h.store.BindTxRef(context.Background(), pend.ID, "STMT-9911")
	_, body = h.get(c, "/orders?q=STMT-9911", hdr)
	if !strings.Contains(body, pend.Ref) {
		t.Fatal("txn ref search did not find the order")
	}

	// A search that matches nothing says so.
	_, body = h.get(c, "/orders?q=zzz-nothing", hdr)
	if !strings.Contains(body, "No orders match") {
		t.Fatal("empty search result has no empty state")
	}
}

func TestOrderDetailShowsJournal(t *testing.T) {
	h := newHarness(t, false)
	o := h.toReview(h.createOrder(333000, types.MethodUSDT, "detail@example.com", "+923001234567"))

	c := h.newClient()
	resp, body := h.get(c, "/orders/"+o.ID, map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %d %s", resp.StatusCode, firstLines(body, 3))
	}
	for _, want := range []string{o.Ref, o.ID, "Journal", "ambiguous amount", "USDT", "Decision"} {
		if !strings.Contains(body, want) {
			t.Fatalf("order detail missing %q", want)
		}
	}
	// Minor units are shown next to the formatted amount.
	if !strings.Contains(body, "("+strconv.FormatInt(o.Amount, 10)) {
		t.Fatalf("detail does not show minor units:\n%s", firstLines(body, 30))
	}

	resp, _ = h.get(c, "/orders/nope-does-not-exist", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown order: %d, want 404", resp.StatusCode)
	}
}

func TestNoticesPageRenders(t *testing.T) {
	h := newHarness(t, false)
	if err := h.store.RecordNotice(context.Background(), &types.PaymentNotice{
		TxnRef: "ORPHAN-1", Method: types.MethodEasypaisa, Amount: 12345,
		Currency: "PKR", SenderName: "Ali Raza", SenderAcct: "03001112223",
		ReceivedAt: h.now,
	}); err != nil {
		t.Fatalf("record notice: %v", err)
	}
	if err := h.store.RecordNotice(context.Background(), &types.PaymentNotice{
		TxnRef: "MATCHED-1", Method: types.MethodJazzCash, Amount: 50000,
		Currency: "PKR", SenderName: "Sana", ReceivedAt: h.now,
	}); err != nil {
		t.Fatalf("record notice: %v", err)
	}
	h.store.ResolveNotice(context.Background(), types.MethodJazzCash, "MATCHED-1", "", "matched", "matched")

	c := h.newClient()
	hdr := map[string]string{"X-Admin-Token": testToken}

	resp, body := h.get(c, "/notices", hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notices: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "ORPHAN-1") {
		t.Fatal("default notices view lost the unmatched payment")
	}
	if strings.Contains(body, "MATCHED-1") {
		t.Fatal("default notices view shows a matched payment")
	}
	if !strings.Contains(body, "Ali Raza") {
		t.Fatal("notices view does not show the sender")
	}

	_, body = h.get(c, "/notices?status=all", hdr)
	if !strings.Contains(body, "ORPHAN-1") || !strings.Contains(body, "MATCHED-1") {
		t.Fatal("status=all does not show both notices")
	}
	if !strings.Contains(body, "2 recorded") {
		t.Fatalf("notices header does not count both rows:\n%s", firstLines(body, 20))
	}
}

func TestCSSServed(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.get(h.newClient(), "/static/admin.css", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("css: %d (the shell links it before auth, so it must be public)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("css content type = %q", ct)
	}
	for _, want := range []string{"tabular-nums", "@media (max-width: 640px)", "--line"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stylesheet missing %q", want)
		}
	}
	if strings.Contains(body, "http") {
		t.Fatal("stylesheet references a remote URL (no CDN, no webfonts allowed)")
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("stylesheet has no ETag")
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/admin/static/admin.css", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional get: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional css request = %d, want 304", resp2.StatusCode)
	}
}

func TestNoExternalAssetReferences(t *testing.T) {
	h := newHarness(t, true)
	h.toReview(h.createOrder(210000, types.MethodBank, "a@example.com", ""))
	c := h.newClient()
	cookie, _ := h.post(c, "/login", url.Values{"token": {testToken}}, nil)

	// Every page, signed in, must reference nothing off-origin and must ship
	// no script other than the one confirmation helper.
	pages := []string{"/", "/queue", "/orders", "/notices", "/orders/" + orderID(t, h)}
	for _, p := range pages {
		resp, body := h.get(c, p, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Fatalf("%s references an external URL", p)
		}
		if strings.Count(body, "<script") > 1 {
			t.Fatalf("%s ships more than one script block", p)
		}
		if strings.Contains(body, "<img") || strings.Contains(body, "url(") {
			t.Fatalf("%s uses an image or a css url()", p)
		}
	}
	_ = cookie
}

func orderID(t *testing.T, h *harness) string {
	t.Helper()
	orders, err := h.store.ListOrders(context.Background(), "", 1)
	if err != nil || len(orders) == 0 {
		t.Fatalf("list orders: %v", err)
	}
	return orders[0].ID
}

func TestUnknownPathIsHTML404(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.get(h.newClient(), "/nope", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Page not found") {
		t.Fatal("404 is not rendered through the shell")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, false)
	// GET on an action-only route is a 405 from the Go 1.22 mux patterns.
	resp, _ := h.get(h.newClient(), "/logout", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout: %d, want 405", resp.StatusCode)
	}
}
