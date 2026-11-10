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
