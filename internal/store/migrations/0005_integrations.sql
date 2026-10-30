-- Operator-editable integration settings (Telegram bot token, chat id).
--
-- Why the database and not only the environment: the console has to be able to
-- turn the bot on without a redeploy, and an operator who has just created a
-- bot in BotFather should not need shell access to the box. Values here override
-- the environment; the environment stays the source for a read-only deployment.
--
-- The SQLite file is the secret boundary: it already holds every order, so a
-- token in this table adds no new exposure, and the console never displays a
-- stored token back to the browser (it renders a mask and a "replace" field).
CREATE TABLE IF NOT EXISTS integrations (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
