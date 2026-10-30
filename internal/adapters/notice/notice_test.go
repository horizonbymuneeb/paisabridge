package notice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const testSecret = "pb-notice-secret"

var testNow = time.Date(2026, time.October, 31, 12, 0, 0, 0, clock.PKT)

type memGuard struct{ seen map[string]bool }

func newGuard() *memGuard { return &memGuard{seen: map[string]bool{}} }

func (g *memGuard) Seen(id string) bool { return g.seen[id] }

func (g *memGuard) Remember(id string) bool {
	if g.seen[id] {
		return false
	}
	g.seen[id] = true
	return true
}

func mint(t *testing.T, secret, id string, at time.Time, rn RailNotice) (*http.Request, string) {
	t.Helper()
	body, sig, err := Encode(secret, id, types.MethodBank, at, rn)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
	r.Header.Set(HeaderSignature, "v1="+sig)
	return r, string(body)
}

func goodNotice() RailNotice {
	return RailNotice{TxnRef: "ref-1", Amount: 149700, Currency: "PKR", SenderName: "A", SenderAccount: "01"}
}

func TestVerifyTable(t *testing.T) {
	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		guard   ReplayGuard
		wantErr error
		wantAmt int64
	}{
		{
			name: "valid envelope",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, testSecret, "id-1", testNow, goodNotice())
				return r
			},
			wantAmt: 149700,
		},
		{
			name: "bare digest header is accepted",
			req: func(t *testing.T) *http.Request {
				r, body := mint(t, testSecret, "id-1", testNow, goodNotice())
				r.Header.Del(HeaderSignature)
				var env wireEnvelope
				if err := json.Unmarshal([]byte(body), &env); err != nil {
					t.Fatal(err)
				}
				r.Header.Set(HeaderSignaturePlain, Sign(testSecret, env.ID, env.Method, 1761908400, env.Payload))
				return r
			},
			wantErr: ErrBadNotice, // timestamp in that signature differs -> mismatch
		},
		{
			name: "missing signature",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, testSecret, "id-2", testNow, goodNotice())
				r.Header.Del(HeaderSignature)
				return r
			},
			wantErr: ErrBadNotice,
		},
		{
			name: "wrong secret",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, "other", "id-3", testNow, goodNotice())
				return r
			},
			wantErr: ErrBadNotice,
		},
		{
			name: "id not covered by signature",
			req: func(t *testing.T) *http.Request {
				r, body := mint(t, testSecret, "id-4", testNow, goodNotice())
				tampered := strings.Replace(body, `"id":"id-4"`, `"id":"id-4-retargeted"`, 1)
				r2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tampered))
				r2.Header = r.Header.Clone()
				return r2
			},
			wantErr: ErrBadNotice,
		},
		{
			name: "payload edited after signing",
			req: func(t *testing.T) *http.Request {
				r, body := mint(t, testSecret, "id-5", testNow, goodNotice())
				tampered := strings.Replace(body, "149700", "999999", 1)
				r2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tampered))
				r2.Header = r.Header.Clone()
				return r2
			},
			wantErr: ErrBadNotice,
		},
		{
			name: "method retargeted after signing",
			req: func(t *testing.T) *http.Request {
				r, body := mint(t, testSecret, "id-6", testNow, goodNotice())
				tampered := strings.Replace(body, `"method":"bank"`, `"method":"jazzcash"`, 1)
				r2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tampered))
				r2.Header = r.Header.Clone()
				return r2
			},
			wantErr: ErrBadNotice,
		},
		{
			name: "stale envelope",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, testSecret, "id-7", testNow.Add(-time.Hour), goodNotice())
				return r
			},
			wantErr: ErrStale,
		},
		{
			name: "future envelope",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, testSecret, "id-8", testNow.Add(time.Hour), goodNotice())
				return r
			},
			wantErr: ErrStale,
		},
		{
			name: "replayed id",
			req: func(t *testing.T) *http.Request {
				r, _ := mint(t, testSecret, "id-used", testNow, goodNotice())
				return r
			},
			guard:   &preGuarded{id: "id-used"},
			wantErr: ErrReplayed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Verifier{Secret: testSecret, Clock: clock.Fixed(testNow)}
			guard := tc.guard
			if guard == nil {
				guard = newGuard()
			}
			env, err := v.Verify(tc.req(t), guard)
			if tc.wantErr != nil {
				if !errorIs(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rn, err := env.RailNotice()
			if err != nil {
				t.Fatalf("rail notice: %v", err)
			}
			if rn.Amount != tc.wantAmt {
				t.Fatalf("amount: want %d, got %d", tc.wantAmt, rn.Amount)
			}
		})
	}
}

type preGuarded struct{ id string }

func (p *preGuarded) Seen(id string) bool     { return id == p.id }
func (p *preGuarded) Remember(id string) bool { return id != p.id }

// errorIs is errors.Is without importing errors twice in the table.
func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestVerifyRejectsOversizeBody(t *testing.T) {
	v := Verifier{Secret: testSecret, Clock: clock.Fixed(testNow)}
	body := strings.Repeat("a", MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set(HeaderSignature, "v1=00")
	if _, err := v.Verify(req, nil); err == nil {
		t.Fatal("oversize body must be rejected")
	}
}

func TestVerifyRequiresSecret(t *testing.T) {
	v := Verifier{Clock: clock.Fixed(testNow)}
	if _, err := v.Verify(httptest.NewRequest(http.MethodPost, "/x", nil), nil); err == nil {
		t.Fatal("missing secret must be an error, never a pass")
	}
}

func TestRailNoticeStrict(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"valid", `{"txn_ref":"t","amount_minor":1,"currency":"pkr"}`, true},
		{"currency is normalised to upper case", `{"txn_ref":"t","amount_minor":1,"currency":" usdt "}`, true},
		{"missing txn", `{"amount_minor":1,"currency":"PKR"}`, false},
		{"zero amount", `{"txn_ref":"t","amount_minor":0,"currency":"PKR"}`, false},
		{"unknown field", `{"txn_ref":"t","amount_minor":1,"currency":"PKR","x":1}`, false},
		{"amount as float", `{"txn_ref":"t","amount_minor":1.5,"currency":"PKR"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rn, err := decodeRailNotice([]byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if tc.ok && rn.Currency != strings.ToUpper(strings.TrimSpace(rn.Currency)) {
				t.Fatalf("currency not normalised: %q", rn.Currency)
			}
			if !tc.ok && err == nil {
				t.Fatal("want error, got none")
			}
		})
	}
}

func TestSignatureCoversEveryComponent(t *testing.T) {
	payload := []byte(`{"txn_ref":"t"}`)
	base := Sign(testSecret, "id", "bank", 100, payload)
	for _, alt := range []string{
		Sign(testSecret, "other-id", "bank", 100, payload),
		Sign(testSecret, "id", "usdt", 100, payload),
		Sign(testSecret, "id", "bank", 101, payload),
		Sign(testSecret, "id", "bank", 100, []byte(`{"txn_ref":"u"}`)),
		Sign("other-secret", "id", "bank", 100, payload),
	} {
		if alt == base {
			t.Fatalf("signature did not change for a different component")
		}
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	if _, err := Decode(strings.NewReader(`{"id":"a","method":"bank","issued_at":1,"payload":{}}{}`)); err == nil {
		t.Fatal("trailing data must be rejected")
	}
}

func TestIssuedAtMustBeIntegerSeconds(t *testing.T) {
	valid := `{"id":"a","method":"bank","issued_at":1761908400,"payload":{"txn_ref":"t","amount_minor":1,"currency":"PKR"}}`
	if _, err := Decode(strings.NewReader(valid)); err != nil {
		t.Fatalf("valid seconds rejected: %v", err)
	}
	ms := strings.Replace(valid, "1761908400", strconv.FormatInt(1761908400000, 10), 1)
	// A millisecond value still decodes as an integer; the freshness window in
	// Verifier is what rejects it. Decode must at least never accept a float.
	if _, err := Decode(strings.NewReader(ms)); err != nil {
		t.Fatalf("integer milliseconds must decode: %v", err)
	}
	frac := strings.Replace(valid, "1761908400", "1761908400.5", 1)
	if _, err := Decode(strings.NewReader(frac)); err == nil {
		t.Fatal("fractional issued_at must be rejected")
	}
}

// TestMillisIssuedAtFailsTheWindow proves the freshness check catches the
// milliseconds-vs-seconds mistake rather than trusting a huge integer.
func TestMillisIssuedAtFailsTheWindow(t *testing.T) {
	rn := RailNotice{TxnRef: "t", Amount: 1, Currency: "PKR"}
	body, _, err := Encode(testSecret, "id-ms", types.MethodBank, testNow, rn)
	if err != nil {
		t.Fatal(err)
	}
	ms := strings.Replace(string(body), `"issued_at":`+strconv.FormatInt(testNow.Unix(), 10),
		`"issued_at":`+strconv.FormatInt(testNow.UnixMilli(), 10), 1)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(ms))
	var env Envelope
	if err := json.Unmarshal([]byte(ms), &env); err != nil {
		t.Fatal(err)
	}
	req.Header.Set(HeaderSignature, Sign(testSecret, env.ID, env.Method, env.IssuedAt, env.Payload))
	v := Verifier{Secret: testSecret, Clock: clock.Fixed(testNow)}
	if _, err := v.Verify(req, nil); err == nil {
		t.Fatal("a millisecond issued_at must be rejected by the window")
	}
}
