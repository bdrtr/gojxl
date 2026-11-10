package jxl

import (
	"math/rand"
	"testing"
)

func benchIDCT(b *testing.B, rows, cols int, f func(rows, cols int, in, out []float32, stride int, scratch []float32)) {
	in := make([]float32, rows*cols)
	for i := range in {
		in[i] = rand.Float32()
	}
	work := make([]float32, len(in))
	out := make([]float32, rows*cols)
	scratch := make([]float32, rows*cols+3*256)
	b.SetBytes(int64(rows * cols))
	for i := 0; i < b.N; i++ {
		copy(work, in)
		f(rows, cols, work, out, cols, scratch)
	}
}

func BenchmarkIDCT32Scalar(b *testing.B) { benchIDCT(b, 32, 32, scaledIDCT2DScalar) }
func BenchmarkIDCT32(b *testing.B)       { benchIDCT(b, 32, 32, scaledIDCT2D) }
func BenchmarkIDCT16x8Scalar(b *testing.B) {
	benchIDCT(b, 16, 8, scaledIDCT2DScalar)
}
func BenchmarkIDCT16x8(b *testing.B) { benchIDCT(b, 16, 8, scaledIDCT2D) }
func BenchmarkIDCT8Scalar(b *testing.B) {
	benchIDCT(b, 8, 8, func(r, c int, in, out []float32, s int, _ []float32) { idct8x8Scalar(in, out, s) })
}
func BenchmarkIDCT8(b *testing.B) {
	benchIDCT(b, 8, 8, func(r, c int, in, out []float32, s int, _ []float32) { idct8x8(in, out, s) })
}

func benchEPF(b *testing.B, f func(a *epf4Args)) {
	n := 256
	mk := func() []float32 {
		s := make([]float32, n+8)
		for i := range s {
			s[i] = rand.Float32()
		}
		return s
	}
	rm := [8]float32{1, 2, 2, 2, 2, 2, 2, 1}
	sig := make([]float32, n/8)
	for i := range sig {
		sig[i] = -0.5
	}
	a := &epf4Args{sv: mk(), svu: mk(), sh: mk(), shl: mk(), sig: sig, rowMul: &rm, n: n}
	for c := 0; c < 3; c++ {
		a.o[c], a.c[c], a.l[c], a.r[c], a.u[c], a.d[c] = mk(), mk(), mk(), mk(), mk(), mk()
	}
	b.SetBytes(int64(n))
	for i := 0; i < b.N; i++ {
		f(a)
	}
}

func BenchmarkEPF4Scalar(b *testing.B) { benchEPF(b, func(a *epf4Args) { epfRow4Scalar(a, 0) }) }
func BenchmarkEPF4(b *testing.B)       { benchEPF(b, epfRow4) }

func TestSIMDMatchesScalar(t *testing.T) {
	for _, d := range [][2]int{{8, 8}, {16, 16}, {32, 32}, {16, 8}, {8, 16}, {32, 8}, {8, 32}, {64, 64}, {32, 64}, {64, 32}, {256, 256}} {
		rows, cols := d[0], d[1]
		in := make([]float32, rows*cols)
		for i := range in {
			in[i] = rand.Float32()*2 - 1
		}
		a, b := append([]float32(nil), in...), append([]float32(nil), in...)
		o1, o2 := make([]float32, rows*cols), make([]float32, rows*cols)
		sc := make([]float32, rows*cols+3*256)
		scaledIDCT2DScalar(rows, cols, a, o1, cols, sc)
		scaledIDCT2D(rows, cols, b, o2, cols, sc)
		for i := range o1 {
			if d := o1[i] - o2[i]; d > 1e-3 || d < -1e-3 {
				t.Fatalf("%dx%d idx %d: %v vs %v", rows, cols, i, o1[i], o2[i])
			}
		}
	}
}
