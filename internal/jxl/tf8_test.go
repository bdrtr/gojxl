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

func BenchmarkXYBTo8(b *testing.B) {
	ih := &ImageHeader{}
	ih.Opsin = defaultOpsinInverse()
	ih.ToneMapping.IntensityTarget = 255
	ih.Colour.TransferFunction = TFSRGB
	c := newXYBConv(ih, nil)
	c.tf8 = buildTF8(c.f)
	const w = 2048
	rng := rand.New(rand.NewSource(5))
	x, y, bb := make([]float32, w), make([]float32, w), make([]float32, w)
	for i := range x {
		x[i] = (rng.Float32() - 0.5) * 0.02
		y[i] = 0.2 + rng.Float32()*0.6
		bb[i] = 0.2 + rng.Float32()*0.6
	}
	dst := make([]byte, 4*w)
	b.SetBytes(w)
	for i := 0; i < b.N; i++ {
		xybTo8(c, x, y, bb, dst, 4)
	}
}

// Gamma curves give 0 up to linear 1e-5 and for negative values, as
// libjxl's output stage (OpGamma) does.
func TestGammaZeroBelow(t *testing.T) {
	for _, tf := range []uint32{TFGamma, TFDCI} {
		f := tfFunc(tf, 0.45455, 255)
		lut := buildTFLUT(f)
		lut.powerLaw()
		px := []float32{-0.5, -1e-6, 0, 5e-6, 1e-5, 2e-5, 0.5}
		lut.apply(px, f)
		for i, want := range []bool{false, false, false, false, false, true, true} {
			if (px[i] > 0) != want {
				t.Errorf("tf %d: sample %d -> %g", tf, i, px[i])
			}
		}
	}
	if tfZeroesNegatives(TFSRGB) {
		t.Error("sRGB mirrors negative values")
	}
	// interpolation near zero stays within 0.5/65535 of the curve
	f := tfFunc(TFGamma, 0.45455, 255)
	lut := buildTFLUT(f)
	lut.powerLaw()
	for v := float32(1.1e-5); v < 0.01; v *= 1.01 {
		px := []float32{v}
		lut.apply(px, f)
		if d := math.Abs(float64(px[0] - f(v))); d > 0.5/65535 {
			t.Fatalf("v %g: %g vs %g", v, px[0], f(v))
		}
	}
}
