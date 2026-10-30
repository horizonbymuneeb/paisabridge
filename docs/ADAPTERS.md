# Adding a payment rail

An adapter turns one provider's inbound payload into a `types.PaymentNotice`.
That is the whole job. It never decides if an order is complete, never touches
the database, and never sends a notification.

## The contract

```go
type PaymentAdapter interface {
    Method() types.Method
    Supports() bool                                             // true = self-verifying
    VerifyWebhook(r *http.Request) (*types.PaymentNotice, error) // must verify signature
    VerifyAPI(ctx context.Context, txnRef string) (*types.PaymentNotice, error)
}
```

Both verification paths must return `adapter.ErrInvalidSignature` for a bad
signature and `adapter.ErrUnsupported` when the rail has no such capability.
Returning a notice is a promise that the payload was authenticated.

## Checklist for a new rail

1. `internal/adapters/<rail>/<rail>.go` with a package doc comment stating the
   trust model and every assumption, marked `TODO(provider-credentials)` where a
   value depends on the merchant onboarding pack — never invent an endpoint.
2. Signature verification first; constant-time comparison
   (`provider.EqualHex` handles the length-safe compare).
3. Replay guard: call the injected `Guard` after the signature passes.
4. Timestamp window: reject stale notices (`clock` package supplies the zone).
5. Amounts: convert decimal strings to minor units with
   `provider.DecimalToMinor`; never parse through `float64`.
6. Table-driven tests. Every test file must contain at least one case that
   **fails when signature verification is deleted**; prove it by mutation.
7. Register the rail in `internal/adapters/registry/registry.go` (`Config`
   field + `entries` slice). A rail with an empty secret is skipped at boot,
   so no deployment is ever forced to configure rails it does not sell.

## Trust tiers

| Tier | Meaning | Consequence of a match |
|------|---------|------------------------|
| Provider-signed | the provider itself authenticated the message | fulfil |
| Merchant status API | we re-query the provider and it confirms | fulfil |
| Chain watcher | N confirmations of an on-chain transfer | fulfil |
| Relay envelope only | our relay signed, the underlying alert did not | `review` |

The last row is the one that matters: an e-mail or SMS alert is unsigned
evidence. It may open a review, never a delivery.

## Signature schemes implemented

| Rail | Scheme | Notes |
|------|--------|-------|
| Binance Pay | `hex(HMAC-SHA256(secret, timestamp + "\n" + nonce + "\n" + body + "\n"))` | ms timestamp, 5-minute window |
| JazzCash | `HMAC-SHA256` over sorted `pp_` fields (minus `pp_SecureHash*`), key = integrity salt | both documented joining conventions accepted; the matched one is reported so it can be pinned |
| Easypaisa | AES-ECB + PKCS#7 over `transactionId&code&desc` (`merchantHashedResp`) | ECB is malleable; a ciphertext-binding test pins the realistic forgery |
| NayaPay / SadaPay / Payoneer | relay envelope (ours) + provider status API where one exists | labelled as an in-house protocol, never presented as a provider signature |
| Bank (IBFT/RAAST/SMS) | relay envelope only | always lands in `review`, never auto-fulfils |
| USDT BEP-20 / TRC-20 | on-chain transfer + confirmations + contract check | 15 (BSC) / 19 (Tron) confirmations, configurable |

## Envelope

Every rail that is reached through a relay speaks one signed envelope:

```
{"id":"...","method":"bank","issued_at":1761700000,"payload":{...}}

X-Paisabridge-Signature: v1=<hex HMAC-SHA256(secret, "v1."+id+"."+method+"."+issued_at+"."+payloadBytes)>
```

Ship the same body to `/api/webhooks/{method}`. Use `cmd/pbsend` or
`notice.Encode` to mint one; do not hand-assemble the JSON, because the
signature covers exact bytes and any re-serialisation breaks it.
