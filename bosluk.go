package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// yerelPencere: boşluk öncesindeki kaç satırdan yerel kayıt hızı hesaplanacağı.
const yerelPencere = 1000

// bosluk, tabloyu ID sırasıyla tek geçişte okur ve boşlukları aralık olarak yazar.
// Her boşluk için komşu kayıtların zamanı ve boşluk öncesindeki yerel kayıt hızı da
// kaydedilir; rapor bu ikisinden "bu kadar ID bu sürede oluşabilir miydi" sorusunu sorar.
func bosluk(args []string) error {
	fsx := flag.NewFlagSet("bosluk", flag.ExitOnError)
	dsn := fsx.String("dsn", "", `MySQL bağlantısı, ör. "root:sifre@tcp(127.0.0.1:3306)/"`)
	tablo := fsx.String("tablo", "", "incelenecek tablo (db.tablo)")
	idSutun := fsx.String("id", "id", "AUTO_INCREMENT sütunu")
	zamanSutun := fsx.String("zaman", "", "kayıt zamanı sütunu (ör. created_at); boşsa zaman analizi yapılmaz")
	tz := fsx.String("tz", "Europe/Istanbul", "DATETIME sütunlarının saat dilimi")
	etiket := fsx.String("etiket", "guncel", "bu yedeğin adı (ör. 2023); karşılaştırmada kullanılır")
	parca := fsx.Int("parca", 500000, "her sorguda okunacak satır")
	mod := fsx.String("mod", "otomatik", "otomatik | sirali (ID birincil anahtarsa) | tarama (index'ten bağımsız, 2 tam tarama)")
	girdi := fsx.String("girdi", "", `DSN yerine dosyadan oku: satır başına "id" ya da "id<TAB>zaman" (sıralı); "-" = stdin`)
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz bosluk --dsn DSN --tablo db.tablo [--zaman created_at] [--etiket 2024]")
		fmt.Fprintln(os.Stderr, "Tabloyu ID sırasıyla okur, boşlukları bosluklar_<etiket>.tsv dosyasına yazar.")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if *tablo == "" && *girdi == "" {
		fsx.Usage()
		return fmt.Errorf("--tablo gerekli")
	}
	if *dsn == "" && *girdi == "" {
		fsx.Usage()
		return fmt.Errorf("--dsn ya da --girdi gerekli")
	}
	loc, err := konumYukle(*tz)
	if err != nil {
		return err
	}
	if err := klasorHazirla(*klasor); err != nil {
		return err
	}

	tarayici := &boslukTarayici{loc: loc}
	var kaynak string
	if *girdi != "" {
		kaynak = "dosya:" + *girdi
		err = tarayici.dosyadan(*girdi)
	} else {
		kaynak, err = tarayici.mysqldan(*dsn, *tablo, *idSutun, *zamanSutun, *parca, *mod)
	}
	if err != nil {
		return err
	}

	cikti := filepath.Join(*klasor, "bosluklar_"+*etiket+".tsv")
	if err := tarayici.yaz(cikti, *tablo, *etiket, kaynak, *zamanSutun, *tz); err != nil {
		return err
	}
	ozet := fmt.Sprintf("%s satır, %s boşluk aralığı, %s eksik ID",
		sayiYaz(tarayici.satir), sayiYaz(int64(len(tarayici.bosluklar))), sayiYaz(tarayici.eksik))
	gunlukYaz(*klasor, ozet)
	fmt.Printf("\n%s\n→ %s\n", ozet, cikti)
	return nil
}

type boslukKaydi struct {
	aralik
	onceki, sonraki int64   // komşu kayıtların zamanı (unix), yoksa 0
	yerelHiz        float64 // boşluk öncesi saniyede kayıt, bilinmiyorsa 0
}

type boslukTarayici struct {
	loc        *time.Location
	min, max   int64
	satir      int64
	eksik      int64
	ilkZaman   int64
	sonZaman   int64
	bosluklar  []boslukKaydi
	oncekiID   int64
	oncekiZ    int64
	basladi    bool
	halka      [yerelPencere]int64 // son satırların zamanları
	halkaN     int
	siraliDegil bool
}

// ekle akıştaki bir sonraki satırı işler. zaman 0 ise bilinmiyor demektir.
func (t *boslukTarayici) ekle(id, zaman int64) error {
	if t.basladi {
		if id <= t.oncekiID {
			return fmt.Errorf("ID sırası bozuk: %d, %d'den sonra geldi (girdi ID'ye göre sıralı olmalı)", id, t.oncekiID)
		}
		if id != t.oncekiID+1 {
			b := boslukKaydi{aralik: aralik{t.oncekiID + 1, id - 1}}
			if zaman > 0 && t.oncekiZ > 0 {
				b.onceki, b.sonraki = t.oncekiZ, zaman
			}
			b.yerelHiz = t.yerelHiz()
			t.bosluklar = append(t.bosluklar, b)
			t.eksik += b.adet()
		}
	} else {
		t.min = id
		t.basladi = true
	}
	t.oncekiID, t.oncekiZ = id, zaman
	t.max = id
	t.satir++
	if zaman > 0 {
		if t.ilkZaman == 0 || zaman < t.ilkZaman {
			t.ilkZaman = zaman
		}
		if zaman > t.sonZaman {
			t.sonZaman = zaman
		}
		t.halka[t.halkaN%yerelPencere] = zaman
		t.halkaN++
	}
	return nil
}

// yerelHiz, son yerelPencere satırın ne kadar sürede eklendiğinden saniyede kayıt hızını bulur.
func (t *boslukTarayici) yerelHiz() float64 {
	n := t.halkaN
	if n < 2 {
		return 0
	}
	k := n
	if k > yerelPencere {
		k = yerelPencere
	}
	enEski := t.halka[(n-k)%yerelPencere]
	enYeni := t.halka[(n-1)%yerelPencere]
	dt := enYeni - enEski
	if dt < 1 {
		dt = 1
	}
	return float64(k-1) / float64(dt)
}

func (t *boslukTarayici) mysqldan(dsn, tablo, idSutun, zamanSutun string, parca int, mod string) (string, error) {
	// Salt-okuma oturumu; TIMESTAMP değerleri UTC gelir.
	conn, cfg, kapat, err := baglan(dsn)
	if err != nil {
		return "", err
	}
	defer kapat()
	ctx := context.Background()
	bilgi, err := tabloBilgisiAl(conn, tablo, idSutun)
	if err != nil {
		return "", err
	}
	if mod == "otomatik" {
		mod = "tarama"
		if bilgi.idPKBasi {
			mod = "sirali"
		}
	}
	kaynak := fmt.Sprintf("mysql://%s@%s (%s) mod=%s", cfg.User, cfg.Addr, bilgi.surum, mod)

	tq, idq := tirnakla(tablo), sutunTirnak(idSutun)
	var mn, mx sql.NullInt64
	fmt.Fprintf(os.Stderr, "MIN/MAX(%s) okunuyor...\n", idSutun)
	if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s", idq, idq, tq)).Scan(&mn, &mx); err != nil {
		return "", err
	}
	if !mn.Valid {
		return "", fmt.Errorf("tablo boş")
	}
	fmt.Fprintf(os.Stderr, "Aralık: %s – %s, mod: %s\n", sayiYaz(mn.Int64), sayiYaz(mx.Int64), mod)
	if mod == "tarama" {
		zt, err := t.tarama(conn, tablo, idSutun, zamanSutun, mn.Int64, mx.Int64, bilgi.satirTahmin)
		if zamanSutun != "" {
			kaynak += " zaman_turu=" + zt
		}
		return kaynak, err
	}

	secim := idq
	if zamanSutun != "" {
		secim += ", " + sutunTirnak(zamanSutun)
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s > ? ORDER BY %s LIMIT ?", secim, tq, idq, idq)
	stmt, err := conn.PrepareContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer stmt.Close()

	basla := time.Now()
	sonRapor := time.Time{}
	son := mn.Int64 - 1
	zamanTuru := ""
	for {
		rows, err := stmt.QueryContext(ctx, son, parca)
		if err != nil {
			return "", err
		}
		if zamanSutun != "" && zamanTuru == "" {
			if ct, err := rows.ColumnTypes(); err == nil && len(ct) > 1 {
				zamanTuru = strings.ToUpper(ct[1].DatabaseTypeName())
			}
		}
		n := 0
		var id int64
		var zb sql.RawBytes
		for rows.Next() {
			var z int64
			if zamanSutun != "" {
				if err := rows.Scan(&id, &zb); err != nil {
					rows.Close()
					return "", err
				}
				z = zamanCoz(zb, zamanTuru, t.loc)
			} else if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", err
			}
			if err := t.ekle(id, z); err != nil {
				rows.Close()
				return "", err
			}
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", err
		}
		rows.Close()
		if n == 0 {
			break
		}
		son = id
		if time.Since(sonRapor) > 2*time.Second || id >= mx.Int64 {
			ilerlemeYaz(basla, mn.Int64, mx.Int64, id, t.satir, t.eksik)
			sonRapor = time.Now()
		}
		if n < parca {
			break
		}
	}
	fmt.Fprintln(os.Stderr)
	if zamanSutun != "" {
		kaynak += fmt.Sprintf(" zaman_turu=%s", zamanTuru)
	}
	return kaynak, nil
}

func ilerlemeYaz(basla time.Time, mn, mx, id, satir, eksik int64) {
	oran := 1.0
	if mx > mn {
		oran = float64(id-mn) / float64(mx-mn)
	}
	gecen := time.Since(basla)
	kalan := time.Duration(0)
	if oran > 0 {
		kalan = time.Duration(float64(gecen) * (1 - oran) / oran)
	}
	hiz := float64(satir) / gecen.Seconds()
	fmt.Fprintf(os.Stderr, "\r  %%%5.1f | ID %s | %s satır/sn | eksik %s | kalan ~%s      ",
		oran*100, sayiYaz(id), sayiYaz(int64(hiz)), sayiYaz(eksik), kalan.Round(time.Second))
}

func (t *boslukTarayici) dosyadan(yol string) error {
	var r io.Reader = os.Stdin
	if yol != "-" {
		f, err := os.Open(yol)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == "id" {
			continue
		}
		idStr, zStr, _ := strings.Cut(line, "\t")
		id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		if err != nil {
			continue // başlık vb.
		}
		var z int64
		if zStr != "" {
			z = zamanCoz([]byte(strings.TrimSpace(zStr)), "", t.loc)
		}
		if err := t.ekle(id, z); err != nil {
			return err
		}
	}
	return sc.Err()
}

// zamanCoz bir zaman hücresini unix saniyeye çevirir. TIMESTAMP oturumda UTC gelir,
// DATETIME saat dilimsizdir (loc ile yorumlanır), tam sayılar epoch kabul edilir.
func zamanCoz(b []byte, tur string, loc *time.Location) int64 {
	s := strings.TrimSpace(string(b))
	if s == "" || strings.HasPrefix(s, "0000-00-00") {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e11 { // milisaniye
			n /= 1000
		}
		return n
	}
	l := loc
	if tur == "TIMESTAMP" {
		l = time.UTC
	}
	for _, f := range []string{"2006-01-02 15:04:05.999999", "2006-01-02T15:04:05Z07:00", "2006-01-02"} {
		if tm, err := time.ParseInLocation(f, s, l); err == nil {
			return tm.Unix()
		}
	}
	return 0
}

func (t *boslukTarayici) yaz(yol, tablo, etiket, kaynak, zamanSutun, tz string) error {
	f, err := os.Create(yol)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintf(w, "# dbiz=%s\n# tur=bosluk\n# tablo=%s\n# etiket=%s\n# kaynak=%s\n", surum, tablo, etiket, kaynak)
	fmt.Fprintf(w, "# zaman_sutunu=%s\n# tz=%s\n# min=%d\n# max=%d\n# satir=%d\n# eksik=%d\n# ilk_zaman=%d\n# son_zaman=%d\n# olusturma_utc=%s\n",
		zamanSutun, tz, t.min, t.max, t.satir, t.eksik, t.ilkZaman, t.sonZaman, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "bas\tson\tadet\tonceki_zaman\tsonraki_zaman\tsure_sn\tyerel_hiz")
	for _, b := range t.bosluklar {
		sure := int64(-1)
		if b.onceki > 0 && b.sonraki > 0 {
			sure = b.sonraki - b.onceki
		}
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t%.4f\n", b.bas, b.son, b.adet(), b.onceki, b.sonraki, sure, b.yerelHiz)
	}
	return w.Flush()
}
