package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"os"
	"strings"
	"time"
)

// bitHarita, [min,max] aralığındaki her ID için bir bit tutar: 400 milyon ID ≈ 50 MB.
type bitHarita []uint64

func (b bitHarita) koy(i int64)      { b[i>>6] |= 1 << (uint64(i) & 63) }
func (b bitHarita) var_(i int64) bool { return b[i>>6]&(1<<(uint64(i)&63)) != 0 }

// tarama, ID'nin birincil anahtar olmadığı (ya da index'in güvenilmez olduğu) durumda
// sıralama ve rastgele okuma yapmadan çalışır:
//  1. geçiş: tablo sırasız okunur, mevcut ID'ler bit haritasına işlenir → boşluklar.
//  2. geçiş (zaman sütunu varsa): yalnızca boşluk komşularının ve yerel hız için gereken
//     kayıtların zamanı toplanır.
func (t *boslukTarayici) tarama(conn *sql.Conn, tablo, idSutun, zamanSutun string, mn, mx, tahmin int64) (string, error) {
	ctx := context.Background()
	n := mx - mn + 1
	if n > 16_000_000_000 {
		return "", fmt.Errorf("ID aralığı çok geniş (%s): tarama modu için bellek yetmez", sayiYaz(n))
	}
	fmt.Fprintf(os.Stderr, "Bit haritası: %.0f MB\n", float64(n)/8/(1<<20))
	bh := make(bitHarita, (n+63)/64)
	tq, iq := tirnakla(tablo), sutunTirnak(idSutun)

	fmt.Fprintln(os.Stderr, "1. geçiş: ID'ler okunuyor")
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s", iq, tq))
	if err != nil {
		return "", err
	}
	basla, sonRapor := time.Now(), time.Now()
	var id, okunan int64
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		if id >= mn && id <= mx {
			bh.koy(id - mn)
		}
		okunan++
		if okunan%100000 == 0 && time.Since(sonRapor) > 2*time.Second {
			gecisIlerleme(basla, okunan, tahmin)
			sonRapor = time.Now()
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	gecisIlerleme(basla, okunan, okunan)
	fmt.Fprintln(os.Stderr)

	t.min, t.max, t.satir, t.basladi = mn, mx, okunan, true
	t.bosluklar = nil
	t.eksik = 0
	// Boşlukları çıkar; tamamen dolu kelimeler hızla atlanır.
	var i int64
	for i < n {
		w := i >> 6
		if i&63 == 0 && bh[w] == ^uint64(0) {
			i += 64
			continue
		}
		if bh.var_(i) {
			i++
			continue
		}
		bas := i
		for i < n && !bh.var_(i) {
			i++
		}
		g := boslukKaydi{aralik: aralik{bas + mn, i - 1 + mn}}
		t.bosluklar = append(t.bosluklar, g)
		t.eksik += g.adet()
	}

	if zamanSutun == "" || len(t.bosluklar) == 0 {
		return "", nil
	}

	// 2. geçiş için gereken ID'ler: komşular ve komşudan yerelPencere kayıt geride olan.
	gerek := make(map[int64]int64, len(t.bosluklar)*3)
	geri := make([]int64, len(t.bosluklar))
	for k, g := range t.bosluklar {
		gerek[g.bas-1] = 0
		gerek[g.son+1] = 0
		geri[k] = geriSay(bh, g.bas-1-mn, yerelPencere-1) + mn
		gerek[geri[k]] = 0
	}
	fmt.Fprintf(os.Stderr, "2. geçiş: %s kaydın zamanı okunuyor\n", sayiYaz(int64(len(gerek))))
	rows, err = conn.QueryContext(ctx, fmt.Sprintf("SELECT %s, %s FROM %s", iq, sutunTirnak(zamanSutun), tq))
	if err != nil {
		return "", err
	}
	zamanTuru := ""
	if ct, err := rows.ColumnTypes(); err == nil && len(ct) > 1 {
		zamanTuru = strings.ToUpper(ct[1].DatabaseTypeName())
	}
	basla, sonRapor = time.Now(), time.Now()
	okunan = 0
	var zb sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&id, &zb); err != nil {
			rows.Close()
			return "", err
		}
		okunan++
		if _, ok := gerek[id]; ok || okunan%64 == 0 {
			z := zamanCoz(zb, zamanTuru, t.loc)
			if ok {
				gerek[id] = z
			}
			if z > 0 {
				if t.ilkZaman == 0 || z < t.ilkZaman {
					t.ilkZaman = z
				}
				if z > t.sonZaman {
					t.sonZaman = z
				}
			}
		}
		if okunan%100000 == 0 && time.Since(sonRapor) > 2*time.Second {
			gecisIlerleme(basla, okunan, t.satir)
			sonRapor = time.Now()
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	gecisIlerleme(basla, okunan, okunan)
	fmt.Fprintln(os.Stderr)

	for k := range t.bosluklar {
		g := &t.bosluklar[k]
		o, s := gerek[g.bas-1], gerek[g.son+1]
		if o > 0 && s > 0 {
			g.onceki, g.sonraki = o, s
		}
		// Komşu ile geri sayılan kayıt arasında kaç mevcut satır olduğu bilinir.
		if gz := gerek[geri[k]]; gz > 0 && o > 0 {
			satirSay := float64(sayAralik(bh, geri[k]-mn, g.bas-1-mn) - 1)
			dt := o - gz
			if dt < 1 {
				dt = 1
			}
			if satirSay > 0 {
				g.yerelHiz = satirSay / float64(dt)
			}
		}
	}
	return zamanTuru, nil
}

// geriSay, i konumundan geriye doğru k mevcut ID sayıp ulaşılan konumu döner (yoksa en baştaki mevcut).
func geriSay(bh bitHarita, i int64, k int) int64 {
	son := i
	p := i - 1
	for p >= 0 && k > 0 {
		if p&63 == 63 {
			// Tam kelime: k'yı aşmıyorsa topluca say.
			w := bh[p>>6]
			if c := bits.OnesCount64(w); c < k {
				if c > 0 {
					k -= c
					son = p - 63 + int64(bits.TrailingZeros64(w))
				}
				p -= 64
				continue
			}
		}
		if bh.var_(p) {
			k--
			son = p
		}
		p--
	}
	return son
}

// sayAralik [a,b] konumlarındaki mevcut ID sayısı.
func sayAralik(bh bitHarita, a, b int64) int64 {
	var c int64
	for i := a; i <= b; {
		if i&63 == 0 && i+63 <= b {
			c += int64(bits.OnesCount64(bh[i>>6]))
			i += 64
			continue
		}
		if bh.var_(i) {
			c++
		}
		i++
	}
	return c
}

func gecisIlerleme(basla time.Time, okunan, toplam int64) {
	gecen := time.Since(basla)
	hiz := float64(okunan) / gecen.Seconds()
	oran := 0.0
	if toplam > 0 {
		oran = float64(okunan) / float64(toplam)
	}
	kalan := "?"
	if hiz > 0 && toplam > okunan {
		kalan = (time.Duration(float64(toplam-okunan)/hiz) * time.Second).Round(time.Second).String()
	} else if toplam <= okunan {
		kalan = "0s"
	}
	fmt.Fprintf(os.Stderr, "\r  ~%%%5.1f | %s satır | %s satır/sn | kalan ~%s      ",
		minF(oran*100, 100), sayiYaz(okunan), sayiYaz(int64(hiz)), kalan)
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
