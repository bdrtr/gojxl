package enc

import (
	"math/rand"
	"testing"
)

func BenchmarkEncode(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	w, h := 2048, 1024
	im := &Image{W: w, H: h, BitDepth: 8}
	for c := 0; c < 3; c++ {
		ch := make([]int32, w*h)
		for i := range ch {
			x, y := i%w, i/w
			ch[i] = int32((x*3+y*2+c*50)&255) ^ int32(r.Intn(8))
		}
		im.Chans = append(im.Chans, ch)
	}
	b.SetBytes(int64(w * h))
	for i := 0; i < b.N; i++ {
		if _, err := EncodeLossless(im, Options{Effort: 2}); err != nil {
			b.Fatal(err)
		}
	}
}
