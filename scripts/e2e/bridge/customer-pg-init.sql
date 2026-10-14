-- Sample customer data ("Acme" — fake).
CREATE DATABASE logo;
\connect logo
CREATE TABLE fatura (id int PRIMARY KEY, musteri text, tutar numeric(12,2), tarih date);
CREATE TABLE stok (id int PRIMARY KEY, urun text, adet int);
INSERT INTO fatura SELECT g, 'Musteri ' || g, (g * 137.5)::numeric(12,2), date '2026-10-01' + (g % 30)
  FROM generate_series(1, 42) g;
INSERT INTO stok SELECT g, 'Urun ' || g, g * 3 FROM generate_series(1, 10) g;
