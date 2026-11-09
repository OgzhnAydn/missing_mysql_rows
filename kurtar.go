package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// kurtar, verilen ID aralıklarındaki tam satırları (genellikle eski bir yedekten)
// salt-okuma bağlantısıyla CSV'ye döker.
func kurtar(args []string) error {
	fsx := flag.NewFlagSet("kurtar", flag.ExitOnError)
	dsn := fsx.String("dsn", "", "satırların bulunduğu (eski) yedeğin bağlantısı")
	tablo := fsx.String("tablo", "", "tablo (db.tablo)")
	idSutun := fsx.String("id", "id", "ID sütunu")
	aralikDosya := fsx.String("aralik", "", "aralık dosyası (bas/son sütunlu TSV, ör. silinen_2023_2024.tsv)")
	parca := fsx.Int64("parca", 100000, "sorgu başına en fazla ID")
	klasor := fsx.String("klasor", "dbiz_cikti", "çıktı klasörü")
	fsx.Usage = func() {
		fmt.Fprintln(os.Stderr, "dbiz kurtar --dsn DSN --tablo db.tablo --aralik silinen_2023_2024.tsv")
		fsx.PrintDefaults()
	}
	fsx.Parse(args)
	if *dsn == "" || *tablo == "" || *aralikDosya == "" {
		fsx.Usage()
		return fmt.Errorf("--dsn, --tablo ve --aralik gerekli")
	}
	yol := *aralikDosya
	if _, err := os.Stat(yol); os.IsNotExist(err) {
		yol = filepath.Join(*klasor, *aralikDosya)
	}
	_, araliklar, err := bosluklariOku(yol)
	if err != nil {
		return err
	}

	cfg, err := mysql.ParseDSN(*dsn)
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return err
	}

	ad := "kurtarilan_" + strings.TrimSuffix(filepath.Base(yol), filepath.Ext(yol)) + ".csv"
	cikti := filepath.Join(*klasor, ad)
	f, err := os.Create(cikti)
	if err != nil {
		return err
	}
	defer f.Close()
	f.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(f)
	defer w.Flush()

	q := fmt.Sprintf("SELECT * FROM %s WHERE %s BETWEEN ? AND ? ORDER BY %s", tirnakla(*tablo), sutunTirnak(*idSutun), sutunTirnak(*idSutun))
	var toplam int64
	baslikYazildi := false
	for _, a := range araliklar {
		for bas := a.bas; bas <= a.son; bas += *parca {
			son := minI(bas+*parca-1, a.son)
			rows, err := conn.QueryContext(ctx, q, bas, son)
			if err != nil {
				return err
			}
			cols, _ := rows.Columns()
			if !baslikYazildi {
				w.Write(cols)
				baslikYazildi = true
			}
			vals := make([]sql.RawBytes, len(cols))
			ptr := make([]any, len(cols))
			for i := range vals {
				ptr[i] = &vals[i]
			}
			for rows.Next() {
				if err := rows.Scan(ptr...); err != nil {
					rows.Close()
					return err
				}
				rec := make([]string, len(cols))
				for i, v := range vals {
					if v == nil {
						rec[i] = "NULL"
					} else {
						rec[i] = metinDeger(v)
					}
				}
				w.Write(rec)
				toplam++
			}
			rows.Close()
		}
		fmt.Fprintf(os.Stderr, "\r  %s satır", sayiYaz(toplam))
	}
	fmt.Fprintln(os.Stderr)
	beklenen := toplamAdet(araliklar)
	ozet := fmt.Sprintf("%s satır kurtarıldı (aralıklarda beklenen %s)", sayiYaz(toplam), sayiYaz(beklenen))
	gunlukYaz(*klasor, ozet)
	fmt.Printf("%s\n→ %s\n", ozet, cikti)
	if toplam < beklenen {
		fmt.Println("Not: Eksik kalanlar bu yedekte de yok; daha eski bir yedek ya da binlog satır görüntüleri denenebilir.")
	}
	return nil
}
