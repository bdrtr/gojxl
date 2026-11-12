package bmp

import (
	"bytes"
	"os"
	"testing"
)

// The decodes match Pillow's: RLE8, 32-bit with an alpha mask, RGB565 and
// 1-bit bitmaps (references: width, height, RGBA pixels).
func TestDecodeMatchesPillow(t *testing.T) {
	for _, name := range []string{"rle8", "argb", "rgb565", "mono"} {
		data, err := os.ReadFile("testdata/" + name + ".bmp")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := os.ReadFile("testdata/" + name + ".rgba")
		if err != nil {
			t.Fatal(err)
		}
		img, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(img.Pix, ref[8:]) {
			t.Errorf("%s differs from Pillow", name)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, name := range []string{"rle8", "argb", "rgb565", "mono"} {
		b, err := os.ReadFile("testdata/" + name + ".bmp")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		Decode(data)
	})
}
