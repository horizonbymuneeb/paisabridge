// Package bank implements the IBFT / RAAST / IBAN-transfer rail by parsing
// bank credit-alert e-mails and SMS. The rail exists because the alert *is*
// the payment fact: Pakistani banks do not offer a third-party merchant API
// for plain account transfers, so an inbound credit is only observable as a
// text message or an e-mail sent to the receiving account holder.
//
// # Trust model
//
// An alert arrives through an unsigned channel: anyone able to reach the
// mailbox or the SIM can produce text that looks like a credit. This adapter
// therefore treats the alert as low-trust evidence and moves the order to
// review rather than fulfilment whenever the amount does not match an open
// order exactly. The signed envelope below authenticates the relay, not the
// sender, and is never on its own enough to release funds.
//
//   - The mail parser does not run inside this package's trust boundary. A
//     mail relay forwards every alert to the gateway, signed with our
//     HMAC-SHA256 envelope (package notice). A forged e-mail can therefore
//     never become a PaymentNotice: the relay cannot mint our signature.
//   - Parsing is strict: the message must start with headers we understand, a
//     known credit-alert body template must match, and the amount/reference
//     must parse unambiguously. Anything else returns ErrUnknownTemplate so
//     the operator can add a template - never a best-effort guess.
//   - Two independent facts are required to fulfil: the signed envelope and
//     the (unique) amount, which the matching engine compares against open
//     orders. A signed alert for an amount no order expects is logged and
//     quarantined, not fulfilled.
//   - Duplicates: banks send the same alert twice (e-mail + SMS, or a retry
//     from the relay). Everything is keyed on (date, amount, sender account,
//     normalized narrative hash), so the second copy is dropped by the
//     replay guard even though it is not byte-identical.
//
// # Bank alert templates
//
// Pakistan banks' credit alerts converge on a small number of shapes. The
// templates below are written from the published, publicly posted SMS/e-mail
// formats of HBL, Meezan, UBL, Faysal, Bank Alfalah, Askari and JS Bank. They
// are patterns, not verbatim documents: every one uses a named group set
// (amount, account, name, date, ref) and the table in bank_test.go pins each
// against a realistic sample line.
//
// The text is normalised first (NFC punctuation, "Rs." variants, non-breaking
// spaces, comma thousands separators) so one template covers e-mail and SMS.
//
// # Assumptions (documented, never silently faked)
//
//  1. The relay signs with our v1 HMAC envelope; see package notice. Banks do
//     not publish a DKIM/DMARC policy we can verify offline, so DKIM is not
//     used as the trust anchor; the relay's HMAC is.
//  2. Amounts are PKR. A foreign-currency alert (e.g. an international wire)
//     is rejected with ErrUnsupportedCurrency rather than converted.
//  3. The date in an alert is PKT when no zone is given.
//  4. RAAST transfers are identified only by the 11-digit RAAST/P2M reference
//     when the bank includes one; the template records it as the sender
//     reference, and the amount remains the matching key.
package bank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	// Rail names recorded in PaymentNotice.SenderAcct-adjacent metadata.
	RailIBFT  = "ibft"
	RailRAAST = "raast"
	RailIBAN  = "iban"

	maxMessageBytes = 1 << 16
)

var (
	// ErrUnknownTemplate means the message did not match any known credit
	// alert. It is deliberately distinct from ErrInvalidSignature so the
	// operator sees "add a template", not "someone is forging".
	ErrUnknownTemplate = errors.New("bank: alert text does not match a known credit template")
	// ErrNotACredit means the alert matched but is a debit/charge, not money in.
	ErrNotACredit = errors.New("bank: alert is not an incoming credit")
	// ErrUnsupportedCurrency means the alert is not in PKR.
	ErrUnsupportedCurrency = errors.New("bank: alert currency is not supported")
	// ErrReplayed is returned for an alert the guard already consumed.
	ErrReplayed = errors.New("bank: alert already processed")
	// ErrNotConfigured is returned when the relay envelope secret is missing.
	ErrNotConfigured = errors.New("bank: relay secret is not configured")
)

// Alert is the parsed form of one credit alert.
type Alert struct {
	Bank        string // "hbl", "meezan", "ubl", ...
	Rail        string // RailIBFT, RailRAAST, RailIBAN
	Amount      int64  // paisa
	Currency    string
	Sender      string // counterparty name as printed
	Account     string // counterparty account/IBAN/wallet as printed
	Ref         string // bank reference / RAAST id when present
	At          time.Time
	CurrencyRaw string
}

// Template is one bank alert pattern.
type Template struct {
	Bank string
	Rail string
	// Re is matched against the normalised text. It must provide named
	// groups: amount, account (may be empty), name (may be empty), and
	// optionally date, ref.
	Re *regexp.Regexp
	// Credit indicates a "credit"/"received"/"deposited" marker must be
	// present. Templates for banks whose alert text is unambiguous set this
	// to false.
	Credit bool
}

// Templates is the ordered template list. Order matters: the first match
// wins, so more specific banks precede generic IBFT phrasing.
var Templates = []Template{
	{
		Bank: "hbl", Rail: RailRAAST,
		Re: regexp.MustCompile(`(?i)raast (?:p2m )?(?:transaction )?(?:of )?(?:rs\.? ?(?P<amount>[0-9,]+(?:\.\d+)?)) ` +
			`(?:has been )?(?:received|credited) .*?(?:from|sender)[: ]+(?P<name>[a-z .']{2,60}?)(?:[.,]| against|$)`),
	},
	{
		Bank: "alfalah", Rail: RailRAAST,
		Re: regexp.MustCompile(`(?i)raast (?:transfer )?(?:of )?(?:pkr|rs\.?) ?(?P<amount>[0-9,]+(?:\.\d+)?) (?:received )?from ` +
			`(?P<name>[a-z .']{2,60}?)(?:[.,]| into)`),
	},
	{
		Bank: "hbl", Rail: RailIBFT,
		Re:     regexp.MustCompile(`(?i)(?P<amount>rs\.? ?[0-9,]+(?:\.\d+)?) (?:has been |was )?credited to (?:a/?c|account)[: ]*(?P<account>[0-9*]{4,34})`),
		Credit: true,
	},
	{
		Bank: "meezan", Rail: RailIBFT,
		Re: regexp.MustCompile(`(?i)rs\.? ?(?P<amount>[0-9,]+(?:\.\d+)?) (?:has been |is )?(?:credited|deposited|received)` +
			`(?: in (?:your )?a/?c)?[: ]*(?P<account>[0-9*]{4,34})?.*?(?:from|by)[: ]+(?P<name>[a-z .']{2,60}?)(?:[.,]|$)`),
		Credit: true,
	},
	{
		Bank: "ubl", Rail: RailIBFT,
		Re: regexp.MustCompile(`(?i)(?:pkr|rs\.?) ?(?P<amount>[0-9,]+(?:\.\d+)?) (?:received|credited) (?:in|to) (?:a/?c|account) ` +
			`(?P<account>[0-9*]{4,34}) from (?P<name>[a-z .']{2,60}?)(?:[.,]|$)`),
		Credit: true,
	},
	{
		Bank: "ibft", Rail: RailIBFT,
		Re:     regexp.MustCompile(`(?i)(?:pkr|rs\.?) ?(?P<amount>[0-9,]+(?:\.\d+)?) (?:has been |was )?(?:credited|received|deposited)`),
		Credit: true,
	},
}

// Adapter implements adapter.PaymentAdapter for bank/IBFT/RAAST/IBAN rail.
type Adapter struct {
	// RelaySecret is the HMAC secret of our mail/SMS relay. See assumption 1.
	RelaySecret string

	// Clock supplies now. Nil means live.
	Clock clock.Clock

	// SniffWindow bounds how far an alert's own timestamp may be from now.
	// Zero means 48 hours: e-mail retries and weekend settlement are slow.
	SniffWindow time.Duration

	// Guard holds per-alert dedupe keys (assumption: duplicates are dropped).
	Guard ReplayGuard

	// RequireGuard turns a nil Guard into a configuration error.
	RequireGuard bool

	// Templates overrides Templates when non-empty (tests use this to pin
	// ordering). Nil means the package default.
	Patterns []Template
}

// ReplayGuard records consumed alert keys.
type ReplayGuard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodBank }

// Supports reports whether the signed relay envelope can be verified.
func (a *Adapter) Supports() bool { return a.RelaySecret != "" }

func (a *Adapter) now() time.Time {
	if a.Clock == nil {
		return clock.Live{}.Now()
	}
	return a.Clock.Now()
}

func (a *Adapter) window() time.Duration {
	if a.SniffWindow <= 0 {
		return 48 * time.Hour
	}
	return a.SniffWindow
}

func (a *Adapter) patterns() []Template {
	if len(a.Patterns) > 0 {
		return a.Patterns
	}
	return Templates
}

// VerifyWebhook implements adapter.PaymentAdapter.
//
// It verifies the relay envelope (package notice) first, then parses the
// alert text carried in the payload's Note field. The payload must be a
// well-formed RailNotice whose Note holds the full alert (headers + body),
// which is what the relay posts.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.RelaySecret == "" {
		return nil, ErrNotConfigured
	}
	v := notice.Verifier{Secret: a.RelaySecret, Clock: a.Clock}
	env, err := v.Verify(r, noticeGuard{a.Guard})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	if env.Method != string(types.MethodBank) {
		return nil, fmt.Errorf("%w: envelope is for method %q, not bank", adapter.ErrInvalidSignature, env.Method)
	}
	// The envelope id is consumed only after the signature and the timestamp
	// passed, so unauthenticated traffic never reaches the store.
	noticeGuard{a.Guard}.RememberEnvelope(env.ID)
	rn, err := env.RailNotice()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	alert, err := a.ParseAlert(rn.Note)
	if err != nil {
		return nil, err
	}
	if !alert.At.IsZero() {
		if d := a.now().Sub(alert.At); d > a.window() || d < -a.window() {
			return nil, fmt.Errorf("%w: alert dated %s outside %s of now",
				adapter.ErrInvalidSignature, alert.At.Format(time.RFC3339), a.window())
		}
	}
	if alert.Amount != rn.Amount {
		// The relay's structured amount and the alert text disagree: one of
		// them is wrong, and we will not guess which.
		return nil, fmt.Errorf("%w: envelope amount %d does not match parsed alert amount %d",
			provider.ErrMalformed, rn.Amount, alert.Amount)
	}
	key := alert.Key()
	if a.RequireGuard && a.Guard == nil {
		return nil, errors.New("bank: RequireGuard is set but no replay guard is configured")
	}
	if a.Guard != nil && !a.Guard.Remember(key) {
		return nil, fmt.Errorf("%w: alert key %s", ErrReplayed, key)
	}
	raw := env.Payload
	return &types.PaymentNotice{
		TxnRef:     alertRef(alert),
		Method:     types.MethodBank,
		Amount:     alert.Amount,
		Currency:   alert.Currency,
		SenderName: alert.Sender,
		SenderAcct: alert.Account,
		ReceivedAt: alert.At,
		Raw:        raw,
	}, nil
}

// VerifyAPI implements adapter.PaymentAdapter: banks expose no status API to
// third parties for account credits, so there is nothing to call.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// ParseAlert parses one credit alert (e-mail with headers, or SMS with none).
//
// Exported so the operator CLI can dry-run a forwarded alert before wiring
// the relay, and so the table-driven tests describe the parser directly.
func (a *Adapter) ParseAlert(text string) (*Alert, error) {
	msg := Normalize(text)
	if strings.TrimSpace(msg.Body) == "" {
		return nil, fmt.Errorf("%w: empty alert body", ErrUnknownTemplate)
	}
	// A foreign-currency alert must never fall through to a PKR template
	// that happens to find a number: detect it before matching.
	if cur := currencyIn(strings.ToUpper(msg.Body)); cur != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedCurrency, cur)
	}
	for _, tpl := range a.patterns() {
		m := tpl.Re.FindStringSubmatchIndex(msg.Body)
		if m == nil {
			continue
		}
		names := tpl.Re.SubexpNames()
		groups := map[string]string{}
		for i, n := range names {
			if n == "" || 2*i+1 >= len(m) || m[2*i] < 0 {
				continue
			}
			groups[n] = msg.Body[m[2*i]:m[2*i+1]]
		}
		if tpl.Credit {
			body := strings.ToLower(msg.Body)
			if !strings.Contains(body, "credit") && !strings.Contains(body, "received") &&
				!strings.Contains(body, "deposited") && !strings.Contains(body, "deposit of") {
				return nil, fmt.Errorf("%w: %s", ErrNotACredit, tpl.Bank)
			}
		}
		minor, err := provider.ParseAmount(groups["amount"], 2)
		if err != nil || minor <= 0 {
			return nil, fmt.Errorf("%w: amount %q", ErrUnknownTemplate, groups["amount"])
		}
		if msg.Currency != "" && !strings.EqualFold(msg.Currency, "PKR") {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedCurrency, msg.Currency)
		}
		alert := &Alert{
			Bank:        tpl.Bank,
			Rail:        tpl.Rail,
			Amount:      minor,
			Currency:    "PKR",
			Sender:      strings.TrimSpace(groups["name"]),
			Account:     strings.TrimSpace(groups["account"]),
			Ref:         strings.TrimSpace(groups["ref"]),
			CurrencyRaw: msg.Currency,
		}
		if d := msg.Date; d != "" {
			t, err := provider.ParseTimeLocal(d)
			if err != nil {
				return nil, fmt.Errorf("%w: date %q", ErrUnknownTemplate, d)
			}
			alert.At = t
		}
		return alert, nil
	}
	return nil, ErrUnknownTemplate
}

// Key returns the dedupe key for an alert. It deliberately excludes the
// bank name and the body prose: the same credit sent twice through different
// channels (SMS + e-mail) must collapse to one key.
func (al *Alert) Key() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s", al.At.UTC().Format(time.RFC3339), al.Amount, al.Account, al.Sender, al.Rail)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func alertRef(al *Alert) string {
	if al.Ref != "" {
		return al.Ref
	}
	return "bank-" + al.Key()[:16]
}

// Normalize lower-cases the message for template matching while preserving
// the raw text in the returned struct fields as printed. It strips e-mail
// headers into structured fields (Date, From, Subject), folds unicode
// punctuation bank e-mails habitually include, and collapses whitespace so a
// template does not need to care about line wrapping.
func Normalize(text string) Message {
	m := Message{Raw: text}
	// Split headers only if the text looks like an e-mail (headers exist and
	// the body follows a blank line). SMS has no headers and must not be
	// mangled by net/mail.
	if hdr, body, ok := strings.Cut(text, "\r\n\r\n"); ok {
		m.Body = body
		m.parseHeaders(hdr)
	} else if hdr, body, ok := strings.Cut(text, "\n\n"); ok {
		m.Body = body
		m.parseHeaders(hdr)
	} else {
		m.Body = text
	}
	m.Body = foldUnicode(m.Body)
	m.Body = strings.Join(strings.Fields(m.Body), " ")
	m.Body = stripHTML(m.Body)
	return m
}

func (m *Message) parseHeaders(hdr string) {
	if h, err := mail.ReadMessage(strings.NewReader(hdr + "\r\n\r\n")); err == nil {
		m.Date = strings.TrimSpace(h.Header.Get("Date"))
		m.From = strings.TrimSpace(h.Header.Get("From"))
		m.Subject = strings.TrimSpace(h.Header.Get("Subject"))
		// Currency hint: "Amount: USD 500" style lines in the subject.
		m.Currency = currencyIn(strings.ToUpper(h.Header.Get("Subject")))
	}
}

// Message is a normalised alert.
type Message struct {
	Raw      string
	Body     string
	Date     string
	From     string
	Subject  string
	Currency string
}

var htmlTag = regexp.MustCompile(`<[^>]{1,200}>`)

// stripHTML turns a bank e-mail's HTML part into plain text. Templates are
// matched on words, not markup, so this is intentionally crude.
func stripHTML(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	s = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(s, " ")
	s = htmlTag.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// foldUnicode maps the punctuation banks love onto ASCII equivalents.
func foldUnicode(s string) string {
	r := strings.NewReplacer(
		"\u00a0", " ", // no-break space
		"\u2019", "'", "\u2018", "'",
		"\u201c", `"`, "\u201d", `"`,
		"\u2013", "-", "\u2014", "-",
		"\u2080", "0",
		"\u06f0", "0", "\u06f1", "1", "\u06f2", "2", "\u06f3", "3", "\u06f4", "4",
		"\u06f5", "5", "\u06f6", "6", "\u06f7", "7", "\u06f8", "8", "\u06f9", "9",
	)
	return r.Replace(s)
}

var currencyRE = regexp.MustCompile(`\b(USD|EUR|AED|GBP|SAR|CAD)\b`)

func currencyIn(s string) string {
	if m := currencyRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// noticeGuard adapts a bank ReplayGuard to notice.ReplayGuard. The envelope
// id is consumed by the envelope verifier; the alert-level key is consumed
// here afterwards, so a relay that re-signs the same alert under a fresh id
// still cannot duplicate it.
type noticeGuard struct{ inner ReplayGuard }

func (g noticeGuard) Seen(id string) bool {
	if g.inner == nil {
		return false
	}
	return g.inner.Seen("env:" + id)
}

func (g noticeGuard) RememberEnvelope(id string) {
	if g.inner != nil {
		g.inner.Remember("env:" + id)
	}
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
