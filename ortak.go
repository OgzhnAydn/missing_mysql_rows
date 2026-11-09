package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// aralik kapalı bir ID aralığıdır: [bas, son].
type aralik struct{ bas, son int64 }

func (a aralik) adet() int64 { return a.son - a.bas + 1 }

// varOlanlar, [min,max] içinde boşluklar dışında kalan (mevcut) aralıkları döner.
// boşluklar sıralı ve çakışmasız olmalı.
func varOlanlar(bosluklar []aralik, min, max int64) []aralik {
	var out []aralik
	cur := min
	for _, b := range bosluklar {
		if b.son < cur {
			continue
		}
		if b.bas > max {
			break
		}
		if b.bas > cur {
			out = append(out, aralik{cur, minI(b.bas-1, max)})
		}
		cur = b.son + 1
	}
	if cur <= max {
		out = append(out, aralik{cur, max})
	}
	return out
}

// tumleyen, sıralı aralıkların [lo,hi] içindeki tümleyenini döner.
func tumleyen(a []aralik, lo, hi int64) []aralik { return varOlanlar(a, lo, hi) }

// kesisim iki sıralı aralık listesinin kesişimini döner.
func kesisim(a, b []aralik) []aralik {
	var out []aralik
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo := maxI(a[i].bas, b[j].bas)
		hi := minI(a[i].son, b[j].son)
		if lo <= hi {
			out = append(out, aralik{lo, hi})
		}
		if a[i].son < b[j].son {
			i++
		} else {
			j++
		}
	}
	return out
}

func toplamAdet(a []aralik) int64 {
	var t int64
	for _, x := range a {
		t += x.adet()
	}
	return t
}

// icerenIndeks, id'yi içeren aralığın indeksini döner (yoksa -1).
func icerenIndeks(a []aralik, id int64) int {
	i := sort.Search(len(a), func(i int) bool { return a[i].son >= id })
	if i < len(a) && a[i].bas <= id {
		return i
	}
	return -1
}

func minI(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxI(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// tsvDosya, "# anahtar=deger" başlık satırları ve sekmeyle ayrılmış veri satırlarından oluşur.
type tsvDosya struct {
	meta    map[string]string
	baslik  []string
	satirlar [][]string
}

func tsvOku(yol string) (*tsvDosya, error) {
	f, err := os.Open(yol)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &tsvDosya{meta: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#")), "="); ok {
				if _, var_ := t.meta[k]; var_ {
					t.meta[k] += "\n" + v
				} else {
					t.meta[k] = v
				}
			}
			continue
		}
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if t.baslik == nil {
			t.baslik = cols
			continue
		}
		t.satirlar = append(t.satirlar, cols)
	}
	return t, sc.Err()
}

// tsvAkis büyük TSV dosyalarını belleğe almadan satır satır işler.
func tsvAkis(yol string, fn func(baslik map[string]int, cols []string) error) error {
	f, err := os.Open(yol)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var baslik map[string]int
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if baslik == nil {
			baslik = map[string]int{}
			for i, c := range cols {
				baslik[c] = i
			}
			continue
		}
		if err := fn(baslik, cols); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (t *tsvDosya) sutun(ad string) int {
	for i, b := range t.baslik {
		if b == ad {
			return i
		}
	}
	return -1
}

// bosluklariOku, bosluklar_<etiket>.tsv dosyasını aralık listesi olarak okur.
func bosluklariOku(yol string) (*tsvDosya, []aralik, error) {
	t, err := tsvOku(yol)
	if err != nil {
		return nil, nil, err
	}
	ib, is := t.sutun("bas"), t.sutun("son")
	if ib < 0 || is < 0 {
		return nil, nil, fmt.Errorf("%s: bas/son sütunu yok", yol)
	}
	out := make([]aralik, 0, len(t.satirlar))
	for _, s := range t.satirlar {
		b, _ := strconv.ParseInt(s[ib], 10, 64)
		e, _ := strconv.ParseInt(s[is], 10, 64)
		out = append(out, aralik{b, e})
	}
	return t, out, nil
}

func sha256Dosya(yol string) (string, int64, error) {
	f, err := os.Open(yol)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// temizle, değeri tek satırlık TSV hücresine uygun hale getirir.
func temizle(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
}

// metinDeger, ham bayt değeri okunabilir metne çevirir; UTF-8 değilse onaltılık yazar.
func metinDeger(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return "0x" + hex.EncodeToString(b)
}

func zamanYaz(unix int64, loc *time.Location) string {
	if unix <= 0 {
		return "—"
	}
	return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04:05")
}

func sureYaz(sn int64) string {
	if sn < 0 {
		return "—"
	}
	d := time.Duration(sn) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d sn", sn)
	case d < time.Hour:
		return fmt.Sprintf("%.1f dk", d.Minutes())
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f sa", d.Hours())
	default:
		return fmt.Sprintf("%.1f gün", d.Hours()/24)
	}
}

func sayiYaz(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// konumYukle --tz değerini çözer.
func konumYukle(tz string) (*time.Location, error) {
	if tz == "" || strings.EqualFold(tz, "local") {
		return time.Local, nil
	}
	return time.LoadLocation(tz)
}

// tabloAyir "db.tablo" biçimini parçalar.
func tabloAyir(s string) (db, tablo string) {
	if d, t, ok := strings.Cut(s, "."); ok {
		return d, t
	}
	return "", s
}

func tirnakla(s string) string {
	db, t := tabloAyir(s)
	if db == "" {
		return "`" + strings.ReplaceAll(t, "`", "``") + "`"
	}
	return "`" + strings.ReplaceAll(db, "`", "``") + "`.`" + strings.ReplaceAll(t, "`", "``") + "`"
}

func sutunTirnak(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
