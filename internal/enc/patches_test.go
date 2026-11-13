package enc

import (
	"math"
	"testing"

	"gojxl/internal/jxl"
)

// textImage draws rows of a few distinct glyphs on a flat background.
func textImage(w, h int) *Image {
	glyphs := [][]string{
		{"..##..", ".#..#.", "#....#", "######", "#....#", "#....#", "#....#"},
		{"#####.", "#....#", "#####.", "#....#", "#....#", "#....#", "#####."},
		{".####.", "#....#", "#.....", "#.....", "#.....", "#....#", ".####."},
	}
	im := &Image{W: w, H: h, BitDepth: 8}
	for c := 0; c < 3; c++ {
		ch := make([]int32, w*h)
		for i := range ch {
			ch[i] = []int32{235, 238, 240}[c]
		}
		im.Chans = append(im.Chans, ch)
	}
	k := 0
	for y0 := 8; y0+8 < h; y0 += 16 {
		for x0 := 8; x0+7 < w; x0 += 12 {
			g := glyphs[k%len(glyphs)]
			k++
			for gy, row := range g {
				for gx, p := range row {
					if p == '#' {
						for c := 0; c < 3; c++ {
							im.Chans[c][(y0+gy)*w+x0+gx] = []int32{20, 30, 60}[c]
						}
					}
				}
			}
		}
	}
	return im
}

func decodeRGB(t *testing.T, data []byte, w, h int) ([3][]float32, int) {
	t.Helper()
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	f, err := d.NextFrame()
	if err != nil {
		t.Fatal(err)
	}
	frames = d.FramesDecoded()
	var out [3][]float32
	tmp := make([]float32, 3*w)
	for c := range out {
		out[c] = make([]float32, w*h)
	}
	for y := 0; y < h; y++ {
		r, g, b := f.RGBRow(y, tmp)
		copy(out[0][y*w:], r[:w])
		copy(out[1][y*w:], g[:w])
		copy(out[2][y*w:], b[:w])
	}
	return out, frames
}

// Repeated glyphs go to a reference frame: the file shrinks and decodes to
// the same quality.
func TestPatches(t *testing.T) {
	im := textImage(200, 120)
	for _, d := range []float32{1, 3} {
		base := LossyOptions{Distance: d, EPFIters: -1, Sharpness: -1, LibAQ: true}
		with, err := EncodeVarDCT(im, base)
		if err != nil {
			t.Fatal(err)
		}
		base.NoPatches = true
		without, err := EncodeVarDCT(im, base)
		if err != nil {
			t.Fatal(err)
		}
		px, frames := decodeRGB(t, with, im.W, im.H)
		if frames != 2 {
			t.Fatalf("d%v: %d frames, want a patch reference and the image", d, frames)
		}
		var se float64
		for c := 0; c < 3; c++ {
			for i, v := range px[c] {
				e := float64(v)*255 - float64(im.Chans[c][i])
				se += e * e
			}
		}
		psnr := 10 * math.Log10(255*255/(se/float64(3*im.W*im.H)))
		t.Logf("d%v: %d bytes with patches, %d without, PSNR %.2f", d, len(with), len(without), psnr)
		if len(with) >= len(without) || psnr < 30 {
			t.Errorf("d%v: %d vs %d bytes, PSNR %.2f", d, len(with), len(without), psnr)
		}
	}
	// a smooth image has no patches
	data, err := EncodeVarDCT(acsTestImage(), LossyOptions{Distance: 1, EPFIters: -1, Sharpness: -1, LibAQ: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, frames := decodeRGB(t, data, 136, 120); frames != 1 {
		t.Errorf("smooth image: %d frames", frames)
	}
}

// Images with alpha get patches too (the alpha channel is left to the
// frame: kNone); the alpha decodes exactly.
func TestPatchesAlpha(t *testing.T) {
	im := textImage(200, 120)
	a := make([]int32, im.W*im.H)
	for i := range a {
		if i%im.W < 150 { // binary alpha, as palette PNGs with tRNS have
			a[i] = 255
		}
	}
	im.Chans = append(im.Chans, a)
	im.Alpha = true
	data, err := EncodeVarDCT(im, LossyOptions{Distance: 1, EPFIters: -1, Sharpness: -1, LibAQ: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.NextFrame()
	if err != nil {
		t.Fatal(err)
	}
	if n := d.FramesDecoded(); n != 2 {
		t.Fatalf("%d frames, want a patch reference and the image", n)
	}
	ec := f.EC[0]
	for y := 0; y < im.H; y++ {
		for x := 0; x < im.W; x++ {
			if got, want := math.Round(float64(ec.Row(y)[x])*255), float64(a[y*im.W+x]); got != want {
				t.Fatalf("alpha (%d,%d): %v, want %v", x, y, got, want)
			}
		}
	}
}
