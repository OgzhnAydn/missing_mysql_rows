# dbiz — MySQL silinmiş kayıt incelemesi

> ## ⬇ Aracı kullanacaksanız
>
> **Bu sayfadaki kodu indirmeyin.** Sağdaki **Releases** bölümünden en son sürümün
> **`vaka-kiti.zip`** dosyasını indirin.
>
> 1. ZIP'e sağ tıklayın → **Tümünü ayıkla** (örn. `D:\dbiz`)
> 2. **`BASLAT.bat`**'a çift tıklayın
> 3. Yalnızca MySQL kullanıcı adı ve parolası sorulur. Tablolar, sütunlar ve binlog
>    dosyaları otomatik bulunur, size onaylatılır, inceleme başlar.
> 4. Bittiğinde açılan `sonuc` klasöründeki `rapor_*.html` rapordur.
>
> Ayrıntılar zip içindeki `KILAVUZ.txt` dosyasında. Kurulum gerekmez.

Tek dosyalık araç: Python, `mysqlbinlog` ya da kurulum gerektirmez. Windows için `dbiz.exe`, Linux için `dbiz-linux`.
Veritabanına yalnızca **salt-okuma** oturumuyla bağlanır, hiçbir şey yazmaz.

## Ne yapar

Her eksik ID'yi kanıt gücüne göre sınıflandırır:

| Sınıf | Değerlendirme | Dayanak |
|---|---|---|
| K1 | KESİN SİLİNDİ | Eski yedekte var, incelenen yedekte yok. İçerik eski yedekte (`kurtar`) |
| K2 | KESİN SİLİNDİ | Binlog'da silme kaydı var. Silinen satırın tam görüntüsü binlog'dan çıkarılır |
| K3 | ŞÜPHELİ | Binlog'da eklenmiş, silme kaydı yok (kapsam dışı ya da `sql_log_bin=0` ile silme) |
| K4 | MUHTEMELEN OLUŞMADI | Komşu kayıtlar arasında `ALTER TABLE … AUTO_INCREMENT` var |
| K5 | OLUŞMADI (KOŞULLU) | Kesintisiz binlog kapsamında ama ekleme kaydı yok → rollback |
| K6 | MUHTEMELEN OLUŞMADI | Komşu kayıtlar arasındaki süre bu kadar kayıt için çok kısa |
| K7 | SİLİNMİŞ OLABİLİR | Süre yeterince uzun, ama doğrudan kanıt yok |
| K8 | BELİRSİZ | Veri yetersiz |

## Vaka kiti nasıl çalışır

`BASLAT.bat` sırasıyla şunları çalıştırır:

```
dbiz.exe kesfet --yaz vaka.txt          # yalnızca vaka.txt doldurulmamışsa
dbiz.exe vaka --ayar vaka.txt --kontrol
dbiz.exe vaka --ayar vaka.txt
```

`kesfet` bağlantı bilgilerini sorar. Sonra sunucudaki ID sütunlu tablolardan en olası olanı bulur: farklı
veritabanlarında aynı adla duran tablolar yedek kabul edilir ve son ID'ye göre eskiden yeniye sıralanır.
Bunun yanında ID ve zaman sütununu seçer. Disklerde ilk 4 baytı `0xFE 'bin'` olan `*.000123`
dosyalarını arar; sunucunun kendi binlog'larını atlar. Binlog'daki tablo eşleme olaylarından tablonun
canlı sistemdeki adını çıkarır.

`kontrol` tabloyu tanır (sütunlar, ID birincil anahtar mı, zaman sütunu adayları, süre tahmini).
ID birincil anahtarsa sıralı okuma, değilse **tarama modu** kullanılır: tablo iki kez sırasız okunur,
ID'ler bit haritasında tutulur (400M ID ≈ 50 MB); sıralama ve rastgele disk okuması yoktur.

## Adım adım (komutlar tek tek)

**0. Orijinallere dokunmayın.** Önce mühürleyin, sonra kopyada çalışın. MySQL'i orijinal datadir üzerinde başlatmayın:
açılışta purge çalışır ve silinmiş olarak işaretlenmiş kayıtları kalıcı olarak temizler.

```
dbiz muhurle --klasor vaka D:\delil\binlog D:\delil\datadir
```

**1. Her yedek için boşluk taraması** (zaman sütunu varsa mutlaka verin):

```
dbiz bosluk --klasor vaka --dsn "root:SIFRE@tcp(127.0.0.1:3306)/" --tablo yedek2023.tablo --zaman created_at --etiket 2023
dbiz bosluk --klasor vaka --dsn "root:SIFRE@tcp(127.0.0.1:3306)/" --tablo yedek2024.tablo --zaman created_at --etiket 2024
```

Ekranda yüzde, satır/sn ve kalan süre görünür. 400 milyon satırda süre disk hızına bağlıdır; ilk dakikadaki tahmin güvenilirdir.
Sunucuya bağlanılamıyorsa: `mysql --quick -N -e "SELECT id, created_at FROM db.t ORDER BY id" > ids.tsv` ve `dbiz bosluk --girdi ids.tsv ...`

**2. Binlog'lar** (dosya sırası adlarından çözülür, eksik dosya varsa raporda "ZİNCİR KOPUK" çıkar):

```
dbiz binlog --klasor vaka --tablo db.tablo D:\delil\binlog\mysql-bin.0*
```

`--id-sutun`: ID tablonun ilk sütunu değilse sırasını verin.

**3. Yedek karşılaştırma ve kurtarma:**

```
dbiz karsilastir --klasor vaka --eski 2023 --yeni 2024
dbiz kurtar --klasor vaka --dsn "...eski yedek..." --tablo yedek2023.tablo --aralik silinen_2023_2024.tsv
```

**4. Rapor:**

```
dbiz rapor --klasor vaka --etiket 2024 --baslik "Vaka adı"
```

## Çıktılar (`--klasor` içinde)

| Dosya | İçerik |
|---|---|
| `muhur.tsv` | Orijinal dosyaların SHA-256 özetleri |
| `bosluklar_<etiket>.tsv` | Boşluk aralıkları + komşu zamanlar + yerel kayıt hızı |
| `olaylar.tsv` | Tablonun binlog olayları: tür, ID, zaman, dosya, konum, thread |
| `binlog_satir_goruntuleri.csv` | Silinen satırların ve UPDATE önce/sonra hallerinin tam içeriği |
| `sorgu_olaylari.tsv` | Tabloyla ilgili DDL ve STATEMENT biçimli sorgular |
| `binlog_kapsam.tsv` | Dosya başına hash, zaman aralığı, zincir sürekliliği |
| `silinen_*.tsv`, `beliren_*.tsv` | Yedekler arası silinen / sonradan elle eklenen ID'ler |
| `kurtarilan_*.csv` | Eski yedekten çıkarılmış tam satırlar |
| `siniflandirma_<etiket>.tsv` | Her aralığın sınıfı |
| `rapor_<etiket>.html` | Çevrimdışı açılan adli rapor (yazdırılabilir) |
| `islem_gunlugu.txt` | Çalıştırılan her komut (parola maskeli) |

## Sınırlar

- Binlog kullanıcı adı ve IP adresi **içermez**. Kişi tespiti için general_log, audit log ya da uygulama günlükleri gerekir.
- `sql_log_bin=0` ile binlog'a yazılmadan yapılan ekleme+silme bu yöntemle ayırt edilemez.
- STATEMENT biçimli binlog'da tek tek ID yoktur; sorgu metni `sorgu_olaylari.tsv`'ye düşer.
- Süre analizi (K6/K7) dolaylı göstergedir; tek başına kanıt değildir.
- Henüz yok: InnoDB sayfalarından silinmiş kayıt oyma (`.ibd` carving), PostgreSQL/MSSQL.

## Derleme ve sürüm çıkarma

```
go build -ldflags "-s -w" -o dbiz.exe .
GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o dbiz-linux .
```

Sürüm paketi: `dbiz.exe`'yi `vaka-kiti/` içine kopyala, SHA-256'sını `dbiz.exe.sha256.txt`'ye yaz,
klasörü `vaka-kiti.zip` olarak sıkıştır ve GitHub'da **Releases → Draft a new release** ile
ek dosya olarak yükle. Derleme çıktıları depoya girmez (`.gitignore`).
