package main

import (
	"bufio"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sınıflar, kanıt gücüne göre sıralıdır. Bir ID için önce doğrudan kanıta
// (yedek farkı, binlog) bakılır; yoksa boşluğun bütününe ait dolaylı kanıta.
type sinif struct {
	kod, kesinlik, grup, aciklama string
}

var siniflar = []sinif{
	{"K1", "KESİN SİLİNDİ", "kesin", "Eski yedekte mevcut, incelenen yedekte yok. Satırın tam içeriği eski yedekte duruyor (dbiz kurtar)."},
	{"K2", "KESİN SİLİNDİ", "kesin", "Binlog'da silme (ya da ID değiştirme) kaydı var. Silinen satırın tam görüntüsü binlog_satir_goruntuleri.csv dosyasında."},
	{"K3", "ŞÜPHELİ", "supheli", "Binlog'da eklendiği görülüyor ama silme kaydı yok. Silme, eldeki binlog'ların kapsamı dışında ya da binlog'a yazılmadan (sql_log_bin=0) yapılmış olabilir."},
	{"K4", "MUHTEMELEN OLUŞMADI", "olusmadi", "Bu aralıkta sayaç ALTER TABLE … AUTO_INCREMENT ile elle ileri alınmış."},
	{"K5", "OLUŞMADI (KOŞULLU)", "olusmadi", "Aralık kesintisiz binlog kapsamında ama hiçbir ekleme kaydı yok: kayıt commit edilmemiş (rollback ya da sayaç atlaması). Tek istisna: binlog'a yazılmadan (sql_log_bin=0) ekleyip silme."},
	{"K6", "MUHTEMELEN OLUŞMADI", "olusmadi", "Komşu kayıtlar arasındaki süre, bu kadar kaydın o dönemin olağan hızıyla oluşması için çok kısa."},
	{"K7", "SİLİNMİŞ OLABİLİR", "supheli", "Komşu kayıtlar arasındaki süre bu kadar kaydın oluşmasına yetecek kadar uzun; ancak doğrudan kanıt yok."},
	{"K8", "BELİRSİZ", "belirsiz", "Değerlendirme için yeterli veri yok (zaman sütunu, binlog ya da yedek farkı yok)."},
}

func sinifBul(kod string) sinif {
	for _, s := range siniflar {
		if s.kod == kod {
			return s
		}
	}
	return siniflar[len(siniflar)-1]
}

type gapKaydi struct {
	aralik
	onceki, sonraki int64
	sure            int64
	hiz             float64
	isaretli        int // aralıkta binlog olayı olan ID sayısı
}

type idIz struct {
	ins, del         bool
	delZaman         int64
	delThread        string
	delDosya         string
}

type komsuIz struct {
	zaman, anahtar int64
}

// konumAnahtari binlog olaylarını dosya sıra numarası ve pozisyona göre sıralanabilir yapar.
func konumAnahtari(dosya, konum string) int64 {
	sira := siraNo(dosya)
	if sira < 0 {
		return 0
	}
	return sira<<32 | atoi64(konum)
}

type segment struct {
	aralik
	kod string
	not string
	gap *gapKaydi
}

func rapor(args []string) error {
	fsx := flag.NewFlagSet("rapor", flag.ExitOnError)
	etiket := fsx.String("etiket", "guncel", "incelenen yedeğin etiketi (bosluklar_<etiket>.tsv)")
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	kisa := fsx.Float64("oran-kisa", 0.2, "gözlenen süre / beklenen süre bunun altındaysa 'muhtemelen oluşmadı'")
	uzun := fsx.Float64("oran-uzun", 0.5, "gözlenen süre / beklenen süre bunun üstündeyse 'silinmiş olabilir'")
	tzStr := fsx.String("tz", "", "raporda zamanların gösterileceği saat dilimi (varsayılan: boşluk dosyasındaki)")
	baslik := fsx.String("baslik", "", "rapor başlığına eklenecek dosya/vaka adı")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz rapor --etiket 2024 [--baslik \"Vaka 2026/123\"]")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)

	girdiler := map[string]bool{}
	gapYol := filepath.Join(*klasor, "bosluklar_"+*etiket+".tsv")
	gt, err := tsvOku(gapYol)
	if err != nil {
		return fmt.Errorf("%s okunamadı (önce 'dbiz bosluk --etiket %s'): %w", gapYol, *etiket, err)
	}
	girdiler[gapYol] = true
	tz := *tzStr
	if tz == "" {
		tz = gt.meta["tz"]
	}
	loc, err := konumYukle(tz)
	if err != nil {
		return err
	}

	col := func(ad string) int { return gt.sutun(ad) }
	gaps := make([]gapKaydi, 0, len(gt.satirlar))
	for _, s := range gt.satirlar {
		g := gapKaydi{aralik: aralik{atoi64(s[col("bas")]), atoi64(s[col("son")])},
			onceki: atoi64(s[col("onceki_zaman")]), sonraki: atoi64(s[col("sonraki_zaman")]), sure: atoi64(s[col("sure_sn")])}
		g.hiz, _ = strconv.ParseFloat(s[col("yerel_hiz")], 64)
		gaps = append(gaps, g)
	}
	gapAraliklari := make([]aralik, len(gaps))
	for i := range gaps {
		gapAraliklari[i] = gaps[i].aralik
	}

	// Yedekler arası silinenler (bu etikete göre yeni olan tüm karşılaştırmalar).
	var yedekSilinen []aralik
	var yedekKaynak []string
	silDosyalar, _ := filepath.Glob(filepath.Join(*klasor, "silinen_*_"+*etiket+".tsv"))
	for _, d := range silDosyalar {
		t, a, err := bosluklariOku(d)
		if err != nil {
			continue
		}
		girdiler[d] = true
		yedekSilinen = append(yedekSilinen, a...)
		yedekKaynak = append(yedekKaynak, fmt.Sprintf("%s → %s: %s ID", t.meta["eski"], t.meta["yeni"], sayiYaz(toplamAdet(a))))
	}
	yedekSilinen = birlestir(yedekSilinen)
	var belirenToplam int64
	belDosyalar, _ := filepath.Glob(filepath.Join(*klasor, "beliren_*_"+*etiket+".tsv"))
	for _, d := range belDosyalar {
		if _, a, err := bosluklariOku(d); err == nil {
			belirenToplam += toplamAdet(a)
			girdiler[d] = true
		}
	}

	// Binlog kapsamı.
	var kapsamlar []aralik // unix zaman aralıkları
	var kapsamNotlari []string
	var kapsamDosyalar *tsvDosya
	binlogVar := false
	if kt, err := tsvOku(filepath.Join(*klasor, "binlog_kapsam.tsv")); err == nil {
		binlogVar = true
		kapsamDosyalar = kt
		girdiler[filepath.Join(*klasor, "binlog_kapsam.tsv")] = true
		for _, k := range strings.Split(kt.meta["kapsam"], "\n") {
			if a, b, ok := strings.Cut(k, "-"); ok {
				kapsamlar = append(kapsamlar, aralik{atoi64(a), atoi64(b)})
			}
		}
		if n := kt.meta["not"]; n != "" {
			kapsamNotlari = strings.Split(n, "\n")
		}
	}

	// Binlog olayları: yalnızca boşluk içindeki ID'ler ve boşluk komşuları tutulur.
	komsu := map[int64]komsuIz{} // komşu ID → binlog'daki ekleme zamanı ve konumu
	for _, g := range gaps {
		komsu[g.bas-1] = komsuIz{}
		komsu[g.son+1] = komsuIz{}
	}
	izler := map[int64]*idIz{}
	var updateSay int64
	olayYol := filepath.Join(*klasor, "olaylar.tsv")
	if _, err := os.Stat(olayYol); err == nil && binlogVar {
		girdiler[olayYol] = true
		err := tsvAkis(olayYol, func(h map[string]int, c []string) error {
			id, err := strconv.ParseInt(c[h["id"]], 10, 64)
			if err != nil {
				return nil
			}
			tur := c[h["tur"]]
			z := atoi64(c[h["zaman"]])
			if tur == "INSERT" {
				if v, ok := komsu[id]; ok && v.zaman == 0 {
					komsu[id] = komsuIz{z, konumAnahtari(c[h["dosya"]], c[h["konum"]])}
				}
			}
			if tur == "UPDATE" {
				updateSay++
			}
			gi := icerenIndeks(gapAraliklari, id)
			if gi < 0 {
				return nil
			}
			iz := izler[id]
			if iz == nil {
				iz = &idIz{}
				izler[id] = iz
				gaps[gi].isaretli++
			}
			switch tur {
			case "INSERT":
				iz.ins = true
			case "DELETE", "ID_DEGISTI":
				iz.del = true
				if iz.delZaman == 0 || z < iz.delZaman {
					iz.delZaman, iz.delThread, iz.delDosya = z, c[h["thread"]], c[h["dosya"]]
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	// Tabloyla ilgili sorgular: sayaç atlatma ve toplu silme izleri.
	type sorguKaydi struct {
		tur, dosya, thread, sorgu string
		zaman                     int64
	}
	var sorgular []sorguKaydi
	var sayacOlaylari []komsuIz // zaman + binlog konumu
	aiDeseni := regexp.MustCompile(`(?i)AUTO_INCREMENT\s*=`)
	sorguYol := filepath.Join(*klasor, "sorgu_olaylari.tsv")
	if _, err := os.Stat(sorguYol); err == nil && binlogVar {
		girdiler[sorguYol] = true
		tsvAkis(sorguYol, func(h map[string]int, c []string) error {
			s := sorguKaydi{tur: c[h["tur"]], zaman: atoi64(c[h["zaman"]]), dosya: c[h["dosya"]], thread: c[h["thread"]], sorgu: c[h["sorgu"]]}
			if s.tur != "ROWS_QUERY" || len(sorgular) < 300 {
				sorgular = append(sorgular, s)
			}
			if s.tur == "DDL" && aiDeseni.MatchString(s.sorgu) {
				sayacOlaylari = append(sayacOlaylari, komsuIz{s.zaman, konumAnahtari(s.dosya, c[h["konum"]])})
			}
			return nil
		})
	}

	// Sınıflandırma.
	ayniKapsamda := func(t1, t2 int64) bool {
		if t1 <= 0 || t2 <= 0 {
			return false
		}
		for _, k := range kapsamlar {
			if t1 >= k.bas && t2 <= k.son {
				return true
			}
		}
		return false
	}
	var segmentler []segment
	for gi := range gaps {
		g := &gaps[gi]
		// Boşluğun zaman penceresi: önce binlog'daki komşu ekleme zamanları (saat dilimi
		// sorunu yok), yoksa tablodaki zaman sütunu.
		k1, k2 := komsu[g.bas-1], komsu[g.son+1]
		t1, t2 := k1.zaman, k2.zaman
		kaynak := "binlog"
		binlogKomsu := t1 > 0 && t2 > 0
		if !binlogKomsu {
			t1, t2, kaynak = g.onceki, g.sonraki, "tablo"
		}
		gapKod, gapNot := "K8", ""
		// Sayaç değişikliği iki komşu eklemenin arasında mı? Komşular binlog'daysa
		// konum sırasıyla (aynı saniyedeki olayları da ayırır), değilse zamanla bakılır.
		sayac := false
		for _, s := range sayacOlaylari {
			if binlogKomsu && k1.anahtar > 0 && k2.anahtar > 0 {
				if s.anahtar > k1.anahtar && s.anahtar < k2.anahtar {
					sayac = true
				}
			} else if t1 > 0 && t2 > 0 && s.zaman >= t1 && s.zaman <= t2 {
				sayac = true
			}
			if sayac {
				break
			}
		}
		oranStr := ""
		oran := -1.0
		if g.sure >= 0 && g.hiz > 0 {
			beklenen := float64(g.adet()) / g.hiz
			oran = float64(g.sure) / beklenen
			oranStr = fmt.Sprintf("gözlenen %s / beklenen ~%s", sureYaz(g.sure), sureYaz(int64(beklenen)))
		}
		switch {
		case sayac:
			gapKod, gapNot = "K4", "komşu kayıtlar arasında AUTO_INCREMENT değişikliği var"
		case binlogVar && ayniKapsamda(t1, t2):
			gapKod, gapNot = "K5", "zaman penceresi kaynağı: "+kaynak
		case oran >= 0 && oran < *kisa:
			gapKod, gapNot = "K6", oranStr
		case oran > *uzun:
			gapKod, gapNot = "K7", oranStr
		case oran >= 0:
			gapKod, gapNot = "K8", oranStr+" (ara bölge)"
		}

		yedekKesisim := kesisim([]aralik{g.aralik}, yedekSilinen)
		if g.isaretli == 0 && len(yedekKesisim) == 0 {
			segmentler = append(segmentler, segment{aralik: g.aralik, kod: gapKod, not: gapNot, gap: g})
			continue
		}
		// Karışık boşluk: ID ID yürü, aynı sınıftaki ardışık ID'leri birleştir.
		yi := 0
		var cur *segment
		for id := g.bas; id <= g.son; id++ {
			kod, not := gapKod, gapNot
			for yi < len(yedekKesisim) && yedekKesisim[yi].son < id {
				yi++
			}
			if yi < len(yedekKesisim) && yedekKesisim[yi].bas <= id {
				kod, not = "K1", ""
			} else if iz := izler[id]; iz != nil {
				if iz.del {
					kod = "K2"
					not = fmt.Sprintf("ilk silme %s, thread %s, %s", zamanYaz(iz.delZaman, loc), iz.delThread, iz.delDosya)
				} else if iz.ins {
					kod, not = "K3", ""
				}
			}
			if cur != nil && cur.kod == kod && cur.son == id-1 {
				cur.son = id
				continue
			}
			segmentler = append(segmentler, segment{aralik: aralik{id, id}, kod: kod, not: not, gap: g})
			cur = &segmentler[len(segmentler)-1]
		}
	}

	// Sınıflandırma dosyası.
	sinYol := filepath.Join(*klasor, "siniflandirma_"+*etiket+".tsv")
	if err := siniflandirmaYaz(sinYol, segmentler, loc); err != nil {
		return err
	}

	// Özet.
	type toplam struct{ aralik, adet int64 }
	ozet := map[string]*toplam{}
	for _, s := range siniflar {
		ozet[s.kod] = &toplam{}
	}
	for _, s := range segmentler {
		ozet[s.kod].aralik++
		ozet[s.kod].adet += s.adet()
	}
	grupAdet := map[string]int64{}
	for _, s := range siniflar {
		grupAdet[s.grup] += ozet[s.kod].adet
	}

	// Girdi dosyalarının özetleri.
	var girdiListesi []string
	for y := range girdiler {
		girdiListesi = append(girdiListesi, y)
	}
	sort.Strings(girdiListesi)

	var muhur *tsvDosya
	if m, err := tsvOku(filepath.Join(*klasor, "muhur.tsv")); err == nil {
		muhur = m
	}
	gunluk, _ := os.ReadFile(filepath.Join(*klasor, "islem_gunlugu.txt"))

	// HTML.
	var b strings.Builder
	e := html.EscapeString
	baslikMetin := "Silinmiş Kayıt İncelemesi"
	if *baslik != "" {
		baslikMetin += " — " + *baslik
	}
	fmt.Fprintf(&b, `<!doctype html><html lang="tr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>%s</style></head><body><main>`, e(baslikMetin), raporCSS)
	fmt.Fprintf(&b, `<header><div class="ust">Veritabanı adli incelemesi · dbiz %s</div><h1>%s</h1><dl class="kunye">`, surum, e(baslikMetin))
	kunye := [][2]string{
		{"Tablo", gt.meta["tablo"]},
		{"İncelenen yedek", *etiket},
		{"Kaynak", gt.meta["kaynak"]},
		{"ID aralığı", sayiYaz(atoi64(gt.meta["min"])) + " – " + sayiYaz(atoi64(gt.meta["max"]))},
		{"Mevcut satır", sayiYaz(atoi64(gt.meta["satir"]))},
		{"Kayıt dönemi", zamanYaz(atoi64(gt.meta["ilk_zaman"]), loc) + " – " + zamanYaz(atoi64(gt.meta["son_zaman"]), loc)},
		{"Saat dilimi", loc.String()},
		{"Rapor tarihi", time.Now().In(loc).Format("2006-01-02 15:04:05")},
	}
	for _, k := range kunye {
		fmt.Fprintf(&b, "<div><dt>%s</dt><dd>%s</dd></div>", e(k[0]), e(k[1]))
	}
	b.WriteString("</dl></header>")

	// Sonuç kartları.
	toplamEksik := atoi64(gt.meta["eksik"])
	b.WriteString(`<section><h2>Sonuç</h2><div class="kartlar">`)
	kart := func(cls, etiket string, n int64, alt string) {
		fmt.Fprintf(&b, `<div class="kart %s"><div class="kn">%s</div><div class="ke">%s</div><div class="ka">%s</div></div>`, cls, sayiYaz(n), e(etiket), e(alt))
	}
	kart("", "eksik ID", toplamEksik, fmt.Sprintf("%s boşluk aralığında", sayiYaz(int64(len(gaps)))))
	kart("kesin", "kesin silinmiş", grupAdet["kesin"], "yedek farkı ya da binlog silme kaydı")
	kart("supheli", "şüpheli", grupAdet["supheli"], "silinmiş olabilir, doğrudan kanıt eksik")
	kart("olusmadi", "oluşmamış", grupAdet["olusmadi"], "rollback / sayaç atlaması")
	kart("belirsiz", "belirsiz", grupAdet["belirsiz"], "yeterli veri yok")
	b.WriteString(`</div>`)
	var uyarilar []string
	for _, n := range kapsamNotlari {
		if strings.Contains(n, "KOPUK") || strings.Contains(n, "hata") || strings.Contains(n, "STATEMENT") {
			uyarilar = append(uyarilar, n)
		}
	}
	if belirenToplam > 0 {
		uyarilar = append(uyarilar, fmt.Sprintf("%s ID eski yedekte boşken incelenen yedekte var: sonradan elle ID verilerek eklenmiş kayıtlar (beliren_*.tsv).", sayiYaz(belirenToplam)))
	}
	if !binlogVar {
		uyarilar = append(uyarilar, "Binlog analizi yapılmadı; K2/K3/K5 sınıfları değerlendirilemedi.")
	}
	if updateSay > 0 {
		uyarilar = append(uyarilar, fmt.Sprintf("Binlog'da bu tabloda %s satır güncellemesi (UPDATE) var. Silme dışında değiştirilen kayıtların önceki/sonraki hali binlog_satir_goruntuleri.csv dosyasında.", sayiYaz(updateSay)))
	}
	if len(silDosyalar) == 0 {
		uyarilar = append(uyarilar, "Yedek karşılaştırması yapılmadı; K1 sınıfı değerlendirilemedi.")
	}
	if gt.meta["zaman_sutunu"] == "" {
		uyarilar = append(uyarilar, "Zaman sütunu verilmedi; komşu süre analizi (K6/K7) yapılamadı.")
	}
	if len(uyarilar) > 0 {
		b.WriteString(`<div class="uyari"><b>Dikkat</b><ul>`)
		for _, u := range uyarilar {
			fmt.Fprintf(&b, "<li>%s</li>", e(u))
		}
		b.WriteString(`</ul></div>`)
	}
	b.WriteString(`</section>`)

	// Sınıflar tablosu.
	b.WriteString(`<section><h2>Kanıt sınıfları</h2><table><thead><tr><th>Sınıf</th><th>Değerlendirme</th><th>Açıklama</th><th class="s">Aralık</th><th class="s">ID</th></tr></thead><tbody>`)
	for _, s := range siniflar {
		t := ozet[s.kod]
		fmt.Fprintf(&b, `<tr class="%s"><td><span class="rozet %s">%s</span></td><td><b>%s</b></td><td>%s</td><td class="s">%s</td><td class="s">%s</td></tr>`,
			map[bool]string{true: "bos", false: ""}[t.adet == 0], s.grup, s.kod, e(s.kesinlik), e(s.aciklama), sayiYaz(t.aralik), sayiYaz(t.adet))
	}
	b.WriteString(`</tbody></table></section>`)

	// Kesin silinenler ve en büyük aralıklar.
	segTablo := func(baslikM string, liste []segment, sinir int) {
		fmt.Fprintf(&b, `<section><h2>%s</h2>`, e(baslikM))
		if len(liste) == 0 {
			b.WriteString(`<p class="sessiz">Kayıt yok.</p></section>`)
			return
		}
		b.WriteString(`<table><thead><tr><th class="s">Başlangıç</th><th class="s">Bitiş</th><th class="s">Adet</th><th>Sınıf</th><th>Önceki kayıt</th><th>Sonraki kayıt</th><th>Not</th></tr></thead><tbody>`)
		for i, s := range liste {
			if i >= sinir {
				break
			}
			sn := sinifBul(s.kod)
			fmt.Fprintf(&b, `<tr><td class="s">%s</td><td class="s">%s</td><td class="s">%s</td><td><span class="rozet %s">%s</span></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				sayiYaz(s.bas), sayiYaz(s.son), sayiYaz(s.adet()), sn.grup, s.kod,
				e(zamanYaz(s.gap.onceki, loc)), e(zamanYaz(s.gap.sonraki, loc)), e(s.not))
		}
		b.WriteString(`</tbody></table>`)
		if len(liste) > sinir {
			fmt.Fprintf(&b, `<p class="sessiz">İlk %d gösterildi; tamamı siniflandirma_%s.tsv dosyasında (%s aralık).</p>`, sinir, e(*etiket), sayiYaz(int64(len(liste))))
		}
		b.WriteString(`</section>`)
	}
	var kesinler, buyukler []segment
	for _, s := range segmentler {
		if sinifBul(s.kod).grup == "kesin" {
			kesinler = append(kesinler, s)
		}
	}
	buyukler = append(buyukler, segmentler...)
	sort.SliceStable(buyukler, func(i, j int) bool { return buyukler[i].adet() > buyukler[j].adet() })
	segTablo("Kesin silinmiş aralıklar", kesinler, 500)
	segTablo("En büyük aralıklar", buyukler, 50)

	// Yedek karşılaştırması.
	if len(yedekKaynak) > 0 {
		b.WriteString(`<section><h2>Yedek karşılaştırması</h2><ul>`)
		for _, k := range yedekKaynak {
			fmt.Fprintf(&b, "<li>%s</li>", e(k))
		}
		b.WriteString(`</ul></section>`)
	}

	// Binlog kapsamı.
	if kapsamDosyalar != nil {
		b.WriteString(`<section><h2>Binlog kapsamı ve süreklilik</h2>`)
		if len(kapsamlar) > 0 {
			b.WriteString(`<p>Kesintisiz kapsam aralıkları:</p><ul>`)
			for _, k := range kapsamlar {
				fmt.Fprintf(&b, "<li>%s – %s (%s)</li>", e(zamanYaz(k.bas, loc)), e(zamanYaz(k.son, loc)), e(sureYaz(k.son-k.bas)))
			}
			b.WriteString(`</ul>`)
		}
		if len(kapsamNotlari) > 0 {
			b.WriteString(`<ul class="notlar">`)
			for _, n := range kapsamNotlari {
				fmt.Fprintf(&b, "<li>%s</li>", e(n))
			}
			b.WriteString(`</ul>`)
		}
		kt := kapsamDosyalar
		c := func(r []string, ad string) string {
			if i := kt.sutun(ad); i >= 0 && i < len(r) {
				return r[i]
			}
			return ""
		}
		b.WriteString(`<table class="kucuk"><thead><tr><th>Dosya</th><th>İlk olay</th><th>Son olay</th><th class="s">Tablo olayı</th><th>SHA-256</th><th>Durum</th></tr></thead><tbody>`)
		for _, r := range kt.satirlar {
			durum := "tamam"
			if h := c(r, "hata"); h != "" {
				durum = h
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td class="s">%s</td><td class="hash">%s</td><td>%s</td></tr>`,
				e(c(r, "dosya")), e(zamanYaz(atoi64(c(r, "ilk_zaman")), loc)), e(zamanYaz(atoi64(c(r, "son_zaman")), loc)),
				sayiYaz(atoi64(c(r, "tablo_olay"))), e(c(r, "sha256")), e(durum))
		}
		b.WriteString(`</tbody></table></section>`)
	}

	// Tabloyla ilgili sorgular.
	if len(sorgular) > 0 {
		b.WriteString(`<section><h2>Tabloyla ilgili sorgular (binlog)</h2><table class="kucuk"><thead><tr><th>Zaman</th><th>Tür</th><th>Thread</th><th>Dosya</th><th>Sorgu</th></tr></thead><tbody>`)
		for i, s := range sorgular {
			if i >= 300 {
				break
			}
			q := s.sorgu
			if len(q) > 400 {
				q = q[:400] + "…"
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td class="s">%s</td><td>%s</td><td class="kod">%s</td></tr>`,
				e(zamanYaz(s.zaman, loc)), e(s.tur), e(s.thread), e(s.dosya), e(q))
		}
		b.WriteString(`</tbody></table></section>`)
	}

	// Delil bütünlüğü.
	b.WriteString(`<section><h2>Delil bütünlüğü</h2>`)
	if muhur != nil {
		b.WriteString(`<h3>Mühürlenen orijinal dosyalar</h3><table class="kucuk"><thead><tr><th>Dosya</th><th class="s">Boyut</th><th>SHA-256</th><th>Mühür (UTC)</th></tr></thead><tbody>`)
		for _, r := range muhur.satirlar {
			if len(r) < 5 {
				continue
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td class="s">%s</td><td class="hash">%s</td><td>%s</td></tr>`, e(r[4]), sayiYaz(atoi64(r[1])), e(r[0]), e(r[3]))
		}
		b.WriteString(`</tbody></table>`)
	} else {
		b.WriteString(`<p class="sessiz">Orijinal dosyalar mühürlenmemiş (dbiz muhurle).</p>`)
	}
	b.WriteString(`<h3>Bu raporun dayandığı ara dosyalar</h3><table class="kucuk"><thead><tr><th>Dosya</th><th>SHA-256</th></tr></thead><tbody>`)
	for _, y := range girdiListesi {
		h, _, _ := sha256Dosya(y)
		fmt.Fprintf(&b, `<tr><td>%s</td><td class="hash">%s</td></tr>`, e(filepath.Base(y)), e(h))
	}
	b.WriteString(`</tbody></table>`)
	if len(gunluk) > 0 {
		fmt.Fprintf(&b, `<h3>İşlem günlüğü</h3><pre>%s</pre>`, e(string(gunluk)))
	}
	b.WriteString(`</section>`)

	fmt.Fprintf(&b, `<section><h2>Yöntem ve sınırlar</h2><ol class="yontem">
<li><b>Boşluk:</b> Tablo, ID sırasıyla tek geçişte okunmuş; ardışık iki ID arasındaki her atlama bir aralık olarak kaydedilmiştir. Veritabanına salt-okuma oturumuyla bağlanılmıştır.</li>
<li><b>Yedek farkı (K1):</b> Eski yedekte bulunup incelenen yedekte bulunmayan ID'ler kesin silinmiştir; satır içeriği eski yedektedir.</li>
<li><b>Binlog (K2, K3, K5):</b> Binlog dosyaları doğrudan ayrıştırılmıştır. Binlog kullanıcı adı ve IP adresi içermez; yalnızca zaman, thread ve sunucu kimliği içerir. Kişi tespiti için general_log, denetim (audit) kaydı ya da uygulama günlükleri gerekir.</li>
<li><b>Geri alınan işlemler</b> binlog'a yazılmaz. Bu nedenle kesintisiz kapsam içinde ekleme kaydı olmayan ID'ler commit edilmemiş kabul edilir (K5). Binlog'u oturum bazında kapatma (sql_log_bin=0) yetkisi olan bir kullanıcının iz bırakmadan ekleyip silmesi bu yöntemle ayırt edilemez.</li>
<li><b>Süre analizi (K6, K7):</b> Boşluğun komşu kayıtları arasındaki süre, boşluk öncesindeki son %d kaydın hızıyla beklenen süreye oranlanır (eşikler: <%.2f oluşmadı, >%.2f silinmiş olabilir). Bu dolaylı bir göstergedir, tek başına kanıt değildir.</li>
<li><b>Saat dilimi:</b> Binlog zamanları UTC'dir; tablo DATETIME değerleri %s olarak yorumlanmıştır. Mümkün olduğunda zaman penceresi binlog'daki komşu ekleme zamanlarından alınır.</li>
</ol></section>`, yerelPencere, *kisa, *uzun, e(gt.meta["tz"]))

	b.WriteString(`<footer>Bu rapor dbiz tarafından üretilmiştir. Sınıflandırma, yukarıda listelenen ara dosyalardan yeniden üretilebilir.</footer></main></body></html>`)

	htmlYol := filepath.Join(*klasor, "rapor_"+*etiket+".html")
	if err := os.WriteFile(htmlYol, []byte(b.String()), 0o644); err != nil {
		return err
	}
	ozetMetin := fmt.Sprintf("%s eksik ID: %s kesin silinmiş, %s şüpheli, %s oluşmamış, %s belirsiz",
		sayiYaz(toplamEksik), sayiYaz(grupAdet["kesin"]), sayiYaz(grupAdet["supheli"]), sayiYaz(grupAdet["olusmadi"]), sayiYaz(grupAdet["belirsiz"]))
	gunlukYaz(*klasor, ozetMetin)
	fmt.Printf("%s\n→ %s\n→ %s\n", ozetMetin, sinYol, htmlYol)
	return nil
}

func birlestir(a []aralik) []aralik {
	if len(a) == 0 {
		return a
	}
	sort.Slice(a, func(i, j int) bool { return a[i].bas < a[j].bas })
	out := []aralik{a[0]}
	for _, x := range a[1:] {
		s := &out[len(out)-1]
		if x.bas <= s.son+1 {
			s.son = maxI(s.son, x.son)
		} else {
			out = append(out, x)
		}
	}
	return out
}

func siniflandirmaYaz(yol string, seg []segment, loc *time.Location) error {
	f, err := os.Create(yol)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	fmt.Fprintf(w, "# dbiz=%s\n# tur=siniflandirma\n", surum)
	fmt.Fprintln(w, "bas\tson\tadet\tsinif\tdegerlendirme\tonceki_kayit\tsonraki_kayit\tnot")
	for _, s := range seg {
		sn := sinifBul(s.kod)
		fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", s.bas, s.son, s.adet(), s.kod, sn.kesinlik,
			zamanYaz(s.gap.onceki, loc), zamanYaz(s.gap.sonraki, loc), temizle(s.not))
	}
	return w.Flush()
}

const raporCSS = `
:root{--zemin:#f6f7f9;--kagit:#fff;--yazi:#1b2430;--soluk:#5b6675;--cizgi:#dde2e8;--lacivert:#0c3557;
--kesin:#b42318;--kesin-z:#fdecea;--supheli:#b54708;--supheli-z:#fef4e6;--olusmadi:#2e6b4f;--olusmadi-z:#e8f3ed;--belirsiz:#5b6675;--belirsiz-z:#eef1f4}
@media (prefers-color-scheme:dark){:root{--zemin:#0f141a;--kagit:#161d25;--yazi:#e6eaef;--soluk:#9aa5b3;--cizgi:#2a3440;--lacivert:#8db8e0;
--kesin:#f97066;--kesin-z:#3a1714;--supheli:#fdb022;--supheli-z:#3a2a10;--olusmadi:#6cc4a0;--olusmadi-z:#13301f;--belirsiz:#9aa5b3;--belirsiz-z:#1e252d}}
*{box-sizing:border-box}body{margin:0;background:var(--zemin);color:var(--yazi);font:14px/1.5 "Segoe UI",system-ui,-apple-system,sans-serif}
main{max-width:1180px;margin:0 auto;padding:32px 16px 64px}
header{border-bottom:3px solid var(--lacivert);padding-bottom:16px;margin-bottom:8px}
.ust{color:var(--soluk);font-size:12px;letter-spacing:.06em;text-transform:uppercase}
h1{margin:4px 0 16px;font-size:26px;color:var(--lacivert)}
h2{font-size:17px;color:var(--lacivert);margin:32px 0 12px;padding-bottom:6px;border-bottom:1px solid var(--cizgi)}
h3{font-size:14px;margin:20px 0 8px}
.kunye{display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:8px 24px;margin:0}
.kunye div{min-width:0}.kunye dt{color:var(--soluk);font-size:12px}.kunye dd{margin:0;font-weight:600;overflow-wrap:anywhere}
.kartlar{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px}
.kart{background:var(--kagit);border:1px solid var(--cizgi);border-top:4px solid var(--lacivert);border-radius:6px;padding:14px}
.kart.kesin{border-top-color:var(--kesin)}.kart.supheli{border-top-color:var(--supheli)}.kart.olusmadi{border-top-color:var(--olusmadi)}.kart.belirsiz{border-top-color:var(--belirsiz)}
.kn{font-size:26px;font-weight:700;font-variant-numeric:tabular-nums}.ke{font-weight:600}.ka{color:var(--soluk);font-size:12px}
.uyari{margin-top:16px;background:var(--supheli-z);border-left:4px solid var(--supheli);padding:10px 14px;border-radius:4px}.uyari ul{margin:6px 0 0;padding-left:18px}
table{width:100%;border-collapse:collapse;background:var(--kagit);border:1px solid var(--cizgi);font-size:13px;display:block;overflow-x:auto}
thead,tbody{display:table;width:100%;border-collapse:collapse}
th,td{text-align:left;padding:7px 10px;border-bottom:1px solid var(--cizgi);vertical-align:top}
th{background:var(--zemin);font-size:12px;color:var(--soluk);font-weight:600;white-space:nowrap}
.s{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
tr.bos td{color:var(--soluk)}
.kucuk{font-size:12px}.hash{font-family:Consolas,monospace;font-size:11px;overflow-wrap:anywhere;min-width:200px}
.kod{font-family:Consolas,monospace;font-size:11px;overflow-wrap:anywhere}
.rozet{display:inline-block;padding:1px 8px;border-radius:10px;font-size:12px;font-weight:700;white-space:nowrap}
.rozet.kesin{background:var(--kesin-z);color:var(--kesin)}.rozet.supheli{background:var(--supheli-z);color:var(--supheli)}
.rozet.olusmadi{background:var(--olusmadi-z);color:var(--olusmadi)}.rozet.belirsiz{background:var(--belirsiz-z);color:var(--belirsiz)}
.sessiz{color:var(--soluk)}.notlar li{margin:2px 0}
pre{background:var(--kagit);border:1px solid var(--cizgi);padding:10px;font-size:11px;overflow-x:auto;white-space:pre-wrap}
.yontem li{margin:6px 0}footer{margin-top:40px;color:var(--soluk);font-size:12px;border-top:1px solid var(--cizgi);padding-top:12px}
@media print{body{background:#fff}main{max-width:none;padding:0}table{display:table}section{break-inside:avoid-page}}
`
