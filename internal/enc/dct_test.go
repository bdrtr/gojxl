package enc

import (
	"math"
	"math/rand"
	"testing"
)

func TestFastDCT(t *testing.T) {
	for _, n := range []int{8, 16, 32} {
		src := make([]float32, n*n)
		for i := range src {
			src[i] = rand.Float32()
		}
		a, b := make([]float32, n*n), make([]float32, n*n)
		fdctN(src, n, n, a)
		fdctNRef(src, n, n, b)
		for i := range a {
			if d := a[i] - b[i]; d > 1e-4 || d < -1e-4 {
				t.Fatalf("n=%d i=%d %v vs %v", n, i, a[i], b[i])
			}
		}
	}
}

func TestCbrtFast(t *testing.T) {
	for i := 0; i < 1000000; i++ {
		v := float64(float32(float64(i)*1.3e-6)) + opsinBias
		if a, b := float32(math.Cbrt(v)), float32(cbrtFast(v)); a != b {
			t.Fatalf("cbrt(%g): %g != %g", v, b, a)
		}
	}
}
