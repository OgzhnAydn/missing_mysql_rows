// dbiz — MySQL silinmiş kayıt incelemesi (boşluk tarama, binlog korelasyonu,
// yedek karşılaştırma, kurtarma ve adli rapor). Tek dosya, bağımlılıksız çalışır.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "time/tzdata"
)

const surum = "0.1.0"

const kullanim = `dbiz ` + surum + ` — MySQL silinmiş kayıt incelemesi

KULLANIM
  dbiz <komut> [seçenekler]

EN KOLAY YOL
  kesfet      Yedek tablolarını, sütunları ve binlog'ları bulup vaka.txt'yi doldurur
  vaka        Tüm adımları tek ayar dosyasıyla çalıştırır (dbiz vaka --ornek)

KOMUTLAR (tek tek)
  kontrol     Tabloyu tanır: sütunlar, index durumu, süre tahmini
  muhurle    Orijinal dosyaların SHA-256 özetini alır (her şeyden önce)
  bosluk      Tablodaki ID boşluklarını aralık olarak çıkarır
  binlog      Binlog dosyalarını ayrıştırır (mysqlbinlog gerekmez)
  karsilastir İki yedeğin boşluk listesini karşılaştırır → yedekler arası silinenler
  kurtar      Silinen aralıkların tam satırlarını eski yedekten CSV'ye döker
  rapor       Her boşluğu kanıt düzeyine göre sınıflandırır, HTML rapor üretir

Her komutun seçenekleri için: dbiz <komut> -h
Tüm çıktılar --klasor ile verilen klasöre yazılır (varsayılan: dbiz_cikti).
Araç veritabanına yalnızca okuma amaçlı bağlanır; hiçbir şey yazmaz.

ÖRNEK
  dbiz muhurle D:\delil\mysql-bin.* D:\delil\datadir
  dbiz bosluk --dsn "root:sifre@tcp(127.0.0.1:3306)/" --tablo arsiv.islem --zaman created_at --etiket 2024
  dbiz binlog --tablo arsiv.islem D:\delil\mysql-bin.0*
  dbiz rapor --etiket 2024
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(kullanim)
		os.Exit(2)
	}
	komutlar := map[string]func([]string) error{
		"muhurle":     muhurle,
		"bosluk":      bosluk,
		"binlog":      binlogKomut,
		"karsilastir": karsilastir,
		"kurtar":      kurtar,
		"rapor":       rapor,
		"kontrol":     kontrol,
		"kesfet":      kesfet,
		"vaka":        vaka,
	}
	k, ok := komutlar[os.Args[1]]
	if !ok {
		if os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "yardim" {
			fmt.Print(kullanim)
			return
		}
		fmt.Fprintf(os.Stderr, "bilinmeyen komut: %s\n\n%s", os.Args[1], kullanim)
		os.Exit(2)
	}
	if err := k(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "HATA:", err)
		os.Exit(1)
	}
}

var sifreDeseni = regexp.MustCompile(`:[^:@/]*@`)

// gunlukYaz her komut çalışmasını klasördeki işlem günlüğüne ekler (delil zinciri).
// DSN içindeki parola maskelenir.
func gunlukYaz(klasor, not string) {
	args := make([]string, len(os.Args))
	for i, a := range os.Args {
		args[i] = sifreDeseni.ReplaceAllString(a, ":***@")
	}
	host, _ := os.Hostname()
	satir := fmt.Sprintf("%s\tdbiz %s\t%s\t%s\t%s\n",
		time.Now().UTC().Format(time.RFC3339), surum, host, strings.Join(args, " "), not)
	f, err := os.OpenFile(filepath.Join(klasor, "islem_gunlugu.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(satir)
}

func klasorHazirla(klasor string) error {
	return os.MkdirAll(klasor, 0o755)
}
