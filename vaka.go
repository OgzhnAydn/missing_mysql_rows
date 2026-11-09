package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ornekAyar = `# dbiz vaka ayarı — yalnızca bu dosyayı düzenleyip kaydedin, sonra BASLAT.bat'a çift tıklayın.
# Satır başında # olanlar açıklamadır, dikkate alınmaz.

# Yedeklerin yüklendiği MySQL sunucusu (yedeğin KOPYASI; orijinal datadir üzerinde MySQL açmayın)
dsn = root:SIFRE@tcp(127.0.0.1:3306)/

# Yedekler: "yedek ETİKET = veritabanı.tablo", ESKİDEN YENİYE sırayla.
# En alttaki, incelenen (en yeni) yedektir. Tek yedek de olur.
yedek 2023 = yedek2023.tablo_adi
yedek 2024 = yedek2024.tablo_adi

# AUTO_INCREMENT sütunu ve kayıt zamanı sütunu (bilmiyorsanız önce: dbiz vaka --ayar vaka.txt --kontrol)
id    = id
zaman = created_at
tz    = Europe/Istanbul

# Binlog dosyaları (desen kullanılabilir). Yoksa satırı boş bırakın.
binlog = D:\delil\binlog\mysql-bin.0*
# Binlog'da tablonun ORİJİNAL adı (canlı sistemdeki veritabanı.tablo; yedeğin yüklendiği ad değil)
binlog_tablo = canli_veritabani.tablo_adi
# ID'nin tablodaki sütun sırası; boşsa en yeni yedekten otomatik bulunur
binlog_id_sutun =

# Mühürlenecek orijinal delil dosya/klasörleri (birden çok satır olabilir).
# Kullanmak için baştaki # işaretini silip yolu yazın.
# muhur = D:\delil\binlog

# Çıktı klasörü (boş bırakılırsa bu klasörün içinde "sonuc" klasörü açılır) ve rapor başlığı
klasor = sonuc
baslik = Vaka adı / dosya no
`

type vakaAyar struct {
	dsn, id, zaman, tz, klasor, baslik string
	binlog, binlogTablo, binlogIDSutun  string
	yedekler                            [][2]string // etiket, tablo
	muhur                               []string
}

func vakaAyarOku(yol string) (*vakaAyar, error) {
	f, err := os.Open(yol)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a := &vakaAyar{id: "id", tz: "Europe/Istanbul", klasor: "sonuc"}
	sc := bufio.NewScanner(f)
	no := 0
	for sc.Scan() {
		no++
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\uFEFF"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: '=' yok: %s", yol, no, line)
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if strings.HasPrefix(k, "yedek ") {
			et := strings.TrimSpace(k[len("yedek "):])
			if et == "" || v == "" {
				return nil, fmt.Errorf("%s:%d: 'yedek ETİKET = db.tablo' biçiminde olmalı", yol, no)
			}
			a.yedekler = append(a.yedekler, [2]string{et, v})
			continue
		}
		switch k {
		case "dsn":
			a.dsn = v
		case "id":
			a.id = v
		case "zaman":
			a.zaman = v
		case "tz":
			a.tz = v
		case "klasor":
			if v != "" {
				a.klasor = v
			}
		case "baslik":
			a.baslik = v
		case "binlog":
			a.binlog = v
		case "binlog_tablo":
			a.binlogTablo = v
		case "binlog_id_sutun":
			a.binlogIDSutun = v
		case "muhur":
			if v != "" {
				a.muhur = append(a.muhur, v)
			}
		default:
			return nil, fmt.Errorf("%s:%d: bilinmeyen ayar %q", yol, no, k)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if a.dsn == "" || strings.Contains(a.dsn, "SIFRE") {
		return nil, fmt.Errorf("ayar dosyasında dsn düzenlenmemiş")
	}
	if len(a.yedekler) == 0 {
		return nil, fmt.Errorf("ayar dosyasında en az bir 'yedek ETİKET = db.tablo' satırı olmalı")
	}
	return a, nil
}

// vaka, ayar dosyasına göre tüm adımları sırayla çalıştırır. Uzun süren boşluk
// taramaları tamamlanmışsa yeniden yapılmaz (--yeniden ile zorlanır).
func vaka(args []string) error {
	fsx := flag.NewFlagSet("vaka", flag.ExitOnError)
	ayarYol := fsx.String("ayar", "vaka.txt", "ayar dosyası")
	sadeceKontrol := fsx.Bool("kontrol", false, "yalnızca tabloları kontrol et, tarama yapma")
	ornek := fsx.Bool("ornek", false, "örnek ayar dosyası oluştur")
	yeniden := fsx.Bool("yeniden", false, "tamamlanmış boşluk taramalarını da yeniden yap")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz vaka --ornek                 örnek vaka.txt oluşturur")
		fmt.Fprintln(os.Stderr, "dbiz vaka --ayar vaka.txt --kontrol   tabloları tanır, süre tahmini verir")
		fmt.Fprintln(os.Stderr, "dbiz vaka --ayar vaka.txt          tüm incelemeyi çalıştırır")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)

	if *ornek {
		if _, err := os.Stat(*ayarYol); err == nil {
			return fmt.Errorf("%s zaten var; üzerine yazılmadı", *ayarYol)
		}
		if err := os.WriteFile(*ayarYol, []byte(ornekAyar), 0o644); err != nil {
			return err
		}
		fmt.Printf("%s oluşturuldu. Düzenleyip şunu çalıştırın:\n  dbiz vaka --ayar %s --kontrol\n", *ayarYol, *ayarYol)
		return nil
	}

	a, err := vakaAyarOku(*ayarYol)
	if err != nil {
		return err
	}
	if err := klasorHazirla(a.klasor); err != nil {
		return err
	}
	baslangic := time.Now()
	adim := 0
	baslik := func(s string) {
		adim++
		fmt.Printf("\n══ %d. %s ══ (%s)\n", adim, s, time.Since(baslangic).Round(time.Second))
	}

	// Kontrol: her yedek tablosu erişilebilir mi, ID/zaman sütunları var mı.
	conn, _, kapat, err := baglan(a.dsn)
	if err != nil {
		return err
	}
	var enYeni *tabloBilgi
	for _, y := range a.yedekler {
		baslik("Kontrol: " + y[0] + " (" + y[1] + ")")
		b, err := tabloBilgisiAl(conn, y[1], a.id)
		if err != nil {
			kapat()
			return err
		}
		if a.zaman != "" && !sutunVar(b, a.zaman) {
			kapat()
			return fmt.Errorf("%s tablosunda zaman sütunu %q yok (adaylar: %s)", y[1], a.zaman, strings.Join(b.zamanAdaylari, ", "))
		}
		tabloBilgisiYaz(y[1], a.id, b)
		if *sadeceKontrol {
			hizTesti(conn, y[1], a.id, b)
		}
		enYeni = b
	}
	kapat()

	// Yol kontrolleri: uzun taramalardan sonra değil, en başta hata versin.
	if a.binlog != "" {
		baslik("Kontrol: binlog dosyaları")
		var bulunan []string
		for _, d := range strings.Fields(a.binlog) {
			m, _ := filepath.Glob(d)
			bulunan = append(bulunan, m...)
		}
		if len(bulunan) == 0 {
			return fmt.Errorf("binlog dosyası bulunamadı: %s (yolu düzeltin ya da binlog satırını boş bırakın)", a.binlog)
		}
		fmt.Printf("%d dosya bulundu: %s … %s\n", len(bulunan), filepath.Base(bulunan[0]), filepath.Base(bulunan[len(bulunan)-1]))
		if a.binlogTablo == "" {
			fmt.Println("! binlog_tablo boş: en yeni yedeğin adı kullanılacak; canlı sistemdeki ad farklıysa binlog sonucu boş çıkar.")
		}
	}
	for _, m := range a.muhur {
		if g, _ := filepath.Glob(m); len(g) == 0 {
			return fmt.Errorf("mühürlenecek yol bulunamadı: %s", m)
		}
	}
	if *sadeceKontrol {
		fmt.Println("\nKontrol tamam. Tüm inceleme için:  dbiz vaka --ayar", *ayarYol)
		return nil
	}
	gunlukYaz(a.klasor, "vaka başladı: "+*ayarYol)

	k := []string{"--klasor", a.klasor}
	calistir := func(ad string, fn func([]string) error, argv ...string) error {
		if err := fn(append(append([]string{}, k...), argv...)); err != nil {
			return fmt.Errorf("%s: %w", ad, err)
		}
		return nil
	}

	if len(a.muhur) > 0 {
		baslik("Orijinal dosyaları mühürleme")
		if err := calistir("muhurle", muhurle, a.muhur...); err != nil {
			return err
		}
	}

	for _, y := range a.yedekler {
		baslik("Boşluk taraması: " + y[0])
		hedef := filepath.Join(a.klasor, "bosluklar_"+y[0]+".tsv")
		if _, err := os.Stat(hedef); err == nil && !*yeniden {
			fmt.Printf("%s zaten var, atlandı (yeniden taramak için --yeniden)\n", hedef)
			continue
		}
		argv := []string{"--dsn", a.dsn, "--tablo", y[1], "--id", a.id, "--tz", a.tz, "--etiket", y[0]}
		if a.zaman != "" {
			argv = append(argv, "--zaman", a.zaman)
		}
		if err := calistir("bosluk", bosluk, argv...); err != nil {
			return err
		}
	}

	if a.binlog != "" {
		baslik("Binlog ayrıştırma")
		bt := a.binlogTablo
		if bt == "" {
			bt = a.yedekler[len(a.yedekler)-1][1]
			fmt.Printf("binlog_tablo verilmedi, %s kullanılıyor (binlog'da adı farklıysa sonuç boş çıkar)\n", bt)
		}
		sira := a.binlogIDSutun
		if sira == "" {
			sira = strconv.Itoa(enYeni.idSira)
		}
		desenler := strings.Fields(a.binlog)
		if err := calistir("binlog", binlogKomut, append([]string{"--tablo", bt, "--id-sutun", sira}, desenler...)...); err != nil {
			return err
		}
	}

	son := a.yedekler[len(a.yedekler)-1]
	for _, y := range a.yedekler[:len(a.yedekler)-1] {
		baslik("Yedek karşılaştırma: " + y[0] + " → " + son[0])
		if err := calistir("karsilastir", karsilastir, "--eski", y[0], "--yeni", son[0]); err != nil {
			return err
		}
		silYol := filepath.Join(a.klasor, "silinen_"+y[0]+"_"+son[0]+".tsv")
		if t, err := tsvOku(silYol); err == nil && atoi64(t.meta["adet"]) > 0 {
			baslik("Kurtarma: " + y[0] + " yedeğinden")
			if err := calistir("kurtar", kurtar, "--dsn", a.dsn, "--tablo", y[1], "--id", a.id, "--aralik", silYol); err != nil {
				return err
			}
		}
	}

	baslik("Rapor")
	argv := []string{"--etiket", son[0], "--tz", a.tz}
	if a.baslik != "" {
		argv = append(argv, "--baslik", a.baslik)
	}
	if err := calistir("rapor", rapor, argv...); err != nil {
		return err
	}
	fmt.Printf("\nBitti (%s). Rapor: %s\n", time.Since(baslangic).Round(time.Second), filepath.Join(a.klasor, "rapor_"+son[0]+".html"))
	return nil
}

func sutunVar(b *tabloBilgi, ad string) bool {
	for _, s := range b.sutunlar {
		if strings.EqualFold(s.ad, ad) {
			return true
		}
	}
	return false
}
