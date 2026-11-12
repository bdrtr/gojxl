package enc

import "gojxl/internal/jxl"

// Forward transforms of the 8x8 varblock types made of smaller parts:
// IDENTITY (acs 1), DCT2X2 (2), DCT4X4 (3) and AFV0-3 (14-17), the exact
// inverses of the decoder's. Coefficients use the decoder's layout; index
// 0 holds the block average (the DC the LF image carries).

// fdct8Kind computes the coefficients of an 8x8 block of the given small
// AC strategy from pixels at src (row stride).
func fdct8Kind(acs int, src []float32, stride int, out []float32) {
	switch acs {
	case 12, 13:
		fdctSplit(acs, src, stride, out)
	case 1:
		fwdIdentity(src, stride, out)
	case 2:
		fwdDCT2(src, stride, out)
	case 3:
		fwdDCT4(src, stride, out)
	case 14, 15, 16, 17:
		fwdAFV(acs-14, src, stride, out)
	default:
		panic("fdct8Kind: not an 8x8 small type")
	}
}

// quadDC stores the averages m[y*2+x] of four 4x4 (or split) parts as the
// decoder's b00, b01, b10, b11 at indices 0, 1, 8, 9.
func quadDC(m [4]float32, out []float32) {
	out[0] = (m[0] + m[1] + m[2] + m[3]) / 4
	out[1] = (m[0] + m[1] - m[2] - m[3]) / 4
	out[8] = (m[0] - m[1] + m[2] - m[3]) / 4
	out[9] = (m[0] - m[1] - m[2] + m[3]) / 4
}

// fwdIdentity: each 4x4 quarter keeps its pixels as differences from the
// pixel at (1, 1), whose slot (0, 0) holds the quarter's average; the
// pixel at (0, 0) moves to slot (1, 1).
func fwdIdentity(src []float32, stride int, out []float32) {
	var m [4]float32
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			var p [4][4]float32
			var sum float32
			for iy := 0; iy < 4; iy++ {
				for ix := 0; ix < 4; ix++ {
					p[iy][ix] = src[(4*y+iy)*stride+4*x+ix]
					sum += p[iy][ix]
				}
			}
			m[y*2+x] = sum / 16
			center := p[1][1]
			for iy := 0; iy < 4; iy++ {
				for ix := 0; ix < 4; ix++ {
					v := p[iy][ix] - center
					switch {
					case iy == 0 && ix == 0:
						continue // slot of the average
					case iy == 1 && ix == 1:
						v = p[0][0] - center
					}
					out[(y+iy*2)*8+x+ix*2] = v
				}
			}
		}
	}
	quadDC(m, out)
}

// fwdDCT2 is the hierarchical 2x2 Haar transform: levels of 8, 4 and 2
// pixels, each leaving the averages in the top left quarter.
func fwdDCT2(src []float32, stride int, out []float32) {
	var b [64]float32
	for y := 0; y < 8; y++ {
		copy(b[y*8:y*8+8], src[y*stride:y*stride+8])
	}
	for _, s := range []int{8, 4, 2} {
		var t [64]float32
		n := s / 2
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				a, bb := b[2*y*8+2*x], b[2*y*8+2*x+1]
				c, d := b[(2*y+1)*8+2*x], b[(2*y+1)*8+2*x+1]
				t[y*8+x] = (a + bb + c + d) / 4
				t[y*8+n+x] = (a + bb - c - d) / 4
				t[(y+n)*8+x] = (a - bb + c - d) / 4
				t[(y+n)*8+n+x] = (a - bb - c + d) / 4
			}
		}
		for y := 0; y < s; y++ {
			copy(b[y*8:y*8+s], t[y*8:y*8+s])
		}
	}
	copy(out[:64], b[:])
}

// fwdDCT4: four 4x4 DCTs, coefficients interleaved.
func fwdDCT4(src []float32, stride int, out []float32) {
	var m [4]float32
	var blk [16]float32
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			fdctRect(src[4*y*stride+4*x:], stride, 4, 4, blk[:])
			m[y*2+x] = blk[0]
			for iy := 0; iy < 4; iy++ {
				for ix := 0; ix < 4; ix++ {
					if iy|ix != 0 {
						out[(y+iy*2)*8+x+ix*2] = blk[iy*4+ix]
					}
				}
			}
		}
	}
	quadDC(m, out)
}

var afvBasis = jxl.AFVBasis()

// fwdAFV: a 4x4 corner (flipped towards (afvY, afvX)) in the AFV basis,
// the 4x4 next to it as a DCT and the other 4x8 half as a DCT.
func fwdAFV(kind int, src []float32, stride int, out []float32) {
	afvX, afvY := kind&1, kind/2
	// corner pixels in the basis orientation
	var p [16]float32
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 4; ix++ {
			yy, xx := iy, ix
			if afvY == 1 {
				yy = 3 - iy
			}
			if afvX == 1 {
				xx = 3 - ix
			}
			p[yy*4+xx] = src[(iy+afvY*4)*stride+afvX*4+ix]
		}
	}
	var dcs [3]float32
	for j := 0; j < 16; j++ {
		var a float32
		for i, v := range p {
			a += afvBasis[j][i] * v
		}
		if j == 0 {
			dcs[0] = a
			continue
		}
		out[(j/4)*2*8+(j%4)*2] = a
	}
	// the 4x4 DCT beside the corner
	var blk [32]float32
	xo := 4
	if afvX == 1 {
		xo = 0
	}
	fdctRect(src[afvY*4*stride+xo:], stride, 4, 4, blk[:16])
	dcs[1] = blk[0]
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 4; ix++ {
			if iy|ix != 0 {
				out[iy*2*8+ix*2+1] = blk[iy*4+ix]
			}
		}
	}
	// the 4x8 DCT of the other half
	yo := 4
	if afvY == 1 {
		yo = 0
	}
	fdctRect(src[yo*stride:], stride, 4, 8, blk[:])
	dcs[2] = blk[0]
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 8; ix++ {
			if iy|ix != 0 {
				out[(1+iy*2)*8+ix] = blk[iy*8+ix]
			}
		}
	}
	// dcs = 4(b00 + b10 + b01), b00 + b10 - b01, b00 - b10
	s := dcs[0] / 4
	b01 := (s - dcs[1]) / 2
	u := (s + dcs[1]) / 2
	out[0], out[1], out[8] = (u+dcs[2])/2, b01, (u-dcs[2])/2
}
