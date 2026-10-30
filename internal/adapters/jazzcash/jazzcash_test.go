package jazzcash

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
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// testSalt is a fixture standing in for the merchant Integrity Salt.
const testSalt = "pb-jazzcash-integrity-salt"

var testNow = time.Date(2026, time.October, 31, 14, 5, 12, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{Salt: testSalt, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

// happyForm returns the callback fields the Hosted Checkout posts back. It
// deliberately mixes pg_ and pp_ keys so the ordering rules are exercised.
func happyForm(txn string, amount string) url.Values {
	return url.Values{
		"pp_Version":         {"1.1"},
		"pp_TxnType":         {"MWALLET"},
		"pp_Language":        {"EN"},
		"pp_MerchantID":      {"MC12345"},
		"pp_SubMerchantID":   {""},
		"pp_Password":        {"x"},
		"pp_BankID":          {"TBANK"},
		"pp_ProductID":       {"RETL"},
		"pp_TxnRefNo":        {txn},
		"pp_Amount":          {amount},
		"pp_TxnCurrency":     {"PKR"},
		"pp_TxnDateTime":     {testNow.Format("20060102150405")},
		"pp_BillReference":   {"ORDER-" + txn},
		"pp_Description":     {"paisabridge order"},
		"pp_TxnExpiryDateTi": {"20261031235959"},
		"pp_ReturnURL":       {"https://merchant.example/api/webhooks/jazzcash"},
		"pp_SecureHashType":  {"HMACSHA256"},
		"pp_SecureHash":      {""},
		"ppmpf_1":            {""},
		"ppmpf_2":            {""},
	}
}

func withHash(t *testing.T, form url.Values, scheme string) url.Values {
	t.Helper()
	out := url.Values{}
	for k, v := range form {
		out[k] = v
	}
	out.Set("pp_SecureHash", Sign(testSalt, scheme, out))
	return out
}

func reqWith(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/jazzcash", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestSigningMaterialOrdering(t *testing.T) {
	form := url.Values{
		"pp_b":              {"2"},
		"pg_a":              {"1"},
		"pp_a":              {"1"},
		"pp_SecureHash":     {"deadbeef"},
		"pp_SecureHashType": {"HMACSHA256"},
	}
	got := SigningMaterial(form)
	// pg_ first, then pp_, each sorted; hash fields excluded.
	wantKV := "pg_a=1&pp_a=1&pp_b=2"
	if got[SchemeKV] != wantKV {
		t.Fatalf("kv material: want %q, got %q", wantKV, got[SchemeKV])
	}
	if got[SchemeValues] != "112" {
		t.Fatalf("values material: want %q, got %q", "112", got[SchemeValues])
	}
}

func TestVerifyFormTable(t *testing.T) {
	tests := []struct {
		name    string
		scheme  string
		mode    string
		mutate  func(t *testing.T, form url.Values) url.Values
		wantErr error
		wantAmt int64
	}{
		{
			name:   "kv convention",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				return withHash(t, f, SchemeKV)
			},
			wantAmt: 149700,
		},
		{
			name:   "concatenated-values convention",
			scheme: SchemeValues,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				return withHash(t, f, SchemeValues)
			},
			wantAmt: 149700,
		},
		{
			name:   "pinned mode rejects the other convention",
			scheme: SchemeValues,
			mode:   SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				return withHash(t, f, SchemeValues)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "missing hash is rejected",
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				return f
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "tampered amount is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				signed := withHash(t, f, SchemeKV)
				signed.Set("pp_Amount", "1") // attacker edits after signing
				return signed
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "tampered txn ref is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				signed := withHash(t, f, SchemeKV)
				signed.Set("pp_TxnRefNo", "JC-ATTACKER")
				return signed
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "signature made with another salt is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				f.Set("pp_SecureHash", Sign("attacker-salt", SchemeKV, f))
				return f
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "HMACSHA1 hash type is rejected, not downgraded",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				f.Set("pp_SecureHashType", "HMACSHA1")
				return withHash(t, f, SchemeKV)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "non-success code is not a payment",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "999")
				f.Set("pp_ResponseMessage", "Invalid Merchant")
				return withHash(t, f, SchemeKV)
			},
			wantErr: ErrNotSuccessful,
		},
		{
			name:   "stale callback is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				f.Set("pp_TxnDateTime", testNow.Add(-72*time.Hour).Format("20060102150405"))
				return withHash(t, f, SchemeKV)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:   "zero amount is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				f.Set("pp_Amount", "0")
				return withHash(t, f, SchemeKV)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name:   "non-PKR currency is rejected",
			scheme: SchemeKV,
			mutate: func(t *testing.T, f url.Values) url.Values {
				f.Set("pp_ResponseCode", "000")
				f.Set("pp_TxnCurrency", "USD")
				return withHash(t, f, SchemeKV)
			},
			wantErr: provider.ErrMalformed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Mode = tc.mode
			form := tc.mutate(t, happyForm("JC-TXN-77", "149700"))
			got, err := a.VerifyForm(reqWith(t, form.Encode()))
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
			if got.Notice.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, got.Notice.Amount)
			}
			if got.Scheme != tc.scheme {
				t.Fatalf("scheme: want %q, got %q", tc.scheme, got.Scheme)
			}
			if got.Notice.Method != types.MethodJazzCash {
				t.Fatalf("method: got %q", got.Notice.Method)
			}
		})
	}
}

func TestVerifyFormDecimalAmountIsAccepted(t *testing.T) {
	a := newAdapter(t)
	form := happyForm("JC-DEC-1", "1497.00")
	form.Set("pp_ResponseCode", "000")
	form = withHash(t, form, SchemeKV)
	got, err := a.VerifyForm(reqWith(t, form.Encode()))
	if err != nil {
		t.Fatalf("decimal amount: %v", err)
	}
	if got.Notice.Amount != 149700 {
		t.Fatalf("want 149700 paisa, got %d", got.Notice.Amount)
	}
}

func TestReplayIsRejected(t *testing.T) {
	a := newAdapter(t)
	form := happyForm("JC-REPLAY-77", "149700")
	form.Set("pp_ResponseCode", "000")
	body := withHash(t, form, SchemeKV).Encode()

	if _, err := a.VerifyWebhook(reqWith(t, body)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := a.VerifyWebhook(reqWith(t, body)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("byte-identical replay: want ErrReplayed, got %v", err)
	}
	// A second callback with a *fresh valid signature* but the same txn ref
	// is equally a duplicate: idempotency keys on the rail reference, not the
	// signature, so re-signing cannot launder a duplicate.
	again := withHash(t, form, SchemeValues)
	if _, err := a.VerifyWebhook(reqWith(t, again.Encode())); !errors.Is(err, ErrReplayed) {
		t.Fatalf("re-signed duplicate: want ErrReplayed, got %v", err)
	}
}

func TestNotConfiguredWithoutSalt(t *testing.T) {
	a := &Adapter{Clock: clock.Fixed(testNow)}
	if _, err := a.VerifyForm(reqWith(t, "pp_TxnRefNo=X")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a salt")
	}
}

func TestVerifyAPIUnsupported(t *testing.T) {
	a := newAdapter(t)
	if _, err := a.VerifyAPI(context.Background(), "JC-1"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestParseFormRejectsNonFormBody(t *testing.T) {
	a := newAdapter(t)
	for _, body := range []string{"", "  ", `{"pp_TxnRefNo":"x"}`} {
		if _, err := a.VerifyForm(reqWith(t, body)); err == nil {
			t.Fatalf("body %q: want error", body)
		}
	}
}
