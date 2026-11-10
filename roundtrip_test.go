package gojxl

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"math/rand"
	"testing"
)

func randomImage(kind string, w, h int, r *rand.Rand) image.Image {
	switch kind {
	case "gray":
		im := image.NewGray(image.Rect(0, 0, w, h))
		for i := range im.Pix {
			im.Pix[i] = uint8(r.Intn(256))
		}
		return im
	case "gray16":
		im := image.NewGray16(image.Rect(0, 0, w, h))
		r.Read(im.Pix)
		return im
	case "rgba64":
		im := image.NewNRGBA64(image.Rect(0, 0, w, h))
		r.Read(im.Pix)
		return im
	case "smooth":
		im := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				im.Set(x, y, color.NRGBA{uint8(x * 3), uint8(y * 2), uint8(x + y), 255})
			}
		}
		return im
	}
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	r.Read(im.Pix)
	if kind == "rgb" {
		for i := 3; i < len(im.Pix); i += 4 {
			im.Pix[i] = 255
		}
	}
	return im
}

func sameImage(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds().Size() != b.Bounds().Size() {
		t.Fatalf("size mismatch %v vs %v", a.Bounds(), b.Bounds())
	}
	ab, bb := a.Bounds(), b.Bounds()
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			r1, g1, b1, a1 := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			r2, g2, b2, a2 := b.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("pixel (%d,%d) differs: %v,%v,%v,%v vs %v,%v,%v,%v", x, y, r1, g1, b1, a1, r2, g2, b2, a2)
			}
		}
	}
}

func TestLosslessRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	sizes := [][2]int{{1, 1}, {7, 3}, {256, 256}, {257, 1}, {1, 300}, {300, 260}, {513, 77}}
	kinds := []string{"rgb", "rgba", "gray", "gray16", "rgba64", "smooth"}
	for _, sz := range sizes {
		for _, k := range kinds {
			for _, e := range []int{1, 2, 3} {
				img := randomImage(k, sz[0], sz[1], r)
				data, err := EncodeBytes(img, &EncodeOptions{Effort: e})
				if err != nil {
					t.Fatalf("%s %v e%d: encode: %v", k, sz, e, err)
				}
				out, err := Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("%s %v e%d: decode: %v", k, sz, e, err)
				}
				sameImage(t, img, out)
			}
		}
	}
}

func psnr(a, b image.Image) float64 {
	var se float64
	n := 0
	ab, bb := a.Bounds(), b.Bounds()
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			r1, g1, b1, _ := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			for _, d := range []float64{float64(r1) - float64(r2), float64(g1) - float64(g2), float64(b1) - float64(b2)} {
				d /= 257
				se += d * d
				n++
			}
		}
	}
	if se == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/(se/float64(n)))
}

func TestLossyRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for _, sz := range [][2]int{{1, 1}, {9, 17}, {256, 256}, {300, 260}, {700, 500}} {
		img := randomImage("smooth", sz[0], sz[1], r)
		if sz[0]%2 == 1 {
			// add a varying alpha channel
			nm := img.(*image.NRGBA)
			for i := 3; i < len(nm.Pix); i += 4 {
				nm.Pix[i] = uint8(i / 4 % 251)
			}
		}
		data, err := EncodeBytes(img, &EncodeOptions{Distance: 1})
		if err != nil {
			t.Fatal(err)
		}
		out, err := Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%v: %v", sz, err)
		}
		if nm, ok := img.(*image.NRGBA); ok && sz[0]%2 == 1 {
			o := out.(*image.NRGBA)
			for i := 3; i < len(nm.Pix); i += 4 {
				if nm.Pix[i] != o.Pix[i] {
					t.Fatalf("%v: alpha mismatch at %d", sz, i)
				}
			}
			continue
		}
		if p := psnr(img, out); p < 28 {
			t.Fatalf("%v: PSNR %.1f too low", sz, p)
		}
	}
}
