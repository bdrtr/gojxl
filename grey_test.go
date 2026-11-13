package gojxl

import (
	"image"
	"image/color"
	"testing"
)

// Colour images whose samples are all grey (as grey+alpha PNGs decode)
// are stored as grey, lossless and lossy; lossless keeps every sample.
func TestGreyContentStoredAsGrey(t *testing.T) {
	const w, h = 70, 45
	for _, alpha := range []bool{false, true} {
		src := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := uint8(x*3 + y*2)
				a := uint8(255)
				if alpha {
					a = uint8(100 + x)
				}
				src.SetNRGBA(x, y, color.NRGBA{v, v, v, a})
			}
		}
		for _, d := range []float32{0, 1} {
			data, err := EncodeBytes(src, &EncodeOptions{Distance: d})
			if err != nil {
				t.Fatal(err)
			}
			info, err := Stat(data)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Grey || info.HasAlpha != alpha {
				t.Errorf("alpha=%v d=%g: grey %v, alpha %v", alpha, d, info.Grey, info.HasAlpha)
			}
			if d > 0 {
				continue
			}
			img, err := DecodeBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					r, g, b, a := img.At(x, y).RGBA()
					sr, _, _, sa := src.At(x, y).RGBA()
					if r != sr || g != sr || b != sr || a != sa {
						t.Fatalf("alpha=%v: pixel (%d,%d) = %d %d %d %d, want %d %d", alpha, x, y, r, g, b, a, sr, sa)
					}
				}
			}
		}
	}
	// one coloured pixel keeps the image in colour
	src := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	src.Pix[4*77] = 10
	data, err := EncodeBytes(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := Stat(data); info.Grey {
		t.Error("colour image stored as grey")
	}
}
