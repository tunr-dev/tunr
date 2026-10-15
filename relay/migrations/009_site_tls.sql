-- 009_site_tls.sql
--
-- Agent Bridge, P8-T1: uçtan uca TLS. Connector'ın sertifika parmak izi
-- (hex SHA-256) — portalda gösterilir ki müşteri satıcıya kanal dışından
-- iletebilsin. update.sh her deploy'da yeniden çalıştırır → idempotent.

ALTER TABLE sites ADD COLUMN IF NOT EXISTS tls_fingerprint TEXT;
