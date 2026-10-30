// Package usdt implements the stablecoin rail: USDT on BEP-20 (BNB Smart
// Chain, chain id 56), USDT on TRC-20 (Tron), and Binance Pay.
//
// # The three ingestion paths, and why they differ
//
//  1. On-chain (BEP-20 / TRC-20). We never trust an HTTP body for this. The
//     adapter's job is to validate a transfer the watcher already fetched
//     from a full node / indexer and to refuse anything that is not the exact
//     token contract, chain and confirmations the configuration demands.
//     Exactness here is the whole security model: on-chain transfers are
//     final and anyone can send USDT to our address, so "amount seen" is only
//     meaningful together with (contract, chain, confirmations).
//  2. Binance Pay. Binance signs every callback with HMAC-SHA256 over
//     "timestamp\nnonce\nbody\n" keyed with the Pay secret. That scheme is
//     published by Binance and reproduced here exactly. The callback also
//     carries a nonce, which Binance requires merchants to deduplicate - see
//     ReplayGuard.
//  3. The signed notice feed (package notice) is used only for the on-chain
//     watcher, whose relay is ours.
//
// # Signature verification (Binance Pay)
//
// Published scheme: the signature is the hex HMAC-SHA256 of
//
//	timestamp + "\n" + nonce + "\n" + body + "\n"
//
// with the merchant Pay secret ("Counterparty secret") as the key. The
// timestamp is the "BinancePay-Timestamp" header in milliseconds, the nonce is
// "BinancePay-Nonce", the signature is "BinancePay-Signature", and the
// certificate is "BinancePay-Certificate-SN". This adapter verifies the
// signature and the timestamp; the certificate serial is kept in the raw
// payload for audit but is not validated against Binance's CA chain, because
// that requires downloading and rotating Binance's certificate (documented
// assumption, not a silent omission).
//
// # Assumptions (documented, never silently faked)
//
//  1. On-chain: token contracts are configuration, never hard-coded guesswork.
//     The well-known contracts (USDT BEP-20 0x55d3...7955, USDT TRC-20
//     TR7NHqj...jLi6) are provided as exported constants with their source
//     named, and must be confirmed by the operator against the official
//     Tether/Binance listings before funding an address. Confirmations are
//     configuration too: TRC-20 needs ~19 blocks (SRBFT finality), BEP-20
//     needs ~15 (BSC finality heuristic).
//  2. Binance Pay amounts: the payload carries a decimal string; it is parsed
//     to micro-USDT (6 decimals) without floating point.
//  3. Binance Pay status "PAY_SUCCESS" means paid; any other status is
//     authenticated and rejected as ErrNotSuccessful so a cancellation can
//     never fulfil an order.
//  4. Timestamp skew window defaults to 5 minutes (Binance's own SDK uses
//     30s for API calls; webhook delivery can be slower, 5 minutes is a
//     documented widening).
//  5. VerifyAPI: no on-chain status call is invented. Adapter.VerifyAPI
//     returns ErrUnsupported. Binance Pay's query-order API exists but needs
//     a signed request with the API key/secret pair, which is a different
//     credential from the Pay secret; it is not implemented here.
//
// # Endpoints
//
// None are embedded: no URL is needed to verify a callback, and the on-chain
// watcher's node/indexer URL is deployment configuration (BaseURL, default "",
// TODO(provider-credentials)).
package usdt

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/clock"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/provider"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

const (
	// USDTBEP20 is the USDT contract on BNB Smart Chain (BSC), chain id 56.
	// Source: Tether's published contract list and BscScan listing. Operators
	// must re-confirm this against the official listing before funding.
	USDTBEP20 = "0x55d398326f99059fF775485246999027B3197955"
	// ChainBSC is the BNB Smart Chain id.
	ChainBSC = 56
	// ChainTron is the chain id used for TRC-20 transfers. Tron has no
	// official EVM-style chain id; TIP-21/tronweb conventionally reports 1
	// (0x2b6653dc is the network id). It is a label, not a security
	// primitive - the token contract is what identifies USDT on Tron.
	ChainTron = 1

	// USDTTron is the TRC-20 USDT contract on Tron. Source: Tether's
	// published contract list and Tronscan. Re-confirm before funding.
	USDTTron = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

	// Binance Pay headers (published names).
	HeaderBinanceTimestamp = "BinancePay-Timestamp"
	HeaderBinanceNonce     = "BinancePay-Nonce"
	HeaderBinanceSignature = "BinancePay-Signature"
	HeaderBinanceCertSN    = "BinancePay-Certificate-SN"

	maxBodyBytes = 1 << 16
)

var (
	// ErrWrongContract is returned when a transfer is not the configured
	// USDT contract on the configured chain.
	ErrWrongContract = errors.New("usdt: transfer is not the configured USDT contract on the configured chain")
	// ErrUnderConfirmed is returned before enough confirmations exist.
	ErrUnderConfirmed = errors.New("usdt: transfer does not have enough confirmations yet")
	// ErrNotSuccessful is returned for an authenticated Binance Pay callback
	// that is not a successful payment.
	ErrNotSuccessful = errors.New("usdt: binance pay callback is not a successful payment")
	// ErrReplayed is returned for a consumed Binance Pay nonce.
	ErrReplayed = errors.New("usdt: binance pay nonce already used")
	// ErrNotConfigured is returned when required credentials are missing.
	ErrNotConfigured = errors.New("usdt: adapter is not configured")
)

// Transfer is one on-chain USDT transfer as reported by a watcher.
type Transfer struct {
	Chain         int64
	Token         string // contract address
	TxHash        string
	LogIndex      int
	From          string
	To            string
	Raw           int64 // amount in the token's smallest unit
	Decimals      uint8
	Confirmations int64
	BlockTime     time.Time
}

// Adapter implements adapter.PaymentAdapter for USDT.
type Adapter struct {
	// BinancePaySecret is the merchant Pay secret (HMAC key). See the
	// published scheme above.
	BinancePaySecret string

	// ReceiverAddress is the USDT address we accept deposits on.
	ReceiverAddress string

	// ExpectedContract is the USDT contract for the configured chain, and
	// ExpectedChain its chain id (56 for BSC). Both must be set for on-chain
	// verification; they are configuration so a misconfiguration fails
	// closed instead of accepting an impostor token.
	ExpectedContract string
	ExpectedChain    int64

	// MinConfirmations is the number of blocks before a transfer counts.
	MinConfirmations int64

	// BaseURL is the node/indexer used by the out-of-process watcher.
	// Empty by default - TODO(provider-credentials).
	BaseURL string

	// Clock supplies now. Nil means live.
	Clock clock.Clock

	// MaxSkew is the Binance Pay timestamp tolerance. Zero means 5 minutes.
	MaxSkew time.Duration

	// Nonces records consumed Binance Pay nonces.
	Nonces ReplayGuard

	// RequireGuard turns a nil Nonces into a configuration error.
	RequireGuard bool
}

// ReplayGuard records consumed Binance Pay nonces.
type ReplayGuard interface {
	Seen(id string) bool
	Remember(id string) bool
}

// Method implements adapter.PaymentAdapter.
func (a *Adapter) Method() types.Method { return types.MethodUSDT }

// Supports reports whether any ingestion path is configured.
func (a *Adapter) Supports() bool {
	return a.BinancePaySecret != "" || (a.ExpectedContract != "" && a.ReceiverAddress != "")
}

func (a *Adapter) now() time.Time {
	if a.Clock == nil {
		return clock.Live{}.Now()
	}
	return a.Clock.Now()
}

func (a *Adapter) maxSkew() time.Duration {
	if a.MaxSkew <= 0 {
		return 5 * time.Minute
	}
	return a.MaxSkew
}

func (a *Adapter) minConfirmations() int64 {
	if a.MinConfirmations <= 0 {
		// Default per chain family: TRC-20 relies on SRBFT irreversibility
		// after one solid block cycle (~19 blocks); BEP-20 uses BSC's
		// finality heuristic (~15 blocks).
		if strings.EqualFold(a.ExpectedContract, USDTTron) {
			return 19
		}
		return 15
	}
	return a.MinConfirmations
}

// VerifyWebhook implements adapter.PaymentAdapter for Binance Pay callbacks.
//
// It is the only HTTP entry point: on-chain transfers arrive through
// VerifyTransfer, not through HTTP, because the watcher is not a provider.
func (a *Adapter) VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) {
	if a.BinancePaySecret == "" {
		return nil, ErrNotConfigured
	}
	tsRaw, err := provider.Header(r.Header, HeaderBinanceTimestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	nonce, err := provider.Header(r.Header, HeaderBinanceNonce)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	sig, err := provider.Header(r.Header, HeaderBinanceSignature)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	body, err := provider.Body(r, maxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", adapter.ErrInvalidSignature, err)
	}
	if !provider.EqualHex(sig, BinanceSignature(a.BinancePaySecret, tsRaw, nonce, body)) {
		return nil, fmt.Errorf("%w: %s mismatch", adapter.ErrInvalidSignature, HeaderBinanceSignature)
	}
	// Timestamp is milliseconds. Parse before trusting it, and bound it: a
	// captured callback must not stay valid forever.
	ms, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || ms <= 0 {
		return nil, fmt.Errorf("%w: %s %q is not milliseconds", adapter.ErrInvalidSignature, HeaderBinanceTimestamp, tsRaw)
	}
	sent := time.UnixMilli(ms)
	if d := a.now().Sub(sent); d > a.maxSkew() || d < -a.maxSkew() {
		return nil, fmt.Errorf("%w: callback timestamp %s outside %s of now",
			adapter.ErrInvalidSignature, sent.UTC().Format(time.RFC3339), a.maxSkew())
	}

	var p binancePay
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: body: %v", provider.ErrMalformed, err)
	}
	if !strings.EqualFold(p.BizStatus, "PAY_SUCCESS") {
		return nil, fmt.Errorf("%w: bizStatus %q", ErrNotSuccessful, p.BizStatus)
	}
	if strings.TrimSpace(p.Data.MerchantTradeNo) == "" || strings.TrimSpace(p.BizIDStr) == "" {
		return nil, fmt.Errorf("%w: merchantTradeNo/prepayId missing", provider.ErrMalformed)
	}
	minor, err := provider.ParseAmount(p.Data.TotalFee, 6) // USDT micro-units
	if err != nil || minor <= 0 {
		return nil, fmt.Errorf("%w: totalFee %q", provider.ErrMalformed, p.Data.TotalFee)
	}
	if c := strings.ToUpper(strings.TrimSpace(p.Data.Currency)); c != "" && c != "USDT" {
		return nil, fmt.Errorf("%w: currency %q is not USDT", provider.ErrMalformed, p.Data.Currency)
	}
	if a.RequireGuard && a.Nonces == nil {
		return nil, errors.New("usdt: RequireGuard is set but no nonce store is configured")
	}
	if a.Nonces != nil && !a.Nonces.Remember(nonce) {
		return nil, fmt.Errorf("%w: nonce %s", ErrReplayed, nonce)
	}
	return &types.PaymentNotice{
		TxnRef:     p.Data.MerchantTradeNo,
		Method:     types.MethodBinance,
		Amount:     minor,
		Currency:   "USDT",
		SenderName: p.Data.PayerNickname,
		SenderAcct: p.Data.PayerOpenID,
		ReceivedAt: sent.In(clock.PKT),
		Raw:        body,
	}, nil
}

// VerifyAPI implements adapter.PaymentAdapter. See assumption 5.
func (a *Adapter) VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error) {
	return nil, adapter.ErrUnsupported
}

// BinanceSignature computes the published Binance Pay callback signature:
// hex(HMAC-SHA256(secret, timestamp + "\n" + nonce + "\n" + body + "\n")).
//
// It is exported so the relay, the tests and the operator tooling all use one
// definition; a test that hand-writes the expectation would not fail if the
// verifier were deleted.
func BinanceSignature(secret, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write(body)
	mac.Write([]byte("\n"))
	return hex.EncodeToString(mac.Sum(nil))
}

// binancePay is the subset of Binance Pay's PAY_SUCCESS callback shape that
// this adapter reads.
type binancePay struct {
	BizType   string `json:"bizType"`
	BizStatus string `json:"bizStatus"`
	Data      struct {
		MerchantTradeNo string `json:"merchantTradeNo"`
		PayerOpenID     string `json:"payerOpenId"`
		PayerNickname   string `json:"payerNickname"`
		Currency        string `json:"currency"`
		TotalFee        string `json:"totalFee"`
		TransactionFee  string `json:"transactionFee"`
		TransactTime    int64  `json:"transactTime"`
	} `json:"data"`
	BizIDStr string `json:"prepayId"`
	Hash     string `json:"-"` // reserved
}

// VerifyTransfer validates an on-chain USDT transfer reported by the watcher
// and turns it into a notice. It refuses:
//
//   - a transfer on a chain other than ExpectedChain;
//   - a transfer of any token other than ExpectedContract (both address
//     formats: hex for BEP-20, base58 for TRC-20; compared case-insensitively
//     for hex, case-sensitively for base58);
//   - a transfer to an address other than ReceiverAddress;
//   - a transfer without MinConfirmations confirmations;
//   - a zero or negative amount;
//   - a duplicate (chain, txHash, logIndex) key.
//
// This is the function the watcher calls. It never performs network I/O, so it
// is trivially testable and cannot be tricked by a fake HTTP endpoint.
func (a *Adapter) VerifyTransfer(t Transfer) (*types.PaymentNotice, error) {
	if a.ExpectedContract == "" || a.ExpectedChain == 0 || a.ReceiverAddress == "" {
		return nil, ErrNotConfigured
	}
	if t.Chain != a.ExpectedChain {
		return nil, fmt.Errorf("%w: chain %d, want %d", ErrWrongContract, t.Chain, a.ExpectedChain)
	}
	if !sameToken(t.Token, a.ExpectedContract) {
		return nil, fmt.Errorf("%w: token %q", ErrWrongContract, t.Token)
	}
	if !sameToken(t.To, a.ReceiverAddress) {
		return nil, fmt.Errorf("%w: transfer to %q is not our receiver", ErrWrongContract, t.To)
	}
	if t.Confirmations < a.minConfirmations() {
		return nil, fmt.Errorf("%w: %d/%d confirmations", ErrUnderConfirmed, t.Confirmations, a.minConfirmations())
	}
	decimals := t.Decimals
	if decimals == 0 {
		decimals = 6 // USDT on both chains uses 6 decimals
	}
	if decimals != 6 {
		return nil, fmt.Errorf("%w: token decimals %d, USDT uses 6", ErrWrongContract, decimals)
	}
	if t.Raw <= 0 {
		return nil, fmt.Errorf("%w: non-positive amount %d", provider.ErrMalformed, t.Raw)
	}
	// USDT has 6 decimals, i.e. "micro-USDT" == the token's smallest unit, so
	// the raw value is already in types.Money minor units: no conversion.
	ref := fmt.Sprintf("%s:%s:%d", chainName(t.Chain), strings.ToLower(t.TxHash), t.LogIndex)
	if a.RequireGuard && a.Nonces == nil {
		return nil, errors.New("usdt: RequireGuard is set but no transfer dedupe is configured")
	}
	if a.Nonces != nil && !a.Nonces.Remember("chain:"+ref) {
		return nil, fmt.Errorf("%w: transfer %s", ErrReplayed, ref)
	}
	received := t.BlockTime
	if received.IsZero() {
		received = a.now()
	}
	return &types.PaymentNotice{
		TxnRef:     ref,
		Method:     types.MethodUSDT,
		Amount:     t.Raw,
		Currency:   "USDT",
		SenderName: t.From,
		SenderAcct: t.From,
		ReceivedAt: received.In(clock.PKT),
	}, nil
}

// sameToken compares contract/address strings. Hex addresses (BEP-20) are
// compared case-insensitively because EIP-55 checksums are presentation, not
// identity; base58 addresses (Tron) are case-sensitive because base58 encodes
// the value.
func sameToken(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if strings.HasPrefix(a, "0x") || strings.HasPrefix(b, "0x") {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func chainName(id int64) string {
	switch id {
	case ChainBSC:
		return "bsc"
	case 1:
		return "eth"
	}
	return "chain" + strconv.FormatInt(id, 10)
}

// NewMemGuard returns an in-memory nonce/transfer guard for tests.
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
