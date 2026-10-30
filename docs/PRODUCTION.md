# Running in production

Everything here is about the things that go wrong after the demo works.

## Deployment shape

```
        TLS
   ┌──────────┐        ┌────────────────────────┐
   │  Caddy   │───────►│  paisabridge (loopback) │───► SQLite file
   └──────────┘        └────────────────────────┘
        ▲                          ▲
        │                          │ webhooks (signed)
   storefront                      │
                              rails / relay
```

One process per SQLite file. Two processes on one file will produce
`database is locked` under load; if you need horizontal scale, run one writer
per database and put a queue in front of it.

### systemd unit

```ini
[Unit]
Description=paisabridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=paisa
EnvironmentFile=/etc/paisabridge/env
ExecStart=/usr/local/bin/paisabridge
Restart=always
RestartSec=3
# The gateway only needs its own data directory and outbound HTTPS.
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes
ReadWritePaths=/var/lib/paisabridge
StateDirectory=paisabridge
WorkingDirectory=/var/lib/paisabridge

[Install]
WantedBy=multi-user.target
```

`EnvironmentFile` should be `chmod 600`, owned by the service user.

## Before you take real money

Run this list once. Each item has burned a real deployment somewhere.

1. **Generated secrets, not typed ones.** `openssl rand -hex 32` for every rail
   secret and the admin token. A six-character secret is a week of brute force.
2. **`PB_REQUIRE_SIGNATURE=true`** (the default). Never disable it outside a
   test.
3. **TLS in front.** The gateway speaks plain HTTP and trusts
   `X-Forwarded-For` only when `PB_TRUST_PROXY=1`; set that only behind a proxy
   you control, otherwise a client can spoof its IP and dodge rate limits.
4. **Backups scheduled before the first order.** See below.
5. **Admin token stored in a password manager**, not in a shell history.
6. **Clock synchronised.** Signature windows and order expiry both assume NTP.
   A drifting clock rejects real payments.

## Backups

SQLite is safe to copy live with the `.backup` command:

```bash
sqlite3 /var/lib/paisabridge/paisabridge.db \
        ".backup '/var/backups/pb-$(date +%F).db'"
```

Cron it daily, keep 14 days, and restore-test it once:

```bash
systemctl stop paisabridge
cp /var/backups/pb-2026-01-01.db /var/lib/paisabridge/paisabridge.db
systemctl start paisabridge
curl -fsS localhost:8080/api/healthz
```

## Monitoring

Two signals catch almost everything:

| Signal | Meaning | Alert on |
|--------|---------|----------|
| `POST /api/webhooks/*` returning 401 | possible forgery, or a rotated secret on one side | any sustained rate |
| orders stuck in `review` | bank rail waiting for a human | older than 30 minutes |

`/metrics` is exposed when `PB_METRICS=1`. The metric names and what they mean
are in the release notes of the version you deploy.

## Upgrades

```bash
git fetch --tags
git checkout v0.2.0
make build
systemctl restart paisabridge
```

Migrations run at startup and are additive; there is no down-migration. Take a
backup before every upgrade that changes a migration.

## Incident playbook

**Payments arrive but orders stay pending.** Check the webhook response in the
log. `no_open_order` means the amount did not match any open order - usually the
buyer sent the list price instead of the unique amount. The notice is in
`GET /api/notices?status=unmatched`; reconcile or refund by hand.

**`replayed envelope id`.** Normal; the guard dropped a duplicate. If it happens
constantly, your relay is retrying instead of honouring the 200.

**Everything 500s after a restart.** Almost always a `PB_*` secret that
changed on one side. Compare the relay secret and the provider dashboard value.

**The gateway will not boot: `no rail is configured`.** You removed every
secret. Set at least one, or `PB_DEV=1` for a scratch instance.

## Capacity

Order create is two indexed inserts; a 2-vCPU box handles a few hundred per
second. The real limits are the provider's webhook rate and SQLite's single
writer. If you outgrow it, split rails into separate deployments - each writes
its own database, and the storefront stitches the results.
