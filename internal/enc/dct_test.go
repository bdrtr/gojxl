package enc

import (
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
