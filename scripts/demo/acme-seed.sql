-- "Acme Muhasebe" demo ERP (Logo-style schema). Every row is made up.
-- October 2026 has a few deliberate discrepancies for the reconciliation
-- demo: unpaid invoices, one partial payment and one duplicate payment.

CREATE DATABASE logo;
\connect logo

SELECT setseed(0.42);

CREATE TABLE cari (
  id        int PRIMARY KEY,
  unvan     text NOT NULL,          -- company name
  vergi_no  text NOT NULL,          -- tax number (fake)
  sehir     text NOT NULL,
  vade_gun  int  NOT NULL           -- payment terms, days
);

CREATE TABLE fatura (
  id        int PRIMARY KEY,
  fatura_no text NOT NULL UNIQUE,
  cari_id   int NOT NULL REFERENCES cari(id),
  tarih     date NOT NULL,
  vade      date NOT NULL,
  tutar     numeric(12,2) NOT NULL, -- net
  kdv       numeric(12,2) NOT NULL, -- VAT 20%
  toplam    numeric(12,2) NOT NULL
);

CREATE TABLE tahsilat (
  id        int PRIMARY KEY,
  fatura_id int NOT NULL REFERENCES fatura(id),
  tarih     date NOT NULL,
  tutar     numeric(12,2) NOT NULL,
  kanal     text NOT NULL           -- EFT, havale, kredi kartı, çek
);

CREATE TABLE stok (
  id     int PRIMARY KEY,
  kod    text NOT NULL,
  urun   text NOT NULL,
  adet   int  NOT NULL,
  birim_fiyat numeric(12,2) NOT NULL
);

-- Not granted to the vendor: the read-only role never sees this table.
CREATE TABLE bordro (
  id      int PRIMARY KEY,
  ad      text NOT NULL,
  pozisyon text NOT NULL,
  maas    numeric(12,2) NOT NULL
);

INSERT INTO cari
SELECT g,
       (ARRAY['Yılmaz','Kaya','Demir','Şahin','Çelik','Aydın','Öztürk','Arslan','Doğan','Koç'])[1 + (g % 10)]
         || ' ' ||
       (ARRAY['Tekstil','Gıda','Lojistik','Yapı','Otomotiv','Kimya','Mobilya','Elektrik'])[1 + (g * 7 % 8)]
         || (CASE WHEN g % 3 = 0 THEN ' A.Ş.' ELSE ' Ltd. Şti.' END),
       lpad((1000000000 + g * 7919)::text, 10, '0'),
       (ARRAY['İstanbul','Ankara','İzmir','Bursa','Kocaeli','Konya'])[1 + (g % 6)],
       (ARRAY[30, 45, 60])[1 + (g % 3)]
FROM generate_series(1, 40) g;

INSERT INTO fatura
SELECT g,
       'ACM2026' || lpad(g::text, 6, '0'),
       1 + (g * 13 % 40),
       d,
       d + (SELECT vade_gun FROM cari WHERE id = 1 + (g * 13 % 40)),
       net,
       round(net * 0.20, 2),
       round(net * 1.20, 2)
FROM (
  SELECT g,
         date '2026-07-01' + ((g - 1) * 123 / 620) AS d,
         round((1500 + random() * 48500)::numeric, 2) AS net
  FROM generate_series(1, 620) g
) s;

-- Payments: everything before October is paid in full; October is mostly paid.
INSERT INTO tahsilat
SELECT f.id, f.id, f.tarih + 10 + (f.id % 25), f.toplam,
       (ARRAY['EFT','Havale','Kredi kartı','Çek'])[1 + (f.id % 4)]
FROM fatura f
WHERE f.tarih < date '2026-10-01'
   OR (f.id % 9 <> 0);                      -- every 9th October invoice is unpaid

-- One partial payment and one duplicate payment in October.
UPDATE tahsilat SET tutar = round(tutar * 0.6, 2)
WHERE fatura_id = (SELECT min(id) FROM fatura WHERE tarih >= date '2026-10-01' AND id % 9 <> 0);
INSERT INTO tahsilat
SELECT 100000 + t.id, t.fatura_id, t.tarih + 2, t.tutar, 'EFT'
FROM tahsilat t
WHERE t.fatura_id = (SELECT max(id) FROM fatura WHERE tarih >= date '2026-10-01' AND id % 9 <> 0);

INSERT INTO stok
SELECT g,
       'STK-' || lpad(g::text, 4, '0'),
       (ARRAY['Pamuk iplik','Ambalaj kutusu','Etiket rulosu','Palet','Koli bandı','Streç film'])[1 + (g % 6)]
         || ' ' || (10 * g) || ' br',
       (random() * 900)::int,
       round((5 + random() * 495)::numeric, 2)
FROM generate_series(1, 60) g;

INSERT INTO bordro
SELECT g,
       'Çalışan ' || g,
       (ARRAY['Muhasebe uzmanı','Mali müşavir','Stajyer','Ofis yöneticisi'])[1 + (g % 4)],
       round((25000 + random() * 60000)::numeric, 2)
FROM generate_series(1, 18) g;

ANALYZE;
