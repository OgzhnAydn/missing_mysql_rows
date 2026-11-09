package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/term"
)

// kesfet, aracı kullanacak kişinin tablo adlarını, sütunları ve binlog yollarını
// bilmesine gerek bırakmaz: sunucudaki yedek tablolarını ve diskteki binlog
// dosyalarını bulur, vaka.txt'yi doldurur.
func kesfet(args []string) error {
	fsx := flag.NewFlagSet("kesfet", flag.ExitOnError)
	dsn := fsx.String("dsn", "", "MySQL bağlantısı (boşsa sorulur)")
	ara := fsx.String("ara", "", "binlog aranacak klasörler, ';' ile ayrılmış (boşsa tüm diskler)")
	yaz := fsx.String("yaz", "vaka.txt", "doldurulacak ayar dosyası")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz kesfet [--dsn DSN] [--ara \"D:\\delil;E:\\\"] [--yaz vaka.txt]")
		fmt.Fprintln(os.Stderr, "Yedek tablolarını, ID/zaman sütunlarını ve binlog dosyalarını bulup vaka.txt'yi doldurur.")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)

	if *dsn == "" {
		d, err := dsnSor()
		if err != nil {
			return err
		}
		*dsn = d
	}
	conn, _, kapat, err := baglan(*dsn)
	if err != nil {
		return err
	}
	defer kapat()
	ctx := context.Background()

	// 1. Tablolar
	fmt.Println("\nTablolar taranıyor...")
	tablolar, err := tablolariBul(conn)
	if err != nil {
		return err
	}
	if len(tablolar) == 0 {
		return fmt.Errorf("sunucuda ID sütunlu kullanıcı tablosu bulunamadı (yedekler bu sunucuya yüklenmiş mi?)")
	}
	gruplar := map[string][]*kesifTablo{}
	for _, t := range tablolar {
		k := strings.ToLower(t.tablo)
		gruplar[k] = append(gruplar[k], t)
	}
	// En olası grup: en çok satırlı tablo adı; birden çok veritabanında geçmesi (yedekler) öne çıkarır.
	var enIyi string
	var enIyiPuan float64
	for k, g := range gruplar {
		var satir int64
		for _, t := range g {
			satir += t.satir
		}
		puan := float64(satir)
		if len(g) > 1 {
			puan *= 2
		}
		if puan > enIyiPuan {
			enIyi, enIyiPuan = k, puan
		}
	}
	// Aynı adlı tablolardan yalnızca aynı ID sütununa sahip olanlar aynı tablonun yedekleridir.
	idSay := map[string]int{}
	for _, t := range gruplar[enIyi] {
		idSay[t.id]++
	}
	var ortakID string
	for id, n := range idSay {
		if n > idSay[ortakID] {
			ortakID = id
		}
	}
	var sec []*kesifTablo
	for _, t := range gruplar[enIyi] {
		if t.id == ortakID {
			sec = append(sec, t)
		}
	}
	for _, t := range sec {
		if t.sonID == 0 {
			var mx *int64
			conn.QueryRowContext(ctx, fmt.Sprintf("SELECT MAX(%s) FROM %s", sutunTirnak(t.id), tirnakla(t.db+"."+t.tablo))).Scan(&mx)
			if mx != nil {
				t.sonID = *mx
			}
		}
	}
	// Eskiden yeniye: daha küçük son ID daha eski yedektir.
	sort.Slice(sec, func(i, j int) bool { return sec[i].sonID < sec[j].sonID })

	sort.Slice(tablolar, func(i, j int) bool { return tablolar[i].satir > tablolar[j].satir })
	fmt.Println("\nEn büyük tablolar:")
	for i, t := range tablolar {
		if i >= 12 {
			break
		}
		isaret := ""
		for _, s := range sec {
			if s == t {
				isaret = "  ← seçildi"
			}
		}
		fmt.Printf("  %-45s ~%14s satır  ID=%s%s\n", t.db+"."+t.tablo, sayiYaz(t.satir), t.id, isaret)
	}
	etiketler := etiketUret(sec)
	fmt.Println("\nYedek olarak seçilenler (eskiden yeniye):")
	for i, t := range sec {
		fmt.Printf("  yedek %-10s = %s.%s   (son ID %s)\n", etiketler[i], t.db, t.tablo, sayiYaz(t.sonID))
	}
	zaman := zamanSec(sec[len(sec)-1].tarihler)
	fmt.Printf("ID sütunu: %s   Zaman sütunu: %s\n", sec[0].id, bosIse(zaman, "(yok)"))

	// 2. Binlog dosyaları
	var kendi string
	conn.QueryRowContext(ctx, "SELECT IFNULL(@@log_bin_basename,'')").Scan(&kendi)
	kokler := strings.FieldsFunc(*ara, func(r rune) bool { return r == ';' })
	if len(kokler) == 0 {
		kokler = diskler()
	}
	fmt.Printf("\nBinlog dosyaları aranıyor: %s\n", strings.Join(kokler, " "))
	bgruplar := binlogAra(kokler, kendi)
	var binlogDesen, binlogTablo string
	var binlogAdaylari []string
	if len(bgruplar) > 0 {
		b := bgruplar[0]
		binlogDesen = b.desen
		fmt.Printf("Seçilen: %s (%d dosya)\n", b.desen, len(b.dosyalar))
		for _, o := range bgruplar[1:] {
			fmt.Printf("  diğer: %s (%d dosya)\n", o.desen, len(o.dosyalar))
		}
		fmt.Printf("Binlog'da tablonun gerçek adı aranıyor (%s)...\n", sec[0].tablo)
		adaylar := binlogTabloBul(b.dosyalar, sec[0].tablo)
		if len(adaylar) > 0 {
			binlogTablo = adaylar[0].ad
			for i, a := range adaylar {
				isaret := ""
				if i == 0 {
					isaret = "  ← seçildi (en çok olay)"
				}
				fmt.Printf("  %-40s %s olay%s\n", a.ad, sayiYaz(a.olay), isaret)
				binlogAdaylari = append(binlogAdaylari, fmt.Sprintf("%s (%s olay)", a.ad, sayiYaz(a.olay)))
			}
			if len(adaylar) > 1 {
				fmt.Println("! Binlog'da bu adda birden çok tablo var; vaka.txt'de binlog_tablo satırını kontrol edin.")
			}
		} else {
			fmt.Println("Binlog'da bu tabloya ait olay bulunamadı; binlog_tablo elle kontrol edilmeli.")
		}
	} else {
		fmt.Println("Binlog dosyası bulunamadı. Varsa klasörünü vaka.txt'deki binlog satırına yazın.")
	}

	// 3. vaka.txt
	hedef := *yaz
	if b, err := os.ReadFile(hedef); err == nil && !strings.Contains(string(b), "SIFRE") {
		hedef = strings.TrimSuffix(hedef, filepath.Ext(hedef)) + "_onerilen.txt"
		fmt.Printf("\n%s zaten doldurulmuş; öneri %s dosyasına yazılıyor.\n", *yaz, hedef)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# dbiz vaka ayarı — 'dbiz kesfet' tarafından %s tarihinde dolduruldu.\n", time.Now().Format("2006-01-02 15:04"))
	sb.WriteString("# Lütfen kontrol edin; yanlışsa düzeltip kaydedin. # ile başlayan satırlar açıklamadır.\n\n")
	sb.WriteString("# MySQL bağlantısı\n")
	fmt.Fprintf(&sb, "dsn = %s\n\n", *dsn)
	sb.WriteString("# Yedekler, ESKİDEN YENİYE (en alttaki incelenen yedek)\n")
	for i, t := range sec {
		fmt.Fprintf(&sb, "yedek %s = %s.%s\n", etiketler[i], t.db, t.tablo)
	}
	sb.WriteString("\n# ID ve kayıt zamanı sütunu\n")
	fmt.Fprintf(&sb, "id    = %s\nzaman = %s\ntz    = Europe/Istanbul\n\n", sec[0].id, zaman)
	sb.WriteString("# Binlog dosyaları ve tablonun binlog'daki (canlı sistemdeki) adı\n")
	fmt.Fprintf(&sb, "binlog = %s\nbinlog_tablo = %s\nbinlog_id_sutun =\n", binlogDesen, binlogTablo)
	if len(binlogAdaylari) > 1 {
		sb.WriteString("# Binlog'da bu adla geçen tablolar (canlı sistemdeki doğru olanı binlog_tablo'ya yazın):\n")
		for _, a := range binlogAdaylari {
			fmt.Fprintf(&sb, "#   %s\n", a)
		}
	}
	sb.WriteString("\n")
	sb.WriteString("# Mühürlenecek orijinal delil klasörleri (isteğe bağlı; # işaretini silip yolu yazın)\n")
	if len(bgruplar) > 0 {
		fmt.Fprintf(&sb, "muhur = %s\n\n", binlogDesen)
	} else {
		sb.WriteString("# muhur = D:\\delil\n\n")
	}
	sb.WriteString("klasor = sonuc\nbaslik = \n")
	if len(sec) < len(tablolar) {
		sb.WriteString("\n# --- Sunucudaki diğer büyük tablolar (yanlış tablo seçildiyse yukarıdaki yedek satırlarını bunlarla değiştirin)\n")
		for i, t := range tablolar {
			if i >= 15 {
				break
			}
			fmt.Fprintf(&sb, "#   %s.%s  ~%s satır  id=%s\n", t.db, t.tablo, sayiYaz(t.satir), t.id)
		}
	}
	if len(bgruplar) > 1 {
		sb.WriteString("\n# --- Bulunan diğer binlog grupları\n")
		for _, o := range bgruplar[1:] {
			fmt.Fprintf(&sb, "#   %s  (%d dosya)\n", o.desen, len(o.dosyalar))
		}
	}
	if err := os.WriteFile(hedef, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("\n→ %s yazıldı.\n", hedef)
	return nil
}

func bosIse(s, v string) string {
	if s == "" {
		return v
	}
	return s
}

func dsnSor() (string, error) {
	r := bufio.NewReader(os.Stdin)
	sor := func(soru, varsayilan string) string {
		fmt.Printf("%s [%s]: ", soru, varsayilan)
		s, _ := r.ReadString('\n')
		s = strings.TrimSpace(strings.TrimPrefix(s, string(rune(0xFEFF))))
		if s == "" {
			return varsayilan
		}
		return s
	}
	fmt.Println("MySQL bağlantı bilgileri (yedeklerin yüklü olduğu sunucu). Varsayılan için Enter'a basın.")
	host := sor("Sunucu adresi", "127.0.0.1")
	port := sor("Port", "3306")
	user := sor("Kullanıcı", "root")
	fmt.Print("Parola: ")
	var parola string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		parola = string(b)
	} else {
		s, _ := r.ReadString('\n')
		parola = strings.TrimRight(s, "\r\n")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = user, parola, "tcp", host+":"+port
	return cfg.FormatDSN(), nil
}

type kesifTablo struct {
	db, tablo, id string
	satir, sonID  int64
	tarihler      []string
}

// tablolariBul, sistem dışı veritabanlarındaki ID sütunlu tabloları listeler.
// ID: AUTO_INCREMENT sütun; yoksa (yedek yüklenirken düşmüşse) tek sütunlu tamsayı birincil anahtar.
func tablolariBul(conn *sql.Conn) ([]*kesifTablo, error) {
	ctx := context.Background()
	rows, err := conn.QueryContext(ctx, `SELECT t.TABLE_SCHEMA, t.TABLE_NAME, IFNULL(t.TABLE_ROWS,0), IFNULL(t.AUTO_INCREMENT,0),
		c.COLUMN_NAME, c.DATA_TYPE, c.COLUMN_KEY, c.EXTRA
		FROM information_schema.TABLES t
		JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA=t.TABLE_SCHEMA AND c.TABLE_NAME=t.TABLE_NAME
		WHERE t.TABLE_TYPE='BASE TABLE'
		  AND t.TABLE_SCHEMA NOT IN ('mysql','sys','information_schema','performance_schema')
		ORDER BY t.TABLE_SCHEMA, t.TABLE_NAME, c.ORDINAL_POSITION`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type ham struct {
		t          *kesifTablo
		autoInc    string
		pkler      []string
		pkTamsayi  bool
	}
	var sira []string
	hamlar := map[string]*ham{}
	for rows.Next() {
		var db, tb, col, tur, key, extra string
		var satir, ai int64
		if err := rows.Scan(&db, &tb, &satir, &ai, &col, &tur, &key, &extra); err != nil {
			return nil, err
		}
		k := db + "." + tb
		h := hamlar[k]
		if h == nil {
			h = &ham{t: &kesifTablo{db: db, tablo: tb, satir: satir}}
			if ai > 0 {
				h.t.sonID = ai - 1
			}
			hamlar[k] = h
			sira = append(sira, k)
		}
		tur = strings.ToLower(tur)
		if strings.Contains(strings.ToLower(extra), "auto_increment") {
			h.autoInc = col
		}
		if key == "PRI" {
			h.pkler = append(h.pkler, col)
			h.pkTamsayi = strings.HasSuffix(tur, "int")
		}
		switch tur {
		case "datetime", "timestamp", "date":
			h.t.tarihler = append(h.t.tarihler, col)
		}
	}
	var out []*kesifTablo
	for _, k := range sira {
		h := hamlar[k]
		switch {
		case h.autoInc != "":
			h.t.id = h.autoInc
		case len(h.pkler) == 1 && h.pkTamsayi:
			h.t.id = h.pkler[0]
		default:
			continue
		}
		out = append(out, h.t)
	}
	return out, rows.Err()
}

var yilDeseni = regexp.MustCompile(`(19|20)\d{2}`)

// etiketUret yedek etiketlerini veritabanı adındaki yıldan, yoksa adın kendisinden türetir.
func etiketUret(t []*kesifTablo) []string {
	out := make([]string, len(t))
	kullanilan := map[string]bool{}
	for i, x := range t {
		e := yilDeseni.FindString(x.db)
		if e == "" || kullanilan[e] {
			e = regexp.MustCompile(`[^A-Za-z0-9_]+`).ReplaceAllString(x.db, "_")
		}
		for kullanilan[e] {
			e += "_" + strconv.Itoa(i+1)
		}
		kullanilan[e] = true
		out[i] = e
	}
	return out
}

var zamanAdDeseni = regexp.MustCompile(`(?i)creat|olus|oluş|kayit|kayıt|insert|ekle|eklen`)

func zamanSec(adaylar []string) string {
	for _, a := range adaylar {
		if zamanAdDeseni.MatchString(a) {
			return a
		}
	}
	if len(adaylar) > 0 {
		return adaylar[0]
	}
	return ""
}

func diskler() []string {
	var out []string
	for c := 'C'; c <= 'Z'; c++ {
		k := string(c) + `:\`
		if _, err := os.Stat(k); err == nil {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		out = []string{"/"}
	}
	return out
}

type binlogGrubu struct {
	desen    string
	dosyalar []string
}

var binlogAdDeseni = regexp.MustCompile(`^(.+)\.(\d{6})$`)

var atlanacakKlasorler = map[string]bool{
	"windows": true, "program files": true, "program files (x86)": true, "programdata": false,
	"$recycle.bin": true, "system volume information": true, "appdata": true, "node_modules": true,
	".git": true, "winsxs": true, "proc": true, "sys": true, "dev": true,
}

// binlogAra, kökler altında MySQL binlog dosyalarını (ad: önek.000123, ilk 4 bayt 0xFE 'bin')
// bulur ve klasör+önek bazında gruplar. Bu sunucunun kendi binlog'ları delil değildir, atlanır.
func binlogAra(kokler []string, kendiTaban string) []binlogGrubu {
	gruplar := map[string][]string{}
	var taranan int64
	son := time.Now()
	kendiTaban = strings.ToLower(filepath.Clean(kendiTaban))
	for _, kok := range kokler {
		filepath.WalkDir(kok, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if atlanacakKlasorler[strings.ToLower(d.Name())] && p != kok {
					return fs.SkipDir
				}
				return nil
			}
			taranan++
			if time.Since(son) > 2*time.Second {
				fmt.Fprintf(os.Stderr, "\r  %s dosyaya bakıldı, %d binlog grubu bulundu   ", sayiYaz(taranan), len(gruplar))
				son = time.Now()
			}
			m := binlogAdDeseni.FindStringSubmatch(d.Name())
			if m == nil || !binlogMu(p) {
				return nil
			}
			taban := filepath.Join(filepath.Dir(p), m[1])
			if kendiTaban != "" && strings.ToLower(taban) == kendiTaban {
				return nil
			}
			gruplar[taban] = append(gruplar[taban], p)
			return nil
		})
	}
	fmt.Fprintf(os.Stderr, "\r  %s dosyaya bakıldı, %d binlog grubu bulundu   \n", sayiYaz(taranan), len(gruplar))
	var out []binlogGrubu
	for taban, d := range gruplar {
		sort.Strings(d)
		out = append(out, binlogGrubu{desen: taban + ".[0-9]*", dosyalar: d})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].dosyalar) > len(out[j].dosyalar) })
	return out
}

func binlogMu(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	n, _ := f.Read(b)
	return n == 4 && string(b) == "\xfebin"
}

type binlogAday struct {
	ad   string
	olay int64
}

// binlogTabloBul, binlog'daki tablo eşleme olaylarından tablonun veritabanı adını bulur
// (canlı sistemdeki ad, yedeğin yüklendiği addan farklı olabilir). Aynı adlı tablo birden
// çok veritabanında geçiyorsa en çok olayı olan önce gelir.
func binlogTabloBul(dosyalar []string, tablo string) []binlogAday {
	say := map[string]int64{}
	for i, d := range dosyalar {
		fmt.Fprintf(os.Stderr, "\r  %d/%d %s   ", i+1, len(dosyalar), filepath.Base(d))
		p := replication.NewBinlogParser()
		p.SetVerifyChecksum(false)
		// Satır verisini çözmeye gerek yok; yalnızca tablo eşleme olayları sayılır.
		p.SetRowsEventDecodeFunc(func(*replication.RowsEvent, []byte) error { return nil })
		p.ParseFile(d, 0, func(e *replication.BinlogEvent) error {
			if tm, ok := e.Event.(*replication.TableMapEvent); ok && strings.EqualFold(string(tm.Table), tablo) {
				say[string(tm.Schema)+"."+string(tm.Table)]++
			}
			return nil
		})
	}
	fmt.Fprintln(os.Stderr)
	var out []binlogAday
	for ad, n := range say {
		out = append(out, binlogAday{ad, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].olay > out[j].olay })
	return out
}
