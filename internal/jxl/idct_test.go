package jxl

import (
	"math"
	"math/rand"
	"testing"
)

func refIDCT(c []float32) []float32 {
	n := len(c)
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		s := float64(c[0])
		for k := 1; k < n; k++ {
			s += math.Sqrt2 * float64(c[k]) * math.Cos(math.Pi*float64((2*i+1)*k)/float64(2*n))
		}
		out[i] = float32(s)
	}
	return out
}

func TestIDCT(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16, 32, 64, 128, 256} {
		c := make([]float32, n)
		for i := range c {
			c[i] = rand.Float32()*2 - 1
		}
		want := refIDCT(c)
		got := append([]float32(nil), c...)
		idct1d(got, make([]float32, 2*n))
		for i := range got {
			if d := math.Abs(float64(got[i] - want[i])); d > 1e-4*float64(n) {
				t.Fatalf("n=%d i=%d got %v want %v", n, i, got[i], want[i])
			}
		}
	}
}
