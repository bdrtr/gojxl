# gojxl — saf Go JPEG XL kod çözücü ve kodlayıcı

`gojxl`, JPEG XL (ISO/IEC 18181) için **cgo ve assembly kullanmayan, saf Go** bir
kod çözücü (decoder) ve kayıpsız/kayıplı kodlayıcıdır (encoder). Hedef: web'deki WASM
tabanlı dönüştürücülerden daha hızlı, tüm çekirdekleri kullanan bir araç.

## Özellikler

**Kod çözücü (tam)**
- Container (`jxlc`/`jxlp` kutuları) ve çıplak codestream
- Entropi: ANS, prefix (Brotli tarzı) kodlar, hibrit tamsayılar, LZ77, bağlam kümeleme
- **Modular**: MA ağaçları, tüm predictor'lar (Weighted dahil), RCT, Palette (delta
  girdileri dahil), Squeeze
- **VarDCT**: tüm 27 dönüşüm tipi (DCT2…DCT256, AFV, IDENTITY, DCT4x8…),
  dequant matrisleri (RAW dahil), CfL, adaptif LF yumuşatma, çok geçişli (progressive) HF
- Restorasyon filtreleri: Gaborish, EPF (1–3 iterasyon)
- Patches, splines, photon noise (libjxl ile bit düzeyinde aynı RNG)
- Upsampling (2×/4×/8×, özel ağırlıklar), YCbCr + kroma alt örnekleme (JPEG rekonstrüksiyonu)
- LF frame'ler, referans frame'ler, tüm blend modları, animasyon, crop'lu katmanlar
- Ekstra kanallar (alpha, premultiplied alpha, spot renkler), 1–32 bit tamsayı/float örnekler
- ICC profil çözme (`UnpredictICC`), PNG çıktısına iCCP olarak gömme, yönlendirme (orientation)
- Hızlı PNG okuyucu (tablo tabanlı inflate, filtre çözme ile boru hattı; büyük dosyalarda image/png'den ~3× hızlı) ve paralel PNG yazıcı; animasyonlu dosyalar APNG olarak yazılır (tüm kareler, süreler, döngü sayısı)
- SSIMULACRA 2 algısal kalite ölçütü (libjxl `tools/ssimulacra2.cc` portu; `gojxl.SSIMULACRA2`)
- Animasyon kodlama: animasyonlu GIF/APNG → kayıpsız ya da kayıplı (`-d`) JXL animasyonu (yalnız değişen bölge kırpılarak
  kodlanır, aynı kareler birleştirilir; `EncodeAnimation`, `DecodeAnimatedInput`)
- **Renk yönetimi** (saf Go CMS): XYB görüntüler orijinal uzaylarında çıkar —
  enum uzaylar (Display P3, Rec.2020/2100, DCI, gamma, doğrusal, PQ, HLG + OOTF)
  için ICC v4 profili sentezlenir; matris/TRC ICC profilleri (RGB ve gri)
  doğrudan hedeflenir; PQ/HLG çıktılarına PNG `cICP` eklenir
- **CMYK → sRGB**: LUT tabanlı ICC profilleri (mft1/mft2/mAB, 4B CLUT,
  lcms tarzı tetrahedral interpolasyon), Black ekstra kanalı ile
- Bozuk girdilere karşı dayanıklı (panik → hata), devasa boyut koruması

**Kayıplı kodlayıcı (VarDCT)**
- sRGB → XYB; DCT8, DCT16, DCT32, dikdörtgen DCT16x8/8x16/32x16/16x32 ve
  bölünmüş DCT4x8/8x4 varblokları, hız-bozulma (RD) maliyetiyle seçilir
- libjxl'in adaptif niceleme haritası, karo başına chroma-from-luma, ters
  Gaborish; LF ve alfa öğrenilmiş MA ağaçlarıyla
- Aynı SSIMULACRA 2 puanında cjxl -e7'den %1–11 küçük (6 test görüntüsü)
- Efor seviyeleri (`-e`, varsayılan 7): 5–6 32x16/16x32'yi, 4 tüm
  dikdörtgenleri, 3 ayrıca DCT32'yi atlar; 2 DCT8 dışında yalnız DCT16'yı,
  1 yalnız DCT8'i dener. libjxl gibi 1–4 uyarlamalı niceleme ve Gaborish
  kullanmaz (bunlar hızlı eforlarda SSIMULACRA 2'ye göre dosyayı %1–15
  büyütüyordu). 5 MP fotoğrafta, d=1, tek çekirdek:

  | | boyut | SSIMULACRA 2 | CPU |
  |---|---|---|---|
  | gojxl -e7 | 589 KB | 84.10 | 1.38 s |
  | gojxl -e4 | 666 KB | 82.53 | 0.92 s |
  | gojxl -e2 | 710 KB | 83.07 | 0.64 s |
  | gojxl -e1 | 733 KB | 81.79 | 0.45 s |
  | cjxl -e7 | 679 KB | 84.02 | 1.34 s |
  | cjxl -e3 | 854 KB | 82.83 | 0.24 s |
  | cjxl -e1 | 912 KB | 82.83 | 0.24 s |

**JPEG → JXL kayıpsız dönüştürme**
- Saf Go JPEG ayrıştırıcı (baseline, progressive, restart işaretleri,
  4:4:4/4:2:2/4:2:0, gri, Adobe RGB JPEG), DCT katsayıları korunur
- JPEG niceleme tabloları RAW dequant matrisi, luma-DC kovalı blok bağlam
  haritası, istatistiğe göre katsayı sırası, öğrenilmiş ağaçla DC, ICC korunur
- Tam sayı-tam JPEG CfL (4:4:4 ve RGB JPEG'lerde büyük kazanç)
- **`jbrd` kutusu**: orijinal JPEG dosyası bayt-birebir geri üretilebilir;
  ICC parçaları codestream'deki profilden kurulur, EXIF/XMP ayrı `Exif`/`xml `
  kutularına (Brotli ile sıkıştırılmış `brob`) taşınır, kalan veri Brotli ile
  sıkıştırılır
- ICC profilleri libjxl tarzı tahminle (etiket tablosu, eğri/CLUT/mluc
  öngörüsü) ve LZ77'li entropi kodlamasıyla saklanır
- **JXL → JPEG geri üretim** gojxl'in kendisinde: `gojxl decode foto.jxl foto.jpg`
  hem gojxl'in hem cjxl'in dosyalarından orijinal JPEG'i bayt-birebir verir
  (saf Go Brotli çözücü, progressive/restart/CfL/Exif/XMP/ICC dahil)
- Test: cjpeg/jpegtran/ImageMagick/ffmpeg/Pillow/Go ile üretilmiş ve sistemdeki
  gerçek JPEG'lerin hepsi (baseline, progressive, restart, 4:2:0/4:2:2/4:4:0/4:4:4,
  gri, RGB, EXIF/XMP/ICC/COM, kuyruk verisi) djxl ile bayt-birebir geri döner
- Boyut cjxl ile aynı düzeyde (ör. 10.3 MB JPEG → 7.93 MB; cjxl 7.95 MB);
  bu 11 MP dosyada süre tek çekirdekte 1.75 s (cjxl 0.74 s), 4 çekirdekte
  0.66 s (cjxl 0.39 s)
- `gojxl encode foto.jpg foto.jxl` JPEG girdisinde varsayılan olarak bunu yapar
  (`-pixels` ile pikselleri kodlar)

**Renk yönetimi (kodlayıcı)**
- Girdinin renk uzayı korunur: PNG `cICP`, `iCCP`, `sRGB`, `gAMA`/`cHRM`
  (PNG 3. sürümdeki öncelik sırasıyla), JPEG ICC profilleri; kütüphanede
  `EncodeOptions.ICC` ile verilebilir
- ICC profili libjxl gibi mümkünse eşdeğer enum kodlamaya çevrilir (sRGB,
  Display P3, BT.2100, DCI-P3, Adobe RGB/ProPhoto gibi özel primerler, gamma,
  doğrusal, 709): profilden beyaz nokta (`chad`/`wtpt`) ve primerler geri
  hesaplanır, eğri ve dönüşüm kodlanmış uzayda 1/255 içinde doğrulanır;
  karşılığı olmayan profiller (ör. ölçülmüş ekran profilleri, LUT tabanlılar)
  olduğu gibi saklanır. Sonuçlar cjxl'in seçimleriyle aynı
- Kayıpsızda pikseller ve profil aynen geri döner (djxl ve gojxl ile doğrulandı)
- Kayıplıda pikseller XYB'den önce profil/enum üzerinden doğrusal sRGB'ye
  çevrilir (geniş gamut dahil); djxl ve gojxl çıktıları kaynak uzayında
  aynı sonucu verir. Matris/TRC olmayan profiller ve PQ/HLG kayıplıda
  `ErrUnsupportedColour` döner (kayıpsız her profili saklar)
- 16-bit girdiler kayıplıda da 16-bit hassasiyetle XYB'ye geçer ve başlık
  16-bit yazılır (önceden 8-bit'e indiriliyordu)
- JXL → JPEG (`ConvertToJPEG`) çözülen piksellerin profilini APP2 ICC olarak yazar

**Metadata (Exif, XMP, yönlendirme)**
- PNG `eXIf`/`iTXt` (XMP, sıkıştırılmış dahil) ve JPEG APP1 Exif/XMP
  JXL `Exif`/`xml ` kutularına taşınır (küçükse Brotli `brob`); kütüphanede
  `EncodeOptions.Exif`/`XMP`
- Exif yönlendirmesi JXL başlığına yazılır (cjxl gibi; JPEG dönüştürmede de):
  telefon fotoğrafları JXL görüntüleyicilerde doğru döner. JPEG geri
  üretimi bayt-birebir kalır
- Çözmede pikseller döndürülür; PNG çıktısına `eXIf` + `iTXt`, JPEG çıktısına
  APP1 yazılır ve Exif'teki yön 1'e çekilir (djxl gibi; çift döndürme olmaz)
- `gojxl info` renk kodlamasını (jxlinfo biçiminde) ve metadata boyutlarını gösterir

**Kayıpsız kodlayıcı (Modular)**
- Efor 1–3: sabit bağlam ağaçları, YCoCg, Gradient/Weighted predictor (hızlı)
- Efor 4+ (varsayılan 4): **MA ağacı öğrenme** — örneklenmiş piksellerden
  entropi kazancına göre açgözlü bölme (histogram çıkarma ile); akış/grup
  kimliği dahil 16 özellik + önceki kanallara referans veren özellikler
  (16–23), 5–14 aday predictor, yaprak başına en iyi predictor
- RCT seçimi (42 varyant tahminle; efor 5+'ta adaylar için küçük ağaç
  öğrenerek), kanal paletleri (global ve **grup başına yerel**), otomatik
  bit derinliği indirimi (ör. 16-bit kapta 8-bit veri)
- Küme başına hibrit uint yapılandırması araması, kümelenmiş ANS
- Otomatik palet (≤1024 renk), gruplar arası paralel kodlama; 8/16-bit, gri/RGB/RGBA

## Kurulum ve kullanım

```sh
go build -o gojxl ./cmd/gojxl                       # taşınabilir, skaler
GOEXPERIMENT=simd go build -o gojxl ./cmd/gojxl     # AVX2/FMA SIMD çekirdekleri (amd64)

./gojxl decode resim.jxl cikti.png          # JXL → PNG (animasyon → APNG)
./gojxl decode -depth 16 resim.jxl c.png    # 16-bit PNG
./gojxl decode -bench 5 resim.jxl           # hız ölçümü
./gojxl decode foto.jxl foto.jpg            # yeniden sıkıştırılmış JPEG'i aynen geri ver
./gojxl encode girdi.png cikti.jxl          # PNG → kayıpsız JXL (efor 4, renk profili korunur)
./gojxl encode anim.gif cikti.jxl           # GIF/APNG animasyonu → JXL animasyonu
./gojxl compare orijinal.png bozuk.jxl      # SSIMULACRA 2 + PSNR (PNG/JPEG/GIF/JXL)
./gojxl encode foto.jpg foto.jxl            # JPEG → JXL kayıpsız yeniden sıkıştırma (~%15-30 küçük)
./gojxl encode -e 1 girdi.png cikti.jxl     # en hızlı kayıpsız
./gojxl encode -d 1 girdi.png cikti.jxl     # kayıplı (d≈1 görsel olarak kayıpsız)
./gojxl encode -e 3 -d 1 girdi.png cikti.jxl  # daha hızlı kayıplı
./gojxl info resim.jxl
```

Kütüphane olarak:

```go
import "gojxl"

img, err := gojxl.Decode(r)                 // image.Image
anim, err := gojxl.DecodeAll(data)          // tüm kareler + süreler
err = gojxl.Encode(w, img, &gojxl.EncodeOptions{Effort: 5})       // kayıpsız
err = gojxl.Encode(w, img, &gojxl.EncodeOptions{Distance: 1.0})   // kayıplı
err = gojxl.Encode(w, img, &gojxl.EncodeOptions{ICC: profil})     // pikseller profil uzayında
jxlData, err := gojxl.ConvertToJXL(dosya, &gojxl.EncodeOptions{Distance: 1}) // PNG/APNG/GIF/JPEG → JXL, renk korunur
jxlData, err := gojxl.TranscodeJPEG(jpegBytes)                     // JPEG → JXL
err = gojxl.ConvertToPNG(w, jxlData, nil)                          // JXL → PNG (akışlı)
err = gojxl.ConvertToJPEG(w, jxlData, 90)                          // JXL → JPEG (orijinal varsa o)
jpegBytes, err := gojxl.ReconstructJPEG(jxlData)                   // JXL → orijinal JPEG
```

`image.Decode` ile de çalışır (paket `init` içinde format kaydı yapar).

## Doğruluk

- Resmi [JPEG XL conformance](https://github.com/libjxl/conformance) setinin
  40 dosyasından 33'ü libjxl (djxl) ile 16-bit çıktıda **112–200 dB PSNR**
  (pratikte birebir); kayıpsız dosyalar bit-birebir.
- Renk uzayları (cjxl ile üretilmiş P3, Rec.2100 PQ/HLG, DCI, ProPhoto,
  Adobe98, gamma, doğrusal): djxl ile 65–116 dB; kayıpsız olanlar bit-birebir.
- ICC profilli XYB dosyalarda (grayscale, patches, progressive) djxl doğrusal
  sRGB çıkarır, gojxl orijinal ICC uzayında; gojxl çıktısı lcms ile sRGB'ye
  çevrildiğinde djxl'in sRGB çıktısıyla 8-bit sınırında (~52 dB) örtüşür.
- CMYK (SWOP profili): lcms perceptual dönüşümüyle 69 dB, djxl ile 46 dB.
- Kodlayıcının ürettiği dosyalar djxl ile bit-birebir çözülür; round-trip
  testleri (`go test ./...`) 1×1'den çok gruplu görüntülere kadar kapsar.
- Bozuk girdiler: JXL çözücü, PNG okuyucu ve JPEG ayrıştırıcı için fuzz
  hedefleri var (`go test -run x -fuzz FuzzDecode .`); bulunan askılar,
  dev ayırmalar ve karesel yavaşlıklar düzeltildi, girdileri
  `testdata/fuzz` altında gerileme testi olarak duruyor.

## Performans (Ryzen 7 8845HS, yük altındaki dizüstü)

Kod çözme (yalnız piksellere), MP/s (yüksek iyi; dosyalar cjxl v0.12 ile):

| Dosya | libjxl 1T | libjxl 4T | gojxl 1T | gojxl 4T |
|---|---|---|---|---|
| 4K foto, VarDCT d1 | 36 | 119 | 20 | 60 |
| JPEG→JXL (1280×1600, 4:2:0) | 74 | 203 | 44 | 89 |
| 4K foto, kayıpsız e7 | 4.8 | 18.1 | 4.2 | 15.7 |

JXL → PNG dönüştürme (dosya okuma, çözme, PNG yazma; duvar süresi):

| Dosya | djxl 1T | gojxl 1T | djxl 4T | gojxl 4T |
|---|---|---|---|---|
| 4K foto, VarDCT d1 | **0.60 s** | 0.75 s | 0.41 s | **0.24 s** |
| JPEG→JXL (1280×1600) | 0.13 s | 0.13 s | 0.09 s | **0.05 s** |
| 4K foto, kayıpsız e7 | **2.10 s** | 2.20 s | 0.82 s | **0.61 s** |

libjxl AVX2 SIMD kullanır; gojxl skaler saf Go'dur ve yalnız çözmede tek
çekirdekte ~1.2–1.8× geridedir, PNG yazımı da paralel olduğundan çok
çekirdekli dönüştürmede öndedir. WASM derlemeleri çoğunlukla tek thread
çalışır ve native'in kabaca yarısı hızındadır.

Çözücü filtreleri (Gaborish, EPF) düzlemleri yerinde işler, tek geçişli
karelerde katsayılar grup grup piksele çevrilir ve `ConvertToPNG` satırları
ara görüntü olmadan PNG yazıcısına verir. Tepe bellek (4 çekirdek): 4K
VarDCT → PNG ~140 MB (djxl ~100 MB), 5 MP kayıplı 86 MB (djxl 67 MB),
5 MP kayıpsız ~97 MB (djxl 73 MB).

Kayıpsız sıkıştırma (bayt, düşük iyi; cjxl v0.12):

| Görüntü | gojxl e3 | gojxl e4 | gojxl e5 | cjxl e3 | cjxl e7 |
|---|---|---|---|---|---|
| 4K foto (3840×2160) | 13 020 959 | 12 199 796 | **12 100 313** | 13 090 634 | 12 580 188 |
| bike (2048×2560 foto) | 5 191 607 | 4 861 168 | **4 798 091** | 5 212 891 | 4 866 475 |
| 640×480 foto kırpıntısı | 463 711 | 438 506 | **436 616** | 465 109 | 446 345 |
| blendmodes (16-bit gradyan, RGBA) | 1 001 071 | 513 533 | **441 742** | 1 068 867 | 627 621 |
| sunset_logo (16-bit, kanal paleti) | 2 171 443 | 792 362 | **721 658** | 3 210 948 | 888 081 |
| delta_palette (sentetik) | 450 762 | 357 819 | **245 463** | 467 687 | 372 699 |
| patches_lossless (ekran görüntüsü) | 144 185 | **63 258** | 65 731 | 213 670 | 107 500 |
| lz77_flower | 280 686 | 267 233 | **261 153** | 281 699 | 270 951 |

Kodlama süresi, efor 4 ve cjxl e7 (duvar süresi):

| | gojxl 1T | cjxl 1T | gojxl 4T | cjxl 4T |
|---|---|---|---|---|
| 640×480 foto | **0.33 s** | 0.44 s | **0.13 s** | 0.39 s |
| bike (5 MP) | **2.7 s** | 4.8 s | **0.81 s** | 1.31 s |

Efor 5–6 daha yavaştır (RCT denemeleri, daha büyük ağaçlar). Tepe bellek
5 MP'de 185 MB (cjxl e7 236 MB): gruplar kendi karolarına ayrılır, görüntü
düzlemleri ve pikseller artıklar önbelleğe alınır alınmaz bırakılır.

## Mimari

```
internal/bitio     LSB-first bit okuyucu/yazıcı, U32/U64/F16
internal/entropy   ANS/prefix/LZ77/bağlam haritası kod çözücü
internal/jxl       header'lar, Modular, VarDCT, filtreler, render, blending
internal/enc       kodlayıcılar: Modular (ağaç öğrenme, ANS), VarDCT, JPEG yeniden sıkıştırma
internal/jpeg      JPEG ayrıştırıcı/yazıcı (baseline + progressive, bayt-birebir), jbrd
internal/brotli    Brotli (RFC 7932) çözücü (kodlayıcı: internal/enc/brotli.go)
cmd/gojxl          CLI
```

Kod çözme grup bazında paraleldir; tek geçişli VarDCT karelerinde her
grup çözülür çözülmez nicemlemeden çıkarılıp IDCT'den geçirilir (tüm
katsayılar hiç bellekte tutulmaz). 2B IDCT her geçişi bloğun bütün
sütunları üzerinde birlikte yapar. Filtreler (EPF/Gaborish) önbellekte
kalacak şekilde karolara bölünmüştür; son renk dönüşümü (XYB→sRGB→8-bit)
kesin eşikli bir tabloyla çıktı tamponuna tek geçişte yazılır.

## SIMD

`GOEXPERIMENT=simd` ile derlendiğinde (Go 1.26+, amd64) `simd/archsimd`
paketiyle yazılmış vektör çekirdekleri devreye girer; CPU AVX2+FMA
desteklemiyorsa çalışma anında skaler koda düşülür. Hâlâ saf Go'dur (cgo/asm yok).

| Çekirdek | skaler | SIMD | hızlanma |
|---|---|---|---|
| 8×8 IDCT | 110 ns | 44 ns | 2.5× |
| 32×32 IDCT | 3.8 µs | 1.7 µs | 2.3× |
| EPF satırı (256 px) | 2.0 µs | 0.48 µs | 4.2× |

Vektör çekirdeklerinden sonra YMM yazmaçlarının üst yarısı temizlenir
(VZEROUPPER); aksi halde ardından gelen skaler kod yanlış bağımlılıklar
yüzünden yavaşlar. Tek çekirdekte 4K VarDCT çözme: skaler 20 MP/s, SIMD
24 MP/s.

## Yol haritası

- Algısal (butteraugli benzeri) uyarlamalı nicemleme, daha fazla dönüşüm tipi
- Grup başına RCT seçimi, daha hızlı ağaç öğrenme (efor 5+), daha hızlı JPEG dönüştürme
- HDR ton eşleme (PQ/HLG → SDR), ICC'yi zorla sRGB'ye çevirme seçeneği
- Akış (streaming) kod çözme; kayıplı kodlamada alfa kanalı (şu an kayıpsız ve cjxl'den büyük)
