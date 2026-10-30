// Command pbsend mints a signed notice envelope and POSTs it to a gateway.
//
// It exists because the envelope signature covers exact bytes: hand-rolling the
// JSON in a shell or in another language will reorder keys and the signature
// will not match. Use this tool (or the same function) wherever a relay, a
// test harness or a demo needs to produce an authentic notice.
//
//	pbsend -url http://127.0.0.1:8080/api/webhooks/bank \
//	       -secret $PB_BANK_RELAY_SECRET -method bank \
//	       -amount 149900 -txn IBFT-1 -note "Rs 1499.00 has been credited to A/C 12345678901234 from Ali Raza"
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapters/notice"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

func main() {
	var (
		url        = flag.String("url", "http://127.0.0.1:8080/api/webhooks/bank", "webhook URL")
		secret     = flag.String("secret", os.Getenv("PB_BANK_RELAY_SECRET"), "relay HMAC secret (env PB_BANK_RELAY_SECRET)")
		method     = flag.String("method", "bank", "rail: bank, easypaisa, jazzcash, nayapay, sadapay, usdt, payoneer")
		amount     = flag.Int64("amount", 0, "amount in minor units (paisa / micro-USDT)")
		currency   = flag.String("currency", "PKR", "currency code")
		txn        = flag.String("txn", "", "provider transaction reference (idempotency key)")
		sender     = flag.String("sender", "", "sender display name")
		senderAcct = flag.String("sender-account", "", "sender account / address")
		note       = flag.String("note", "", "provider narrative / memo")
		id         = flag.String("id", "", "envelope id (default: derived from txn)")
		verbose    = flag.Bool("v", false, "print the envelope and response body")
	)
	flag.Parse()
	if *amount <= 0 || *txn == "" {
		fmt.Fprintln(os.Stderr, "pbsend: -amount and -txn are required")
		os.Exit(2)
	}
	envID := *id
	if envID == "" {
		envID = "ntc-" + *txn
	}
	rn := notice.RailNotice{
		TxnRef:        *txn,
		Amount:        *amount,
		Currency:      *currency,
		SenderName:    *sender,
		SenderAccount: *senderAcct,
		ReceivedAt:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Note:          *note,
	}
	body, sig, err := notice.Encode(*secret, envID, types.Method(*method), time.Now(), rn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pbsend:", err)
		os.Exit(1)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "envelope: %s\nsignature: v1=%s\n", body, sig)
	}
	req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "pbsend:", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(notice.HeaderSignature, "v1="+sig)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pbsend:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, bytes.TrimSpace(rb))
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}
