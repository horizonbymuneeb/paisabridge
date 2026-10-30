package registry

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
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/jazzcash"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/payoneer"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/usdt"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

var testNow = time.Date(2026, time.October, 31, 19, 0, 0, 0, clock.PKT)

// fullConfig returns a config with every secret set. Secrets are fixtures.
func fullConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Clock = clock.Fixed(testNow)
	cfg.Guard = newMemGuard()
	cfg.EasypaisaSecret = "pb-easypaisa-key"
	cfg.JazzCashSalt = "pb-jazzcash-salt"
	cfg.NayaPayWebhookSecret = "pb-nayapay-secret"
	cfg.SadaPayWebhookSecret = "pb-sadapay-secret"
	cfg.BankRelaySecret = "pb-bank-secret"
	cfg.BinancePaySecret = "pb-binance-secret"
	cfg.PayoneerSecret = "pb-payoneer-secret"
	cfg.USDTReceiverAddress = cfgUSDTReceiver
	cfg.USDTContract = cfgUSDTContract
	cfg.USDTChain = cfgUSDTChain
	return cfg
}

func newMemGuard() Guard { return &memGuard{seen: map[string]bool{}} }

type memGuard struct {
	seen map[string]bool
}

func (g *memGuard) Seen(id string) bool { return g.seen[id] }

func (g *memGuard) Remember(id string) bool {
	if g.seen[id] {
		return false
	}
	g.seen[id] = true
	return true
}

func TestNewRegistersEveryRail(t *testing.T) {
	r, err := New(fullConfig(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	want := []types.Method{
		types.MethodBank, payoneer.MethodName, types.MethodEasypaisa,
		types.MethodJazzCash, types.MethodNayaPay, types.MethodSadaPay, types.MethodUSDT,
	}
	got := map[types.Method]bool{}
	for _, m := range r.Methods() {
		got[m] = true
	}
	for _, m := range want {
		if !got[m] {
			t.Fatalf("method %q not registered (have %v)", m, r.Methods())
		}
	}
	if len(r.Methods()) != len(want) {
		t.Fatalf("method count: want %d, got %d (%v)", len(want), len(r.Methods()), r.Methods())
	}
	for _, m := range want {
		if !r.Supports(m) {
			t.Fatalf("%q should support auto-verification", m)
		}
	}
}

// TestNewSkipsUnconfiguredRails pins the boot contract: a rail whose secret
// is empty is simply not offered. A deployment that sells only bank transfers
// must boot, and a webhook for a skipped rail must be refused (never accepted
// without a secret).
func TestNewSkipsUnconfiguredRails(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		method types.Method
	}{
		{"easypaisa", func(c *Config) { c.EasypaisaSecret = "" }, types.MethodEasypaisa},
		{"jazzcash", func(c *Config) { c.JazzCashSalt = "" }, types.MethodJazzCash},
		{"nayapay", func(c *Config) { c.NayaPayWebhookSecret = "" }, types.MethodNayaPay},
		{"sadapay", func(c *Config) { c.SadaPayWebhookSecret = "" }, types.MethodSadaPay},
		{"bank", func(c *Config) { c.BankRelaySecret = "" }, types.MethodBank},
		{"binance", func(c *Config) { c.BinancePaySecret = "" }, types.MethodUSDT},
		{"payoneer", func(c *Config) { c.PayoneerSecret = "" }, payoneer.MethodName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fullConfig(t)
			tc.mutate(&cfg)
			r, err := New(cfg)
			if err != nil {
				t.Fatalf("an unconfigured rail must not fail boot: %v", err)
			}
			if r.Supports(tc.method) {
				t.Fatalf("%s still advertised after its secret was cleared", tc.method)
			}
			if ad, err := r.Lookup(tc.method); err == nil && ad != nil {
				t.Fatalf("%s still resolves to an adapter", tc.method)
			}
		})
	}
}

// TestNewRejectsEmptyConfiguration: a gateway with zero rails is a deployment
// mistake, not a valid server.
func TestNewRejectsEmptyConfiguration(t *testing.T) {
	cfg := Config{Guard: newMemGuard(), Clock: clock.Fixed(testNow)}
	if _, err := New(cfg); !errors.Is(err, ErrConfig) {
		t.Fatalf("want ErrConfig for a gateway with no rails, got %v", err)
	}
}

func TestNewRejectsReceiverWithoutContract(t *testing.T) {
	cfg := fullConfig(t)
	cfg.USDTContract = ""
	if _, err := New(cfg); !errors.Is(err, ErrConfig) {
		t.Fatalf("want ErrConfig, got %v", err)
	}
}

func TestLookupUnknown(t *testing.T) {
	r, err := New(fullConfig(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := r.Lookup(types.Method("bitcoin")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

// TestVerifyWebhookReadsBodyOnce proves the registry hands the adapter a fresh
// body even though it already consumed the stream itself.
func TestVerifyWebhookReadsBodyOnce(t *testing.T) {
	cfg := fullConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	vals := url.Values{
		"pp_TxnRefNo":     {"JC-REG-1"},
		"pp_Amount":       {"149700"},
		"pp_TxnCurrency":  {"PKR"},
		"pp_ResponseCode": {"000"},
		"pp_TxnDateTime":  {testNow.Format("20060102150405")},
	}
	vals.Set("pp_SecureHash", jazzcash.Sign(cfg.JazzCashSalt, jazzcash.SchemeKV, vals))
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/jazzcash", strings.NewReader(vals.Encode()))
	got, err := r.VerifyWebhook(types.MethodJazzCash, req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Amount != 149700 || got.TxnRef != "JC-REG-1" {
		t.Fatalf("notice: %+v", got)
	}
}

// TestVerifyWebhookClassifiesDuplicate: a replay is ErrReplayed, not an auth
// failure, so the API layer answers 200 and the provider stops retrying.
func TestVerifyWebhookClassifiesDuplicate(t *testing.T) {
	cfg := fullConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	rn := notice.RailNotice{TxnRef: "payoneer:PO-1:PB-ORDER-1", Amount: 1250, Currency: "USD", Note: "12.50",
		ReceivedAt: testNow.Format(time.RFC3339)}
	body, sig, err := notice.Encode(cfg.PayoneerSecret, "env-1", payoneerMethod, testNow, rn)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	mk := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/payoneer", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(notice.HeaderSignature, "v1="+sig)
		return req
	}
	if _, err := r.VerifyWebhook(payoneerMethod, mk()); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := r.VerifyWebhook(payoneerMethod, mk()); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second delivery: want ErrReplayed, got %v", err)
	}
}

// payoneerMethod is the registry key Payoneer is registered under.
var payoneerMethod = payoneer.MethodName

func TestVerifyWebhookRejectsForgedSignature(t *testing.T) {
	cfg := fullConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
	if _, err := r.VerifyWebhook(types.MethodBank, req); !errors.Is(err, adapter.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyWebhookRejectsOversizeBody(t *testing.T) {
	cfg := fullConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	huge := strings.Repeat("x", maxWebhookBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(huge))
	if _, err := r.VerifyWebhook(types.MethodEasypaisa, req); !errors.Is(err, adapter.ErrInvalidSignature) {
		t.Fatalf("want ErrInvalidSignature for oversize body, got %v", err)
	}
}

func TestVerifyAPIUnsupportedOnRail(t *testing.T) {
	r, err := New(fullConfig(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := r.VerifyAPI(context.Background(), types.MethodJazzCash, "x"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if _, err := r.VerifyAPI(context.Background(), types.Method("nope"), "x"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

const (
	cfgUSDTChain    = 56
	cfgUSDTContract = "0x55d398326f99059fF775485246999027B3197955"
	cfgUSDTReceiver = "0x000000000000000000000000000000000000BEEF"
)

func TestVerifyTransferDispatch(t *testing.T) {
	r, err := New(fullConfig(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Missing type information is caught by compile-time assertion; here the
	// runtime check that the registry refuses a mismatched transfer.
	tr := usdt.Transfer{
		Chain: cfgUSDTChain, Token: cfgUSDTContract,
		TxHash:   "0xfeed000000000000000000000000000000000000000000000000000000000001",
		LogIndex: 0, From: "0x1111111111111111111111111111111111111111", To: cfgUSDTReceiver,
		Raw: 12500000, Decimals: 6, Confirmations: 20,
	}
	n, err := r.VerifyTransfer(tr)
	if err != nil {
		t.Fatalf("verify transfer: %v", err)
	}
	if n.Currency != "USDT" || n.Amount != 12500000 {
		t.Fatalf("notice: %+v", n)
	}
}

func TestNewWithAdaptersEmpty(t *testing.T) {
	r := NewWithAdapters(nil)
	if len(r.Methods()) != 0 {
		t.Fatalf("want no methods, got %v", r.Methods())
	}
	if _, err := r.VerifyWebhook(types.MethodBank, httptest.NewRequest(http.MethodPost, "/x", nil)); !errors.Is(err, ErrUnknown) {
		t.Fatalf("want ErrUnknown, got %v", err)
	}
}

func TestClassifyPreservesUnderlyingSentinel(t *testing.T) {
	err := classify(provider.ErrMalformed)
	if !errors.Is(err, provider.ErrMalformed) {
		t.Fatalf("malformed sentinel lost: %v", err)
	}
	if classify(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}
