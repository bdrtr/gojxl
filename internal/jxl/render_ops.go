package jxl

import (
	"math"
)

// mirror reflects an out-of-range coordinate (libjxl Mirror).
func mirror(x, size int) int {
	for x < 0 || x >= size {
		if x < 0 {
			x = -x - 1
		} else {
			x = 2*size - 1 - x
		}
	}
	return x
}

func buildUpsampleKernel(weights []float32, shift int) []float32 {
	N := 1 << shift
	H := N / 2
	k := make([]float32, N*N*25)
	for ky := 0; ky < H; ky++ {
		for kx := 0; kx < H; kx++ {
			o0 := (ky*N + kx) * 25
			o1 := (ky*N + (N - 1 - kx)) * 25
			o2 := ((N-1-ky)*N + kx) * 25
			o3 := ((N-1-ky)*N + (N - 1 - kx)) * 25
			for py := 0; py < 5; py++ {
				for px := 0; px < 5; px++ {
					j := 5*ky + py
					i := 5*kx + px
					my, mx := min(i, j), max(i, j)
					w := weights[5*H*my-my*(my-1)/2+mx-my]
					k[o0+py*5+px] = w
					k[o1+py*5+(4-px)] = w
					k[o2+(4-py)*5+px] = w
					k[o3+(4-py)*5+(4-px)] = w
				}
			}
		}
	}
	return k
}

// upsample applies the non-separable 2x/4x/8x upsampler and crops to (w,h).
func upsample(p *Plane, factor int, ih *ImageHeader, w, h int) *Plane {
	var shift int
	var weights []float32
	switch factor {
	case 2:
		shift, weights = 1, ih.Up2Weights[:]
	case 4:
		shift, weights = 2, ih.Up4Weights[:]
	case 8:
		shift, weights = 3, ih.Up8Weights[:]
	default:
		return p
	}
	N := factor
	kern := buildUpsampleKernel(weights, shift)
	ow, oh := p.W*N, p.H*N
	out := newPlane(ow, oh)
	// padded input with mirrored border of 2
	pw := p.W + 4
	pad := make([]float32, pw*(p.H+4))
	for y := -2; y < p.H+2; y++ {
		src := p.Row(mirror(y, p.H))
		dst := pad[(y+2)*pw : (y+3)*pw]
		for x := -2; x < p.W+2; x++ {
			dst[x+2] = src[mirror(x, p.W)]
		}
	}
	_ = parallelFor(p.H, func(y int) error {
		var win [25]float32
		for x := 0; x < p.W; x++ {
			mn, mx := float32(math.Inf(1)), float32(math.Inf(-1))
			for iy := 0; iy < 5; iy++ {
				row := pad[(y+iy)*pw+x : (y+iy)*pw+x+5]
				for ix := 0; ix < 5; ix++ {
					v := row[ix]
					win[iy*5+ix] = v
					if v < mn {
						mn = v
					}
					if v > mx {
						mx = v
					}
				}
			}
			for oy := 0; oy < N; oy++ {
				orow := out.Pix[(y*N+oy)*ow+x*N:]
				for ox := 0; ox < N; ox++ {
					kk := kern[(N*oy+ox)*25 : (N*oy+ox)*25+25]
					var s float32
					for i := 0; i < 25; i++ {
						s += win[i] * kk[i]
					}
					if s < mn {
						s = mn
					}
					if s > mx {
						s = mx
					}
					orow[ox] = s
				}
			}
		}
		return nil
	})
	return cropPlane(out, w, h)
}

// srgbLUT maps linear [0,1] to sRGB with linear interpolation.
const srgbLUTSize = 1 << 14

var srgbLUT = func() []float32 {
	t := make([]float32, srgbLUTSize+2)
	for i := range t {
		v := float64(i) / srgbLUTSize
		if v <= 0.0031308 {
			t[i] = float32(v * 12.92)
		} else {
			t[i] = float32(1.055*math.Pow(v, 1/2.4) - 0.055)
		}
	}
	return t
}()

func srgbEncodeSlice(px []float32) {
	for i, v := range px {
		if v >= 0 && v < 1 {
			f := v * srgbLUTSize
			k := int(f)
			fr := f - float32(k)
			px[i] = srgbLUT[k] + (srgbLUT[k+1]-srgbLUT[k])*fr
		} else if v < 0 {
			px[i] = -srgbFromLinear(-v)
		} else {
			px[i] = srgbFromLinear(v)
		}
	}
}
