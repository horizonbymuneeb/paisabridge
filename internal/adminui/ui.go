// Package adminui is the server-rendered review interface for paisabridge.
//
// The bank rail intentionally parks payments in `review` and a human has to
// confirm them; before this package that meant hand-writing curl calls
// against /api/admin/approve. adminui turns the same operations into four
// pages (dashboard, review queue, orders, notices) plus a login form, using
// only the standard library: html/template is parsed from embedded files, the
// single CSS file is embedded too, and there is no build step, no npm, no CDN
// and no JavaScript framework. The only script is ~20 inline lines that wire
// the approve/reject forms and their confirmation prompts.
//
// # Mounting
//
//	admin, err := adminui.New(adminui.Config{Store: st, AdminToken: cfg.AdminToken})
//	srv.Handler = mountBase("/admin/", admin)
//
// Everything the UI does happens under the /admin/ prefix, which is why a
// plain http.StripPrefix works. Inside that prefix:
//
//	GET|POST /login   token form; on success sets the admin cookie
//	POST     /logout  clears the cookie
//	GET      /        dashboard
//	GET      /queue   review queue (also accepts POST /queue for the buttons)
//	GET      /orders  orders list with status filter, ref search and paging
//	GET      /orders/{id}
//	GET      /notices unmatched inbound payments
//
// # Authentication
//
// The UI accepts the SAME secret the JSON admin API already uses
// (X-Admin-Token == PB_ADMIN_TOKEN), in either of two forms:
//
//   - the request carries a valid X-Admin-Token (or Authorization: Bearer)
//     header, which is convenient for scripting and health probes;
//   - the browser completed the login form, which sets an HttpOnly,
//     SameSite=Strict cookie holding an HMAC of the token.
//
// The cookie never contains the token itself: it carries a random session id
// plus an HMAC-SHA256 over that id keyed by the token, so a stolen cookie
// cannot be turned back into the API credential, and the cookie stops working
// the moment PB_ADMIN_TOKEN is rotated (the HMAC no longer verifies).
//
// The state-changing buttons are plain HTML form POSTs guarded by a CSRF
// token bound to the session, so they work with JavaScript disabled and the
// page ships no fetch() calls. Every action re-checks the permission anyway.
package adminui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/version"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

// CookieName is the session cookie the login form sets.
const CookieName = "pb_admin"

// Default session lifetime. The cookie is re-issued on every page view, so an
// operator working a queue stays logged in and an idle tab expires.
const sessionTTL = 12 * time.Hour

// Config configures the review UI.
type Config struct {
	// Store is required.
	Store *store.Store
	// Wallet is optional. When set, the ledger is used to separate inbound
	// money (order payments that are not a refund) from wallet-paid orders;
	// when nil the dashboard reports the value of verified orders instead and
	// says so on the page.
	Wallet *wallet.Wallet
	// AdminToken enables the UI and is the only credential it accepts. When
	// empty, New returns nil and no handler, because a review UI without a
	// credential would be an unauthenticated write path to order state.
	AdminToken string
	// PageSize is the rows per page for lists (default 50).
	PageSize int
	// InsecureCookie drops the Secure attribute on the session cookie. Set it
	// only when the UI is served over plain HTTP on localhost, because browsers
	// silently reject a Secure cookie on http:// origins and login would loop.
	InsecureCookie bool
	// Now is optional clock injection for tests and for the "age" columns.
	Now func() time.Time
	// Logger is optional (defaults to slog.Default()).
	Logger *slog.Logger
}

// Server is the review UI handler. One Server owns one route table; mount it
// under a prefix with http.StripPrefix.
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	tmpl *template.Template
	log  *slog.Logger
	now  func() time.Time
	css  string
	key  []byte // HMAC key derived from AdminToken (cookie + CSRF)
}

// New builds the handler. It returns (nil, nil) when cfg.AdminToken is empty:
// the UI is disabled, and the caller mounts nothing.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("adminui: store is required")
	}
	if cfg.AdminToken == "" {
		return nil, nil
	}
	if cfg.PageSize <= 0 || cfg.PageSize > 500 {
		cfg.PageSize = 50
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	css, err := fs.ReadFile(assets, "static/admin.css")
	if err != nil {
		return nil, fmt.Errorf("adminui: read css: %w", err)
	}
	sum := sha256.Sum256([]byte(cfg.AdminToken))
	s := &Server{
		cfg:  cfg,
		mux:  http.NewServeMux(),
		tmpl: tmpl,
		log:  cfg.Logger,
		now:  cfg.Now,
		css:  string(css),
		key:  sum[:],
	}
	s.routes()
	return s, nil
}

// ServeHTTP routes the request. The mux is the single owner of the pattern
// table so every path under the mount is accounted for.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.handleDashboard)
	m.HandleFunc("GET /static/admin.css", s.handleCSS)

	m.HandleFunc("GET /login", s.handleLoginForm)
	m.HandleFunc("POST /login", s.handleLogin)
	m.HandleFunc("POST /logout", s.handleLogout)

	m.HandleFunc("GET /queue", s.handleQueue)
	m.HandleFunc("POST /queue", s.handleQueueAction)

	m.HandleFunc("GET /orders", s.handleOrders)
	m.HandleFunc("GET /orders/{id}", s.handleOrder)

	m.HandleFunc("GET /notices", s.handleNotices)

	// Anything else under the mount is a 404 through the handler below rather
	// than the mux's bare text response, so the shell stays consistent.
	m.HandleFunc("/", s.handleNotFound)
}

// Disabled explains the "do not mount" case to whoever wires the server.
var Disabled = errors.New("adminui: disabled because no admin token is configured")

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

// authed reports whether the request carries a usable credential, either a
// valid X-Admin-Token header or a valid session cookie.
func (s *Server) authed(r *http.Request) bool {
	if tok := headerToken(r); tok != "" && s.tokenEqual(tok) {
		return true
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if _, ok := s.verifySession(c.Value); ok {
			return true
		}
	}
	return false
}

// headerToken reads the credential from X-Admin-Token or a bearer header,
// matching internal/api's convention exactly.
func headerToken(r *http.Request) string {
	if v := r.Header.Get("X-Admin-Token"); v != "" {
		return v
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) tokenEqual(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) == 1
}

// sessionID mints a random session identifier.
func sessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// signCookie returns id.<hmac(id)> in base64url. The HMAC key is derived from
// the admin token, so rotating the token invalidates every live session.
func (s *Server) signCookie(id string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("adminui-session\x00" + id))
	return id + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifySession checks the cookie signature and returns the session id.
func (s *Server) verifySession(v string) (string, bool) {
	id, sig, ok := strings.Cut(v, ".")
	if !ok || id == "" || sig == "" {
		return "", false
	}
	want := s.signCookie(id)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", false
	}
	_, wantSig, _ := strings.Cut(want, ".")
	wantRaw, err := base64.RawURLEncoding.DecodeString(wantSig)
	if err != nil {
		return "", false
	}
	if subtle.ConstantTimeCompare(got, wantRaw) != 1 {
		return "", false
	}
	return id, true
}

// csrfToken derives a per-session anti-forgery value. It is not secret — it is
// rendered into forms — but it is bound to the session id, so a third-party
// page cannot mint a valid one.
func (s *Server) csrfToken(sessionID string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("adminui-csrf\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// session returns the session id for the current request. A header-authenticated
// request (no cookie) uses an empty id, and its CSRF value is checked against
// the empty-id derivation, which is still unforgeable without the token.
func (s *Server) session(r *http.Request) string {
	if c, err := r.Cookie(CookieName); err == nil {
		if id, ok := s.verifySession(c.Value); ok {
			return id
		}
	}
	return ""
}

// actor names who performed an action, recorded on the order event.
func actor(r *http.Request) string {
	if r.Header.Get("X-Admin-Token") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return "admin:token"
	}
	return "admin:session"
}

// csrfOK validates the submitted token for the current session.
func (s *Server) csrfOK(r *http.Request) bool {
	got := r.PostFormValue("csrf")
	want := s.csrfToken(s.session(r))
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ---------------------------------------------------------------------------
// shared types for the templates
// ---------------------------------------------------------------------------

// chrome is the part of the page model every template needs.
type chrome struct {
	Title     string
	Nav       string // active nav key
	Token     string // CSRF token for forms on this page
	Version   string
	Now       string // render timestamp, fixed-width UTC
	Review    int    // review-queue badge
	CSRFField string
}

type page struct {
	chrome
	Page       string // template name that defines the body
	Content    string // resolved body template (content.<page>)
	Data       any
	Standalone bool // render without the shell (the sign-in screen)
}

// row is one table row. Cells are a slice so the stylesheet can drop
// low-priority columns at narrow widths instead of shrinking the type, and
// each cell carries the label the stacked phone layout prints above it.
type row struct {
	ID    string // stable row key (order id or txn ref)
	Href  string // where the ref cell links to; empty means plain text
	Title string // full value for the link's title attribute

	Cells []cell

	// Optional trailing column holding the approve/reject forms.
	HasActions bool
	Actions    []action
	Deadline   string // payment-window text shown with the action buttons

	// Money context for the amount column.
	ColumnsHint string // formatted catalog price
	Adjusted    bool
	Offset      int64 // amount - catalog price, in minor units
	OrderID     string
}

// cell is one <td>. Text is escaped by html/template unless IsStat or HTML is
// set, and nothing on these pages ever carries user-controlled HTML.
type cell struct {
	Label   string // data-label for the phone layout
	Class   string
	Text    string
	Sub     string // second line, dimmer
	Mono    bool
	Numeric bool   // right-aligned tabular figure
	Primary bool   // slightly heavier weight
	IsStat  bool   // render as a status chip with a square marker
	Tone    string // "" | ok | warn | bad | mute
	Href    string
	ClassN  string
}

// action is one button in the queue's action column. Confirm is a one-line
// question the page asks before submitting; the button works without it.
type action struct {
	Kind    string // approve | reject
	Label   string
	Order   string
	Method  string
	TxnRef  string
	CSRF    string
	Confirm string
	Hidden  string
}

// ---------------------------------------------------------------------------
// data loading
// ---------------------------------------------------------------------------

const (
	refundKind = wallet.KindRefund
	payKind    = wallet.KindOrderPay
)

// statusCounts is the order count per status.
type statusCounts map[types.OrderStatus]int

// loadOrders reads the newest orders for triage. The store's list is newest
// first and capped at 500; filters are applied in memory because the review
// queue is a small slice of a small table and adding a query here would mean
// touching the store package.
func (s *Server) loadOrders(ctx context.Context, limit int) ([]*types.Order, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	return s.cfg.Store.ListOrders(ctx, "", limit)
}

// countByStatus tallies a slice of orders.
func countByStatus(orders []*types.Order) statusCounts {
	c := statusCounts{}
	for _, o := range orders {
		c[o.Status]++
	}
	return c
}

func (c statusCounts) review() int { return c[types.StatusReview] }

// money aggregates minor-unit amounts per currency.
type moneyTotals struct {
	Currency string
	Minor    int64
	Count    int
}

// todayWindow returns [start, end) for the store's local day. The store writes
// UTC timestamps, so "today" is the UTC day and the page labels it as such.
func (s *Server) todayWindow() (time.Time, time.Time) {
	now := s.now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.Add(24 * time.Hour)
}

// inboundToday sums what arrived today, per currency, either from the ledger
// (when a wallet is wired) or from verified orders. The distinction matters:
// a wallet-paid order is an internal transfer, not new money at the door.
type inboundToday struct {
	From   string // "ledger" or "orders"
	Totals []moneyTotals
	Paid   []moneyTotals // wallet-paid orders, ledger mode only
}

func (s *Server) moneyInToday(ctx context.Context, orders []*types.Order) (inboundToday, error) {
	start, end := s.todayWindow()
	if s.cfg.Wallet == nil {
		out := inboundToday{From: "orders"}
		acc := map[string]*moneyTotals{}
		for _, o := range orders {
			if o.Status != types.StatusFulfilled && o.Status != types.StatusVerified {
				continue
			}
			if o.UpdatedAt.Before(start) || !o.UpdatedAt.Before(end) {
				continue
			}
			m := acc[o.Currency]
			if m == nil {
				m = &moneyTotals{Currency: o.Currency}
				acc[o.Currency] = m
			}
			m.Minor += o.Amount
			m.Count++
		}
		out.Totals = sortedTotals(acc)
		return out, nil
	}

	rows, err := s.cfg.Store.DB().QueryContext(ctx, `SELECT kind, currency,
		SUM(CASE WHEN amount_minor > 0 THEN amount_minor ELSE 0 END),
		SUM(CASE WHEN amount_minor < 0 THEN -amount_minor ELSE 0 END),
		COUNT(*)
		FROM wallet_ledger WHERE created_at >= ? AND created_at < ?
		GROUP BY kind, currency`, start.Format(store.TimeLayout), end.Format(store.TimeLayout))
	if err != nil {
		return inboundToday{}, fmt.Errorf("adminui: ledger totals: %w", err)
	}
	defer rows.Close()
	in, paid := map[string]*moneyTotals{}, map[string]*moneyTotals{}
	for rows.Next() {
		var (
			kind, cur          string
			credit, debit, cnt int64
		)
		if err := rows.Scan(&kind, &cur, &credit, &debit, &cnt); err != nil {
			return inboundToday{}, err
		}
		// A refund is money leaving the door; a wallet payment is internal.
		switch kind {
		case payKind:
			m := paid[cur]
			if m == nil {
				m = &moneyTotals{Currency: cur}
				paid[cur] = m
			}
			m.Minor += credit
			m.Count += int(cnt)
		case refundKind, wallet.KindAdjust:
			m := in[cur]
			if m == nil {
				m = &moneyTotals{Currency: cur}
				in[cur] = m
			}
			m.Minor -= debit
			m.Count += int(cnt)
		default:
			m := in[cur]
			if m == nil {
				m = &moneyTotals{Currency: cur}
				in[cur] = m
			}
			m.Minor += credit
			m.Count += int(cnt)
		}
	}
	if err := rows.Err(); err != nil {
		return inboundToday{}, err
	}
	return inboundToday{From: "ledger", Totals: sortedTotals(in), Paid: sortedTotals(paid)}, nil
}

func sortedTotals(m map[string]*moneyTotals) []moneyTotals {
	out := make([]moneyTotals, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// queryInt reads a positive int query parameter.
func queryInt(r *http.Request, name string, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// withQuery returns u with one parameter replaced, dropping empty values.
func withQuery(r *http.Request, key, val string) string {
	q := r.URL.Query()
	if val == "" {
		q.Del(key)
	} else {
		q.Set(key, val)
	}
	// A filter change resets the cursor.
	if key != "page" {
		q.Del("page")
	}
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	return u.String()
}

// matchOrder reports whether o matches a case-insensitive needle across the
// identifiers an operator can see on screen.
func matchOrder(o *types.Order, needle string) bool {
	if needle == "" {
		return true
	}
	needle = strings.ToLower(needle)
	hay := []string{o.ID, o.Ref, o.TxRef, o.CustomerEmail, o.CustomerPhone,
		o.ProductID, o.ProductName, string(o.Method), o.SMSOTP}
	for _, h := range hay {
		if h != "" && strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}

// versionString is what the footer reports.
func versionString() string {
	if v := version.Get().Version; v != "" {
		return v
	}
	return "dev"
}
