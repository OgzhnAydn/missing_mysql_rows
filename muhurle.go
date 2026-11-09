package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// muhurle, verilen dosya/klasörlerin SHA-256 özetlerini muhur.tsv'ye ekler.
// İncelemeye başlamadan önce orijinaller üzerinde çalıştırılmalıdır.
func muhurle(args []string) error {
	fsx := flag.NewFlagSet("muhurle", flag.ExitOnError)
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz muhurle [--klasor K] <dosya|klasör|desen> ...")
		fmt.Fprintln(os.Stderr, "Orijinal delil dosyalarının SHA-256 özetini alır. Dosyalar yalnızca okunur.")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if fsx.NArg() == 0 {
		fsx.Usage()
		return fmt.Errorf("en az bir dosya ya da klasör verin")
	}
	if err := klasorHazirla(*klasor); err != nil {
		return err
	}

	var yollar []string
	for _, a := range fsx.Args() {
		eslesen, _ := filepath.Glob(a)
		if len(eslesen) == 0 {
			eslesen = []string{a}
		}
		for _, y := range eslesen {
			st, err := os.Stat(y)
			if err != nil {
				return err
			}
			if !st.IsDir() {
				yollar = append(yollar, y)
				continue
			}
			filepath.WalkDir(y, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					yollar = append(yollar, p)
				}
				return nil
			})
		}
	}
	sort.Strings(yollar)

	cikti := filepath.Join(*klasor, "muhur.tsv")
	yeni := false
	if _, err := os.Stat(cikti); os.IsNotExist(err) {
		yeni = true
	}
	f, err := os.OpenFile(cikti, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if yeni {
		fmt.Fprintln(f, "sha256\tboyut\tdegistirme_utc\tmuhur_utc\tyol")
	}
	for _, y := range yollar {
		st, _ := os.Stat(y)
		h, n, err := sha256Dosya(y)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", y, err)
			continue
		}
		abs, _ := filepath.Abs(y)
		fmt.Fprintf(f, "%s\t%d\t%s\t%s\t%s\n", h, n, st.ModTime().UTC().Format(time.RFC3339),
			time.Now().UTC().Format(time.RFC3339), abs)
		fmt.Printf("%s  %s\n", h, y)
	}
	gunlukYaz(*klasor, fmt.Sprintf("%d dosya mühürlendi", len(yollar)))
	fmt.Printf("\n%d dosya → %s\n", len(yollar), cikti)
	return nil
}
