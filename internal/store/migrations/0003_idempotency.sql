-- Idempotency records for client-supplied retry keys.
--
-- A client that POSTs /api/orders and loses the response (timeout, dropped
-- connection, browser reload) must be able to retry without creating a second
-- order and a second reserved amount. The client sends "idempotency_key"; the
-- first request that claims the key stores the id of the order it produced,
-- and every later request with the same key returns that same order.
--
-- Key points:
--   * UNIQUE(scope, key) is the whole mechanism. The insert is the claim: two
--     concurrent requests with the same key cannot both create an order,
--     because the second INSERT fails on the constraint.
--   * scope is the identity the key belongs to (for orders: the buyer user id,
--     or a shared empty scope when the request carries no user). One buyer's
--     retry cannot be answered with another buyer's order, and one buyer
--     cannot burn another's key.
--   * request_hash is the hash of the normalised request body. Reusing a key
--     with a different body is a client bug, not a retry, and must be answered
--     with 409 rather than silently returning an order for a different product.
--   * state lets a claim exist while its order is still being created. The
--     claimant writes state='pending', creates the order, then stores the
--     order_id and moves to 'done'. Another request that arrives in between
--     waits briefly for 'done' instead of creating a second order.
--   * rows are pruned by age: after a day a retry is no longer plausible and a
--     key may be reused.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    scope        TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    order_id     TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (scope, key)
);

CREATE INDEX IF NOT EXISTS ix_idem_created ON idempotency_keys(created_at);
