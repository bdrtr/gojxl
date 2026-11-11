package enc

import (
	"image/png"
	"os"
	"testing"
)

func BenchmarkLossy(b *testing.B) {
	f, err := os.Open(os.Getenv("GOJXL_BENCH_PNG"))
	if err != nil {
		b.Skip("set GOJXL_BENCH_PNG")
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		b.Fatal(err)
	}
	r := img.Bounds()
	im := &Image{W: r.Dx(), H: r.Dy(), BitDepth: 8}
	for c := 0; c < 3; c++ {
		im.Chans = append(im.Chans, make([]int32, im.W*im.H))
	}
	for y := 0; y < im.H; y++ {
		for x := 0; x < im.W; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			im.Chans[0][y*im.W+x], im.Chans[1][y*im.W+x], im.Chans[2][y*im.W+x] = int32(cr>>8), int32(cg>>8), int32(cb>>8)
		}
	}
	b.SetBytes(int64(im.W * im.H))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeVarDCT(im, LossyOptions{Distance: 1, EPFIters: -1, Sharpness: -1, LibAQ: true}); err != nil {
			b.Fatal(err)
		}
	}
}
