package jxl

import (
	"math"
	"math/rand"
	"testing"
)

// The 8-bit table must give q8(f(v)) for the curves it accepts.
func TestTF8MatchesDirect(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for _, tf := range []uint32{TFSRGB, TF709, TFLinear, TFGamma} {
		f := tfFunc(tf, 0.4545, 255)
		tb := buildTF8(f)
		if tb == nil {
			continue
		}
		vals := []float32{0, -1, 1, 2, 1e30, float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), 1e-9, 0.5}
		for i := 0; i < 200000; i++ {
			vals = append(vals, rng.Float32()*1.1-0.05, float32(math.Pow(rng.Float64(), 4)))
		}
		bad := 0
		for _, v := range vals {
			if got, want := tb.get(v), q8(f(v)); got != want {
				bad++
				if bad < 5 {
					t.Errorf("tf %d v %g: got %d want %d", tf, v, got, want)
				}
			}
		}
		if bad > 0 {
			t.Errorf("tf %d: %d mismatches", tf, bad)
		}
	}
}
