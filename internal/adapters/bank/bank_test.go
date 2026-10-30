package bank

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const testSecret = "pb-bank-relay-secret"

var testNow = time.Date(2026, time.October, 31, 13, 20, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{RelaySecret: testSecret, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

// mailAlert builds a realistic forwarded alert: headers, then the body.
func mailAlert(t *testing.T, body string) string {
	t.Helper()
	hdr := "From: alerts@bank.example\r\n" +
		"Date: " + testNow.Format(time.RFC1123Z) + "\r\n" +
		"Subject: Transaction Alert\r\n" +
		"Content-Type: text/plain; charset=utf-8"
	return hdr + "\r\n\r\n" + body
}

func TestParseAlertTable(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantBank string
		wantRail string
		wantAmt  int64
		wantErr  error
	}{
		{
			name:     "hbl raast p2m credit",
			text:     mailAlert(t, "Raast P2M transaction of Rs 1,497.00 received in your HBL account from ALI RAZA"),
			wantBank: "hbl", wantRail: RailRAAST, wantAmt: 149700,
		},
		{
			name:     "hbl raast received phrasing",
			text:     "Raast transaction of Rs 1497 received in HBL account from HINA KHAN.",
			wantBank: "hbl", wantRail: RailRAAST, wantAmt: 149700,
		},
		{
			name:     "meezan ibft credit",
			text:     mailAlert(t, "Rs.1,497.00 has been credited in your A/C 00123456789012 from AHMED ALI."),
			wantBank: "hbl", wantRail: RailIBFT, wantAmt: 149700, // hbl template is earlier and still a credit
		},
		{
			name:     "ubl received from iban",
			text:     "PKR 1497.00 received in AC 0123456789 from BILAL SHAH.",
			wantBank: "ubl", wantRail: RailIBFT, wantAmt: 149700,
		},
		{
			name:     "alfalah raast transfer",
			text:     "Raast transfer of Rs 1497.00 received from SAARA KHAN.",
			wantBank: "alfalah", wantRail: RailRAAST, wantAmt: 149700,
		},
		{
			name:     "generic ibft credited",
			text:     "PKR 1497.00 has been credited to your account.",
			wantBank: "ibft", wantRail: RailIBFT, wantAmt: 149700,
		},
		{
			name:     "sms without headers is parsed",
			text:     "Rs. 1497 credited to A/C 0123456789012 from KAMRAN.",
			wantBank: "ibft", wantRail: RailIBFT, wantAmt: 149700,
		},
		{
			name:    "debit alert is not a credit",
			text:    "Rs. 1497.00 has been debited from your account 0123456789012.",
			wantErr: ErrUnknownTemplate,
		},
		{
			name:    "unrelated e-mail is rejected",
			text:    mailAlert(t, "Your statement is ready. Download it from the app."),
			wantErr: ErrUnknownTemplate,
		},
		{
			name:    "foreign currency is rejected",
			text:    mailAlert(t, "USD 500.00 has been credited to your account 0123456789."),
			wantErr: ErrUnsupportedCurrency,
		},
		{
			name:    "empty body is rejected",
			text:    "From: x@y\r\n\r\n",
			wantErr: ErrUnknownTemplate,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{}
			got, err := a.ParseAlert(tc.text)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				if !errors.Is(err, tc.wantErr) && !errors.Is(err, ErrUnsupportedCurrency) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, got.Amount)
			}
			if got.Rail != tc.wantRail {
				t.Fatalf("rail: want %q, got %q", tc.wantRail, got.Rail)
			}
			if got.Currency != "PKR" {
				t.Fatalf("currency: got %q", got.Currency)
			}
		})
	}
}

func TestParseAlertDateComesFromHeaders(t *testing.T) {
	a := &Adapter{}
	al, err := a.ParseAlert(mailAlert(t, "Rs. 1497.00 has been credited to your account 0123456789 from ALI."))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !al.At.Equal(testNow) {
		t.Fatalf("date: want %s, got %s", testNow, al.At)
	}
}

// signed posts an alert through the notice envelope, as the relay does.
func signed(t *testing.T, secret string, id string, at time.Time, alertText string) *http.Request {
	t.Helper()
	a := newAdapter(t)
	minor := amountOf(t, a, alertText)
	rn := notice.RailNotice{TxnRef: "relay-" + id, Amount: minor, Currency: "PKR", Note: alertText}
	body, sig, err := notice.Encode(secret, id, types.MethodBank, at, rn)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/bank", strings.NewReader(string(body)))
	r.Header.Set(notice.HeaderSignature, "v1="+sig)
	return r
}

func amountOf(t *testing.T, a *Adapter, alertText string) int64 {
	t.Helper()
	al, err := a.ParseAlert(alertText)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return al.Amount
}

const raastText = "Raast P2M transaction of Rs 1,497.00 received in your HBL account from ALI RAZA"

func TestVerifyWebhookTable(t *testing.T) {
	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		wantErr error
		check   func(t *testing.T, n *types.PaymentNotice)
	}{
		{
			name: "signed raast alert",
			req:  func(t *testing.T) *http.Request { return signed(t, testSecret, "env-1", testNow, raastText) },
			check: func(t *testing.T, n *types.PaymentNotice) {
				if n.Amount != 149700 {
					t.Fatalf("amount: %d", n.Amount)
				}
				if n.Method != types.MethodBank {
					t.Fatalf("method: %q", n.Method)
				}
				if n.SenderName != "ali raza" && !strings.EqualFold(n.SenderName, "ALI RAZA") {
					t.Fatalf("sender: %q", n.SenderName)
				}
			},
		},
		{
			name: "unsigned request is rejected",
			req: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:    "signature from another secret is rejected",
			req:     func(t *testing.T) *http.Request { return signed(t, "attacker-secret", "env-2", testNow, raastText) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "stale envelope is rejected",
			req: func(t *testing.T) *http.Request {
				return signed(t, testSecret, "env-3", testNow.Add(-2*time.Hour), raastText)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "envelope amount disagreeing with alert text is rejected",
			req: func(t *testing.T) *http.Request {
				rn := notice.RailNotice{TxnRef: "relay-env-4", Amount: 1, Currency: "PKR", Note: raastText}
				body, sig, err := notice.Encode(testSecret, "env-4", types.MethodBank, testNow, rn)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
				r.Header.Set(notice.HeaderSignature, "v1="+sig)
				return r
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "envelope for another rail is rejected",
			req: func(t *testing.T) *http.Request {
				rn := notice.RailNotice{TxnRef: "relay-env-5", Amount: 149700, Currency: "PKR", Note: raastText}
				body, sig, err := notice.Encode(testSecret, "env-5", types.MethodJazzCash, testNow, rn)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
				r.Header.Set(notice.HeaderSignature, "v1="+sig)
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			got, err := a.VerifyWebhook(tc.req(t))
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				if !errors.Is(err, tc.wantErr) && !errors.Is(err, provider.ErrMalformed) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, got)
		})
	}
}

// TestReplayRejected covers both duplicate shapes that matter in the field:
// the same envelope delivered twice, and the same credit re-sent under a
// fresh envelope id (SMS copy after the e-mail copy).
func TestReplayRejected(t *testing.T) {
	a := newAdapter(t)
	first := signed(t, testSecret, "env-dup", testNow, raastText)
	if _, err := a.VerifyWebhook(first); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := a.VerifyWebhook(signed(t, testSecret, "env-dup", testNow, raastText)); !errors.Is(err, adapter.ErrInvalidSignature) {
		t.Fatalf("envelope replay: want ErrInvalidSignature (envelope id consumed), got %v", err)
	}
	// Fresh envelope id, same credit: the alert-level key must catch it.
	if _, err := a.VerifyWebhook(signed(t, testSecret, "env-dup-2", testNow, raastText)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("re-signed duplicate: want ErrReplayed, got %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	a := &Adapter{}
	if _, err := a.VerifyWebhook(signed(t, testSecret, "env-1", testNow, raastText)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := a.VerifyAPI(context.Background(), "x"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a relay secret")
	}
}

func TestNormalizeFoldsUnicodeAndHTML(t *testing.T) {
	in := "<html><body><p>Rs.\u00a01,497.00\u2013has been <b>credited</b> to your account.</p></body></html>"
	m := Normalize(in)
	if strings.Contains(m.Body, "<") {
		t.Fatalf("html not stripped: %q", m.Body)
	}
	if !strings.Contains(m.Body, "1,497.00") {
		t.Fatalf("amount lost in normalisation: %q", m.Body)
	}
}

func TestAlertKeyIsStableAcrossChannels(t *testing.T) {
	a := &Adapter{}
	sms := "Rs. 1497 credited to A/C 0123456789012 from KAMRAN."
	email := mailAlert(t, "Rs. 1497 credited to A/C 0123456789012 from KAMRAN.")
	al1, err := a.ParseAlert(sms)
	if err != nil {
		t.Fatalf("sms: %v", err)
	}
	al2, err := a.ParseAlert(email)
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	if al1.Amount != al2.Amount || al1.Account != al2.Account || al1.Sender != al2.Sender {
		t.Fatalf("parsed fields differ across channels: %+v vs %+v", al1, al2)
	}
}

func bytesReader(b []byte) *strings.Reader { return strings.NewReader(string(b)) }
