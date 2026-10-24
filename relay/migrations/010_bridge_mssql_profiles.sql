-- 010_bridge_mssql_profiles.sql
--
-- Agent Bridge, rakip özellikleri (docs/internal/bridge/04-rakip-ozellik-plani.md):
--   1. mssql kaynak türü (connector sorguyu salt okunur login'le kendisi çalıştırır)
--   2. kaynak profili: hazır ERP şablonunun adı + müşterinin veritabanında
--      doğrulanmış tablo adları (connector duyurur; relay yalnızca saklar)
--   3. "kimin adına": grant'i isteyen kişi (ajan isteğinde token'ı basan kişi)
--   4. satıcının müşteriye gönderdiği tek kullanımlık bağlantı daveti
--
-- update.sh bu dosyayı her deploy'da yeniden çalıştırır → her ifade idempotent.

ALTER TABLE resources DROP CONSTRAINT IF EXISTS resources_kind_check;
ALTER TABLE resources ADD CONSTRAINT resources_kind_check
    CHECK (kind IN ('postgres', 'mssql', 'tcp', 'http'));

ALTER TABLE resources ADD COLUMN IF NOT EXISTS profile             TEXT;     -- "nebim-v3"
ALTER TABLE resources ADD COLUMN IF NOT EXISTS profile_tables      TEXT[];   -- doğrulanmış, gerçekte var olan tablolar
ALTER TABLE resources ADD COLUMN IF NOT EXISTS profile_verified_at TIMESTAMPTZ;

-- Grant'i isteyen insan. Kişi satıcı org'undan çıkarsa grant kullanılamaz
-- (FindActiveGrant). requested_by_set, eski grant'leri (kişi hiç kaydedilmedi)
-- silinmiş kullanıcıdan ayırır: kullanıcı silinince requested_by NULL olur ama
-- requested_by_set true kalır ve grant yine kullanılamaz.
ALTER TABLE grants ADD COLUMN IF NOT EXISTS requested_by UUID REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE grants ADD COLUMN IF NOT EXISTS requested_by_set BOOLEAN NOT NULL DEFAULT false;

-- Bağlantı daveti: satıcı → müşteri. Token yüksek entropili; yalnız özeti saklanır.
-- Davet bir yetki vermez: kabul eden müşteri org'u satıcıyı bağlar ve bir site
-- açar; grant'ler yine tek tek onaylanır.
CREATE TABLE IF NOT EXISTS bridge_invites (
    id              TEXT PRIMARY KEY,                               -- "inv_…"
    vendor_org_id   TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    customer_hint   TEXT,                                           -- "Kuzucular Yapı" (satıcının notu)
    note            TEXT,                                           -- müşteriye gösterilen kısa mesaj
    created_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    accepted_at     TIMESTAMPTZ,
    accepted_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    customer_org_id TEXT REFERENCES orgs(id) ON DELETE SET NULL,
    revoked_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_bridge_invites_vendor ON bridge_invites (vendor_org_id, created_at DESC);
