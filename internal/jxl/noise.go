package jxl

import (
	"math"

	"gojxl/internal/bitio"
)

func (fd *frameDecoder) readNoise(br *bitio.Reader) error {
	for i := range fd.noiseLUT {
		fd.noiseLUT[i] = float32(br.Read(10)) / 1024
	}
	fd.hasNoise = false
	for _, v := range fd.noiseLUT {
		if v != 0 {
			fd.hasNoise = true
		}
	}
	return nil
}

type xorshift128p struct {
	s0, s1 [8]uint64
}

func splitMix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func newXorshift(seed1, seed2, seed3, seed4 uint32) *xorshift128p {
	r := &xorshift128p{}
	r.s0[0] = splitMix64((uint64(seed1)<<32 + uint64(seed2)) + 0x9E3779B97F4A7C15)
	r.s1[0] = splitMix64((uint64(seed3)<<32 + uint64(seed4)) + 0x9E3779B97F4A7C15)
	for i := 1; i < 8; i++ {
		r.s0[i] = splitMix64(r.s0[i-1])
		r.s1[i] = splitMix64(r.s1[i-1])
	}
	return r
}

// fill produces 16 floats in [1,2).
func (r *xorshift128p) fill(out *[16]float32) {
	for i := 0; i < 8; i++ {
		s1 := r.s0[i]
		s0 := r.s1[i]
		bits := s1 + s0
		r.s0[i] = s0
		s1 ^= s1 << 23
		s1 ^= s0 ^ (s1 >> 18) ^ (s0 >> 5)
		r.s1[i] = s1
		lo, hi := uint32(bits), uint32(bits>>32)
		out[2*i] = math.Float32frombits((lo >> 9) | 0x3F800000)
		out[2*i+1] = math.Float32frombits((hi >> 9) | 0x3F800000)
	}
}

// renderNoise generates, convolves and adds noise to the XYB planes.
func (fd *frameDecoder) renderNoise() {
	fh := fd.fh
	W, H := fd.color[0].W, fd.color[0].H
	var planes [3]*Plane
	for c := range planes {
		planes[c] = newPlane(W, H)
	}
	gd := fh.GroupDim
	up := int(fh.Upsampling)
	type tile struct{ x0, y0 int }
	var tiles []tile
	for gy := 0; gy < fh.YGroups; gy++ {
		for gx := 0; gx < fh.XGroups; gx++ {
			for iy := 0; iy < up; iy++ {
				for ix := 0; ix < up; ix++ {
					tiles = append(tiles, tile{(gx*up + ix) * gd, (gy*up + iy) * gd})
				}
			}
		}
	}
	vis, nonvis := fd.dec.visibleIdx, fd.dec.nonvisibleIdx
	_ = parallelFor(len(tiles), func(i int) error {
		t := tiles[i]
		if t.x0 >= W || t.y0 >= H {
			return nil
		}
		rng := newXorshift(uint32(vis), uint32(nonvis), uint32(t.x0), uint32(t.y0))
		w := min(gd, W-t.x0)
		h := min(gd, H-t.y0)
		var batch [16]float32
		for c := 0; c < 3; c++ {
			p := planes[c]
			for y := 0; y < h; y++ {
				row := p.Pix[(t.y0+y)*W+t.x0 : (t.y0+y)*W+t.x0+w]
				for x := 0; x < w; x += 16 {
					rng.fill(&batch)
					copy(row[x:], batch[:min(16, w-x)])
				}
			}
		}
		return nil
	})
	// convolve
	var conv [3]*Plane
	for c := 0; c < 3; c++ {
		pad, pw := padPlane(planes[c], 2)
		out := newPlane(W, H)
		parallelRows(H, func(lo, hi int) {
			for y := lo; y < hi; y++ {
				o := out.Row(y)
				for x := 0; x < W; x++ {
					var others float32
					for i := 0; i < 5; i++ {
						r0 := pad[(y+0)*pw+x+i]
						r1 := pad[(y+1)*pw+x+i]
						r3 := pad[(y+3)*pw+x+i]
						r4 := pad[(y+4)*pw+x+i]
						others += r0
						others += r1
						others += r3
						others += r4
					}
					m := pad[(y+2)*pw+x:]
					others += m[0]
					others += m[1]
					others += m[3]
					others += m[4]
					o[x] = others*0.16 + m[2]*-3.84
				}
			}
		})
		conv[c] = out
	}
	lut := fd.noiseLUT
	strength := func(v float32) float32 {
		s := v * 6
		if s < 0 {
			s = 0
		}
		fl := float32(math.Floor(float64(s)))
		fr := s - fl
		if s >= 7 {
			fl, fr = 6, 1
		}
		k := int(fl)
		lo, hi := lut[k], lut[k+1]
		r := (hi-lo)*fr + lo
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r
	}
	ytox, ytob := float32(0), float32(1)
	if fd.vd != nil {
		ytox, ytob = fd.vd.baseX, fd.vd.baseB
	}
	X, Y, B := fd.color[0].Pix, fd.color[1].Pix, fd.color[2].Pix
	parallelSamples(W*H, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			x, y := X[i], Y[i]
			sg := strength((y - x) * 0.5)
			sr := strength((y + x) * 0.5)
			nr := conv[0].Pix[i] * 0.22
			ng := conv[1].Pix[i] * 0.22
			nc := conv[2].Pix[i] * 0.22
			red := sr * (0.0078125*nr + 0.9921875*nc)
			green := sg * (0.0078125*ng + 0.9921875*nc)
			rg := red + green
			X[i] = ytox*rg + (red - green) + x
			Y[i] = y + rg
			B[i] = ytob*rg + B[i]
		}
	})
}
