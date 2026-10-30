package nayapay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const testSecret = "pb-nayapay-relay-secret"

// errMismatch matches the "status answer is for another payment" error.
var errMismatch = errors.New("status is for")

var testNow = time.Date(2026, time.October, 31, 9, 30, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{WebhookSecret: testSecret, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

func signedReq(secret, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/nayapay", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if secret != "" {
		r.Header.Set(signatureHeader, Sign(secret, []byte(body)))
	}
	return r
}

const validBody = `{"payment_ref":"NP-PAY-501","order_ref":"PB-ORD-1","status":"SUCCESS",` +
	`"amount":"1497.00","currency":"PKR","payer_name":"Ali Raza","payer_account":"03001234567"}`

func TestVerifyWebhookTable(t *testing.T) {
	fresh := testNow.Unix()
	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		wantErr error
		wantAmt int64
	}{
		{
			name:    "valid signed callback",
			req:     func(t *testing.T) *http.Request { return signedReq(testSecret, validBody) },
			wantAmt: 149700,
		},
		{
			name: "sig with sha256 prefix and upper-case hex",
			req: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(validBody))
				r.Header.Set(signatureHeader, "sha256="+strings.ToUpper(Sign(testSecret, []byte(validBody))))
				return r
			},
			wantAmt: 149700,
		},
		{
			name: "bare redirect without signature is rejected",
			req: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet,
					"/api/webhooks/nayapay?payment_ref=NP-PAY-501&status=SUCCESS&amount=1497.00", nil)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:    "unsigned body is rejected",
			req:     func(t *testing.T) *http.Request { return signedReq("", validBody) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:    "wrong secret is rejected",
			req:     func(t *testing.T) *http.Request { return signedReq("attacker-secret", validBody) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "body tampered after signing is rejected",
			req: func(t *testing.T) *http.Request {
				tampered := strings.Replace(validBody, `"1497.00"`, `"1.00"`, 1)
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tampered))
				r.Header.Set(signatureHeader, Sign(testSecret, []byte(validBody)))
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "non-success status is not a payment",
			req: func(t *testing.T) *http.Request {
				body := strings.Replace(validBody, "SUCCESS", "FAILED", 1)
				return signedReq(testSecret, body)
			},
			wantErr: ErrNotSuccessful,
		},
		{
			name: "missing payment reference is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, `{"order_ref":"PB-ORD-1","status":"SUCCESS","amount":"1497.00","currency":"PKR"}`)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "unknown field is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, `{"payment_ref":"NP-1","status":"SUCCESS","amount":"1497.00","currency":"PKR","bonus":"x"}`)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "stale completion timestamp is rejected",
			req: func(t *testing.T) *http.Request {
				body := `{"payment_ref":"NP-OLD","status":"SUCCESS","amount":"1497.00","currency":"PKR","completed_at":` +
					strconv.FormatInt(testNow.Add(-2*time.Hour).Unix(), 10) + `}`
				return signedReq(testSecret, body)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "fresh completion timestamp is accepted",
			req: func(t *testing.T) *http.Request {
				body := `{"payment_ref":"NP-NEW","status":"SUCCESS","amount":"1497.00","currency":"PKR","completed_at":` +
					strconv.FormatInt(fresh, 10) + `}`
				return signedReq(testSecret, body)
			},
			wantAmt: 149700,
		},
		{
			name: "zero amount is rejected",
			req: func(t *testing.T) *http.Request {
				body := strings.Replace(validBody, `"1497.00"`, `"0"`, 1)
				return signedReq(testSecret, body)
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
			if got.Method != types.MethodNayaPay {
				t.Fatalf("method: got %q", got.Method)
			}
		})
	}
}

func TestReplayRejected(t *testing.T) {
	a := newAdapter(t)
	if _, err := a.VerifyWebhook(signedReq(testSecret, validBody)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := a.VerifyWebhook(signedReq(testSecret, validBody)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: want ErrReplayed, got %v", err)
	}
}

func TestVerifyAPI(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
		wantAmt int64
	}{
		{
			name:    "successful status",
			status:  http.StatusOK,
			body:    `{"payment_ref":"NP-PAY-9","status":"SUCCESS","amount":"1497.00","currency":"PKR"}`,
			wantAmt: 149700,
		},
		{
			name:    "completion token on a 200 response is not success",
			status:  http.StatusOK,
			body:    `{"payment_ref":"NP-PAY-9","status":"PENDING","amount":"1497.00","currency":"PKR"}`,
			wantErr: ErrNotSuccessful,
		},
		{
			name:    "http error is not success",
			status:  http.StatusInternalServerError,
			body:    `{}`,
			wantErr: errors.New("500"),
		},
		{
			name:    "answer about a different payment is rejected",
			status:  http.StatusOK,
			body:    `{"payment_ref":"NP-OTHER","status":"SUCCESS","amount":"1497.00","currency":"PKR"}`,
			wantErr: errMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				if r.URL.Query().Get("payment_ref") != "NP-PAY-9" {
					t.Errorf("query: %q", r.URL.RawQuery)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			a := newAdapter(t)
			a.BaseURL = srv.URL
			a.MerchantToken = "tok"
			got, err := a.VerifyAPI(context.Background(), "NP-PAY-9")
			if gotAuth != "Basic tok" {
				t.Fatalf("authorization header: %q", gotAuth)
			}
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("want error containing %v, got notice %+v", tc.wantErr, got)
				}
				if !errors.Is(err, tc.wantErr) && !strings.Contains(err.Error(), tc.wantErr.Error()) {
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
		})
	}
}

func TestVerifyAPIUnconfigured(t *testing.T) {
	a := newAdapter(t)
	if _, err := a.VerifyAPI(context.Background(), "NP-1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestNotConfiguredWithoutSecret(t *testing.T) {
	a := &Adapter{}
	if _, err := a.VerifyWebhook(signedReq(testSecret, validBody)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a webhook secret")
	}
}
