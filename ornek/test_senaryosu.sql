-- Test senaryosu: bilinen silme/rollback/sayaç atlatma olayları
SET SESSION cte_max_recursion_depth = 1000000;
CREATE DATABASE arsiv;
CREATE TABLE arsiv.islem (id BIGINT AUTO_INCREMENT PRIMARY KEY, created_at DATETIME NOT NULL, tutar INT, aciklama VARCHAR(60));

-- [binlog.000001..2] İlk yükleme: 1..100000, kayıt başına 10 sn
FLUSH BINARY LOGS;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 100000)
SELECT TIMESTAMP('2024-01-01') + INTERVAL n*10 SECOND, n % 1000, CONCAT('kayit ', n) FROM s;

-- Rollback: 5000 ID yanar, zaman akışı kesilmez  → beklenen K6 (binlog dışı analizde)
START TRANSACTION;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 5000)
SELECT NOW(), 0, 'geri alinacak' FROM s;
ROLLBACK;

SELECT MAX(created_at) INTO @t FROM arsiv.islem;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 100000)
SELECT @t + INTERVAL n*10 SECOND, n % 1000, CONCAT('kayit2 ', n) FROM s;

-- [binlog.000003] Buradan sonrası analiz edilen binlog'lar
FLUSH BINARY LOGS;

-- Eski yedek (2023) anlık görüntüsü
CREATE DATABASE y2023;
CREATE TABLE y2023.islem LIKE arsiv.islem;
INSERT INTO y2023.islem SELECT * FROM arsiv.islem;

-- Yedek sonrası binlog'lu silme → K1 (yedek farkı, binlog'da da var)
DELETE FROM arsiv.islem WHERE id BETWEEN 50001 AND 50500;

-- Yedek sonrası binlog'suz silme, eski kayıtlar → K1 (yedekte var)
SET SESSION sql_log_bin = 0;
DELETE FROM arsiv.islem WHERE id BETWEEN 70001 AND 70300;
SET SESSION sql_log_bin = 1;

-- Kapsam içinde rollback → K5
START TRANSACTION;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 1000)
SELECT NOW(), 0, 'geri alinacak 2' FROM s;
ROLLBACK;

SELECT MAX(created_at) INTO @t FROM arsiv.islem;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 2000)
SELECT @t + INTERVAL n*10 SECOND, n % 1000, CONCAT('kayit3 ', n) FROM s;

FLUSH BINARY LOGS;
-- [binlog.000004] Sayaç elle atlatma → K4
ALTER TABLE arsiv.islem AUTO_INCREMENT = 400000;
SELECT MAX(created_at) INTO @t FROM arsiv.islem;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 3000)
SELECT @t + INTERVAL n*10 SECOND, n % 1000, CONCAT('kayit4 ', n) FROM s;

-- Komşuları da binlog'da olan rollback → K5
START TRANSACTION;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 700)
SELECT NOW(), 0, 'geri alinacak 3' FROM s;
ROLLBACK;
SELECT MAX(created_at) INTO @t FROM arsiv.islem;
INSERT INTO arsiv.islem (created_at, tutar, aciklama)
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 500)
SELECT @t + INTERVAL n*10 SECOND, n % 1000, CONCAT('kayit5 ', n) FROM s;

-- Yedekte olmayan yeni kayıtlar: binlog'lu silme → K2, binlog'suz silme → K3
DELETE FROM arsiv.islem WHERE id BETWEEN 400101 AND 400150;
UPDATE arsiv.islem SET tutar = 999999 WHERE id = 400500;
SET SESSION sql_log_bin = 0;
DELETE FROM arsiv.islem WHERE id BETWEEN 401001 AND 401200;
SET SESSION sql_log_bin = 1;
FLUSH BINARY LOGS;
