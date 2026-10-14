-- 002b_users_paddle_customer.sql
--
-- relay/internal/db/db.go reads and writes users.paddle_customer_id
-- (GetOrCreateUser, GetUserByID, the Paddle webhook), but 001_init.sql only
-- created that column on `subscriptions`. On a fresh database every SELECT
-- against users therefore failed, so GetOrCreateUser fell through to INSERT
-- and a returning user's login died on users_email_key. Production most
-- likely got the column out-of-band; this makes fresh installs match.
--
-- Named 002b so it sorts between 002 and 003 and can be cherry-picked to any
-- branch without renumbering. Idempotent.

ALTER TABLE users ADD COLUMN IF NOT EXISTS paddle_customer_id TEXT;
CREATE INDEX IF NOT EXISTS idx_users_paddle_customer
  ON users (paddle_customer_id) WHERE paddle_customer_id IS NOT NULL;
