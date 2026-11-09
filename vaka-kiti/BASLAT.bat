@echo off
chcp 65001 <nul >nul
title dbiz - Silinmis Kayit Incelemesi
cd /d "%~dp0"
powershell -NoProfile -Command "Unblock-File -LiteralPath '%~dp0dbiz.exe'" <nul >nul 2>&1

findstr /c:"SIFRE" "%~dp0vaka.txt" >nul
if not errorlevel 1 (
  echo ============================================================
  echo  ADIM 0 - KESIF: tablolar, sutunlar ve binlog dosyalari bulunuyor
  echo  Sadece MySQL baglanti bilgileri sorulacak.
  echo ============================================================
  "%~dp0dbiz.exe" kesfet --yaz "%~dp0vaka.txt"
  if errorlevel 1 (
    echo.
    echo  Kesif basarisiz. Yukaridaki HATA satirini okuyun ve tekrar deneyin.
    pause
    exit /b 1
  )
  echo.
  echo  vaka.txt Not Defteri'nde aciliyor. Bilgileri kontrol edin,
  echo  gerekirse duzeltip KAYDEDIN ve Not Defteri'ni KAPATIN.
  notepad "%~dp0vaka.txt"
)

echo ============================================================
echo  ADIM 1/2 - KONTROL (hicbir seyi degistirmez)
echo ============================================================
"%~dp0dbiz.exe" vaka --ayar "%~dp0vaka.txt" --kontrol
if errorlevel 1 (
  echo.
  echo  Kontrol basarisiz. Yukaridaki HATA satirini okuyun,
  echo  vaka.txt dosyasini duzeltip kaydedin ve BASLAT.bat'i tekrar calistirin.
  echo.
  pause
  exit /b 1
)

echo.
echo ============================================================
echo  Yukaridaki bilgileri kontrol edin:
echo   - id ve zaman sutunu dogru mu?
echo   - binlog dosya sayisi beklenen gibi mi?
echo   - tahmini sure
echo ============================================================
set "CEVAP="
set /p CEVAP=Incelemeyi baslatmak icin E yazip Enter'a basin (vazgecmek icin H): 
if /i not "%CEVAP%"=="E" (
  echo Vazgecildi. Hicbir islem yapilmadi.
  pause
  exit /b 0
)

echo.
echo ============================================================
echo  ADIM 2/2 - INCELEME (bilgisayari kapatmayin, uykuya almayin)
echo ============================================================
"%~dp0dbiz.exe" vaka --ayar "%~dp0vaka.txt"
if errorlevel 1 (
  echo.
  echo  Inceleme bir hatayla durdu. Yukaridaki HATA satirini gonderin.
  echo  Sorunu giderdikten sonra BASLAT.bat'i tekrar calistirin; biten adimlar atlanir.
) else (
  echo.
  echo  TAMAMLANDI. Rapor "sonuc" klasorunde: rapor_*.html dosyasina cift tiklayin.
  if exist sonuc start "" "sonuc"
)
echo.
pause