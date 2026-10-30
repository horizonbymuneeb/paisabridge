# paisabridge

[![ci](https://github.com/horizonbymuneeb/paisabridge/actions/workflows/ci.yml/badge.svg)](https://github.com/horizonbymuneeb/paisabridge/actions/workflows/ci.yml)
[![Go 1.22+](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**Open-source payment bridge for Pakistan rails + crypto.** One API, one
webhook, one ledger — Easypaisa, JazzCash, NayaPay, SadaPay, IBFT/Raast bank
transfers, USDT (BEP-20/TRC-20), Binance Pay and Payoneer, with
amount-unique order matching, a Telegram bot and a wallet ledger.

MIT licensed. Single Go binary. SQLite by default, no SaaS dependency.

```
order created  ->  unique amount (Rs 1499 -> Rs 1507, within +-Rs 9)
               ->  buyer pays with any rail
               ->  signed webhook / notice feed
               ->  matcher resolves exactly one order
               ->  fulfilled + wallet updated + Telegram notified
```

## Why

Most automated checkout flows for these rails verify payments by parsing bank
SMS/e-mail alerts and matching a hand-tweaked amount. It works, but it is
unsigned, replayable and depends on a human staying awake. paisabridge keeps the
good part - amount-based matching with a zero-typing checkout - and replaces the
rest with signed webhooks, an idempotent ledger and an audited admin override.

## Quick start

```bash
git clone https://github.com/horizonbymuneeb/paisabridge
cd paisabridge
CGO_ENABLED=1 go build ./cmd/paisabridge

export PB_DB=./paisabridge.db
export PB_ADMIN_TOKEN="$(openssl rand -hex 24)"
export PB_BANK_RELAY_SECRET="$(openssl rand -hex 32)"
export PB_PUBLIC_URL=http://localhost:8080
./paisabridge
```

Create an order:

```bash
curl -s -X POST localhost:8080/api/orders \
  -H 'content-type: application/json' \
  -d '{"product_id":"chatgpt-plus","product_name":"ChatGPT Plus 1m",
       "catalog_price_minor":149900,"currency":"PKR","method":"easypaisa",
       "customer_phone":"tg:123456789"}'
```

The response carries both `catalog_price_minor` and the unique `amount_minor`
the buyer must send. See [Usage](#usage) for verification, wallet payments and
the per-rail trust table.

## Usage

### 1. Create an order

Any rail from the list above. The response tells the buyer exactly what to send:

```json
{
  "id": "ord_cb2cd27d78dbe65a8b",
  "ref": "ref_5ad4381b98011df54a",
  "catalog_price_minor": 149900,
  "amount_minor": 150700,
  "currency": "PKR",
  "method": "bank",
  "status": "pending_payment",
  "expires_at": "2026-10-31T08:30:00Z"
}
```

Display `amount_minor` (Rs 1507.00), not the list price: the small offset is what
makes the payment identifiable.

### 2. Verify the payment

Providers that sign push straight to `/api/webhooks/{method}`. For rails that
only produce an e-mail or SMS alert, forward it through your relay, which seals
it with the shared secret:

```bash
go build -o pbsend ./cmd/pbsend

./pbsend -url http://localhost:8080/api/webhooks/bank \
         -secret "$PB_BANK_RELAY_SECRET" -method bank \
         -amount 150700 -txn IBFT-001 \
         -note "Rs 1507.00 has been credited to A/C 12345678901234 from Ali Raza"
# {"matched":true,"order_id":"ord_...","reason":"matched","credited":true}
```

### 3. Deliver and notify

A matched payment moves the order to `fulfilled`. Your storefront hears about it
three ways, pick any:

| Channel | Setup |
|---------|-------|
| Poll | `GET /api/orders/{id}` |
| Outbound webhook | `PB_OUTBOUND_WEBHOOK_URL` + `PB_OUTBOUND_SECRET`, signed `X-PB-Signature` |
| Telegram | `PB_TELEGRAM_TOKEN`, buyer queries `/order <ref>` or gets a push |

### Wallet payments

Customers who top up a wallet pay less and settle instantly:

```bash
curl -X POST localhost:8080/api/wallet/topup -H 'content-type: application/json' \
  -d '{"user_id":"tg:123456789","amount_minor":500000,"idempotency_key":"topup-1"}'

curl -X POST localhost:8080/api/orders/ord_.../claim \
  -H 'content-type: application/json' -d '{"user_id":"tg:123456789"}'
# price 150000, discount 7500 (5%), charged 142500
```

### Rails

| Rail | Verification | Auto-fulfils |
|------|--------------|:------------:|
| NayaPay | provider status API + HMAC | yes |
| JazzCash | `pp_SecureHash` HMAC-SHA256 | yes |
| Easypaisa | AES `merchantHashedResp` | yes |
| SadaPay | relay HMAC + code | yes |
| Binance Pay | published HMAC scheme | yes |
| USDT BEP-20 / TRC-20 | on-chain + confirmations | yes |
| Payoneer | link stamp + relay | yes |
| Bank IBFT/RAAST/e-mail | relay HMAC only | **no - review** |

That last row is deliberate: an alert you can e-mail is an alert anyone can
e-mail, so it opens a review, never a shipment.

## Documentation

| Doc | Contents |
|-----|----------|
| [`docs/QUICKSTART.md`](docs/QUICKSTART.md) | clone -> fulfilled order in 15 minutes |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | components, data flow, state machine |
| [`docs/API.md`](docs/API.md) | every endpoint, request/response, errors |
| [`docs/ADAPTERS.md`](docs/ADAPTERS.md) | adding a rail, signature schemes |
| [`docs/OPERATIONS.md`](docs/OPERATIONS.md) | deploy, Telegram bot, backups, runbook |

## Features

- **Amount-unique matching** - every open order gets a distinct payable amount,
  so a payment needs no reference number typed by the buyer.
- **Signed everything** - provider webhooks where the provider signs, an HMAC
  envelope everywhere else, and duplicate deliveries are refused by a durable
  replay guard.
- **Never auto-delivers from an unsigned source.** E-mail/SMS alerts may open a
  review, not a shipment.
- **Wallet ledger** - append-only, idempotency-keyed, prepay discount in basis
  points.
- **Telegram bot** - status push, `/order`, `/wallet`; long polling, no public
  URL required.
- **Outbound webhooks** - every transition pushed to your storefront, signed,
  with a stable delivery id and bounded retries.
- **Zero external services** - one binary, one SQLite file.

## Status

Stable enough to self-host: 18 packages, 275 tests, `go test -race ./...` green.
CI runs gofmt, vet, race tests, a build and `govulncheck` on every push.

## License

MIT
