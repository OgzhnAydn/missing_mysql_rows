package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
)

// binlogKomut, binlog dosyalarını doğrudan ayrıştırır (mysqlbinlog gerekmez) ve
// incelenen tabloya ait satır olaylarını, silinen/değişen satırların tam görüntüsünü,
// tabloyla ilgili sorguları ve binlog zincirinin kapsamını/sürekliliğini çıkarır.
func binlogKomut(args []string) error {
	fsx := flag.NewFlagSet("binlog", flag.ExitOnError)
	tablo := fsx.String("tablo", "", "incelenecek tablo (db.tablo; db verilmezse tüm veritabanlarında bu ad)")
	idSutun := fsx.Int("id-sutun", 1, "ID sütununun tablodaki sırası (1'den başlar)")
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz binlog --tablo db.tablo [--id-sutun 1] <binlog dosyaları veya desen> ...")
		fmt.Fprintln(os.Stderr, "Örn: dbiz binlog --tablo arsiv.islem D:\\delil\\mysql-bin.0*")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if *tablo == "" || fsx.NArg() == 0 {
		fsx.Usage()
		return fmt.Errorf("--tablo ve en az bir binlog dosyası gerekli")
	}
	if err := klasorHazirla(*klasor); err != nil {
		return err
	}

	var dosyalar []string
	for _, a := range fsx.Args() {
		m, _ := filepath.Glob(a)
		if len(m) == 0 {
			m = []string{a}
		}
		for _, y := range m {
			if strings.HasSuffix(strings.ToLower(y), ".index") {
				continue
			}
			if st, err := os.Stat(y); err == nil && !st.IsDir() {
				dosyalar = append(dosyalar, y)
			}
		}
	}
	if len(dosyalar) == 0 {
		return fmt.Errorf("binlog dosyası bulunamadı: %s", strings.Join(fsx.Args(), " "))
	}
	sort.Slice(dosyalar, func(i, j int) bool {
		si, sj := siraNo(dosyalar[i]), siraNo(dosyalar[j])
		if si != sj {
			return si < sj
		}
		return dosyalar[i] < dosyalar[j]
	})

	b, err := yeniBinlogTarayici(*klasor, *tablo, *idSutun-1)
	if err != nil {
		return err
	}
	defer b.kapat()
	for _, d := range dosyalar {
		b.dosya(d)
	}
	b.kapsamYaz()

	ozet := fmt.Sprintf("%d dosya, tablo olayları: %s INSERT, %s UPDATE, %s DELETE; %d tabloyla ilgili sorgu",
		len(dosyalar), sayiYaz(b.sayac["INSERT"]), sayiYaz(b.sayac["UPDATE"]), sayiYaz(b.sayac["DELETE"]), b.sorguSay)
	if b.ifadeBicimi {
		ozet += "; UYARI: STATEMENT biçimli DML var (ID'ler çıkarılamaz, sorgu_olaylari.tsv'ye bakın)"
	}
	gunlukYaz(*klasor, ozet)
	fmt.Printf("\n%s\n→ %s\n", ozet, *klasor)
	return nil
}

var siraDeseni = regexp.MustCompile(`\.(\d+)$`)

func siraNo(yol string) int64 {
	m := siraDeseni.FindStringSubmatch(filepath.Base(yol))
	if m == nil {
		return -1
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

type dosyaKapsam struct {
	ad, sha     string
	boyut       int64
	sira        int64
	ilk, son    int64
	olay        int64
	tabloOlay   int64
	sonraki     string
	hata        string
}

type binlogTarayici struct {
	db, tablo   string
	idIdx       int
	olaylar     *bufio.Writer
	olaylarF    *os.File
	sorgular    *bufio.Writer
	sorgularF   *os.File
	satirlar    *csv.Writer
	satirlarF   *os.File
	satirBaslik bool
	kapsam      []dosyaKapsam
	cur         *dosyaKapsam
	thread      uint32
	sonSorguNo  int64 // son ROWS_QUERY olayının sorgu_olaylari'ndaki numarası
	sorguSay    int64
	sayac       map[string]int64
	ifadeBicimi bool
	tabloDeseni *regexp.Regexp
}

func yeniBinlogTarayici(klasor, tablo string, idIdx int) (*binlogTarayici, error) {
	db, t := tabloAyir(tablo)
	b := &binlogTarayici{db: db, tablo: t, idIdx: idIdx, sayac: map[string]int64{}}
	b.tabloDeseni = regexp.MustCompile("(?i)(^|[^a-z0-9_$])`?" + regexp.QuoteMeta(t) + "`?([^a-z0-9_$]|$)")
	var err error
	if b.olaylarF, err = os.Create(filepath.Join(klasor, "olaylar.tsv")); err != nil {
		return nil, err
	}
	b.olaylar = bufio.NewWriterSize(b.olaylarF, 1<<20)
	fmt.Fprintf(b.olaylar, "# dbiz=%s\n# tur=binlog_olaylar\n# tablo=%s\n", surum, tablo)
	fmt.Fprintln(b.olaylar, "tur\tid\tzaman\tdosya\tkonum\tthread\tsorgu_no")

	if b.sorgularF, err = os.Create(filepath.Join(klasor, "sorgu_olaylari.tsv")); err != nil {
		return nil, err
	}
	b.sorgular = bufio.NewWriterSize(b.sorgularF, 1<<20)
	fmt.Fprintf(b.sorgular, "# dbiz=%s\n# tur=sorgu_olaylari\n# tablo=%s\n", surum, tablo)
	fmt.Fprintln(b.sorgular, "no\ttur\tzaman\tdosya\tkonum\tthread\tsorgu")

	if b.satirlarF, err = os.Create(filepath.Join(klasor, "binlog_satir_goruntuleri.csv")); err != nil {
		return nil, err
	}
	b.satirlarF.WriteString("\xEF\xBB\xBF") // Excel için UTF-8 BOM
	b.satirlar = csv.NewWriter(b.satirlarF)
	return b, nil
}

func (b *binlogTarayici) kapat() {
	b.olaylar.Flush()
	b.olaylarF.Close()
	b.sorgular.Flush()
	b.sorgularF.Close()
	b.satirlar.Flush()
	b.satirlarF.Close()
}

func (b *binlogTarayici) dosya(yol string) {
	k := dosyaKapsam{ad: filepath.Base(yol), sira: siraNo(yol)}
	fmt.Fprintf(os.Stderr, "  %s ... ", k.ad)
	var err error
	if k.sha, k.boyut, err = sha256Dosya(yol); err != nil {
		k.hata = err.Error()
	}
	b.cur = &k
	b.thread = 0
	p := replication.NewBinlogParser()
	p.SetVerifyChecksum(false)
	p.SetParseTime(true)
	p.SetTimestampStringLocation(time.UTC)
	p.SetUseDecimal(false)
	if err := p.ParseFile(yol, 0, b.olay); err != nil && k.hata == "" {
		k.hata = temizle(err.Error())
	}
	b.kapsam = append(b.kapsam, k)
	durum := "tamam"
	if k.hata != "" {
		durum = "HATA: " + k.hata
	}
	fmt.Fprintf(os.Stderr, "%s olay, %s tablo olayı — %s\n", sayiYaz(k.olay), sayiYaz(k.tabloOlay), durum)
}

func (b *binlogTarayici) olay(e *replication.BinlogEvent) error {
	k := b.cur
	h := e.Header
	k.olay++
	ts := int64(h.Timestamp)
	// FORMAT_DESCRIPTION ve sahte ROTATE olaylarının zamanı 0 olabilir.
	if ts > 0 {
		if k.ilk == 0 || ts < k.ilk {
			k.ilk = ts
		}
		if ts > k.son {
			k.son = ts
		}
	}
	b.isle(e.Event, ts, h.LogPos)
	return nil
}

func (b *binlogTarayici) isle(ev replication.Event, ts int64, pos uint32) {
	k := b.cur
	switch e := ev.(type) {
	case *replication.RotateEvent:
		if len(e.NextLogName) > 0 && string(e.NextLogName) != k.ad {
			k.sonraki = string(e.NextLogName)
		}
	case *replication.QueryEvent:
		b.thread = e.SlaveProxyID
		q := strings.TrimSpace(string(e.Query))
		uq := strings.ToUpper(q)
		if uq == "BEGIN" || uq == "COMMIT" || uq == "ROLLBACK" {
			return
		}
		if !b.tabloDeseni.MatchString(q) {
			return
		}
		tur := "DDL"
		if strings.HasPrefix(uq, "INSERT") || strings.HasPrefix(uq, "DELETE") || strings.HasPrefix(uq, "UPDATE") ||
			strings.HasPrefix(uq, "REPLACE") || strings.HasPrefix(uq, "LOAD") {
			tur = "DML_STATEMENT"
			b.ifadeBicimi = true
		}
		b.sorguYaz(tur, ts, pos, q)
	case *replication.RowsQueryEvent:
		// binlog_rows_query_log_events=ON ise satır olaylarından önce asıl SQL gelir.
		q := strings.TrimSpace(string(e.Query))
		if b.tabloDeseni.MatchString(q) {
			b.sonSorguNo = b.sorguYaz("ROWS_QUERY", ts, pos, q)
		} else {
			b.sonSorguNo = 0
		}
	case *replication.XIDEvent:
		b.sonSorguNo = 0
	case *replication.TransactionPayloadEvent:
		for _, ic := range e.Events {
			b.isle(ic.Event, ts, pos)
		}
	case *replication.RowsEvent:
		if e.Table == nil || !b.tabloEslesir(string(e.Table.Schema), string(e.Table.Table)) {
			return
		}
		k.tabloOlay++
		switch e.Type() {
		case replication.EnumRowsEventTypeInsert:
			for _, r := range e.Rows {
				b.olayYaz("INSERT", r, ts, pos)
			}
		case replication.EnumRowsEventTypeDelete:
			for _, r := range e.Rows {
				b.olayYaz("DELETE", r, ts, pos)
				b.satirYaz(e, "DELETE", r, ts, pos)
			}
		case replication.EnumRowsEventTypeUpdate:
			// UPDATE satırları (önce, sonra) çiftleri halinde gelir.
			for i := 0; i+1 < len(e.Rows); i += 2 {
				b.olayYaz("UPDATE", e.Rows[i], ts, pos)
				once, sonra := idDeger(e.Rows[i], b.idIdx), idDeger(e.Rows[i+1], b.idIdx)
				if once != sonra {
					// ID'nin kendisi değiştirildiyse eski ID artık yok: silme gibi davranır.
					b.olayYaz("ID_DEGISTI", e.Rows[i], ts, pos)
				}
				b.satirYaz(e, "UPDATE_ONCE", e.Rows[i], ts, pos)
				b.satirYaz(e, "UPDATE_SONRA", e.Rows[i+1], ts, pos)
			}
		}
	}
}

func (b *binlogTarayici) tabloEslesir(db, t string) bool {
	if !strings.EqualFold(t, b.tablo) {
		return false
	}
	return b.db == "" || strings.EqualFold(db, b.db)
}

func (b *binlogTarayici) sorguYaz(tur string, ts int64, pos uint32, q string) int64 {
	b.sorguSay++
	if len(q) > 4000 {
		q = q[:4000] + "…"
	}
	fmt.Fprintf(b.sorgular, "%d\t%s\t%d\t%s\t%d\t%d\t%s\n", b.sorguSay, tur, ts, b.cur.ad, pos, b.thread, temizle(q))
	return b.sorguSay
}

func (b *binlogTarayici) olayYaz(tur string, r []any, ts int64, pos uint32) {
	b.sayac[tur]++
	id := idDeger(r, b.idIdx)
	fmt.Fprintf(b.olaylar, "%s\t%s\t%d\t%s\t%d\t%d\t%d\n", tur, id, ts, b.cur.ad, pos, b.thread, b.sonSorguNo)
}

func (b *binlogTarayici) satirYaz(e *replication.RowsEvent, islem string, r []any, ts int64, pos uint32) {
	if !b.satirBaslik {
		bas := []string{"islem", "zaman_utc", "dosya", "konum", "thread"}
		adlar := e.Table.ColumnNameString()
		for i := range r {
			if i < len(adlar) && adlar[i] != "" {
				bas = append(bas, adlar[i])
			} else {
				bas = append(bas, fmt.Sprintf("@%d", i+1))
			}
		}
		b.satirlar.Write(bas)
		b.satirBaslik = true
	}
	rec := []string{islem, time.Unix(ts, 0).UTC().Format("2006-01-02 15:04:05"), b.cur.ad, strconv.Itoa(int(pos)), strconv.Itoa(int(b.thread))}
	for _, v := range r {
		rec = append(rec, degerYaz(v))
	}
	b.satirlar.Write(rec)
}

func idDeger(r []any, idx int) string {
	if idx < 0 || idx >= len(r) {
		return ""
	}
	return degerYaz(r[idx])
}

func degerYaz(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return metinDeger(x)
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999")
	case *big.Int:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// kapsamYaz dosya başına kapsamı ve zincir sürekliliğini binlog_kapsam.tsv'ye yazar.
// Ardışık sıra numaralı ve kopuksuz dosyalar tek bir kapsam aralığında birleştirilir;
// bu aralık içinde binlog'a hiç düşmemiş bir ekleme "commit edilmemiş" demektir.
func (b *binlogTarayici) kapsamYaz() {
	f, err := os.Create(filepath.Join(filepath.Dir(b.olaylarF.Name()), "binlog_kapsam.tsv"))
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	var notlar []string
	type kap struct{ bas, son int64 }
	var kapsamlar []kap
	for i, k := range b.kapsam {
		if k.hata != "" {
			notlar = append(notlar, fmt.Sprintf("%s okunurken hata: %s (dosya kesilmiş/bozuk olabilir)", k.ad, k.hata))
		}
		bitisik := false
		if i > 0 {
			o := b.kapsam[i-1]
			switch {
			case o.sira >= 0 && k.sira >= 0 && k.sira != o.sira+1:
				notlar = append(notlar, fmt.Sprintf("ZİNCİR KOPUK: %s ile %s arasında %d dosya eksik", o.ad, k.ad, k.sira-o.sira-1))
			case o.sonraki != "" && o.sonraki != k.ad:
				notlar = append(notlar, fmt.Sprintf("ZİNCİR KOPUK: %s sonrakini %s olarak gösteriyor, eldeki %s", o.ad, o.sonraki, k.ad))
			case o.hata == "" && k.hata == "":
				bitisik = true
			}
		}
		if k.ilk == 0 {
			continue
		}
		if bitisik && len(kapsamlar) > 0 {
			kapsamlar[len(kapsamlar)-1].son = maxI(kapsamlar[len(kapsamlar)-1].son, k.son)
		} else {
			kapsamlar = append(kapsamlar, kap{k.ilk, k.son})
		}
	}
	if b.ifadeBicimi {
		notlar = append(notlar, "STATEMENT biçimli DML bulundu: bu olaylarda tek tek ID yoktur, sorgu metnine bakılmalı")
	}
	fmt.Fprintf(w, "# dbiz=%s\n# tur=binlog_kapsam\n", surum)
	for _, k := range kapsamlar {
		fmt.Fprintf(w, "# kapsam=%d-%d\n", k.bas, k.son)
	}
	for _, n := range notlar {
		fmt.Fprintf(w, "# not=%s\n", temizle(n))
	}
	fmt.Fprintln(w, "sira\tdosya\tsha256\tboyut\tilk_zaman\tson_zaman\tolay\ttablo_olay\tsonraki\thata")
	for _, k := range b.kapsam {
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", k.sira, k.ad, k.sha, k.boyut, k.ilk, k.son, k.olay, k.tabloOlay, k.sonraki, k.hata)
	}
}
