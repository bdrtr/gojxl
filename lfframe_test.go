package gojxl

import (
	"image/png"
	"os"
	"testing"
)

// A frame whose LF comes from an LF frame that itself uses one (cjxl
// --progressive_dc=2: LF frames at levels 2 and 1) decodes like djxl.
func TestTwoLevelLFFrames(t *testing.T) {
	data, err := os.ReadFile("testdata/progressive_dc2.jxl")
	if err != nil {
		t.Fatal(err)
	}
	img, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/progressive_dc2_djxl.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ref, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != ref.Bounds() {
		t.Fatalf("size %v, djxl %v", img.Bounds(), ref.Bounds())
	}
	worst := 0
	b := ref.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, _ := img.At(x, y).RGBA()
			r2, g2, b2, _ := ref.At(x, y).RGBA()
			for _, d := range [3]int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8)} {
				worst = max(worst, d, -d)
			}
		}
	}
	if worst > 1 {
		t.Fatalf("differs from djxl by up to %d/255", worst)
	}
}
