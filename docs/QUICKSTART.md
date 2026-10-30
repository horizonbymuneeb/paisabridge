# Quickstart

Fifteen minutes from `git clone` to a fulfilled order, using the bank rail so
no provider onboarding is needed.

## 1. Build

```bash
git clone https://github.com/horizonbymuneeb/paisabridge
cd paisabridge
make build          # CGO_ENABLED=1 go build ./cmd/paisabridge
```

You need Go 1.22+ and a C toolchain (the SQLite driver uses cgo; on Debian
`apt install build-essential`).

## 2. Configure

```bash
export PB_DB=./paisabridge.db
export PB_ADMIN_TOKEN="$(openssl rand -hex 24)"
export PB_BANK_RELAY_SECRET="$(openssl rand -hex 32)"
export PB_PUBLIC_URL=http://localhost:8080
export PB_ORDER_TTL=20m
./paisabridge
```

Expected log:

```
level=INFO msg="database ready" path=./paisabridge.db
level=INFO msg="rail easypaisa disabled: EasypaisaSecret not set"
level=INFO msg="listening" addr=:8080 dev_mode=false methods=bank
```

Every rail you did not configure is skipped. That is normal.

## 3. Create an order

```bash
curl -s -X POST localhost:8080/api/orders \
  -H 'content-type: application/json' \
  -d '{"product_id":"chatgpt-plus","product_name":"ChatGPT Plus",
       "catalog_price_minor":149900,"currency":"PKR","method":"bank"};
```

```json
{
  "id": "ord_...",
  "ref": "ref_...",
  "catalog_price_minor": 149900,
  "amount_minor": 150000,
  "status": "pending_payment"
}
```

The buyer must send **`amount_minor`**, not `catalog_price_minor`. The two differ
by a deterministic offset of at most Rs 9 in either direction, so every open
order owns a distinct amount and an inbound payment can be matched without the
buyer typing a reference. Show the paid amount in your checkout UI.

## 4. Point your alert relay at the webhook

A bank sends you an e-mail or SMS for every credit. Forward it to a small
relay you control; the relay signs it with `notice.Encode` (or `cmd/pbsend`) and
POSTs to `/api/webhooks/bank`:

```bash
go build -o pbsend ./cmd/pbsend
./pbsend -url http://localhost:8080/api/webhooks/bank \
         -secret "$PB_BANK_RELAY_SECRET" -method bank \
         -amount 150000 -txn IBFT-001 \
         -note "Rs 1500.00 has been credited to A/C 12345678901234 from Ali Raza"
```

```
HTTP 200
{"matched":true,"order_id":"ord_...","reason":"matched","credited":true}
```

The order is now `fulfilled`. Re-sending the same notice is refused
(`replayed envelope id`) and credits nothing twice.

## 5. Enable the Telegram bot (optional)

```
export PB_TELEGRAM_TOKEN=123456:AA-your-botfather-token
export PB_PUBLIC_URL=https://shop.example.com
```

Orders created with `"customer_phone":"tg:<chat_id>"` receive a status push.
Customers can query `/order <ref>` and `/wallet`.

## 6. Talk to your own storefront

Every state change can be forwarded to your app:

```
export PB_OUTBOUND_WEBHOOK_URL=https://shop.example.com/pb/hook
export PB_OUTBOUND_SECRET="$(openssl rand -hex 32)"
```

Payload: `{"event":"order.fulfilled","order":{...}}`, signed with
`X-PB-Signature: hex(hmac_sha256(secret, body))`.

## What to do next

- Add a merchant rail (NayaPay/JazzCash) so that rail verifies without a human:
  `docs/ADAPTERS.md` and the env vars in `docs/OPERATIONS.md`.
- Put the gateway behind a TLS reverse proxy: `deploy/Caddyfile`.
- Take a backup with `sqlite3 paisabridge.db ".backup backup.db"`.
