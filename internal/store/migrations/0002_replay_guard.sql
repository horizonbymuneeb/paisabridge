-- Replay guard for inbound webhooks.
--
-- The notice envelope id is the idempotency key at the trust boundary: the
-- first request carrying an id is remembered, and every later request with the
-- same id is refused before it reaches the matcher. The table is deliberately
-- tiny and append-only; rows are pruned by the sweeper after the notice
-- window, because an id older than the signature window can no longer be
-- replayed successfully anyway.
CREATE TABLE IF NOT EXISTS webhook_replay (
    envelope_id TEXT PRIMARY KEY,
    method      TEXT NOT NULL,
    first_seen  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_replay_seen ON webhook_replay(first_seen);
