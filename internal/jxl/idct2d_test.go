package jxl

import (
	"math"
	"math/rand"
	"testing"
)

// scaledIDCT2DRef (previous implementation) performs a rows x cols IDCT using libjxl's coefficient layout:
// if rows < cols, in is rows x cols (natural); otherwise in is cols x rows
// (transposed). Output pixels are written to out with the given stride.
// in is clobbered.
func scaledIDCT2DRef(rows, cols int, in []float32, out []float32, stride int, scratch []float32) {
	m := max(rows, cols)
	tmp := scratch[:2*m]
	col := scratch[2*m : 3*m]
	if rows < cols {
		for r := 0; r < rows; r++ {
			idct1d(in[r*cols:(r+1)*cols], tmp)
		}
		cl := col[:rows]
		for x := 0; x < cols; x++ {
			for r := range cl {
				cl[r] = in[r*cols+x]
			}
			idct1d(cl, tmp)
			for r, v := range cl {
				out[r*stride+x] = v
			}
		}
		return
	}
	for a := 0; a < cols; a++ {
		idct1d(in[a*rows:(a+1)*rows], tmp)
	}
	cl := col[:cols]
	for y := 0; y < rows; y++ {
		for a := range cl {
			cl[a] = in[a*rows+y]
		}
		idct1d(cl, tmp)
		copy(out[y*stride:y*stride+cols], cl)
	}
}

func TestIDCT2DMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, sz := range [][2]int{{1, 2}, {2, 1}, {2, 2}, {4, 4}, {4, 8}, {8, 4}, {8, 8}, {8, 16}, {16, 8}, {16, 16}, {16, 32}, {32, 16}, {32, 32}, {32, 64}, {64, 32}, {64, 64}, {64, 128}, {128, 64}, {128, 128}, {128, 256}, {256, 128}, {256, 256}} {
		rows, cols := sz[0], sz[1]
		n := rows * cols
		in := make([]float32, n)
		for i := range in {
			in[i] = rng.Float32()*2 - 1
		}
		stride := cols + 3
		want := make([]float32, rows*stride)
		got := make([]float32, rows*stride)
		scaledIDCT2DRef(rows, cols, append([]float32(nil), in...), want, stride, make([]float32, 3*max(rows, cols)))
		scaledIDCT2DScalar(rows, cols, append([]float32(nil), in...), got, stride, make([]float32, 2*n))
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				w, g := want[y*stride+x], got[y*stride+x]
				if math.Abs(float64(w-g)) > 2e-6*float64(rows*cols) {
					t.Fatalf("%dx%d (%d,%d): got %v want %v", rows, cols, y, x, g, w)
				}
			}
		}
	}
}

func BenchmarkTranspose32(b *testing.B) {
	src, dst := make([]float32, 1024), make([]float32, 1024)
	for i := 0; i < b.N; i++ {
		transposeTo(dst, src, 32, 32)
	}
}

func BenchmarkIDCTLanes32(b *testing.B) {
	x, tmp := make([]float32, 1024), make([]float32, 1024)
	for i := 0; i < b.N; i++ {
		idctLanes(x, 32, 32, tmp)
	}
}

func BenchmarkIDCT1D32Rows(b *testing.B) {
	x, tmp := make([]float32, 1024), make([]float32, 64)
	for i := 0; i < b.N; i++ {
		for r := 0; r < 32; r++ {
			idct1d(x[r*32:(r+1)*32], tmp)
		}
	}
}
