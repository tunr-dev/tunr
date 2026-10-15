-- 008_hosted_agents.sql
--
-- Agent Bridge, P7: tunr bulutunda çalışan (hosted) ajanlar.
--
--   apps.kind       'web' (varsayılan) | 'agent'. Ajan uygulamaları herkese
--                   açık değildir; yalnızca tetikleyiciler ulaşır.
--   apps.volume_mb  >0 ise /data'ya bağlanan kalıcı volume.
--   agents.bootstrap_hash  container'a env ile verilen başlangıç sırrının
--                   sha256'sı; her deploy'da döner. Sır, relay'de 1 saatlik
--                   ajan token'ıyla takas edilir.
--   agents.trigger_secret  tetikleyici isteklerini imzalayan anahtar
--                   (relay → ajan); imzalamak için düz durur.
--   app_triggers    cron ve webhook tetikleyicileri.
--
-- update.sh bu dosyayı her deploy'da yeniden çalıştırır → idempotent.

ALTER TABLE apps ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'web';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS volume_mb INT NOT NULL DEFAULT 0;

ALTER TABLE agents ADD COLUMN IF NOT EXISTS bootstrap_hash TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_secret TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_bootstrap ON agents (bootstrap_hash) WHERE bootstrap_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS app_triggers (
    id          TEXT PRIMARY KEY,                            -- "trg_…"
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('cron', 'webhook')),
    name        TEXT NOT NULL,                               -- webhook: URL segment; cron: label
    spec        TEXT NOT NULL DEFAULT '',                    -- cron: "0 7 * * *" (UTC)
    path        TEXT NOT NULL DEFAULT '/run',                -- path the agent receives the trigger on
    secret      TEXT NOT NULL DEFAULT '',                    -- webhook: callers sign with this (shown once)
    last_fired  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (app_id, kind, name)
);
ALTER TABLE app_triggers ADD COLUMN IF NOT EXISTS secret TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_app_triggers_cron ON app_triggers (kind) WHERE kind = 'cron';
