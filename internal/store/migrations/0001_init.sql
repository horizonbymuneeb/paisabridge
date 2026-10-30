-- 0001_init.sql — paisabridge core schema.
--
-- Design notes
--   * Orders carry a unique amount for their (method, currency) while they are
--     open. A partial unique index enforces that invariant in the database, so
--     the matcher can never accidentally see two open orders with the same
--     amount and an application bug cannot silently produce an ambiguous rail.
--     Historically-closed duplicate amounts are therefore impossible to record
--     at all — order creation re-picks the amount instead (see internal/store).
--   * order_events is the append-only state machine journal. The current
--     status lives on the order row (fast reads), the journal is the truth.
--   * payment_notices records every inbound payment fact exactly once, keyed by
--     (method, txn_ref). The primary key is the duplicate-payment guard: a
--     replayed webhook hits the constraint and never reaches the ledger.
--   * wallet_ledger is the wallet's money. Balance is SUM(amount_minor); there
--     is no mutable balance column anywhere. idempotency_key is unique per
--     (user_id, key) so retrying a credit is a no-op, not a double credit.
--   * UNIQUE(dedupe_key) on wallet_ledger makes a second credit for the same
--     order outcome (e.g. refund after a partial refund) a no-op as well.

CREATE TABLE IF NOT EXISTS orders (
    id             TEXT PRIMARY KEY,
    ref            TEXT NOT NULL UNIQUE,
    product_id     TEXT NOT NULL,
    product_name   TEXT NOT NULL DEFAULT '',
    catalog_price  INTEGER NOT NULL,
    amount         INTEGER NOT NULL,
    currency       TEXT NOT NULL,
    method         TEXT NOT NULL,
    status         TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    customer_email TEXT NOT NULL DEFAULT '',
    customer_phone TEXT NOT NULL DEFAULT '',
    tx_ref         TEXT NOT NULL DEFAULT '',
    sms_otp        TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_orders_open_amount
    ON orders(method, currency, amount)
    WHERE status IN ('created', 'pending_payment', 'verified');

CREATE INDEX IF NOT EXISTS ix_orders_status_expiry ON orders(status, expires_at);
CREATE INDEX IF NOT EXISTS ix_orders_user ON orders(user_id, created_at);

CREATE TABLE IF NOT EXISTS order_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id   TEXT NOT NULL REFERENCES orders(id),
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    actor      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_order_events_order ON order_events(order_id, id);

CREATE TABLE IF NOT EXISTS payment_notices (
    method      TEXT NOT NULL,
    txn_ref     TEXT NOT NULL,
    amount      INTEGER NOT NULL,
    currency    TEXT NOT NULL,
    sender_name TEXT NOT NULL DEFAULT '',
    sender_acct TEXT NOT NULL DEFAULT '',
    received_at TEXT NOT NULL,
    raw         BLOB,
    created_at  TEXT NOT NULL,
    order_id    TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'unmatched',
    PRIMARY KEY (method, txn_ref)
);

CREATE INDEX IF NOT EXISTS ix_notices_order ON payment_notices(order_id);

CREATE TABLE IF NOT EXISTS wallet_ledger (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id         TEXT NOT NULL,
    kind            TEXT NOT NULL,
    amount_minor    INTEGER NOT NULL,
    currency        TEXT NOT NULL,
    order_id        TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL,
    dedupe_key      TEXT NOT NULL UNIQUE,
    memo            TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    UNIQUE (user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS ix_ledger_user ON wallet_ledger(user_id, id);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);
