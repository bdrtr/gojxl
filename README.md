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
- Efor seviyeleri (`-e`, varsayılan 7): 5–6 32x16/16x32'yi, 3–4 tüm
  dikdörtgenleri atlar, 1–2 yalnız DCT8 kullanır (~%40 daha az CPU).
  5 MP fotoğrafta, d=1, tek çekirdek:

  | | boyut | SSIMULACRA 2 | CPU |
  |---|---|---|---|
  | gojxl -e7 | 589 KB | 84.06 | 1.8 s |
  | gojxl -e3 | 686 KB | 83.65 | 1.6 s |
  | gojxl -e1 | 765 KB | 82.32 | 1.1 s |
  | cjxl -e7 | 679 KB | 83.94 | 1.5 s |
  | cjxl -e3 | 854 KB | 82.83 | 0.8 s |

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
- Boyut cjxl ile aynı düzeyde (ör. 10.3 MB JPEG → 7.94 MB; cjxl 7.95 MB)
- `gojxl encode foto.jpg foto.jxl` JPEG girdisinde varsayılan olarak bunu yapar
  (`-pixels` ile pikselleri kodlar)

**Kayıpsız kodlayıcı (Modular)**
- Efor 1–3: sabit bağlam ağaçları, YCoCg, Gradient/Weighted predictor (hızlı)
- Efor 4+ (varsayılan 4): **MA ağacı öğrenme** — örneklenmiş piksellerden
  entropi kazancına göre açgözlü bölme; 16 özellik + önceki kanallara
  referans veren özellikler (16–23), 5–14 aday predictor, yaprak başına
  en iyi predictor
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
./gojxl encode girdi.png cikti.jxl          # PNG → kayıpsız JXL (efor 4)
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
jxlData, err := gojxl.TranscodeJPEG(jpegBytes)                     // JPEG → JXL
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

## Performans (Ryzen 7 8845HS, yük altındaki dizüstü)

Kod çözme, MP/s (yüksek iyi):

| Dosya | libjxl 1T | libjxl MT | gojxl 1T | gojxl MT |
|---|---|---|---|---|
| 4K foto, VarDCT d1 | 33 | 127 | 18 | ~35–50 |
| JPEG→JXL (cafe) | 19 | 78 | **21** | 55 |
| Kayıpsız e7 foto | 5.9 | 17.4 | 5.8 | **19.1** |

WASM derlemeleri tek thread çalışır ve native'in kabaca yarısı hızındadır;
gojxl tüm çekirdekleri kullanarak çoğu dosyada bunun üzerindedir.

Kayıpsız sıkıştırma (bayt, düşük iyi; cjxl v0.12):

| Görüntü | gojxl e3 | gojxl e4 | gojxl e5 | cjxl e3 | cjxl e7 |
|---|---|---|---|---|---|
| 4K foto (3840×2160) | 12 947 496 | 12 052 258 | **11 943 401** | — | 12 420 691 |
| bike (2048×2560 foto) | 5 191 623 | 4 882 756 | **4 820 471** | 5 212 891 | 4 866 475 |
| 640×480 foto kırpıntısı | 463 767 | 439 625 | **437 543** | 465 109 | 446 345 |
| blendmodes (16-bit gradyan, RGBA) | 1 001 120 | 619 258 | **500 531** | 1 068 867 | 627 621 |
| sunset_logo (16-bit, kanal paleti) | 2 171 492 | 845 792 | **761 110** | 3 210 948 | 888 081 |
| delta_palette (sentetik) | 451 023 | 361 108 | **246 695** | 467 687 | 372 699 |
| patches_lossless (ekran görüntüsü) | 144 197 | **69 036** | 69 725 | 213 670 | 107 500 |
| lz77_flower | 280 871 | 268 559 | **263 445** | 281 699 | 270 951 |

Efor 4, tek iş parçacığında cjxl e7 ile benzer CPU süresi harcar (640×480
fotoğrafta 0.75 s'ye karşı 0.93 s) ve çok çekirdekte belirgin şekilde
daha hızlıdır; efor 5–6 daha yavaştır (RCT denemeleri, daha büyük ağaçlar).

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

Kod çözme grup bazında paraleldir; filtreler (EPF/Gaborish) önbellekte
kalacak şekilde karolara bölünmüştür; son renk dönüşümü (XYB→sRGB→8-bit)
çıktı tamponuna tek geçişte yazılır.

## SIMD

`GOEXPERIMENT=simd` ile derlendiğinde (Go 1.26+, amd64) `simd/archsimd`
paketiyle yazılmış vektör çekirdekleri devreye girer; CPU AVX2+FMA
desteklemiyorsa çalışma anında skaler koda düşülür. Hâlâ saf Go'dur (cgo/asm yok).

| Çekirdek | skaler | SIMD | hızlanma |
|---|---|---|---|
| 8×8 IDCT | 97 ns | 42 ns | 2.3× |
| 32×32 IDCT | 5.5 µs | 1.5 µs | 3.6× |
| EPF satırı (256 px) | 4.2 µs | 0.6 µs | 6.8× |

## Yol haritası

- Algısal (butteraugli benzeri) uyarlamalı nicemleme, daha fazla dönüşüm tipi
- Grup başına RCT seçimi, daha hızlı ağaç öğrenme (efor 5+), daha hızlı JPEG dönüştürme
- HDR ton eşleme (PQ/HLG → SDR), ICC'yi zorla sRGB'ye çevirme seçeneği
- Akış (streaming) kod çözme; kayıplı kodlamada alfa kanalı (şu an kayıpsız ve cjxl'den büyük)
