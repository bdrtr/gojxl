package gojxl

import (
	"os"
	"testing"

	"gojxl/internal/jxl"
	"gojxl/internal/tiff"
)

// A 16-bit TIFF comes back exactly from lossless JPEG XL.
func TestTIFFInput(t *testing.T) {
	data, err := os.ReadFile("internal/tiff/testdata/rgb16_lzw.tif")
	if err != nil {
		t.Fatal(err)
	}
	src, err := tiff.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	jx, err := ConvertToJXL(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := jxl.NewDecoder(jx)
	if err != nil {
		t.Fatal(err)
	}
	_ = d // (samples that are 8-bit values times 257 may be stored at 8 bits)
	img, err := DecodeBytesOptions(jx, &Options{Force16: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < src.W*src.H; i++ {
		r, g, b, _ := img.At(i%src.W, i/src.W).RGBA()
		s := src.Samples[3*i:]
		if uint16(r) != s[0] || uint16(g) != s[1] || uint16(b) != s[2] {
			t.Fatalf("pixel %d: %d %d %d, want %v", i, r, g, b, s[:3])
		}
	}
}
