# API reference

Base URL: whatever `PB_PUBLIC_URL` points at (or your reverse proxy host). All bodies are JSON, all amounts
are integer minor units (`paisa` for PKR: 1 PKR = 100; micro-USDT for USDT:
1 USDT = 1,000,000). Timestamps are RFC 3339 UTC.

Errors are always:

```json
{"error": "human readable message", "code": "machine_code"}
```

| HTTP | code | meaning |
|------|------|---------|
| 400 | `invalid_request` | malformed body or missing field |
| 401 | `unauthorized` | bad or missing `X-Admin-Token` |
| 404 | `not_found` | no such order |
| 409 | `order_not_open` | order already terminal / not payable |
| 422 | `amount_window_exhausted` | no unique amount left for this rail |
| 500 | `internal` | bug or storage failure |

---

## POST /api/orders

Create an order and reserve a unique amount.

```json
{
  "product_id": "chatgpt-plus",
  "product_name": "ChatGPT Plus - 1 month",
  "amount_minor": 149900,
  "currency": "PKR",
  "method": "easypaisa",
  "customer_email": "buyer@example.com",
  "customer_phone": "tg:123456789",
  "ttl_seconds": 900
}
```

| Field | Required | Notes |
|-------|:--------:|-------|
| `product_id` | yes | your own catalogue key |
| `product_name` | no | shown in bot/status responses |
| `amount_minor` | yes | list price in minor units, > 0 |
| `currency` | yes | `PKR` or `USDT` |
| `method` | yes | `easypaisa`, `jazzcash`, `nayapay`, `sadapay`, `bank`, `usdt`, `binance`, `payoneer` |
| `customer_email` | no | for receipts |
| `customer_phone` | no | `tg:<chat_id>` binds the order to a Telegram chat |
| `ttl_seconds` | no | default 900, max 3600 |

Response `201`:

```json
{
  "id": "9f2c...",
  "ref": "PB-7F3K2Q",
  "product_id": "chatgpt-plus",
  "catalog_price_minor": 149900,
  "amount_minor": 149700,
  "currency": "PKR",
  "method": "easypaisa",
  "status": "pending_payment",
  "expires_at": "2026-10-31T09:15:00Z",
  "created_at": "2026-10-31T09:00:00Z"
}
```

`amount_minor` is what the buyer must send. It differs from
`catalog_price_minor` by a small bounded offset (max 9 PKR) so two open orders
never share an amount; always display the paid amount, never the list price.

---

## GET /api/orders/{id}

`id` accepts the UUID or the human `ref` (`PB-7F3K2Q`). Returns the order plus
`payment_deadline` and, once matched, `tx_ref`.

## POST /api/orders/{id}/claim

No body. Records that the buyer says they have paid and asks the matcher to
re-check its last window. Returns the current order. Idempotent.

## POST /api/orders/{id}/cancel

Releases the reservation. If a wallet top-up had been applied it is refunded.
Returns the cancelled order. Idempotent; cancelling a terminal order is `409`.

---

## POST /api/webhooks/{method}

The only inbound payment path. `method` is one of the rails above.

**Signed rails** (jazzcash, easypaisa, nayapay, sadapay, payoneer, binance):
the adapter verifies the provider signature before anything else happens. A
failed signature is `401` and is logged, nothing is mutated.

**Unsigned rails** (bank e-mail/SMS parser, bare USDT watcher without chain
confirmation): a shared secret must be presented as `X-Webhook-Signature`
(HMAC-SHA256 hex of the raw body with that rail's relay secret, e.g. `PB_BANK_RELAY_SECRET`), and even then a
match only moves the order to `review` - never straight to `fulfilled`.

Success `200`:

```json
{"ok": true, "order_id": "9f2c...", "status": "fulfilled", "reason": "matched"}
```

Duplicate delivery of the same provider transaction returns
`{"ok": true, "duplicate": true}` and changes nothing.

---

## GET /api/wallet?user_id=tg:123456789

```json
{"user_id": "tg:123456789", "balance_minor": 25000, "currency": "PKR"}
```

## POST /api/wallet/topup

```json
{"user_id": "tg:123456789", "amount_minor": 50000, "method": "nayapay", "txn_ref": "provider-txn-id"}
```

Credits the ledger exactly once per `txn_ref`. For self-hosted deployments this
endpoint is normally called by your own reconciler after the rail's webhook has
been verified; it is idempotent so retries are safe.

---

## POST /api/admin/approve

Header `X-Admin-Token: <PB_ADMIN_TOKEN>`. Manual override for an order the
matcher parked in `review`.

```json
{"order_id": "9f2c...", "txn_ref": "manual-2026-10-31-001", "note": "verified on bank statement"}
```

Moves the order to `fulfilled`, writes an `audit` row (`actor`, `note`,
`before`, `after`). This is the only endpoint that can complete an order without
a verified payment fact, so it must be protected by a strong token and is
rate-limited.

## GET /api/healthz

```json
{"ok": true, "version": "0.1.0", "uptime_s": 120}
```

## Integrator webhook (outbound)

Set `PB_OUTBOUND_WEBHOOK_URL` and paisabridge POSTs every order state change to
your own site:

```json
{"event": "order.fulfilled", "order": { ...full order... }, "signature_at": "2026-10-31T09:05:00Z"}
```

Signed with `X-PB-Signature: hex(hmac_sha256(PB_OUTBOUND_SECRET, body))`.
Retries: 5 attempts with exponential backoff, then parked in the dead-letter
table visible at `GET /api/admin/deadletters`.
