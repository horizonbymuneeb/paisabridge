# Architecture

paisabridge is six small packages wired by `cmd/paisabridge`. Data flows in
one direction: a payment fact enters through an **adapter**, is resolved by the
**matcher**, mutates exactly one **order** in the **store**, moves money in the
**wallet ledger**, and leaves as a notification through the **bot** and the
integrator's webhook.

```
                   +------------------+
 buyer  -------->  |  POST /api/orders|  -> unique amount
                   +------------------+
                            |
                    (pays with any rail)
                            v
   +------------------+   +------------------+   +------------------+
   | provider webhook | ->|   adapter        | ->|    matcher       |
   | bank email/SMS   |   |  (signature      |   | amount + method  |
   | chain watcher    |   |   verified)      |   | + expiry + dedup |
   +------------------+   +------------------+   +------------------+
                                                      |        |
                                                      v        v
                                               +----------+ +----------+
                                               |  store   | |  wallet  |
                                               | (SQLite) | | (ledger) |
                                               +----------+ +----------+
                                                      |
                                        +-------------+-------------+
                                        v                           v
                                 +-------------+            +-------------+
                                 | Telegram bot|            | integrator  |
                                 | status/OTP  |            | webhook/SSE |
                                 +-------------+            +-------------+
```

## Packages

| Package | Responsibility | Must not |
|---------|----------------|----------|
| `internal/types` | shared structs and state enums | import anything else in-repo |
| `internal/unique` | unique-amount picker + discount math | touch I/O |
| `internal/adapter` | the plugin contract, `ErrInvalidSignature` | know about HTTP routes or the DB |
| `internal/adapters/*` | one rail per package, payload → `PaymentNotice` | write to the DB |
| `internal/store` | SQLite schema, migrations, transactional writes | do matching logic |
| `internal/match` | `PaymentNotice` → at most one order | do I/O |
| `internal/wallet` | append-only ledger, balance, refunds | own order state |
| `internal/api` | HTTP surface, auth, request decoding | contain business rules |
| `internal/bot` | Telegram long polling | read the DB directly (uses an interface) |
| `internal/admin` | override, audit trail | be reachable without `X-Admin-Token` |

The directional rule matters: adapters never decide whether a payment is good
for an order, and the matcher never talks to a provider. That keeps every rail
replaceable and every match reproducible from stored facts.

## Order state machine

```
        create
          |
          v
      [created] --begin payment--> [pending_payment]
                                        |     \
                             match      |      \  ambiguity /
                             success    |       \  expiry
                                        v        v        v
                                   [verified]  [review] [expired]
                                        |                   |
                                        v                   v
                                   [fulfilled]        (wallet refund)
```

Rules:

- Only `pending_payment` and `created` orders are matchable.
- `review` is terminal for automation, not for a human: `POST /api/admin/approve`
  moves it forward and writes an audit row.
- `fulfilled`, `expired`, `cancelled` are terminal; any webhook that resolves to
  a terminal order is recorded as a duplicate and does not re-fire fulfilment.

## Money

All amounts are integer minor units: paisa for PKR (1 PKR = 100), micro-USDT
for `USDT` (1 USDT = 1,000,000). No floats anywhere in the ledger. The unique
amount picker and the 5%-style prepay discount both operate on integers
(`internal/unique`).

## Matching

1. Reject if `method` or `currency` do not match the order's rail.
2. Reject if the order is not open (`created` / `pending_payment`) or the
   expiry window has closed.
3. Reject if `txn_ref` has been seen before (replay).
4. Look up open orders by exact amount inside the window.
5. Zero matches → `review` with reason `no_amount`. Two or more → `review` with
   reason `amount_collision`. Exactly one → `verified`, then fulfilment.

## Trust tiers

| Source | Trust | Action on match |
|--------|-------|-----------------|
| Signed provider webhook (HMAC / provider signature verified) | high | fulfil |
| Merchant status API re-query (NayaPay/JazzCash verify endpoint) | high | fulfil |
| Chain watcher with N confirmations | high | fulfil |
| Bank email/SMS alert parser (no signature) | low | `review` |
| Admin manual approval | override | fulfil, audited |

An unsigned alert source can create a `review` row, never a delivery: signature
verification, not parsing, is what authorises fulfilment.
