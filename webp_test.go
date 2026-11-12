package gojxl

import (
	"bytes"
	"image"
	"os"
	"testing"

	"gojxl/internal/webp"
)

// WebP inputs: a lossless one with alpha comes back exactly from lossless
// JPEG XL, and an animation becomes a JPEG XL animation with the same
// composited frames and durations.
func TestWebPInput(t *testing.T) {
	data, err := os.ReadFile("internal/webp/testdata/ll_alpha.webp")
	if err != nil {
		t.Fatal(err)
	}
	src, err := DecodeInput(data)
	if err != nil {
		t.Fatal(err)
	}
	jx, err := ConvertToJXL(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBytes(jx)
	if err != nil {
		t.Fatal(err)
	}
	if !sameNRGBA(t, src, got) {
		t.Error("lossless WebP did not survive lossless JPEG XL")
	}

	data, err = os.ReadFile("internal/webp/testdata/anim.webp")
	if err != nil {
		t.Fatal(err)
	}
	wim, err := webp.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	jx, err = ConvertToJXL(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	anim, err := DecodeAll(jx)
	if err != nil {
		t.Fatal(err)
	}
	cs := wim.Canvases()
	if len(anim.Frames) != len(cs) {
		t.Fatalf("%d frames, WebP has %d", len(anim.Frames), len(cs))
	}
	for i, f := range anim.Frames {
		if want := float64(wim.Frames[i].Duration) / 1000; f.Duration < want-1e-3 || f.Duration > want+1e-3 {
			t.Errorf("frame %d: duration %v, want %v", i, f.Duration, want)
		}
		if !sameNRGBA(t, cs[i], f.Image) {
			t.Errorf("frame %d differs", i)
		}
	}
	// lossy encoding of a lossy WebP works too
	if _, err := ConvertToJXL(mustReadFile(t, "internal/webp/testdata/ly_alpha.webp"), &EncodeOptions{Distance: 1}); err != nil {
		t.Error(err)
	}
}

func mustReadFile(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sameNRGBA compares two images as non-premultiplied 8-bit RGBA (colour
// of fully transparent pixels ignored).
func sameNRGBA(t *testing.T, a, b image.Image) bool {
	t.Helper()
	if a.Bounds().Size() != b.Bounds().Size() {
		t.Logf("sizes %v vs %v", a.Bounds(), b.Bounds())
		return false
	}
	toN := func(m image.Image) *image.NRGBA {
		n := image.NewNRGBA(image.Rect(0, 0, m.Bounds().Dx(), m.Bounds().Dy()))
		for y := 0; y < n.Rect.Dy(); y++ {
			for x := 0; x < n.Rect.Dx(); x++ {
				n.Set(x, y, m.At(m.Bounds().Min.X+x, m.Bounds().Min.Y+y))
			}
		}
		return n
	}
	na, nb := toN(a), toN(b)
	for i := 0; i < len(na.Pix); i += 4 {
		if na.Pix[i+3] == 0 && nb.Pix[i+3] == 0 {
			continue
		}
		if !bytes.Equal(na.Pix[i:i+4], nb.Pix[i:i+4]) {
			t.Logf("pixel %d: %v vs %v", i/4, na.Pix[i:i+4], nb.Pix[i:i+4])
			return false
		}
	}
	return true
}
