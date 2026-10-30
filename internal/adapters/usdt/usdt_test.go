package usdt

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

const (
	testPaySecret = "pb-binance-pay-secret"
	testReceiver  = "0x000000000000000000000000000000000000BEEF"
)

var testNow = time.Date(2026, time.October, 31, 17, 45, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{
		BinancePaySecret: testPaySecret,
		ReceiverAddress:  testReceiver,
		ExpectedContract: USDTBEP20,
		ExpectedChain:    ChainBSC,
		Clock:            clock.Fixed(testNow),
		Nonces:           NewMemGuard(),
		RequireGuard:     true,
	}
}

const binanceBody = `{"bizType":"PAY","bizStatus":"PAY_SUCCESS","prepayId":"295949787482752000",` +
	`"data":{"merchantTradeNo":"PB-USDT-77","payerOpenId":"abc123","payerNickname":"crypto-buyer",` +
	`"currency":"USDT","totalFee":"12.500000","transactionFee":"0","transactTime":1793434800000}}`

func binanceReq(t *testing.T, secret, ts, nonce, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/binance", strings.NewReader(body))
	r.Header.Set(HeaderBinanceTimestamp, ts)
	r.Header.Set(HeaderBinanceNonce, nonce)
	r.Header.Set(HeaderBinanceSignature, BinanceSignature(secret, ts, nonce, []byte(body)))
	return r
}

func TestBinanceSignatureIsThePublishedScheme(t *testing.T) {
	// The material is timestamp + "\n" + nonce + "\n" + body + "\n".
	got := BinanceSignature("secret", "1234", "nonce", []byte(`{"a":1}`))
	want := provider.HMACSHA256Hex("secret", "1234", "\n", "nonce", "\n", `{"a":1}`, "\n")
	if got != want {
		t.Fatalf("signature material drifted: got %s want %s", got, want)
	}
	// And it must change when any one component changes.
	for _, alt := range []struct{ ts, nonce, body string }{
		{"1235", "nonce", `{"a":1}`},
		{"1234", "other", `{"a":1}`},
		{"1234", "nonce", `{"a":2}`},
	} {
		if BinanceSignature("secret", alt.ts, alt.nonce, []byte(alt.body)) == got {
			t.Fatalf("signature did not change for %+v", alt)
		}
	}
}

func TestVerifyWebhookTable(t *testing.T) {
	ts := strconv.FormatInt(testNow.UnixMilli(), 10)
	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		wantErr error
		wantAmt int64
	}{
		{
			name:    "valid binance pay callback",
			req:     func(t *testing.T) *http.Request { return binanceReq(t, testPaySecret, ts, "nonce-1", binanceBody) },
			wantAmt: 12500000,
		},
		{
			name: "missing signature header",
			req: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(binanceBody))
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name:    "wrong secret",
			req:     func(t *testing.T) *http.Request { return binanceReq(t, "attacker", ts, "nonce-2", binanceBody) },
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "tampered body",
			req: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Replace(binanceBody, "12.500000", "1250.000000", 1)))
				r.Header.Set(HeaderBinanceTimestamp, ts)
				r.Header.Set(HeaderBinanceNonce, "nonce-3")
				r.Header.Set(HeaderBinanceSignature, BinanceSignature(testPaySecret, ts, "nonce-3", []byte(binanceBody)))
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "tampered nonce",
			req: func(t *testing.T) *http.Request {
				r := binanceReq(t, testPaySecret, ts, "nonce-4", binanceBody)
				r.Header.Set(HeaderBinanceNonce, "nonce-4-swapped")
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "tampered timestamp",
			req: func(t *testing.T) *http.Request {
				r := binanceReq(t, testPaySecret, ts, "nonce-5", binanceBody)
				r.Header.Set(HeaderBinanceTimestamp, strconv.FormatInt(testNow.Add(time.Second).UnixMilli(), 10))
				return r
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "stale timestamp",
			req: func(t *testing.T) *http.Request {
				old := strconv.FormatInt(testNow.Add(-time.Hour).UnixMilli(), 10)
				return binanceReq(t, testPaySecret, old, "nonce-6", binanceBody)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "milliseconds confusion cannot pass the window",
			req: func(t *testing.T) *http.Request {
				secs := strconv.FormatInt(testNow.Unix(), 10) // seconds, not ms
				return binanceReq(t, testPaySecret, secs, "nonce-7", binanceBody)
			},
			wantErr: adapter.ErrInvalidSignature, // 1970 -> far outside window
		},
		{
			name: "cancelled payment is not a payment",
			req: func(t *testing.T) *http.Request {
				body := strings.Replace(binanceBody, "PAY_SUCCESS", "PAY_CLOSED", 1)
				return binanceReq(t, testPaySecret, ts, "nonce-8", body)
			},
			wantErr: ErrNotSuccessful,
		},
		{
			name: "non-USDT currency is rejected",
			req: func(t *testing.T) *http.Request {
				body := strings.Replace(binanceBody, `"USDT"`, `"BUSD"`, 1)
				return binanceReq(t, testPaySecret, ts, "nonce-9", body)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "zero amount is rejected",
			req: func(t *testing.T) *http.Request {
				body := strings.Replace(binanceBody, `"12.500000"`, `"0"`, 1)
				return binanceReq(t, testPaySecret, ts, "nonce-10", body)
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "duplicate nonce is rejected",
			req: func(t *testing.T) *http.Request {
				// Signed with a nonce the guard has already consumed below.
				return binanceReq(t, testPaySecret, ts, "nonce-used", binanceBody)
			},
			wantErr: ErrReplayed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			if tc.wantErr == ErrReplayed {
				a.Nonces.Remember("nonce-used")
			}
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
			if got.Method != types.MethodBinance {
				t.Fatalf("method: %q", got.Method)
			}
			if got.Currency != "USDT" {
				t.Fatalf("currency: %q", got.Currency)
			}
		})
	}
}

func TestBinanceReplayIsRejected(t *testing.T) {
	a := newAdapter(t)
	ts := strconv.FormatInt(testNow.UnixMilli(), 10)
	nonce := "nonce-X"
	if _, err := a.VerifyWebhook(binanceReq(t, testPaySecret, ts, nonce, binanceBody)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := a.VerifyWebhook(binanceReq(t, testPaySecret, ts, nonce, binanceBody)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: want ErrReplayed, got %v", err)
	}
}

func goodTransfer() Transfer {
	return Transfer{
		Chain:         ChainBSC,
		Token:         USDTBEP20,
		TxHash:        "0xABCdef0000000000000000000000000000000000000000000000000000000001",
		LogIndex:      7,
		From:          "0x1111111111111111111111111111111111111111",
		To:            testReceiver,
		Raw:           12500000, // 12.5 USDT in micro-units
		Decimals:      6,
		Confirmations: 20,
		BlockTime:     testNow.Add(-3 * time.Minute),
	}
}

func TestVerifyTransferTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Transfer)
		wantErr error
		wantAmt int64
	}{
		{name: "valid bep20 transfer", mutate: func(*Transfer) {}, wantAmt: 12500000},
		{
			name:    "checksum casing of the contract does not matter",
			mutate:  func(x *Transfer) { x.Token = strings.ToLower(USDTBEP20) },
			wantAmt: 12500000,
		},
		{
			name:    "impostor token is rejected",
			mutate:  func(x *Transfer) { x.Token = "0x000000000000000000000000000000000000dEaD" },
			wantErr: ErrWrongContract,
		},
		{
			name:    "wrong chain is rejected",
			mutate:  func(x *Transfer) { x.Chain = 1 },
			wantErr: ErrWrongContract,
		},
		{
			name:    "wrong receiver is rejected",
			mutate:  func(x *Transfer) { x.To = "0x2222222222222222222222222222222222222222" },
			wantErr: ErrWrongContract,
		},
		{
			name:    "under-confirmed transfer is held back",
			mutate:  func(x *Transfer) { x.Confirmations = 3 },
			wantErr: ErrUnderConfirmed,
		},
		{
			name:    "wrong decimals are rejected",
			mutate:  func(x *Transfer) { x.Decimals = 18 },
			wantErr: ErrWrongContract,
		},
		{
			name:    "zero amount is rejected",
			mutate:  func(x *Transfer) { x.Raw = 0 },
			wantErr: provider.ErrMalformed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			x := goodTransfer()
			tc.mutate(&x)
			got, err := a.VerifyTransfer(x)
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
			if got.Method != types.MethodUSDT {
				t.Fatalf("method: %q", got.Method)
			}
		})
	}
}

func TestVerifyTransferTRC20(t *testing.T) {
	a := newAdapter(t)
	a.ExpectedContract = USDTTron
	a.ExpectedChain = ChainTron
	x := goodTransfer()
	x.Chain = ChainTron
	x.Token = USDTTron
	x.Confirmations = 19
	if _, err := a.VerifyTransfer(x); err != nil {
		t.Fatalf("trc20: %v", err)
	}
	// Base58 is case-sensitive: a case-flipped contract is a different
	// address entirely.
	x.Token = strings.ToLower(USDTTron)
	x.Confirmations = 30
	if _, err := a.VerifyTransfer(x); !errors.Is(err, ErrWrongContract) {
		t.Fatalf("lower-cased trc20 contract: want ErrWrongContract, got %v", err)
	}
}

func TestVerifyTransferDuplicateIsRejected(t *testing.T) {
	a := newAdapter(t)
	x := goodTransfer()
	if _, err := a.VerifyTransfer(x); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := a.VerifyTransfer(x); !errors.Is(err, ErrReplayed) {
		t.Fatalf("duplicate: want ErrReplayed, got %v", err)
	}
	// Same tx, different log index is a different transfer and must pass.
	x.LogIndex = 8
	if _, err := a.VerifyTransfer(x); err != nil {
		t.Fatalf("different log index: %v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	a := &Adapter{}
	if _, err := a.VerifyWebhook(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := a.VerifyTransfer(goodTransfer()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := a.VerifyAPI(context.Background(), "x"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false when nothing is configured")
	}
}
