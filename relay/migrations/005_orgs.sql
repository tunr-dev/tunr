-- 005_orgs.sql
--
-- Agent Bridge, P1-T1: organizasyonlar ve satıcı ↔ müşteri ilişkisi.
--
-- Bir org satıcı (ajan sahibi), müşteri (site/kaynak sahibi) ya da ikisi
-- birden olabilir. `users` tablosuna ve mevcut kayıt akışına DOKUNULMAZ:
-- kişisel org, kullanıcı bir bridge ucuna ilk geldiğinde lazy oluşturulur
-- (db.EnsurePersonalOrg). Backfill yok.
--
-- update.sh bu dosyayı her deploy'da yeniden çalıştırır → her ifade idempotent.

CREATE TABLE IF NOT EXISTS orgs (
    id                TEXT PRIMARY KEY,                              -- "org_…"
    name              TEXT NOT NULL,
    slug              TEXT NOT NULL UNIQUE,                          -- "acme" → kaynak adresi "acme/erp-db"
    kind              TEXT NOT NULL DEFAULT 'both'
                      CHECK (kind IN ('vendor', 'customer', 'both')),
    -- Kişisel org'un sahibi; ekip org'larında NULL. UNIQUE → EnsurePersonalOrg
    -- yarışta bile kullanıcı başına tek kişisel org üretir.
    personal_user_id  UUID UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    settings          JSONB NOT NULL DEFAULT '{}'::jsonb,            -- ör. {"log_query_text": false}
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS org_members (
    org_id      TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member'
                CHECK (role IN ('owner', 'admin', 'approver', 'member')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_org_members_user ON org_members (user_id);

-- Satıcı ↔ müşteri ilişkisi. Müşteri oluşturur; satıcı ancak bundan sonra
-- o müşterinin kaynaklarını görebilir ve grant isteyebilir.
CREATE TABLE IF NOT EXISTS links (
    id               TEXT PRIMARY KEY,                               -- "lnk_…"
    vendor_org_id    TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    customer_org_id  TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'revoked')),
    created_by       UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at       TIMESTAMPTZ,
    UNIQUE (vendor_org_id, customer_org_id),
    CHECK (vendor_org_id <> customer_org_id)
);
CREATE INDEX IF NOT EXISTS idx_links_customer ON links (customer_org_id);

-- Denetim: mevcut audit_log org kapsamına alınır (ayrı audit_events tablosu yok).
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS org_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_org ON audit_log (org_id, created_at DESC) WHERE org_id IS NOT NULL;
