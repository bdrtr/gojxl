package gojxl

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"testing"

	ijpeg "github.com/bdrtr/gojxl/internal/jpeg"
)

func testPhoto(w, h int, r *rand.Rand) *image.RGBA {
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := func(f float64) uint8 { return uint8(min(255, max(0, 128+100*math.Sin(f)+float64(r.Intn(20))))) }
			im.Set(x, y, color.RGBA{v(float64(x) / 13), v(float64(y) / 17), v(float64(x+y) / 29), 255})
		}
	}
	return im
}

func psnrImages(a, b image.Image) float64 {
	var se float64
	n := 0
	ab := a.Bounds()
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			r1, g1, b1, _ := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(b.Bounds().Min.X+x, b.Bounds().Min.Y+y).RGBA()
			for _, d := range []float64{float64(r1) - float64(r2), float64(g1) - float64(g2), float64(b1) - float64(b2)} {
				d /= 257
				se += d * d
				n++
			}
		}
	}
	return 10 * math.Log10(255*255/max(se/float64(n), 1e-10))
}

func TestTranscodeJPEG(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, sz := range [][2]int{{1, 1}, {17, 9}, {256, 256}, {300, 520}, {600, 333}} {
		for _, grey := range []bool{false, true} {
			var src image.Image = testPhoto(sz[0], sz[1], r)
			if grey {
				g := image.NewGray(src.Bounds())
				for y := 0; y < sz[1]; y++ {
					for x := 0; x < sz[0]; x++ {
						g.Set(x, y, src.At(x, y))
					}
				}
				src = g
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85}); err != nil {
				t.Fatal(err)
			}
			ref, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			jxl, err := TranscodeJPEG(buf.Bytes())
			if err != nil {
				t.Fatalf("%v grey=%v: %v", sz, grey, err)
			}
			out, err := DecodeBytes(jxl)
			if err != nil {
				t.Fatalf("%v grey=%v: decode: %v", sz, grey, err)
			}
			if p := psnrImages(ref, out); p < 38 {
				t.Errorf("%v grey=%v: PSNR %.1f vs image/jpeg", sz, grey, p)
			}
		}
	}
}

func TestTranscodeJPEGCorrupt(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	var buf bytes.Buffer
	jpeg.Encode(&buf, testPhoto(64, 48, r), &jpeg.Options{Quality: 90})
	data := buf.Bytes()
	for i := 0; i < 300; i++ {
		d := append([]byte(nil), data...)
		switch i % 3 {
		case 0:
			d = d[:r.Intn(len(d))]
		default:
			for k := 0; k < 1+r.Intn(8); k++ {
				d[r.Intn(len(d))] = byte(r.Intn(256))
			}
		}
		TranscodeJPEG(d) // must not panic
	}
}

func TestReconstructJPEG(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	for _, q := range []int{50, 90} {
		for _, grey := range []bool{false, true} {
			var src image.Image = testPhoto(203, 117, r)
			if grey {
				g := image.NewGray(src.Bounds())
				for y := 0; y < 117; y++ {
					for x := 0; x < 203; x++ {
						g.Set(x, y, src.At(x, y))
					}
				}
				src = g
			}
			var buf bytes.Buffer
			jpeg.Encode(&buf, src, &jpeg.Options{Quality: q})
			jxl, err := TranscodeJPEG(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if !HasJPEG(jxl) {
				// Go writes an unused second quantisation table for grey
				// images, which jbrd cannot represent (libjxl neither).
				if grey {
					if _, err := ReconstructJPEG(jxl); !errors.Is(err, ErrNoJPEG) {
						t.Fatalf("expected ErrNoJPEG, got %v", err)
					}
					continue
				}
				t.Fatal("no jbrd box")
			}
			back, err := ReconstructJPEG(jxl)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back, buf.Bytes()) {
				t.Fatalf("q=%d grey=%v: reconstructed JPEG differs", q, grey)
			}
		}
	}
}

func app1(payload []byte) []byte {
	n := len(payload) + 2
	return append([]byte{0xff, 0xe1, byte(n >> 8), byte(n)}, payload...)
}

func TestReconstructJPEGMetadata(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	var buf bytes.Buffer
	jpeg.Encode(&buf, testPhoto(80, 60, r), &jpeg.Options{Quality: 90})
	src := buf.Bytes()
	exif := append([]byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08"), bytes.Repeat([]byte("camera "), 200)...)
	xmp := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), bytes.Repeat([]byte("<x:xmpmeta/>"), 100)...)
	var in []byte
	in = append(in, src[:2]...)
	in = append(in, app1(exif)...)
	in = append(in, app1(xmp)...)
	in = append(in, app1(exif)...) // a second Exif marker stays in jbrd
	in = append(in, src[2:]...)
	out, err := TranscodeJPEG(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("brobExif")) || !bytes.Contains(out, []byte("brobxml ")) {
		t.Errorf("metadata boxes missing")
	}
	if len(out) > len(in) {
		t.Errorf("transcoded file larger than input: %d > %d", len(out), len(in))
	}
	rec, err := ReconstructJPEG(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec, in) {
		t.Fatalf("reconstruction differs (%d vs %d bytes)", len(rec), len(in))
	}
}

// A 4:4:4 JPEG (chroma from luma applies) whose chroma is about 128/84
// times the luma: the best factor, 128, does not fit the int8 map that
// decoders read (they clamp it to 127), so the encoder must clamp it too.
func TestReconstructJPEGCfLClamp(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, testPhoto(64, 64, rand.New(rand.NewSource(6))), &jpeg.Options{Quality: 90})
	img, err := ijpeg.Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range img.Comps { // 4:2:0 -> 4:4:4
		c := &img.Comps[i]
		c.H, c.V, c.BW, c.BH = 1, 1, 8, 8
	}
	r := rand.New(rand.NewSource(7))
	y := img.Comps[0].Coef
	for b := 0; b < 64; b++ {
		for _, n := range []int{1, 8, 9} {
			y[b*64+n] = int16((300 + r.Intn(300)) * (1 - 2*r.Intn(2)))
		}
	}
	qY := img.Quant[img.Comps[0].Tq]
	for c := 1; c < 3; c++ {
		qC := img.Quant[img.Comps[c].Tq]
		coef := make([]int16, len(y))
		for i, v := range y {
			n := i % 64
			if n == 0 {
				continue
			}
			// the decoder's prediction with factor 128 (zero residual)
			sq := (int32(1) << 11) * int32(qY[n]) / int32(qC[n])
			cs := (128*(1<<11)/84*sq + 1<<10) >> 11
			coef[i] = int16(min(max((int32(v)*cs+1<<10)>>11, -1000), 1000))
		}
		img.Comps[c].Coef = coef
	}
	data, err := img.Write()
	if err != nil {
		t.Fatal(err)
	}
	jxl, err := TranscodeJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReconstructJPEG(jxl)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatal("reconstructed JPEG differs")
	}
}
