package gojxl

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/bdrtr/gojxl/internal/jxl"
)

func TestAssociatedAlpha(t *testing.T) {
	// Lossless 2x2 fixture with the associated-alpha flag set. Its
	// first two stored pixels are (85,51,17,85) and (34,102,170,170).
	data, err := os.ReadFile("testdata/associated_alpha.jxl")
	if err != nil {
		t.Fatal(err)
	}
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	if i := d.Header.AlphaChannel(); i < 0 || !d.Header.ExtraChannels[i].AlphaAssociated {
		t.Fatal("fixture must have associated alpha")
	}
	want := []color.NRGBA64{
		{R: 65535, G: 39321, B: 13107, A: 21845},
		{R: 13107, G: 39321, B: 65535, A: 43690},
		{},
		{R: 13107, G: 26214, B: 39321, A: 65535},
	}
	check := func(t *testing.T, img image.Image) {
		t.Helper()
		if img.Bounds() != image.Rect(0, 0, 2, 2) {
			t.Fatalf("bounds = %v, want (0,0)-(2,2)", img.Bounds())
		}
		for i, pixel := range want {
			if got := exactNRGBA64(img.At(i%2, i/2)); got != pixel {
				t.Errorf("pixel %d = %v, want %v", i, got, pixel)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		opt   *Options
		model color.Model
	}{
		{"8-bit", nil, color.NRGBAModel},
		{"16-bit", &Options{Force16: true}, color.NRGBA64Model},
	} {
		t.Run(tc.name+"/Decode", func(t *testing.T) {
			img, err := DecodeBytesOptions(data, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if img.ColorModel() != tc.model {
				t.Errorf("unexpected colour model for %T", img)
			}
			check(t, img)
		})
		t.Run(tc.name+"/ConvertToPNG", func(t *testing.T) {
			var buf bytes.Buffer
			if err := ConvertToPNG(&buf, data, tc.opt); err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(&buf)
			if err != nil {
				t.Fatal(err)
			}
			check(t, img)
		})
	}
}
