-- 006_bridge.sql
--
-- Agent Bridge, P1-T2: site, kaynak, ajan, token, grant ve erişim kayıtları.
--
-- Kiracı izolasyonu kuralı: her satır bir org'a bağlanır ve DB katmanındaki
-- her okuma org_id ile süzülür (relay/internal/db/bridge.go).
--
-- update.sh bu dosyayı her deploy'da yeniden çalıştırır → her ifade idempotent.

-- Site: müşterinin bir ağ konumu; tek bir connector'a karşılık gelir.
CREATE TABLE IF NOT EXISTS sites (
    id              TEXT PRIMARY KEY,                               -- "site_…"
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,                                  -- "istanbul-ofis"
    token_hash      TEXT NOT NULL UNIQUE,                           -- site token'ının sha256 özeti (token yüksek entropili)
    status          TEXT NOT NULL DEFAULT 'offline'
                    CHECK (status IN ('online', 'offline', 'disabled')),
    last_seen_at    TIMESTAMPTZ,
    connector_ver   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

-- Kaynak: site içinde adlandırılmış hedef. Hedef ADRES connector yapılandırmasında
-- durur; relay yalnızca adı ve türü bilir.
CREATE TABLE IF NOT EXISTS resources (
    id              TEXT PRIMARY KEY,                               -- "res_…"
    site_id         TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,                                  -- "erp-db"
    kind            TEXT NOT NULL CHECK (kind IN ('postgres', 'tcp', 'http')),
    description     TEXT,
    auto_approve    JSONB,                                          -- ör. {"vendor_org_id":"org_x","mode":"ro","max_ttl":"8h"}
    limits          JSONB NOT NULL DEFAULT '{}'::jsonb,             -- {"max_rows":10000,"max_bytes":52428800,"qps":5}
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'offline', 'disabled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE INDEX IF NOT EXISTS idx_resources_site ON resources (site_id);

-- Ajan: satıcı org'una ait kimlik. external = dışarıda çalışır, hosted = tunr runner'da.
CREATE TABLE IF NOT EXISTS agents (
    id              TEXT PRIMARY KEY,                               -- "agt_…"
    org_id          TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,                                  -- "invoice-reconciler"
    kind            TEXT NOT NULL DEFAULT 'external'
                    CHECK (kind IN ('external', 'hosted')),
    app_id          TEXT REFERENCES apps(id) ON DELETE SET NULL,    -- hosted ise
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'disabled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);

-- Ajana verilmiş token'lar (JWT jti). İptal ve döndürme için ayrı tablo.
CREATE TABLE IF NOT EXISTS agent_tokens (
    id              TEXT PRIMARY KEY,                               -- "atk_…"
    agent_id        TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    created_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_tokens_agent ON agent_tokens (agent_id);

-- Grant: ajan × kaynak × mod × süre. Müşteri onaylar.
-- Durum makinesi (P4): pending → approved | denied; approved → revoked | expired.
CREATE TABLE IF NOT EXISTS grants (
    id                   TEXT PRIMARY KEY,                          -- "grt_…"
    agent_id             TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_id          TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    vendor_org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    customer_org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    mode                 TEXT NOT NULL DEFAULT 'ro' CHECK (mode IN ('ro', 'rw')),
    status               TEXT NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'approved', 'denied', 'revoked', 'expired')),
    reason               TEXT,                                      -- ajanın/satıcının gerekçesi; müşteri okur
    requested_ttl_secs   INT NOT NULL CHECK (requested_ttl_secs > 0),
    approved_by          UUID REFERENCES users(id) ON DELETE SET NULL,
    second_approver      UUID REFERENCES users(id) ON DELETE SET NULL, -- rw için
    approved_at          TIMESTAMPTZ,
    expires_at           TIMESTAMPTZ,
    limits               JSONB NOT NULL DEFAULT '{}'::jsonb,        -- kaynak limitlerini daraltabilir, genişletemez
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_grants_active   ON grants (agent_id, resource_id) WHERE status = 'approved';
CREATE INDEX IF NOT EXISTS idx_grants_expiry   ON grants (expires_at) WHERE status = 'approved';
CREATE INDEX IF NOT EXISTS idx_grants_customer ON grants (customer_org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_grants_vendor   ON grants (vendor_org_id, created_at DESC);

-- Erişim oturumu: bir grant altında açılmış somut akış.
CREATE TABLE IF NOT EXISTS access_sessions (
    id              TEXT PRIMARY KEY,                               -- "acs_…" (grant JWT jti)
    grant_id        TEXT NOT NULL REFERENCES grants(id) ON DELETE CASCADE,
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at       TIMESTAMPTZ,
    close_reason    TEXT,                                           -- client | expired | revoked | killed | invalid_grant | error
    bytes_up        BIGINT NOT NULL DEFAULT 0,
    bytes_down      BIGINT NOT NULL DEFAULT 0,
    query_count     INT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_access_sessions_grant ON access_sessions (grant_id, opened_at DESC);

-- Sorgu özetleri. Sorgu METNİ varsayılan olarak yok; yalnızca müşteri org ayarı açıksa.
CREATE TABLE IF NOT EXISTS access_queries (
    id              BIGSERIAL PRIMARY KEY,
    session_id      TEXT NOT NULL REFERENCES access_sessions(id) ON DELETE CASCADE,
    command         TEXT NOT NULL,                                  -- SELECT | SHOW | blocked:INSERT …
    tables          TEXT[],
    row_count       INT,
    duration_ms     INT,
    blocked         BOOLEAN NOT NULL DEFAULT false,
    query_text      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_access_queries_session ON access_queries (session_id, created_at);

-- Grant değişince relay önbelleği ve açık akışlar için bildirim (route'lardaki
-- LISTEN/NOTIFY deseniyle aynı).
CREATE OR REPLACE FUNCTION notify_grants_changed() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('grants_changed', COALESCE(NEW.id, OLD.id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS grants_changed_trg ON grants;
CREATE TRIGGER grants_changed_trg AFTER INSERT OR UPDATE OR DELETE ON grants
    FOR EACH ROW EXECUTE FUNCTION notify_grants_changed();
