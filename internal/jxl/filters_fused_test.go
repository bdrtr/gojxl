package jxl_test

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"testing"

	"github.com/bdrtr/gojxl/internal/enc"
	"github.com/bdrtr/gojxl/internal/jxl"
)

func fusedTestImage(w, h int, seed int64) *enc.Image {
	rng := rand.New(rand.NewSource(seed))
	im := &enc.Image{W: w, H: h, BitDepth: 8}
	for c := 0; c < 3; c++ {
		ch := make([]int32, w*h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := 128 + 60*math.Sin(float64(x)*0.13+float64(c)) + 50*math.Cos(float64(y)*0.05*float64(c+1))
				if (x/7+y/11)%4 == 0 {
					v += 40
				}
				v += rng.NormFloat64() * 6
				ch[y*w+x] = int32(min(max(v, 0), 255))
			}
		}
		im.Chans = append(im.Chans, ch)
	}
	return im
}

func decodePlanes(t *testing.T, data []byte, separate bool) [3][]float32 {
	t.Helper()
	*jxl.NoFusedFilters = separate
	defer func() { *jxl.NoFusedFilters = false }()
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	f, err := d.NextFrame()
	if err != nil {
		t.Fatal(err)
	}
	var out [3][]float32
	for c := range out {
		out[c] = f.Color[c].Pix
	}
	return out
}

// The fused Gaborish + EPF pass gives exactly the separate passes' result.
func TestGabEPFMatchesSeparate(t *testing.T) {
	sizes := [][2]int{{8, 8}, {9, 13}, {67, 21}, {256, 40}, {300, 257}, {64, 600}}
	for _, procs := range []int{1, 3, runtime.NumCPU()} {
		old := runtime.GOMAXPROCS(procs)
		for i, sz := range sizes {
			for _, cfg := range []struct {
				iters int
				noGab bool
				d     float32
			}{{1, false, 1}, {1, true, 1}, {2, false, 2}, {3, false, 4}, {3, true, 4}} {
				name := fmt.Sprintf("p%d/%dx%d/iters=%d/nogab=%v", procs, sz[0], sz[1], cfg.iters, cfg.noGab)
				data, err := enc.EncodeVarDCT(fusedTestImage(sz[0], sz[1], int64(i)), enc.LossyOptions{Distance: cfg.d, EPFIters: cfg.iters, Sharpness: -1, NoGaborish: cfg.noGab})
				if err != nil {
					t.Fatal(name, err)
				}
				a, b := decodePlanes(t, data, false), decodePlanes(t, data, true)
				// SIMD kernels round their vector part (FMA) and scalar tail
				// differently, so there the result depends on where rows
				// start; index errors are far larger
				tol := 0.0
				if jxl.SIMDActive() {
					tol = 1e-6
				}
				for c := range a {
					for k := range a[c] {
						if math.Abs(float64(a[c][k]-b[c][k])) > tol {
							t.Fatalf("%s: channel %d sample %d (%d,%d): fused %g, separate %g", name, c, k, k%sz[0], k/sz[0], a[c][k], b[c][k])
						}
					}
				}
			}
		}
		runtime.GOMAXPROCS(old)
	}
}
