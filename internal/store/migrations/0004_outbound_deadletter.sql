-- 0004_outbound_deadletter.sql — durable parking space for outbound deliveries.
--
-- The outbound sender (internal/outbound) retries a failed delivery with
-- exponential backoff. Eventually the retries run out: the integrator's site is
-- down for an hour, a proxy in front of it returns 502, or the network path is
-- broken. Without this table that event vanishes after the last error is
-- logged, and the storefront never learns that an order was fulfilled — the
-- worst failure mode of a payment gateway, because the money moved and the
-- order did not.
--
-- Design notes:
--   * A row is written only when every retry has failed. A delivery that
--     succeeded, or was refused with a 4xx (a permanent rejection: retrying
--     would never help), never reaches this table.
--   * delivery_id is UNIQUE and equals the X-PB-Delivery value the receiver
--     saw. It is the outbound counterpart of (method, txn_ref) for inbound
--     notices: re-running a failing delivery can never create two rows, and
--     the receiver can still discard a duplicate after a retry.
--   * body holds the exact JSON bytes that were signed and POSTed. Keeping the
--     bytes verbatim (not re-marshalled) means a later retry produces a body
--     whose X-PB-Signature still verifies; re-encoding the payload would not.
--   * order_id and event are denormalised out of the body so the admin list can
--     filter without parsing JSON on every row.
--   * resolved_at is empty while a delivery is parked. Rows are never deleted
--     on success: "this order was delivered four hours late" is what an
--     operator needs during a reconciliation, and the table holds one row per
--     failed delivery.
--   * attempts counts sends made against the row (the original retry loop plus
--     any admin retries), so a delivery that keeps failing is visible.
CREATE TABLE IF NOT EXISTS outbound_deadletter (
    id          TEXT PRIMARY KEY,
    delivery_id TEXT NOT NULL UNIQUE,
    event       TEXT NOT NULL,
    order_id    TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    attempts    INTEGER NOT NULL DEFAULT 0,
    body        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    retried_at  TEXT NOT NULL DEFAULT '',
    resolved_at TEXT NOT NULL DEFAULT ''
);

-- The admin list is "newest unresolved first", which is exactly this index.
CREATE INDEX IF NOT EXISTS ix_deadletter_open
    ON outbound_deadletter(resolved_at, created_at);
CREATE INDEX IF NOT EXISTS ix_deadletter_order
    ON outbound_deadletter(order_id);
