# paisabridge

**Open-source payment bridge for Pakistan rails + crypto.** One API, one
webhook, one ledger — Easypaisa, JazzCash, NayaPay, SadaPay, IBFT/Raast bank
transfers, USDT (BEP-20/TRC-20), Binance Pay and Payoneer, with
amount-unique order matching, a Telegram bot and a wallet ledger.

MIT licensed. Single Go binary. SQLite by default, no SaaS dependency.

```
order created  ->  unique amount (Rs 1499 -> Rs 1497)
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
export PB_ADMIN_TOKEN=$(openssl rand -hex 24)
export PB_WEBHOOK_SECRET=$(openssl rand -hex 32)
export PB_BASE_URL=http://localhost:8080
./paisabridge
```

Create an order:

```bash
curl -s -X POST localhost:8080/api/orders \
  -H 'content-type: application/json' \
  -d '{"product_id":"chatgpt-plus","product_name":"ChatGPT Plus 1m",
       "amount_minor":149900,"currency":"PKR","method":"easypaisa",
       "customer_phone":"tg:123456789"}'
```

The response carries both `catalog_price_minor` and the unique
`amount_minor` the buyer must send.

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

Stable enough to self-host: 18 packages, 268 tests, `go test -race ./...` green,
CI runs gofmt + vet + race tests + govulncheck on every push.

## License

MIT
