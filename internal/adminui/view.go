package adminui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// static holds the one stylesheet. Templates live in the same tree so both are
// embedded with a single directive and the binary stays self-contained.
//
//go:embed static/admin.css templates/*.html
var assets embed.FS

// allStatuses is the closed state machine, in reading order: what is waiting
// at the top, terminal states at the bottom.
var allStatuses = []types.OrderStatus{
	types.StatusReview,
	types.StatusCreated,
	types.StatusPendingPayment,
	types.StatusVerified,
	types.StatusFulfilled,
	types.StatusExpired,
	types.StatusCancelled,
}

// statusLabel is the human name for a status. English, no emoji.
func statusLabel(s types.OrderStatus) string {
	switch s {
	case types.StatusCreated:
		return "Created"
	case types.StatusPendingPayment:
		return "Pending payment"
	case types.StatusVerified:
		return "Verified"
	case types.StatusFulfilled:
		return "Fulfilled"
	case types.StatusReview:
		return "Review"
	case types.StatusExpired:
		return "Expired"
	case types.StatusCancelled:
		return "Cancelled"
	}
	return string(s)
}

// statusTone maps a status to the small square drawn next to it: warn means a
// human is needed, ok means money is good, bad means money will not come.
func statusTone(s types.OrderStatus) string {
	switch s {
	case types.StatusReview:
		return "warn"
	case types.StatusFulfilled, types.StatusVerified:
		return "ok"
	case types.StatusExpired, types.StatusCancelled:
		return "bad"
	}
	return "mute"
}

// methodLabel is the rail name as an operator sees it.
func methodLabel(m types.Method) string {
	switch m {
	case types.MethodEasypaisa:
		return "Easypaisa"
	case types.MethodJazzCash:
		return "JazzCash"
	case types.MethodNayaPay:
		return "NayaPay"
	case types.MethodSadaPay:
		return "SadaPay"
	case types.MethodBank:
		return "Bank"
	case types.MethodUSDT:
		return "USDT"
	case types.MethodBinance:
		return "Binance"
	}
	if m == "" {
		return "unknown"
	}
	return string(m)
}

// currencyDecimals is how many decimals a currency has: PKR is 2, USDT is 6
// because the store keeps micro-units.
func currencyDecimals(cur string) int {
	if strings.EqualFold(cur, "USDT") {
		return 6
	}
	return 2
}

// groupThousands inserts a comma every three digits, using string slicing so
// no locale data or rounding is involved: 1234567 -> 1,234,567.
func groupThousands(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	first := n % 3
	if first == 0 {
		first = 3
	}
	var b strings.Builder
	b.WriteString(digits[:first])
	for i := first; i < n; i += 3 {
		b.WriteByte(',')
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// formatAmount renders minor units as a grouped decimal string. Money always
// comes out of this function so the tables can rely on one shape, and the
// tables right-align it in a tabular figure font.
func formatAmount(minor int64, currency string) string {
	d := currencyDecimals(currency)
	neg := minor < 0
	if neg {
		minor = -minor
	}
	div := int64(1)
	for i := 0; i < d; i++ {
		div *= 10
	}
	whole := minor / div
	frac := minor % div

	wholeStr := groupThousands(strconv.FormatInt(whole, 10))
	out := wholeStr
	if d > 0 {
		out += "." + fmt.Sprintf("%0*d", d, frac)
	}
	if neg {
		out = "-" + out
	}
	return out
}

// amountPair is what a money cell shows: the formatted value for reading and
// the exact minor units for reconciling against a statement.
func amountPair(minor int64, currency string) (formatted, minorStr string) {
	return formatAmount(minor, currency), strconv.FormatInt(minor, 10)
}

// humanAge renders an elapsed duration the way an operator reads a queue:
// roughly, largest unit first, never more than two parts.
func humanAge(d time.Duration) string {
	if d < 0 {
		return "in " + humanAge(-d)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d / time.Minute)
		return itoa(m) + " min"
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int(d%time.Hour) / int(time.Minute)
		if m == 0 {
			return itoa(h) + " h"
		}
		return itoa(h) + " h " + itoa(m) + " m"
	default:
		days := int(d / (24 * time.Hour))
		h := int(d%(24*time.Hour)) / int(time.Hour)
		if h == 0 {
			return itoa(days) + " d"
		}
		if days == 1 {
			return "1 d " + itoa(h) + " h"
		}
		return itoa(days) + " d " + itoa(h) + " h"
	}
}

// windowText describes the payment window for a row: a countdown while the
// order can still be paid, and a plain statement once it cannot.
func windowText(o *types.Order, now time.Time) string {
	switch o.Status {
	case types.StatusFulfilled:
		return "closed · delivered"
	case types.StatusExpired:
		return "closed · expired"
	case types.StatusCancelled:
		return "closed · cancelled"
	case types.StatusReview:
		if o.ExpiresAt.IsZero() {
			return "awaiting decision"
		}
		if now.After(o.ExpiresAt) {
			return "window passed"
		}
		return humanUntil(o.ExpiresAt.Sub(now))
	}
	if o.ExpiresAt.IsZero() {
		return "-"
	}
	return humanUntil(o.ExpiresAt.Sub(now))
}

// humanUntil is humanAge for a deadline that may already have passed.
func humanUntil(d time.Duration) string {
	if d < 0 {
		return "expired " + humanAge(-d) + " ago"
	}
	return "in " + humanAge(d)
}

func itoa(n int) string { return strconv.Itoa(n) }

// stamp is the fixed-width UTC form used for created/updated timestamps.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// shortID keeps ids legible in narrow columns without hiding the prefix that
// makes them sortable. The full id is always in the link title.
func shortID(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:8] + "..." + id[len(id)-4:]
}

// initials is used for the avatar-like customer mark: no images, no emoji.
func initials(email, phone string) string {
	src := email
	if src == "" {
		src = phone
	}
	if src == "" {
		return "?"
	}
	up := strings.ToUpper(src)
	var b strings.Builder
	for _, r := range up {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == 2 {
			break
		}
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// pct renders a share as a whole percentage, used by the status board.
func pct(part, whole int) string {
	if whole <= 0 {
		return "0%"
	}
	return strconv.Itoa(int(math.Round(float64(part)/float64(whole)*100))) + "%"
}

// buildOrderRow renders one order as a table row. Money is right-aligned in
// the mono face, text stays left-aligned, and the columns are ordered by what
// an operator needs first: who/what, how much, which rail, how old.
func buildOrderRow(o *types.Order, now time.Time, csrf string, withActions bool) row {
	amountFmt, amountMinor := amountPair(o.Amount, o.Currency)
	adjusted := o.Amount != o.CatalogPrice
	catalogFmt, _ := amountPair(o.CatalogPrice, o.Currency)

	r := row{
		ID:     o.ID,
		Href:   "orders/" + o.ID,
		Title:  o.ID,
		Cells:  make([]cell, 0, 8),
		Offset: o.Amount - o.CatalogPrice,
	}

	// 1. Ref + status: the two facts an operator scans for.
	r.Cells = append(r.Cells, cell{
		Label:   "Order",
		Class:   "c-ref",
		Text:    o.Ref,
		Mono:    true,
		Primary: true,
		Sub:     shortID(o.ID),
		Href:    "orders/" + o.ID,
	})
	r.Cells = append(r.Cells, cell{
		Label:  "Status",
		Class:  "c-status",
		Text:   statusLabel(o.Status),
		Tone:   statusTone(o.Status),
		IsStat: true,
		Sub:    humanAge(now.Sub(o.CreatedAt)) + " old",
	})

	// 2. Money. The catalog price only appears when the unique amount was
	// adjusted away from it, so the column stays quiet in the normal case.
	r.Cells = append(r.Cells, cell{
		Label:   "Amount",
		Class:   "num c-amount",
		Text:    amountFmt,
		Sub:     o.Currency + " " + amountMinor,
		Numeric: true,
	})
	r.ColumnsHint = catalogFmt
	r.Adjusted = adjusted

	// 3. Rail, then the transaction reference the bank statement shows.
	r.Cells = append(r.Cells, cell{
		Label: "Method",
		Class: "c-method",
		Text:  methodLabel(o.Method),
		Tone:  "mute",
	})
	txn := o.TxRef
	if txn == "" {
		txn = "-"
	}
	r.Cells = append(r.Cells, cell{
		Label: "Txn ref",
		Class: "c-txn hide-sm",
		Text:  txn,
		Mono:  true,
	})

	// 4. Who. Phone or email; the detail page carries both.
	who := o.CustomerPhone
	if who == "" {
		who = o.CustomerEmail
	}
	if who == "" {
		who = "-"
	}
	r.Cells = append(r.Cells, cell{
		Label: "Customer",
		Class: "c-who hide-sm",
		Text:  who,
		Tone:  "mute",
	})

	// 5. Age and the payment window. A terminal order has no window any more:
	// printing "in 28 min" for a fulfilled or expired order reads as though it
	// were still payable, which is how an operator chases a closed order.
	r.Cells = append(r.Cells, cell{
		Label:   "Window",
		Class:   "c-age hide-xs",
		Text:    windowText(o, now),
		Tone:    "mute",
		Numeric: false,
		ClassN:  "",
	})

	if withActions {
		deadline := windowText(o, now)
		r.Deadline = deadline
		r.Actions = []action{
			{Kind: "approve", Label: "Approve", Order: o.ID, Method: string(o.Method), CSRF: csrf, Confirm: "Approve " + o.Ref + " (" + amountFmt + " " + o.Currency + ")? This marks it paid and queued for delivery."},
			{Kind: "reject", Label: "Reject", Order: o.ID, CSRF: csrf, Confirm: "Reject " + o.Ref + "? The order is cancelled and cannot be fulfilled."},
		}
		r.HasActions = true
	}
	return r
}

// buildNoticeRow renders one inbound payment. The unmatched ones are the work;
// the matched ones are context for the operator deciding on them.
func buildNoticeRow(n *store.Notice, now time.Time) row {
	amountFmt, amountMinor := amountPair(n.Amount, n.Currency)
	r := row{ID: n.TxnRef, Title: n.TxnRef}
	r.Cells = append(r.Cells,
		cell{Label: "Txn ref", Class: "c-ref", Text: n.TxnRef, Mono: true, Primary: true},
		cell{Label: "Status", Class: "c-status", Text: noticeStatusLabel(n.Status), Tone: noticeTone(n.Status), IsStat: true},
		cell{Label: "Amount", Class: "num c-amount", Text: amountFmt, Sub: n.Currency + " " + amountMinor, Numeric: true},
		cell{Label: "Method", Class: "c-method", Text: methodLabel(n.Method), Tone: "mute"},
		cell{Label: "Sender", Class: "c-who hide-sm", Text: dashIfEmpty(n.SenderName), Sub: n.SenderAcct, Tone: "mute"},
		cell{Label: "Outcome", Class: "c-out hide-md", Text: dashIfEmpty(n.Outcome), Mono: true, Tone: "mute"},
		cell{Label: "Received", Class: "c-age hide-xs", Text: humanAge(now.Sub(n.ReceivedAt)) + " ago", Tone: "mute"},
	)
	if n.OrderID != "" {
		r.Href = "orders/" + n.OrderID
	}
	return r
}

func noticeStatusLabel(s string) string {
	switch strings.ToLower(s) {
	case "unmatched":
		return "Unmatched"
	case "matched":
		return "Matched"
	case "review":
		return "Needs review"
	case "late":
		return "Late"
	case "duplicate":
		return "Duplicate"
	case "":
		return "Unknown"
	}
	return s
}

func noticeTone(s string) string {
	switch strings.ToLower(s) {
	case "unmatched", "review":
		return "warn"
	case "matched":
		return "ok"
	case "late", "duplicate":
		return "bad"
	}
	return "mute"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseTemplates loads the embedded templates with a fixed function map. Every
// page is defined in its own file and executed by name, so a page can never
// render with another page's data type by accident.
func parseTemplates() (*template.Template, error) {
	// body dispatches the shell to the page's own content block. html/template's
	// template action only accepts a constant name, so the dispatch is a
	// function on the template set (see the bodyFn hook below).
	// The helper map has to exist before parsing, because the parser resolves
	// every function name as it reads the templates.
	// The shell calls {{body .}}, and html/template resolves every function
	// name while parsing, so `body` must be in the map BEFORE ParseFS. The
	// dispatcher, however, needs the finished set to execute the page block -
	// a chicken-and-egg the closure below resolves by reading `set` at EXECUTE
	// time, by which point it is assigned.
	var set *template.Template
	funcs := templateFunctions()
	funcs["body"] = func(p page) (template.HTML, error) {
		if set == nil {
			return "", errors.New("adminui: template set not initialised")
		}
		return bodyFn(set)(p)
	}
	parsed, err := template.New("admin").Funcs(funcs).ParseFS(fs.FS(assets), "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("adminui: parse templates: %w", err)
	}
	set = parsed
	return parsed, nil
}

// bodyFn returns a function that executes the named template for the given
// page into the surrounding render.
func bodyFn(set *template.Template) func(p page) (template.HTML, error) {
	return func(p page) (template.HTML, error) {
		var buf strings.Builder
		if err := set.ExecuteTemplate(&buf, p.Content, p); err != nil {
			return "", err
		}
		return template.HTML(buf.String()), nil // #nosec G203 -- rendered from our own embedded templates
	}
}

// templateFunctions is the helper set the templates are allowed to use.
func templateFunctions() template.FuncMap {
	funcs := template.FuncMap{
		"amount":      formatAmount,
		"amountMinor": func(minor int64) string { return strconv.FormatInt(minor, 10) },
		"stamp":       stamp,
		"humanAge":    humanAge,
		"humanUntil":  humanUntil,
		"statusLabel": statusLabel,
		"statusTone":  statusTone,
		"methodLabel": methodLabel,
		"shortID":     shortID,
		"initials":    initials,
		"pct":         pct,
		"dash":        dashIfEmpty,
		"half":        func(n int) int { return (n + 1) / 2 },
		"even":        func(i int) bool { return i%2 == 0 },
		"currencyOf":  func(cur string) string { return cur },
		"offsetSign":  offsetSign,
		"title":       strings.Title, // #nosec G103 -- used only for already-English labels
		// "eq" is deliberately NOT overridden: the builtin already compares
		// strings, ints and bools, and a string-only shadow breaks
		// `{{if eq (len .Rows) 50}}` with "expected string; got int".
		"joinCurrencies": func(ts []moneyTotals) string {
			if len(ts) == 0 {
				return "-"
			}
			parts := make([]string, 0, len(ts))
			for _, t := range ts {
				parts = append(parts, formatAmount(t.Minor, t.Currency)+" "+t.Currency)
			}
			return strings.Join(parts, " / ")
		},
	}
	return funcs
}

// offsetSign renders the unique-amount adjustment as a signed minor value.
func offsetSign(n int64) string {
	switch {
	case n > 0:
		return "+" + strconv.FormatInt(n, 10)
	case n < 0:
		return strconv.FormatInt(n, 10)
	}
	return "0"
}
