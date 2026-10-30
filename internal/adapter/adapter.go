// Package adapter defines the payment-rail plugin contract.
package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

var (
	ErrInvalidSignature = errors.New("invalid webhook signature")
	ErrUnsupported      = errors.New("method does not support direct verification API")
)

// PaymentAdapter is one payment rail implementation.
// Implementations must be safe for concurrent use and must never mutate state:
// they only translate provider payloads into normalized types.
type PaymentAdapter interface {
	// Method returns the rail this adapter handles.
	Method() types.Method

	// Supports reports whether this adapter can auto-verify on its own,
	// or needs the generic email/SMS notice feed (bank, USDT, wallets).
	Supports() bool

	// VerifyWebhook validates and decodes an inbound webhook request.
	// It must verify the signature before returning nil.
	VerifyWebhook(r *http.Request) (*types.PaymentNotice, error)

	// VerifyAPI is used for adapters with a merchant status API
	// (NayaPay, JazzCash wallet charge). It returns ErrUnsupported when
	// the adapter only works via webhook/notice feed.
	VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error)
}

// HMACVerifier is a small helper for adapters that sign raw bodies.
func HMACVerifier(secret string, r *http.Request) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("missing webhook secret")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	sig := r.Header.Get("X-Signature")
	if sig == "" {
		sig = r.Header.Get("X-Webhook-Signature")
	}
	if sig == "" {
		return nil, ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, ErrInvalidSignature
	}
	return body, nil
}
