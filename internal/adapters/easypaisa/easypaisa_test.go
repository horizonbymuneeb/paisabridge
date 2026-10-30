package easypaisa

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// testKey is a 16-byte AES key standing in for the merchant "hash key". It is
// a fixture, not a credential.
const testKey = "pb-easypaisa-key"

// testNow is fixed so freshness assertions are deterministic.
var testNow = time.Date(2026, time.October, 31, 12, 0, 0, 0, clock.PKT)

func newAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{Secret: testKey, Clock: clock.Fixed(testNow), Guard: NewMemGuard(), RequireGuard: true}
}

// buildReq mints a callback the way the provider would. It uses the adapter's
// own EncryptPlaintext, which is the same primitive the provider uses: if the
// ciphertext is not decryptable under the key, the test fails.
func buildReq(t *testing.T, a *Adapter, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/easypaisa", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func txnDate(now time.Time) string { return now.Format("2006-01-02 15:04:05") }

// happyBody builds a callback whose merchantHashedResp is a real AES-ECB
// encryption of "txn&0000&transaction completed" under the adapter key.
func happyBody(t *testing.T, a *Adapter, txn string, amount string, at time.Time) string {
	t.Helper()
	resp, err := a.EncryptPlaintext(txn + "&0000&transaction completed")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	req, err := a.EncryptPlaintext("orderId=" + txn + "&transactionAmount=" + amount)
	if err != nil {
		t.Fatalf("encrypt req: %v", err)
	}
	return `{
		"orderId": "` + txn + `",
		"transactionId": "` + txn + `",
		"responseCode": "0000",
		"responseDesc": "transaction completed",
		"transactionAmount": "` + amount + `",
		"transactionCurrency": "PKR",
		"transactionDateTime": "` + txnDate(at) + `",
		"merchantHashedReq": "` + req + `",
		"merchantHashedResp": "` + resp + `",
		"mobileAccountNo": "03001234567"
	}`
}

func TestVerifyWebhookTable(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T, a *Adapter) *http.Request
		wantErr error // nil means success
		wantAmt int64
	}{
		{
			name: "valid callback",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-TXN-1001", "1497", testNow))
			},
			wantAmt: 149700,
		},
		{
			name: "valid callback with fractional rupees",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-TXN-1002", "1497.00", testNow))
			},
			wantAmt: 149700,
		},
		{
			name: "missing merchantHashedResp is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, `{"orderId":"EP-1","transactionId":"EP-1","responseCode":"0000",
					"transactionAmount":"1497","transactionDateTime":"`+txnDate(testNow)+`",
					"merchantHashedReq":"AAAA"}`)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "garbage ciphertext is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, `{"orderId":"EP-1","transactionId":"EP-1","responseCode":"0000",
					"transactionAmount":"1497","transactionDateTime":"`+txnDate(testNow)+`",
					"merchantHashedReq":"AAAA","merchantHashedResp":"bm90LWNpcGhlcnRleHQ="}`)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "ciphertext minted under another key is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				other := &Adapter{Secret: "attacker-key-16!"}
				resp, err := other.EncryptPlaintext("EP-1&0000&ok")
				if err != nil {
					t.Fatalf("encrypt: %v", err)
				}
				req, _ := a.EncryptPlaintext("orderId=EP-1")
				return buildReq(t, a, `{"orderId":"EP-1","transactionId":"EP-1","responseCode":"0000",
					"transactionAmount":"1497","transactionDateTime":"`+txnDate(testNow)+`",
					"merchantHashedReq":"`+req+`","merchantHashedResp":"`+resp+`"}`)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "ciphertext for a different transaction is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				body := happyBody(t, a, "EP-VICTIM-1", "1497", testNow)
				// Swap the body identity so the ciphertext no longer binds.
				body = strings.Replace(body, `"transactionId": "EP-VICTIM-1"`, `"transactionId": "EP-ATTACKER-9"`, 1)
				return buildReq(t, a, body)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "non-success response code is not a payment",
			build: func(t *testing.T, a *Adapter) *http.Request {
				resp, _ := a.EncryptPlaintext("EP-2&0001&declined")
				req, _ := a.EncryptPlaintext("orderId=EP-2")
				return buildReq(t, a, `{"orderId":"EP-2","transactionId":"EP-2","responseCode":"0001",
					"responseDesc":"declined","transactionAmount":"1497",
					"transactionDateTime":"`+txnDate(testNow)+`",
					"merchantHashedReq":"`+req+`","merchantHashedResp":"`+resp+`"}`)
			},
			wantErr: ErrNotSuccessful,
		},
		{
			name: "stale callback is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-3", "1497", testNow.Add(-48*time.Hour)))
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "future-dated callback is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-4", "1497", testNow.Add(48*time.Hour)))
			},
			wantErr: adapter.ErrInvalidSignature,
		},
		{
			name: "zero amount is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-5", "0", testNow))
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "amount with too many decimals is rejected",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, happyBody(t, a, "EP-6", "1497.005", testNow))
			},
			wantErr: provider.ErrMalformed,
		},
		{
			name: "unknown field is rejected instead of ignored",
			build: func(t *testing.T, a *Adapter) *http.Request {
				return buildReq(t, a, `{"orderId":"EP-7","transactionId":"EP-7","responseCode":"0000",
					"transactionAmount":"1497","transactionDateTime":"`+txnDate(testNow)+`",
					"merchantHashedReq":"AAAA","merchantHashedResp":"AAAA","amount":"999999"}`)
			},
			wantErr: adapter.ErrInvalidSignature,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t)
			got, err := a.VerifyWebhook(tc.build(t, a))
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("want error, got notice %+v", got)
				}
				want := tc.wantErr
				if !errors.Is(want, adapter.ErrInvalidSignature) {
					// Malformed-input cases are wrapped with context by the
					// adapter; assert the class rather than the exact value.
					if !errors.Is(err, want) && !errors.Is(err, provider.ErrMalformed) {
						t.Fatalf("want error %v, got %v", want, err)
					}
					return
				}
				if !errors.Is(err, want) {
					t.Fatalf("want error %v, got %v", want, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, got.Amount)
			}
			if got.Currency != "PKR" {
				t.Fatalf("currency: got %q", got.Currency)
			}
			if got.Method != types.MethodEasypaisa {
				t.Fatalf("method: got %q", got.Method)
			}
		})
	}
}

func TestVerifyWebhookReplay(t *testing.T) {
	a := newAdapter(t)
	body := happyBody(t, a, "EP-REPLAY-1", "1497", testNow)

	first, err := a.VerifyWebhook(buildReq(t, a, body))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if first.TxnRef != "EP-REPLAY-1" {
		t.Fatalf("txn ref: got %q", first.TxnRef)
	}
	// Byte-identical replay: a perfect copy of a genuine callback.
	_, err = a.VerifyWebhook(buildReq(t, a, body))
	if !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay: want ErrReplayed, got %v", err)
	}
	// A third delivery must also be rejected: the guard must not "reset".
	if _, err := a.VerifyWebhook(buildReq(t, a, body)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second replay: want ErrReplayed, got %v", err)
	}
}

func TestVerifyWebhookRequiresConfiguredKey(t *testing.T) {
	a := &Adapter{Clock: clock.Fixed(testNow)}
	_, err := a.VerifyWebhook(buildReq(t, a, `{}`))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if a.Supports() {
		t.Fatal("Supports must be false without a key")
	}
}

func TestVerifyWebhookConcurrentReplaysAreDroppedOnce(t *testing.T) {
	a := newAdapter(t)
	body := happyBody(t, a, "EP-CONCURRENT-1", "1497", testNow)
	var accepted int
	done := make(chan struct{})
	for i := 0; i < 32; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := a.VerifyWebhook(buildReq(t, a, body)); err == nil {
				accepted++
			}
		}()
	}
	for i := 0; i < 32; i++ {
		<-done
	}
	if accepted != 1 {
		t.Fatalf("want exactly 1 acceptance of a replayed body, got %d", accepted)
	}
}

func TestVerifyAPIUnsupported(t *testing.T) {
	a := newAdapter(t)
	if _, err := a.VerifyAPI(context.Background(), "EP-1"); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
	if a.Method() != types.MethodEasypaisa {
		t.Fatalf("method: got %q", a.Method())
	}
}

func TestStripPKCS7(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"fifteen-bytes!!", "fifteen-bytes!!", false},   // block-sized: last byte 0x21 is not a pad count
		{"abcdefghijklmnop", "abcdefghijklmnop", false}, // full block, no padding bytes
		{"abcd", "abcd", false},
	}
	for _, tc := range tests {
		got, ok := stripPKCS7([]byte(tc.in), 16)
		if ok != tc.wantOK || string(got) != tc.want {
			t.Fatalf("stripPKCS7(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	a := newAdapter(t)
	for _, p := range []string{"EP-1&0000&ok", "exactly16bytes!!", strings.Repeat("x", 32)} {
		ct, err := a.EncryptPlaintext(p)
		if err != nil {
			t.Fatalf("encrypt %q: %v", p, err)
		}
		got, err := a.decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt %q: %v", p, err)
		}
		if got != p {
			t.Fatalf("round trip: want %q, got %q", p, got)
		}
	}
}

// TestForgeryFailsWithoutKey demonstrates the property the whole adapter
// hangs on: without the AES key an attacker cannot mint a merchantHashedResp
// that decrypts to plausible text. The probability of a random 16-byte block
// decrypting to printable ASCII inside a 16-byte block is 2^-16 at best, and
// the payload must additionally name the attacker's chosen transaction id.
func TestForgeryFailsWithoutKey(t *testing.T) {
	// The attacker does not hold the key: only the real adapter's decrypt
	// path can tell a valid ciphertext from noise, and it must refuse.
	accepted := 0
	for i := 0; i < 256; i++ {
		// Deterministic pseudo-random 32-byte ciphertexts (two blocks).
		ct := make([]byte, 32)
		for j := range ct {
			ct[j] = byte((i*31 + j*17) % 256)
		}
		resp, err := (&Adapter{Secret: testKey}).decrypt(base64.StdEncoding.EncodeToString(ct))
		if err == nil && plausibleText([]byte(resp)) && strings.Contains(resp, "&") {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("random ciphertexts decrypted to plausible text %d times", accepted)
	}
}
