package enc

import (
	"math"
	"math/rand"
	"testing"

	"github.com/bdrtr/gojxl/internal/jxl"
)

// The forward transforms invert the decoder's, and index 0 is the block
// average.
func TestSmallTransformsInvert(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	src := make([]float32, 64)
	for i := range src {
		src[i] = r.Float32()*2 - 1
	}
	var mean float32
	for _, v := range src {
		mean += v / 64
	}
	for _, acs := range []int{1, 2, 3, 12, 13, 14, 15, 16, 17} {
		coef := make([]float32, 64)
		fdct8Kind(acs, src, 8, coef)
		if math.Abs(float64(coef[0]-mean)) > 1e-5 {
			t.Errorf("acs %d: DC %v, mean %v", acs, coef[0], mean)
		}
		got := make([]float32, 64)
		jxl.InverseTransform(acs, coef, got, 8)
		for i := range src {
			if math.Abs(float64(got[i]-src[i])) > 1e-4 {
				t.Fatalf("acs %d: pixel %d: %v, want %v", acs, i, got[i], src[i])
			}
		}
	}
}

// The unit error energies: 64 for DCT8-like positions, 32 for the halves
// of DCT4x8, 16 for DCT4X4 quarters, about 1 for IDENTITY differences.
func TestPixelWeights(t *testing.T) {
	for _, c := range []struct {
		acs, p int
		want   float32
	}{
		{12, 1, 32}, {12, 8, 64}, {3, 2, 16}, {3, 1, 64}, {2, 9, 64}, {2, 63, 4}, {1, 3, 0.9375}, {14, 2, 1},
	} {
		if w := pixelWeights(c.acs)[c.p]; math.Abs(float64(w-c.want)) > 1e-3 {
			t.Errorf("acs %d coefficient %d: weight %v, want %v", c.acs, c.p, w, c.want)
		}
	}
}
