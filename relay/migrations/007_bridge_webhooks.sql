-- 007_bridge_webhooks.sql
--
-- Agent Bridge, P4-T4: giden webhook'lar (grant olayları → Slack köprüsü, SIEM vb.).
-- Gövde HMAC-SHA256 ile imzalanır: X-Tunr-Signature: t=<unix>,v1=<hex>.
-- `secret` imzalamak için relay'de düz durur (doğası gereği); yalnızca
-- oluşturma yanıtında bir kez gösterilir.
--
-- (Roadmap'te 007 hosted ajanlara ayrılmıştı; hosted ajanlar 008 olur.)
-- update.sh bu dosyayı her deploy'da yeniden çalıştırır → idempotent.

CREATE TABLE IF NOT EXISTS bridge_webhooks (
    id          TEXT PRIMARY KEY,                               -- "whk_…"
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_bridge_webhooks_org ON bridge_webhooks (org_id);
