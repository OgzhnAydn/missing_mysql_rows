package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// baglan salt-okuma, UTC oturumlu tek bir bağlantı açar.
func baglan(dsn string) (*sql.Conn, *mysql.Config, func(), error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DSN çözülemedi: %w", err)
	}
	cfg.ParseTime = false
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, nil, fmt.Errorf("bağlanılamadı: %w", err)
	}
	for _, q := range []string{"SET SESSION time_zone='+00:00'", "SET SESSION TRANSACTION READ ONLY"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			conn.Close()
			db.Close()
			return nil, nil, nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	return conn, cfg, func() { conn.Close(); db.Close() }, nil
}

type sutunBilgi struct {
	ad, tur, veriTuru, anahtar, ekstra string
	sira                               int
}

type tabloBilgi struct {
	surum, motor          string
	satirTahmin, boyut    int64
	sutunlar              []sutunBilgi
	idSira                int  // ID sütununun sırası (1'den), binlog için
	idPKBasi              bool // ID, birincil anahtarın ilk sütunu mu
	idIndeksli            bool // ID herhangi bir index'in ilk sütunu mu
	autoInc               bool
	zamanAdaylari         []string
}

func tabloBilgisiAl(conn *sql.Conn, tablo, idSutun string) (*tabloBilgi, error) {
	ctx := context.Background()
	db, t := tabloAyir(tablo)
	if db == "" {
		return nil, fmt.Errorf("tablo adı db.tablo biçiminde olmalı: %s", tablo)
	}
	b := &tabloBilgi{}
	conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&b.surum)
	var motor sql.NullString
	var satir, boyut sql.NullInt64
	err := conn.QueryRowContext(ctx, `SELECT ENGINE, TABLE_ROWS, DATA_LENGTH+INDEX_LENGTH FROM information_schema.TABLES
		WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, db, t).Scan(&motor, &satir, &boyut)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("tablo bulunamadı: %s", tablo)
	} else if err != nil {
		return nil, err
	}
	b.motor, b.satirTahmin, b.boyut = motor.String, satir.Int64, boyut.Int64

	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, ORDINAL_POSITION, COLUMN_TYPE, DATA_TYPE, COLUMN_KEY, EXTRA
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, db, t)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s sutunBilgi
		rows.Scan(&s.ad, &s.sira, &s.tur, &s.veriTuru, &s.anahtar, &s.ekstra)
		b.sutunlar = append(b.sutunlar, s)
		if strings.EqualFold(s.ad, idSutun) {
			b.idSira = s.sira
			b.autoInc = strings.Contains(strings.ToLower(s.ekstra), "auto_increment")
		}
		switch strings.ToLower(s.veriTuru) {
		case "datetime", "timestamp", "date":
			b.zamanAdaylari = append(b.zamanAdaylari, s.ad)
		}
	}
	rows.Close()
	if b.idSira == 0 {
		return nil, fmt.Errorf("%s tablosunda %q sütunu yok", tablo, idSutun)
	}

	irows, err := conn.QueryContext(ctx, `SELECT INDEX_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=? AND SEQ_IN_INDEX=1`, db, t, idSutun)
	if err != nil {
		return nil, err
	}
	for irows.Next() {
		var ad string
		irows.Scan(&ad)
		b.idIndeksli = true
		if ad == "PRIMARY" {
			b.idPKBasi = true
		}
	}
	irows.Close()
	return b, nil
}

// kontrol, taramadan önce tabloyu tanır: sütunlar, index durumu, zaman sütunu adayları
// ve kısa bir okuma testiyle tam taramanın tahmini süresi.
func kontrol(args []string) error {
	fsx := flag.NewFlagSet("kontrol", flag.ExitOnError)
	dsn := fsx.String("dsn", "", "MySQL bağlantısı")
	tablo := fsx.String("tablo", "", "tablo (db.tablo)")
	idSutun := fsx.String("id", "id", "AUTO_INCREMENT sütunu")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz kontrol --dsn DSN --tablo db.tablo")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if *dsn == "" || *tablo == "" {
		fsx.Usage()
		return fmt.Errorf("--dsn ve --tablo gerekli")
	}
	conn, _, kapat, err := baglan(*dsn)
	if err != nil {
		return err
	}
	defer kapat()
	b, err := tabloBilgisiAl(conn, *tablo, *idSutun)
	if err != nil {
		return err
	}
	tabloBilgisiYaz(*tablo, *idSutun, b)
	hizTesti(conn, *tablo, *idSutun, b)
	return nil
}

func tabloBilgisiYaz(tablo, idSutun string, b *tabloBilgi) {
	fmt.Printf("Sunucu         : MySQL %s\n", b.surum)
	fmt.Printf("Tablo          : %s (%s)\n", tablo, b.motor)
	fmt.Printf("Satır (tahmin) : ~%s   Boyut: %.1f GB\n", sayiYaz(b.satirTahmin), float64(b.boyut)/(1<<30))
	fmt.Println("Sütunlar       :")
	for _, s := range b.sutunlar {
		isaret := ""
		if strings.EqualFold(s.ad, idSutun) {
			isaret = "  ← ID"
		}
		fmt.Printf("   %2d  %-28s %-22s %s %s%s\n", s.sira, s.ad, s.tur, s.anahtar, s.ekstra, isaret)
	}
	fmt.Println()
	switch {
	case b.idPKBasi:
		fmt.Printf("✓ %s birincil anahtar: sıralı mod kullanılacak (hızlı, sıralı okuma).\n", idSutun)
	case b.idIndeksli:
		fmt.Printf("! %s birincil anahtar değil, ikincil index: tarama modu kullanılacak (2 tam tarama, sıralama/rastgele okuma yok).\n", idSutun)
		fmt.Println("  Not: 3 günlük LEFT JOIN sorgusunun yavaşlığının olası sebebi budur.")
	default:
		fmt.Printf("! %s üzerinde index yok: tarama modu kullanılacak.\n", idSutun)
	}
	if !b.autoInc {
		fmt.Printf("! %s bu kopyada AUTO_INCREMENT değil (yedekten yüklenirken düşmüş olabilir; analiz yine yapılır).\n", idSutun)
	}
	if len(b.zamanAdaylari) > 0 {
		fmt.Printf("Zaman sütunu adayları: %s   (kayıt anını tutanı 'zaman' olarak verin)\n", strings.Join(b.zamanAdaylari, ", "))
	} else {
		fmt.Println("Zaman sütunu bulunamadı: süre analizi (K6/K7) yapılamaz, diğer sınıflar çalışır.")
	}
	fmt.Printf("Binlog için ID sütun sırası: %d (binlog'daki tablo şeması aynıysa)\n", b.idSira)
}

func hizTesti(conn *sql.Conn, tablo, idSutun string, b *tabloBilgi) {
	ctx := context.Background()
	const n = 200000
	tq, iq := tirnakla(tablo), sutunTirnak(idSutun)
	var q string
	if b.idPKBasi {
		q = fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT %d", iq, tq, iq, n)
	} else {
		q = fmt.Sprintf("SELECT %s FROM %s LIMIT %d", iq, tq, n)
	}
	bas := time.Now()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		fmt.Println("Hız testi yapılamadı:", err)
		return
	}
	say := 0
	var id int64
	for rows.Next() {
		rows.Scan(&id)
		say++
	}
	rows.Close()
	gecen := time.Since(bas)
	if say == 0 {
		return
	}
	hiz := float64(say) / gecen.Seconds()
	gecis := 1.0
	if !b.idPKBasi {
		gecis = 2
	}
	tahmin := time.Duration(float64(b.satirTahmin) / hiz * gecis * float64(time.Second))
	fmt.Printf("Hız testi      : %s satır %s içinde (~%s satır/sn)\n", sayiYaz(int64(say)), gecen.Round(time.Millisecond), sayiYaz(int64(hiz)))
	yuvarla := time.Minute
	if tahmin < time.Minute {
		yuvarla = time.Second
	}
	fmt.Printf("Tahmini süre   : ~%s (ilk satırlar önbellekte olabilir; gerçek süre diske bağlı, tarama sırasında canlı görünür)\n", tahmin.Round(yuvarla))
}
