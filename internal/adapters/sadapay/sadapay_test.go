package sadapay

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

const testSecret = "pb-sadapay-relay-secret"

var testNow = time.Date(2026, time.October, 31, 11, 0, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{WebhookSecret: testSecret, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

func signedReq(secret, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/sadapay", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if secret != "" {
		r.Header.Set(signatureHeader, Sign(secret, []byte(body)))
	}
	return r
}

const validBody = `{"payment_id":"SP-9001","confirmation_code":"418302","status":"COMPLETED",` +
	`"amount":"1497.00","currency":"PKR","payer_name":"Hina","payer_phone":"03211234567"}`

func TestVerifyWebhookTable(t *testing.T) {
	tests := []struct {
		name     string
		req      func(t *testing.T) *http.Request
		wantErr  error
		wantAmt  int64
		wantCode string
	}{
		{
			name:     "valid completed payment",
			req:      func(t *testing.T) *http.Request { return signedReq(testSecret, validBody) },
			wantAmt:  149700,
			wantCode: "418302",
		},
		{
			name:    "unsigned callback is rejected",
			req:     func(t *testing.T) *http.Request { return signedReq("", validBody) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:    "wrong secret is rejected",
			req:     func(t *testing.T) *http.Request { return signedReq("other", validBody) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "confirmation code swapped after signing is rejected",
			req: func(t *testing.T) *http.Request {
				tampered := strings.Replace(validBody, "418302", "000000", 1)
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tampered))
				r.Header.Set(signatureHeader, Sign(testSecret, []byte(validBody)))
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "closed card is not a payment",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, strings.Replace(validBody, "COMPLETED", "FAILED", 1))
			},
			wantErr: ErrNotSuccessful,
		},
		{
			name: "five digit confirmation code is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, strings.Replace(validBody, `"418302"`, `"41830"`, 1))
			},
			wantErr: ErrBadConfirmationCode,
		},
		{
			name: "non-numeric confirmation code is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, strings.Replace(validBody, `"418302"`, `"41A302"`, 1))
			},
			wantErr: ErrBadConfirmationCode,
		},
		{
			name: "missing payment id is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, `{"confirmation_code":"418302","status":"COMPLETED","amount":"1497.00"}`)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "too many decimals is rejected",
			req: func(t *testing.T) *http.Request {
				return signedReq(testSecret, strings.Replace(validBody, `"1497.00"`, `"1497.005"`, 1))
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "stale completion is rejected",
			req: func(t *testing.T) *http.Request {
				body := `{"payment_id":"SP-OLD","confirmation_code":"111111","status":"COMPLETED",` +
					`"amount":"1497.00","completed_at":` + strconv.FormatInt(testNow.Add(-time.Hour).Unix(), 10) + `}`
				return signedReq(testSecret, body)
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
			if got.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, got.Amount)
			}
			code, ok := ConfirmationCodeOf(got.TxnRef)
			if !ok || code != tc.wantCode {
				t.Fatalf("confirmation code: want %q, got %q (ok=%v)", tc.wantCode, code, ok)
			}
			if got.Method != types.MethodSadaPay {
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

func TestNotConfigured(t *testing.T) {
	a := &Adapter{}
	if _, err := a.VerifyWebhook(signedReq(testSecret, validBody)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := a.VerifyAPI(context.Background(), "SP-1"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a secret")
	}
}
