// Package types defines the shared contracts for paisabridge.
// Every adapter speaks these types; the matching engine, wallet and API
// depend only on this package so adapters stay independent.
package types

import "time"

// Money is an integer minor-unit amount with currency.
// PKR uses paisa (1 PKR = 100 paisa), USDT uses micro-units (1 USDT = 1e6).
type Money struct {
	Minor    int64  `json:"minor"`    // amount in minor units
	Currency string `json:"currency"` // "PKR", "USDT"
}

// Method identifies which payment rail an order uses.
type Method string

const (
	MethodEasypaisa Method = "easypaisa"
	MethodJazzCash  Method = "jazzcash"
	MethodNayaPay   Method = "nayapay"
	MethodSadaPay   Method = "sadapay"
	MethodBank      Method = "bank"
	MethodUSDT      Method = "usdt"
	MethodBinance   Method = "binance"
)

// OrderStatus is the order state machine.
type OrderStatus string

const (
	StatusCreated        OrderStatus = "created"
	StatusPendingPayment OrderStatus = "pending_payment"
	StatusVerified       OrderStatus = "verified"
	StatusFulfilled      OrderStatus = "fulfilled"
	StatusReview         OrderStatus = "review"
	StatusExpired        OrderStatus = "expired"
	StatusCancelled      OrderStatus = "cancelled"
)

// Order is one purchase attempt tied to a unique amount.
type Order struct {
	ID            string      `json:"id"`
	Ref           string      `json:"ref"`
	ProductID     string      `json:"product_id"`
	ProductName   string      `json:"product_name,omitempty"`
	CatalogPrice  int64       `json:"catalog_price_minor"`
	Amount        int64       `json:"amount_minor"`
	Currency      string      `json:"currency"`
	Method        Method      `json:"method"`
	Status        OrderStatus `json:"status"`
	CustomerEmail string      `json:"customer_email,omitempty"`
	CustomerPhone string      `json:"customer_phone,omitempty"`
	TxRef         string      `json:"tx_ref,omitempty"`
	SMSOTP        string      `json:"sms_otp,omitempty"`
	ExpiresAt     time.Time   `json:"expires_at"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// IsTerminal reports whether an order can no longer change.
func (o *Order) IsTerminal() bool {
	switch o.Status {
	case StatusFulfilled, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// PaymentNotice is a single inbound payment fact received from any source.
type PaymentNotice struct {
	TxnRef     string    `json:"txn_ref"`
	Method     Method    `json:"method"`
	Amount     int64     `json:"amount_minor"`
	Currency   string    `json:"currency"`
	SenderName string    `json:"sender_name,omitempty"`
	SenderAcct string    `json:"sender_acct,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Raw        []byte    `json:"raw,omitempty"`
}

// MatchOutcome is the result of evaluating a PaymentNotice against orders.
type MatchOutcome struct {
	Matched bool
	OrderID string
	Reason  string
}
