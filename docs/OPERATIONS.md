# Operations

## Build and run

```bash
CGO_ENABLED=1 go build ./cmd/paisabridge      # SQLite driver needs cgo
./paisabridge
```

Single binary, single SQLite file. No external service, no daemon.

## Configuration

Every setting is an environment variable; nothing is required except one rail
secret and an admin token.

| Variable | Default | Meaning |
|----------|---------|---------|
| `PB_ADDR` | `:8080` | listen address |
| `PB_DB` | `paisabridge.db` | SQLite path |
| `PB_ADMIN_TOKEN` | — | required for `/api/admin/*`, generate with `openssl rand -hex 24` |
| `PB_PUBLIC_URL` | `http://localhost:8080` | used in links |
| `PB_ORDER_TTL` | `15m` | payment window for an order |
| `PB_DISCOUNT_BPS` | `500` | wallet prepay discount, basis points |
| `PB_DEV` | `false` | dev mode: boots with no rails so order/wallet endpoints can be exercised |
| `PB_REQUIRE_SIGNATURE` | `true` | reject unsigned inbound notices; never disable in production |
| `PB_EASYPAISA_SECRET` | — | Easypaisa rail |
| `PB_JAZZCASH_SALT` | — | JazzCash integrity salt |
| `PB_NAYAPAY_WEBHOOK_SECRET`, `PB_NAYAPAY_TOKEN` | — | NayaPay rail |
| `PB_SADAPAY_WEBHOOK_SECRET` | — | SadaPay rail |
| `PB_BANK_RELAY_SECRET` | — | bank alert relay |
| `PB_BINANCEPAY_SECRET` | — | Binance Pay + USDT |
| `PB_PAYONEER_SECRET` | — | Payoneer |
| `PB_USDT_RECEIVER_ADDRESS`, `PB_USDT_CONTRACT`, `PB_USDT_CHAIN` | — | on-chain watcher |
| `PB_TELEGRAM_TOKEN` | — | enables the customer bot (long polling, no public URL needed) |
| `PB_TELEGRAM_API_BASE` | `https://api.telegram.org` | override for a relay or a test double |
| `PB_OUTBOUND_WEBHOOK_URL` | — | push every order transition to your storefront |
| `PB_OUTBOUND_SECRET` | — | HMAC key for outbound deliveries (`X-PB-Signature`) |

A rail with no secret is skipped at boot and its webhook route answers
`no_adapter`. Booting with zero rails is an error unless `PB_DEV=true`.

## Telegram bot

The bot uses long polling, so the gateway needs no public URL and no webhook
registration:

```
export PB_TELEGRAM_TOKEN=123456:AA-your-botfather-token
export PB_PUBLIC_URL=https://shop.example.com
./paisabridge        # logs "telegram bot polling"
```

Then in BotFather: `/setcommands` -> `start, order, wallet`. No webhook
registration and no public URL for the bot are needed: it uses long polling.

Customers whose order has `customer_phone = "tg:<chat_id>"` get a status push on
every transition, and the OTP is attached once the order is fulfilled.

## Deploying behind a reverse proxy

```
location /api/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header X-Forwarded-For $remote_addr;
    client_max_body_size 256k;      # notices are tiny; do not accept more
}
```

Terminate TLS at the proxy. The Caddy config the project ships is in
`deploy/Caddyfile`.

## Backups

Everything lives in one SQLite file plus its `-wal`/`-shm` siblings:

```bash
sqlite3 paisabridge.db ".backup '/backups/pb-$(date +%F).db'"
```

Run it from cron daily; keep 14. A restore is "stop the service, replace the
file, start".

## Runbook

| Symptom | Cause | Action |
|---------|-------|--------|
| webhook `401 invalid_signature` | secret mismatch, clock skew, or a hand-built body | re-encode with `cmd/pbsend`; check the relay secret on both ends |
| `notice: replayed envelope id` | duplicate delivery | expected; the guard dropped it, nothing to do |
| `matched:false reason:no_open_order` | amount matches nothing (late payment or typo) | appears under `GET /api/notices?status=unmatched`; reconcile or refund |
| `1234: database is locked` | two processes on one file | run exactly one gateway per SQLite file |
| order stuck in `review` | low-trust source or ambiguous amount | verify in the bank statement, then `POST /api/admin/approve` |
| orders expiring too fast | `PB_ORDER_TTL` too low for the rail | the wallet rails settle instantly, but IBFT e-mail alerts lag minutes; keep >= 15m |

## Health and metrics

`GET /api/healthz` pings the database and returns `503` when it is unreachable -
use it as the container health check. Every request is logged with method, path,
status and duration in the same structured line, so a log-based alert on
`status>=500` and on `401 invalid_signature` (possible forgery attempts) is
enough to start with.
