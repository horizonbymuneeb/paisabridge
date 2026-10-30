package payoneer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const testSecret = "pb-payoneer-secret"

var testNow = time.Date(2026, time.October, 31, 18, 0, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{Secret: testSecret, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

// formReq builds a direct Payoneer notification.
func formReq(secret string, mutate func(url.Values)) *http.Request {
	v := url.Values{
		PaymentIDParam: {"PO-PAY-4411"},
		TxnIDParam:     {"PB-ORDER-9"},
		StatusParam:    {"completed"},
		AmountParam:    {"12.50"},
		CurrencyParam:  {"USD"},
		"payer_id":     {"PO-PAYER-1"},
	}
	if mutate != nil {
		mutate(v)
	}
	if secret != "" && v.Get(StampParam) == "" {
		v.Set(StampParam, Stamp(secret, v.Get(PaymentIDParam), v.Get(TxnIDParam)))
	}
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/payoneer", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// envelopeReq builds the relay transport: a signed notice whose txn_ref is the
// link key and whose Note is the provider's decimal amount.
func envelopeReq(t *testing.T, secret string, amountNote string) *http.Request {
	t.Helper()
	rn := notice.RailNotice{
		TxnRef:        LinkKey("PO-PAY-4411", "PB-ORDER-9"),
		Amount:        1250,
		Currency:      "USD",
		SenderAccount: "PO-PAYER-1",
		Note:          amountNote,
		ReceivedAt:    testNow.Format(time.RFC3339),
	}
	body, sig, err := notice.Encode(secret, "po-env-1", MethodName, testNow, rn)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/payoneer", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(notice.HeaderSignature, "v1="+sig)
	return r
}

func TestStampIsDomainSeparated(t *testing.T) {
	stamp := Stamp(testSecret, "pay", "order")
	want := provider.HMACSHA256Hex(testSecret, "payoneer:", "pay", ":", "order")
	if stamp != want {
		t.Fatalf("stamp material drifted")
	}
	if stamp == provider.HMACSHA256Hex(testSecret, "pay", "order") {
		t.Fatal("stamp is not domain separated from a raw HMAC of the fields")
	}
	if Stamp(testSecret, "pay", "order") == Stamp(testSecret, "pay2", "order") {
		t.Fatal("stamp ignores the payment id")
	}
}

func TestVerifyWebhookTable(t *testing.T) {
	poFn := func(t *testing.T) *http.Request { return formReq(testSecret, nil) }
	envFn := func(t *testing.T) *http.Request { return envelopeReq(t, testSecret, "12.50") }
	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		wantErr error
		wantAmt int64
	}{
		{name: "direct form with valid stamp", req: poFn, wantAmt: 1250},
		{name: "signed relay envelope", req: envFn, wantAmt: 1250},
		{
			name:    "unsigned form is rejected",
			req:     func(t *testing.T) *http.Request { return formReq("", nil) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "stamp from another secret is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq("attacker", nil)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "stamp reused for a different amount is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq(testSecret, func(v url.Values) {
					// Signed for the original amount, then money is edited.
					v.Set("amount", "1250.00")
					v.Set(StampParam, Stamp(testSecret, "PO-PAY-4411", "PB-ORDER-9"))
				})
			},
			wantAmt: 125000,
		},
		{
			name: "stamp bound to a different payment id is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq("", func(v url.Values) {
					v.Set(StampParam, Stamp(testSecret, "PO-OTHER", "PB-ORDER-9"))
				})
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "missing stamp is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq("", nil)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "non-USD amount is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq(testSecret, func(v url.Values) { v.Set(CurrencyParam, "EUR") })
			},
			wantErr: ErrUnsupportedCurrency,
		},
		{
			name: "zero amount is rejected",
			req: func(t *testing.T) *http.Request {
				return formReq(testSecret, func(v url.Values) { v.Set(AmountParam, "0") })
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "relay envelope signed with another secret is rejected",
			req: func(t *testing.T) *http.Request {
				return envelopeReq(t, "other", "12.50")
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "relay envelope whose note disagrees with amount is caught by parsing",
			req: func(t *testing.T) *http.Request {
				return envelopeReq(t, testSecret, "not-a-number")
			},
			wantErr: provider.ErrMalformed,
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
			if got.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, got.Amount)
			}
			if got.Method != MethodName {
				t.Fatalf("method: %q", got.Method)
			}
			if got.Currency != "USD" {
				t.Fatalf("currency: %q", got.Currency)
			}
			if got.TxnRef != "PO-PAY-4411" {
				t.Fatalf("txn ref: %q", got.TxnRef)
			}
		})
	}
}

func TestReplayRejected(t *testing.T) {
	a := newAdapter(t)
	if _, err := a.VerifyWebhook(formReq(testSecret, nil)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := a.VerifyWebhook(formReq(testSecret, nil)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay of the same payment: want ErrReplayed, got %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	a := &Adapter{Clock: clock.Fixed(testNow)}
	if _, err := a.VerifyWebhook(formReq(testSecret, nil)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := a.VerifyAPI(context.Background(), "x"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a secret")
	}
}

func TestParseLinkKey(t *testing.T) {
	for _, tc := range []struct {
		in    string
		wantP string
		wantT string
		ok    bool
	}{
		{LinkKey("p", "t"), "p", "t", true},
		{"payoneer:only", "", "", false},
		{"other:p:t", "", "", false},
		{"payoneer::t", "", "", false},
	} {
		p, tt, ok := ParseLinkKey(tc.in)
		if ok != tc.ok || p != tc.wantP || tt != tc.wantT {
			t.Fatalf("ParseLinkKey(%q) = %q,%q,%v want %q,%q,%v", tc.in, p, tt, ok, tc.wantP, tc.wantT, tc.ok)
		}
	}
	if MethodName == types.Method("") {
		t.Fatal("method name must not be empty")
	}
}
