package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// render executes a page template into a buffer first: a template error must
// never reach the client half-written, and the status code has to be decided
// before the first byte goes out.
func (s *Server) render(w http.ResponseWriter, status int, p page) {
	var buf strings.Builder
	// Every page renders through the shell except the sign-in screen, which
	// deliberately has no navigation or data links on it.
	name := "layout"
	if p.Standalone {
		name = p.Page
		p.Content = p.Page
	} else {
		p.Content = "content." + p.Page
	}
	if err := s.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.log.Error("adminui render", "page", p.Page, "err", err)
		http.Error(w, "internal error while rendering the page", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	// No page references another origin, so default-src 'none' is accurate. The
	// inline allowances cover the one inline script block and the one inline
	// style attribute in the incident list.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

// chromeFor builds the shell model shared by every page. The review badge is
// computed from the same bounded read the pages use, so it can never disagree
// with the queue screen by more than one page of history.
func (s *Server) chromeFor(r *http.Request, ctx context.Context, title, nav string) chrome {
	n := 0
	if orders, err := s.loadOrders(ctx, 500); err == nil {
		n = countByStatus(orders).review()
	}
	return chrome{
		Title:   title,
		Nav:     nav,
		Token:   s.csrfToken(s.session(r)),
		Version: versionString(),
		Now:     s.now().UTC().Format("2006-01-02 15:04:05 MST"),
		Review:  n,
	}
}

// handleCSS serves the one stylesheet with an ETag so a reload is a single
// conditional request.
func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	const etag = `"admin-css-1"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, max-age=300")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/css; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(s.css))
}

// knownMethods maps every path this console serves to the methods it accepts.
// It exists because the catch-all handler owns unmatched requests: without this
// table a POST-only route such as /logout answers 404 to a GET, when the
// correct answer is 405 Method Not Allowed with an Allow header.
var knownMethods = map[string][]string{
	"/":        {"GET"},
	"/login":   {"GET", "POST"},
	"/logout":  {"POST"},
	"/queue":   {"GET", "POST"},
	"/orders":  {"GET"},
	"/notices": {"GET"},
}

// renderError draws the shared error page with an explicit status.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, heading, message, hint string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.render(w, status, page{
		chrome: s.chromeFor(r, ctx, heading, ""),
		Page:   "error",
		Data:   errorPage{Heading: heading, Message: message, Hint: hint},
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if allowed, ok := knownMethods[r.URL.Path]; ok {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		s.renderError(w, r, http.StatusMethodNotAllowed, "Method not allowed",
			"This screen does not accept "+r.Method+".",
			"Use one of: "+strings.Join(allowed, ", ")+".")
		return
	}
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.render(w, http.StatusNotFound, page{
		chrome: s.chromeFor(r, ctx, "Not found", ""),
		Page:   "error",
		Data: errorPage{
			Heading: "Page not found",
			Message: "No review screen lives at " + r.URL.Path + ".",
			Hint:    "The screens are Dashboard, Review queue, Orders and Notices.",
		},
	})
}

// errorPage is the shared 404/500 body.
type errorPage struct {
	Heading string
	Message string
	Hint    string
}

// ---------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------

// dashboardData answers three questions: what is waiting for a human, what
// moved today, and how much money is sitting in open orders right now.
type dashboardData struct {
	Orders       int
	Review       int
	ReviewValue  []moneyTotals
	In           inboundToday
	Open         int
	OpenValue    []moneyTotals
	Counts       []statusCount
	OldestReview *types.Order
	OldestAge    string
	Notices      int
	Unmatched    int
	MoneySource  string
}

type statusCount struct {
	Status types.OrderStatus
	Label  string
	Count  int
	Key    string
}

// handleDashboard renders counts by status, money in today and open orders.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	orders, err := s.loadOrders(ctx, 500)
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	counts := countByStatus(orders)
	now := s.now().UTC()

	d := dashboardData{Orders: len(orders), Review: counts.review()}
	open := map[string]*moneyTotals{}
	reviewVal := map[string]*moneyTotals{}
	for _, o := range orders {
		switch o.Status {
		case types.StatusReview:
			acc(reviewVal, o)
			if d.OldestReview == nil || o.UpdatedAt.Before(d.OldestReview.UpdatedAt) {
				d.OldestReview = o
			}
		case types.StatusCreated, types.StatusPendingPayment, types.StatusVerified:
			acc(open, o)
			d.Open++
		}
	}
	d.ReviewValue = sortedTotals(reviewVal)
	d.OpenValue = sortedTotals(open)
	if d.OldestReview != nil {
		d.OldestAge = humanAge(now.Sub(d.OldestReview.UpdatedAt))
	}
	for _, st := range allStatuses {
		c := counts[st]
		if st != types.StatusReview && c == 0 {
			continue
		}
		d.Counts = append(d.Counts, statusCount{
			Status: st, Label: statusLabel(st), Count: c, Key: string(st),
		})
	}

	in, err := s.moneyInToday(ctx, orders)
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	d.In = in

	// The notice badge is a triage signal over the newest 500 rows, not an
	// accounting total, and the page labels it that way.
	if notices, err := s.cfg.Store.ListNotices(ctx, "", 500); err == nil {
		d.Notices = len(notices)
		for _, n := range notices {
			if n.Status == "unmatched" {
				d.Unmatched++
			}
		}
	}
	if in.From == "ledger" {
		d.MoneySource = "Money in is summed from today's wallet ledger, credits minus refunds. Wallet-paid orders are listed separately because they are an internal transfer, not new money at the door."
	} else {
		d.MoneySource = "No wallet is wired, so money in is the value of orders that reached verified or fulfilled today."
	}

	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Dashboard", "dashboard"),
		Page:   "dashboard", Data: d,
	})
}

// acc adds an order's amount to a per-currency bucket.
func acc(m map[string]*moneyTotals, o *types.Order) {
	t := m[o.Currency]
	if t == nil {
		t = &moneyTotals{Currency: o.Currency}
		m[o.Currency] = t
	}
	t.Minor += o.Amount
	t.Count++
}

// ---------------------------------------------------------------------------
// review queue
// ---------------------------------------------------------------------------

// queueData is the review screen: every order a human must decide.
type queueData struct {
	Rows       []row
	Count      int
	Older      int
	Total      []moneyTotals
	Banner     string
	BannerKind string
	PageNo     int
	NextPage   string
}

// handleQueue lists every order in `review` with the facts needed to decide,
// plus one-click Approve and Reject forms.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	orders, err := s.loadOrders(ctx, 500)
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	now := s.now().UTC()
	pg := queryInt(r, "page", 1)
	limit := s.cfg.PageSize
	csrf := s.csrfToken(s.session(r))

	d := queueData{PageNo: pg}
	totals := map[string]*moneyTotals{}
	var queue []*types.Order
	for _, o := range orders {
		if o.Status != types.StatusReview {
			continue
		}
		queue = append(queue, o)
		acc(totals, o)
	}
	d.Count = len(queue)
	d.Total = sortedTotals(totals)
	start := (pg - 1) * limit
	if start > len(queue) {
		start = len(queue)
	}
	end := start + limit
	if end > len(queue) {
		end = len(queue)
	}
	for _, o := range queue[start:end] {
		d.Rows = append(d.Rows, buildOrderRow(o, now, csrf, true))
	}
	d.Older = len(queue) - end
	if d.Older > 0 {
		d.NextPage = "/queue?page=" + strconv.Itoa(pg+1)
	}
	d.Banner, d.BannerKind = bannerFrom(r)

	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Review queue", "queue"),
		Page:   "queue", Data: d,
	})
}

// bannerFrom reads the flash message a redirect carries back.
func bannerFrom(r *http.Request) (string, string) {
	q := r.URL.Query()
	ref := q.Get("ref")
	switch q.Get("msg") {
	case "":
		return "", ""
	case "approved":
		return "Approved " + ref + ": the order moved from review to verified and is queued for delivery.", "ok"
	case "rejected":
		return "Rejected " + ref + ": the order was cancelled and can no longer be fulfilled.", "warn"
	case "gone":
		return ref + " was already handled, by another operator or the matcher. Nothing changed.", "warn"
	case "badtoken":
		return "That form had expired. Sign in again and retry.", "bad"
	case "failed":
		return "The action failed: " + q.Get("detail"), "bad"
	case "loggedin":
		return "Signed in.", "ok"
	case "loggedout":
		return "Signed out.", "ok"
	default:
		return q.Get("msg"), ""
	}
}

// ---------------------------------------------------------------------------
// orders
// ---------------------------------------------------------------------------

// ordersData is the searchable order list.
type ordersData struct {
	Rows     []row
	Status   string
	Q        string
	Total    int
	Shown    int
	From     int
	To       int
	Prev     string
	Next     string
	HasPrev  bool
	HasNext  bool
	Statuses []statusCount
	Empty    bool
}

// handleOrders renders the order list with a status filter, a ref/id search
// and ?page= paging.
func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	orders, err := s.loadOrders(ctx, 500)
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	now := s.now().UTC()
	status := r.URL.Query().Get("status")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	pg := queryInt(r, "page", 1)
	limit := s.cfg.PageSize
	csrf := s.csrfToken(s.session(r))

	d := ordersData{Status: status, Q: q}
	counts := countByStatus(orders)
	for _, st := range allStatuses {
		d.Statuses = append(d.Statuses, statusCount{
			Status: st, Label: statusLabel(st), Count: counts[st], Key: string(st),
		})
	}

	var found []*types.Order
	for _, o := range orders {
		if status != "" && string(o.Status) != status {
			continue
		}
		if !matchOrder(o, q) {
			continue
		}
		found = append(found, o)
	}
	d.Total = len(found)
	start := (pg - 1) * limit
	if start > len(found) {
		start = len(found)
	}
	end := start + limit
	if end > len(found) {
		end = len(found)
	}
	for _, o := range found[start:end] {
		d.Rows = append(d.Rows, buildOrderRow(o, now, csrf, false))
	}
	d.Shown = len(d.Rows)
	if d.Total > 0 {
		d.From, d.To = start+1, end
	}
	d.Empty = d.Total == 0
	link := func(pageNo int) string {
		v := url.Values{}
		v.Set("status", status)
		v.Set("q", q)
		v.Set("page", strconv.Itoa(pageNo))
		return "?" + v.Encode()
	}
	d.Prev, d.Next = link(pg-1), link(pg+1)
	d.HasPrev = pg > 1
	d.HasNext = end < d.Total

	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Orders", "orders"),
		Page:   "orders", Data: d,
	})
}

// ---------------------------------------------------------------------------
// order detail
// ---------------------------------------------------------------------------

// orderData is one order plus its journal, so an operator can see how it got
// to review before pressing approve.
type orderData struct {
	Order    *types.Order
	Events   []store.OrderEvent
	Age      string
	Left     string
	Notices  []*store.Notice
	Row      row
	CSRF     string
	CanAct   bool
	Terminal bool
	Expired  bool
}

// handleOrder renders one order and its state journal.
func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	id := r.PathValue("id")
	o, err := s.cfg.Store.Order(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.render(w, http.StatusNotFound, page{
			chrome: s.chromeFor(r, ctx, "Order not found", "orders"),
			Page:   "error",
			Data: errorPage{
				Heading: "Order not found",
				Message: "No order matches " + id + ".",
				Hint:    "Search the order list by ref, order id, txn ref, email or phone.",
			},
		})
		return
	}
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	events, err := s.cfg.Store.Events(ctx, o.ID)
	if err != nil {
		events = nil
	}
	var related []*store.Notice
	if notices, err := s.cfg.Store.ListNotices(ctx, "", 300); err == nil {
		for _, n := range notices {
			if n.OrderID == o.ID || (o.TxRef != "" && n.TxnRef == o.TxRef) {
				related = append(related, n)
			}
		}
	}
	now := s.now().UTC()
	csrf := s.csrfToken(s.session(r))
	d := orderData{
		Order:    o,
		Events:   events,
		Age:      humanAge(now.Sub(o.CreatedAt)),
		Left:     humanUntil(o.ExpiresAt.Sub(now)),
		Notices:  related,
		Row:      buildOrderRow(o, now, csrf, true),
		CSRF:     csrf,
		CanAct:   o.Status == types.StatusReview || o.Status == types.StatusPendingPayment,
		Terminal: o.IsTerminal(),
		Expired:  !now.Before(o.ExpiresAt),
	}
	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Order "+o.Ref, "orders"),
		Page:   "order", Data: d,
	})
}

// ---------------------------------------------------------------------------
// notices
// ---------------------------------------------------------------------------

// noticesData lists inbound payments. Unmatched rows are the ones an operator
// has to reconcile by hand; the rest are shown for context.
type noticesData struct {
	Rows      []row
	Status    string
	Count     int
	Unmatched int
	Shown     int
	Statuses  []string
	Empty     bool
}

// handleNotices lists inbound payments, defaulting to unmatched.
func (s *Server) handleNotices(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	status := r.URL.Query().Get("status")
	if status == "" {
		status = "unmatched"
	}
	if status == "all" {
		status = ""
	}
	limit := s.cfg.PageSize
	notices, err := s.cfg.Store.ListNotices(ctx, status, limit)
	if err != nil {
		s.fail(w, r, ctx, err)
		return
	}
	var all []*store.Notice
	if a, err := s.cfg.Store.ListNotices(ctx, "", 500); err == nil {
		all = a
	}
	d := noticesData{Status: status, Shown: len(notices), Empty: len(notices) == 0}
	for _, n := range all {
		d.Count++
		if n.Status == "unmatched" {
			d.Unmatched++
		}
	}
	d.Statuses = []string{"unmatched", "review", "matched", "late", "duplicate", "all"}
	now := s.now().UTC()
	for _, n := range notices {
		d.Rows = append(d.Rows, buildNoticeRow(n, now))
	}
	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Notices", "notices"),
		Page:   "notices", Data: d,
	})
}

// ---------------------------------------------------------------------------
// failures and helpers
// ---------------------------------------------------------------------------

// fail logs and renders the error page with a 500.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, ctx context.Context, err error) {
	s.log.Error("adminui", "path", r.URL.Path, "err", err)
	s.render(w, http.StatusInternalServerError, page{
		chrome: s.chromeFor(r, ctx, "Error", ""),
		Page:   "error",
		Data: errorPage{
			Heading: "Could not read the database",
			Message: err.Error(),
			Hint:    "Check the server log and that the SQLite file is readable.",
		},
	})
}

// redirectLogin sends an unauthenticated browser to the login form, keeping
// the requested path so login lands where it was going.
func (s *Server) redirectLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	if next == "/" {
		next = ""
	}
	// Absolute path: a relative "login" resolves against the CURRENT
	// directory, so /orders/xyz redirected to the non-existent
	// /orders/login instead of the sign-in screen.
	target := "/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseIntDefault is queryInt for form values.
func parseIntDefault(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// flash builds a redirect target that carries one line of feedback back.
func flash(path, msg, ref, detail string) string {
	v := url.Values{}
	v.Set("msg", msg)
	if ref != "" {
		v.Set("ref", ref)
	}
	if detail != "" {
		v.Set("detail", detail)
	}
	return path + "?" + v.Encode()
}
