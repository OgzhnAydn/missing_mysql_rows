package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// karsilastir iki yedeğin boşluk listelerinden, eskide var olup yenide olmayan
// ID'leri (yedekler arası kesin silinenler) ve tersini (sonradan beliren) çıkarır.
// Tam ID listesine gerek yoktur; aralık aritmetiğiyle çalışır.
func karsilastir(args []string) error {
	fsx := flag.NewFlagSet("karsilastir", flag.ExitOnError)
	eski := fsx.String("eski", "", "eski yedeğin etiketi (ör. 2023)")
	yeni := fsx.String("yeni", "", "yeni yedeğin etiketi (ör. 2024)")
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz karsilastir --eski 2023 --yeni 2024")
		fmt.Fprintln(os.Stderr, "Önce her iki yedek için 'dbiz bosluk --etiket ...' çalıştırılmış olmalı.")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if *eski == "" || *yeni == "" {
		fsx.Usage()
		return fmt.Errorf("--eski ve --yeni gerekli")
	}
	et, eb, err := bosluklariOku(filepath.Join(*klasor, "bosluklar_"+*eski+".tsv"))
	if err != nil {
		return err
	}
	yt, yb, err := bosluklariOku(filepath.Join(*klasor, "bosluklar_"+*yeni+".tsv"))
	if err != nil {
		return err
	}
	eMin, eMax := atoi64(et.meta["min"]), atoi64(et.meta["max"])
	yMin, yMax := atoi64(yt.meta["min"]), atoi64(yt.meta["max"])

	eskideVar := varOlanlar(eb, eMin, eMax)
	yenideVar := varOlanlar(yb, yMin, yMax)
	// Eski aralığın tamamında, yenide bulunmayanlar (yeni min'in altı ve max'ın üstü dahil).
	yenideYok := tumleyen(yenideVar, eMin, eMax)
	silinen := kesisim(eskideVar, yenideYok)
	// Eskinin [min,max] aralığında boşluk olup yenide var olan: sonradan elle ID verilerek eklenmiş.
	beliren := kesisim(eb, yenideVar)

	yaz := func(ad, tur string, a []aralik) (string, error) {
		yol := filepath.Join(*klasor, ad)
		f, err := os.Create(yol)
		if err != nil {
			return "", err
		}
		defer f.Close()
		w := bufio.NewWriter(f)
		fmt.Fprintf(w, "# dbiz=%s\n# tur=%s\n# eski=%s\n# yeni=%s\n# tablo=%s\n# adet=%d\n# olusturma_utc=%s\n",
			surum, tur, *eski, *yeni, et.meta["tablo"], toplamAdet(a), time.Now().UTC().Format(time.RFC3339))
		fmt.Fprintln(w, "bas\tson\tadet")
		for _, x := range a {
			fmt.Fprintf(w, "%d\t%d\t%d\n", x.bas, x.son, x.adet())
		}
		return yol, w.Flush()
	}
	ys, err := yaz("silinen_"+*eski+"_"+*yeni+".tsv", "yedekler_arasi_silinen", silinen)
	if err != nil {
		return err
	}
	yb2, err := yaz("beliren_"+*eski+"_"+*yeni+".tsv", "sonradan_beliren", beliren)
	if err != nil {
		return err
	}
	ozet := fmt.Sprintf("%s → %s: %s ID silinmiş (%d aralık), %s ID sonradan belirmiş",
		*eski, *yeni, sayiYaz(toplamAdet(silinen)), len(silinen), sayiYaz(toplamAdet(beliren)))
	gunlukYaz(*klasor, ozet)
	fmt.Printf("%s\n→ %s\n→ %s\n", ozet, ys, yb2)
	if len(silinen) > 0 {
		fmt.Printf("\nSilinen satırların tam içeriği eski yedekte duruyor. Çıkarmak için:\n  dbiz kurtar --dsn <%s yedeğinin DSN'i> --tablo %s --aralik %s\n",
			*eski, et.meta["tablo"], filepath.Base(ys))
	}
	return nil
}
