# gojxl — saf Go JPEG XL kod çözücü ve kodlayıcı

`gojxl`, JPEG XL (ISO/IEC 18181) için **cgo ve assembly kullanmayan, saf Go** bir
kod çözücü (decoder) ve kayıpsız kodlayıcıdır (encoder). Hedef: web'deki WASM
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
- Bozuk girdilere karşı dayanıklı (panik → hata), devasa boyut koruması

**Kayıplı kodlayıcı (VarDCT)**
- sRGB → XYB, DCT8/DCT16/DCT32 varblokları (RD tabanlı seçim), LLF ile tutarlı DC
- Karo başına chroma-from-luma (en küçük kareler), ters Gaborish ön işleme
- Kümelenmiş bağlamlarla ANS, alpha kanalı kayıpsız modular ekstra kanal olarak
- 4K fotoğrafta (d=1): gojxl 2.43 MB / 36.04 dB PSNR, cjxl 2.53 MB / 35.87 dB;
  kodlama 1.7 s (cjxl ~2.5 s)

**Kayıpsız kodlayıcı (Modular)**
- YCoCg RCT, otomatik palet (≤1024 renk, parlaklığa göre sıralı)
- Gradient ve Weighted predictor, sabit bağlam ağaçları (efor 1–3)
- ANS entropi kodlama, gruplar arası paralel kodlama
- 8/16-bit, gri/RGB/RGBA

## Kurulum ve kullanım

```sh
go build -o gojxl ./cmd/gojxl                       # taşınabilir, skaler
GOEXPERIMENT=simd go build -o gojxl ./cmd/gojxl     # AVX2/FMA SIMD çekirdekleri (amd64)

./gojxl decode resim.jxl cikti.png          # JXL → PNG
./gojxl decode -depth 16 resim.jxl c.png    # 16-bit PNG
./gojxl decode -bench 5 resim.jxl           # hız ölçümü
./gojxl encode -e 3 girdi.png cikti.jxl     # PNG/JPEG → kayıpsız JXL
./gojxl encode -d 1 girdi.png cikti.jxl     # kayıplı (d≈1 görsel olarak kayıpsız)
./gojxl info resim.jxl
```

Kütüphane olarak:

```go
import "gojxl"

img, err := gojxl.Decode(r)                 // image.Image
anim, err := gojxl.DecodeAll(data)          // tüm kareler + süreler
err = gojxl.Encode(w, img, &gojxl.EncodeOptions{Effort: 3})       // kayıpsız
err = gojxl.Encode(w, img, &gojxl.EncodeOptions{Distance: 1.0})   // kayıplı
```

`image.Decode` ile de çalışır (paket `init` içinde format kaydı yapar).

## Doğruluk

- Resmi [JPEG XL conformance](https://github.com/libjxl/conformance) setinin
  40 dosyasından 33'ü libjxl (djxl) ile 16-bit çıktıda **112–200 dB PSNR**
  (pratikte birebir); kayıpsız dosyalar bit-birebir.
- Kalan farklar: ICC profilli XYB görüntülerde djxl orijinal ICC uzayına
  dönüştürür, gojxl sRGB çıkarır (ikisi de geçerli); CMYK → RGB dönüşümü tam
  bir ICC renk yönetimi gerektirir (henüz yok).
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

Kayıpsız sıkıştırma (bayt, düşük iyi):

| Görüntü | gojxl e2 | gojxl e3 | cjxl e2 | cjxl e3 | cjxl e7 |
|---|---|---|---|---|---|
| 4K nebula foto | 14 091 832 | **13 679 914** | 14 573 786 | 13 765 478 | 13 708 348 |
| 640×480 foto kırpıntısı | 502 734 | 484 117 | 519 505 | 483 981 | 477 278 |
| 648 renkli görsel | 184 645 | 184 192 | 188 166 | 176 479 | 154 700 |

## Mimari

```
internal/bitio     LSB-first bit okuyucu/yazıcı, U32/U64/F16
internal/entropy   ANS/prefix/LZ77/bağlam haritası kod çözücü
internal/jxl       header'lar, Modular, VarDCT, filtreler, render, blending
internal/enc       kayıpsız kodlayıcı (ağaç, ANS, prefix kodlar)
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
- MA ağacı öğrenme, LZ77
- ICC tabanlı renk yönetimi (CMYK, geniş gamut/HDR çıktısı ve etiketleme)
- APNG/animasyon çıktısı, akış (streaming) kod çözme
