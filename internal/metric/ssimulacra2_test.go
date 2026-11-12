package metric

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// The recursive filter approximates a normalised Gaussian of sigma 1.5.
func TestRecursiveGaussian(t *testing.T) {
	const n = 41
	in := make([]float32, n)
	in[n/2] = 1
	out := make([]float32, n)
	gauss.blur1D(n, func(i int) float32 { return in[i] }, func(i int, v float32) { out[i] = v })
	var sum, worst float64
	for i, v := range out {
		x := float64(i - n/2)
		g := math.Exp(-x*x/(2*1.5*1.5)) / (1.5 * math.Sqrt(2*math.Pi))
		sum += float64(v)
		worst = max(worst, math.Abs(float64(v)-g))
	}
	if math.Abs(sum-1) > 1e-3 || worst > 0.01 {
		t.Fatalf("sum %v, max deviation %v", sum, worst)
	}
}

func TestSSIMULACRA2(t *testing.T) {
	const w, h = 64, 48
	a := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a.SetNRGBA(x, y, color.NRGBA{byte(x * 4), byte(y * 5), byte((x * y) % 256), 255})
		}
	}
	s, err := SSIMULACRA2(a, a)
	if err != nil || s != 100 {
		t.Fatalf("identical: %v %v", s, err)
	}
	prev := 100.0
	for _, noise := range []int{2, 8, 32} {
		b := image.NewNRGBA(a.Rect)
		copy(b.Pix, a.Pix)
		for i := range b.Pix {
			if i%4 != 3 && (i/4)%3 == 0 {
				b.Pix[i] = byte(min(255, int(b.Pix[i])+noise))
			}
		}
		s, err := SSIMULACRA2(a, b)
		if err != nil || s >= prev {
			t.Fatalf("noise %d: score %v (previous %v), %v", noise, s, prev, err)
		}
		prev = s
	}
}

// With transparency the score is the worse of dark and bright backgrounds
// (reference tool): an error hidden on one background still counts.
func TestSSIMULACRA2Alpha(t *testing.T) {
	const w, h = 64, 48
	a := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a.SetNRGBA(x, y, color.NRGBA{byte(x * 4), byte(y * 5), 90, byte(x * 4)})
		}
	}
	b := image.NewNRGBA(a.Rect)
	copy(b.Pix, a.Pix)
	for i := 3; i < len(b.Pix); i += 4 {
		b.Pix[i] = byte(min(255, int(b.Pix[i])+20)) // more opaque
	}
	s, err := SSIMULACRA2(a, b)
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := ssimulacra2(a, b, 0.1)
	s9, _ := ssimulacra2(a, b, 0.9)
	if s != min(s1, s9) || s1 == s9 {
		t.Fatalf("score %v, backgrounds 0.1: %v, 0.9: %v", s, s1, s9)
	}
}
