package jxl

import (
	"fmt"
	"math"
	"math/bits"
	"os"
)

const sqrt2 = float32(math.Sqrt2)

var secHalfTab = func() (t [9][]float32) {
	for lg := 1; lg <= 8; lg++ {
		n := 1 << lg
		t[lg] = make([]float32, n/2)
		for i := range t[lg] {
			t[lg][i] = float32(1 / (2 * math.Cos((float64(i)+0.5)*math.Pi/float64(n))))
		}
	}
	return
}()

// secHalf returns 1/(2*cos((i+0.5)*pi/N)) for i < N/2.
func secHalf(n int) []float32 {
	return secHalfTab[bits.TrailingZeros(uint(n))]
}

// idct1d computes the scaled inverse DCT in place:
// x[n] = c[0] + sqrt2 * sum_{k>0} c[k] cos(pi*(2n+1)*k/(2N)).
// tmp must have len >= len(x).
func idct1d(x, tmp []float32) {
	n := len(x)
	switch n {
	case 1:
		return
	case 2:
		a, b := x[0], x[1]
		x[0], x[1] = a+b, a-b
		return
	case 8:
		idct8(x)
		return
	case 4:
		// even part
		e0 := x[0] + x[2]
		e1 := x[0] - x[2]
		// odd part (B transpose)
		o1 := x[3] + x[1]
		o0 := x[1] * sqrt2
		p0 := o0 + o1
		p1 := o0 - o1
		s := secHalf(4)
		p0 *= s[0]
		p1 *= s[1]
		x[0] = e0 + p0
		x[3] = e0 - p0
		x[1] = e1 + p1
		x[2] = e1 - p1
		return
	}
	h := n / 2
	for i := 0; i < h; i++ {
		tmp[i] = x[2*i]
		tmp[h+i] = x[2*i+1]
	}
	odd := tmp[h:n]
	for i := h - 1; i > 0; i-- {
		odd[i] += odd[i-1]
	}
	odd[0] *= sqrt2
	idct1d(tmp[:h], x[:h])
	idct1d(odd, x[h:n])
	s := secHalf(n)
	for i := 0; i < h; i++ {
		a := tmp[i]
		b := tmp[h+i] * s[i]
		x[i] = a + b
		x[n-1-i] = a - b
	}
}

var (
	sec4  = secHalfTab[2]
	sec8  = secHalfTab[3]
	sec4a = sec4[0]
	sec4b = sec4[1]

	sec8c0, sec8c1, sec8c2, sec8c3 = sec8[0], sec8[1], sec8[2], sec8[3]
)

// idct8 is an unrolled 8-point scaled IDCT.
func idct8(x []float32) {
	_ = x[7]
	// even part: 4-point IDCT of x0,x2,x4,x6
	e0 := x[0] + x[4]
	e1 := x[0] - x[4]
	eo1 := x[6] + x[2]
	eo0 := x[2] * sqrt2
	ep0 := (eo0 + eo1) * sec4a
	ep1 := (eo0 - eo1) * sec4b
	E0 := e0 + ep0
	E3 := e0 - ep0
	E1 := e1 + ep1
	E2 := e1 - ep1
	// odd part: B transpose of x1,x3,x5,x7 then 4-point IDCT
	o0 := x[1] * sqrt2
	o1 := x[3] + x[1]
	o2 := x[5] + x[3]
	o3 := x[7] + x[5]
	f0 := o0 + o2
	f1 := o0 - o2
	fo1 := o3 + o1
	fo0 := o1 * sqrt2
	fp0 := (fo0 + fo1) * sec4a
	fp1 := (fo0 - fo1) * sec4b
	O0 := (f0 + fp0) * sec8[0]
	O3 := (f0 - fp0) * sec8[3]
	O1 := (f1 + fp1) * sec8[1]
	O2 := (f1 - fp1) * sec8[2]
	x[0] = E0 + O0
	x[7] = E0 - O0
	x[1] = E1 + O1
	x[6] = E1 - O1
	x[2] = E2 + O2
	x[5] = E2 - O2
	x[3] = E3 + O3
	x[4] = E3 - O3
}

// dctCos[lg][k*n+i] = cos(pi*(2i+1)k/(2n)) for n = 1<<lg.
var dctCos = func() (t [6][]float64) {
	for lg := range t {
		n := 1 << lg
		t[lg] = make([]float64, n*n)
		for k := 0; k < n; k++ {
			for i := 0; i < n; i++ {
				t[lg][k*n+i] = math.Cos(math.Pi * float64((2*i+1)*k) / float64(2*n))
			}
		}
	}
	return
}()

// dct1dScaled computes the forward scaled DCT (c[0] = mean) directly.
func dct1dScaled(x []float32, out []float32) {
	n := len(x)
	if lg := bits.TrailingZeros(uint(n)); n == 1<<lg && lg < len(dctCos) {
		ct := dctCos[lg]
		for k := 0; k < n; k++ {
			var s float64
			if k == 0 {
				for _, v := range x {
					s += float64(v)
				}
			} else {
				row := ct[k*n : (k+1)*n]
				for i, v := range x {
					s += float64(v) * math.Sqrt2 * row[i]
				}
			}
			out[k] = float32(s / float64(n))
		}
		return
	}
	for k := 0; k < n; k++ {
		var s float64
		for i := 0; i < n; i++ {
			if k == 0 {
				s += float64(x[i])
			} else {
				s += float64(x[i]) * math.Sqrt2 * math.Cos(math.Pi*float64((2*i+1)*k)/float64(2*n))
			}
		}
		out[k] = float32(s / float64(n))
	}
}

// scaledIDCT2D performs a rows x cols IDCT using libjxl's coefficient layout:
// if rows < cols, in is rows x cols (natural); otherwise in is cols x rows
// (transposed). Output pixels are written to out with the given stride.
// in is clobbered; scratch needs 2*rows*cols floats.
//
// Both passes run over all lanes (columns of the current layout) at once,
// with one transpose in between, so no column is gathered or scattered on
// its own.
func scaledIDCT2DScalar(rows, cols int, in []float32, out []float32, stride int, scratch []float32) {
	n := rows * cols
	tmp, t := scratch[:n], scratch[n:2*n]
	if rows < cols {
		// in[r][x]: along r for every x, then along x for every r
		idctLanes(in[:n], rows, cols, tmp)
		transposeTo(t, in[:n], rows, cols) // t[x][r]
		idctLanes(t, cols, rows, tmp)
		for r := 0; r < rows; r++ {
			o := out[r*stride : r*stride+cols]
			for x := range o {
				o[x] = t[x*rows+r]
			}
		}
		return
	}
	// in[x][y]: along x for every y, then along y for every x
	idctLanes(in[:n], cols, rows, tmp)
	transposeTo(t, in[:n], cols, rows) // t[y][x]
	idctLanes(t, rows, cols, tmp)
	for y := 0; y < rows; y++ {
		copy(out[y*stride:y*stride+cols], t[y*cols:(y+1)*cols])
	}
}

// transposeTo writes the transpose of the r x c array src to dst (c x r).
func transposeTo(dst, src []float32, r, c int) {
	src, dst = src[:r*c], dst[:r*c]
	for i := 0; i < r; i++ {
		row := src[i*c : (i+1)*c]
		for j, v := range row {
			dst[j*r+i] = v
		}
	}
}

// idctLanes computes the scaled IDCT of length n along the first axis of
// the n x L array x (L independent lanes) in place; tmp needs n*L floats.
func idctLanes(x []float32, n, L int, tmp []float32) {
	switch n {
	case 1:
		return
	case 2:
		a, b := x[:L], x[L:2*L]
		for l, av := range a {
			bv := b[l]
			a[l], b[l] = av+bv, av-bv
		}
		return
	case 4:
		x0, x1, x2, x3 := x[:L], x[L:2*L], x[2*L:3*L], x[3*L:4*L]
		x1, x2, x3 = x1[:len(x0)], x2[:len(x0)], x3[:len(x0)]
		for l, v0 := range x0 {
			v1, v2, v3 := x1[l], x2[l], x3[l]
			e0, e1 := v0+v2, v0-v2
			o1 := v3 + v1
			o0 := v1 * sqrt2
			p0 := (o0 + o1) * sec4a
			p1 := (o0 - o1) * sec4b
			x0[l], x3[l] = e0+p0, e0-p0
			x1[l], x2[l] = e1+p1, e1-p1
		}
		return
	case 8:
		idct8Lanes(x, L)
		return
	}
	h := n / 2
	// even rows to tmp[:h]; odd rows plus the odd row before (B transpose)
	// to tmp[h:]
	for i := 0; i < h; i++ {
		copy(tmp[i*L:(i+1)*L], x[2*i*L:(2*i+1)*L])
	}
	odd := tmp[h*L : n*L]
	o0, x1 := odd[:L], x[L:2*L]
	x1 = x1[:len(o0)]
	for l := range o0 {
		o0[l] = x1[l] * sqrt2
	}
	for i := 1; i < h; i++ {
		d := odd[i*L : (i+1)*L]
		a, b := x[(2*i+1)*L:(2*i+2)*L], x[(2*i-1)*L:2*i*L]
		a, b = a[:len(d)], b[:len(d)]
		for l := range d {
			d[l] = a[l] + b[l]
		}
	}
	idctLanes(tmp[:h*L], h, L, x[:h*L])
	idctLanes(odd, h, L, x[h*L:n*L])
	s := secHalf(n)
	for i := 0; i < h; i++ {
		a, b := tmp[i*L:(i+1)*L], tmp[(h+i)*L:(h+i+1)*L]
		lo, hi := x[i*L:(i+1)*L], x[(n-1-i)*L:(n-i)*L]
		b, lo, hi = b[:len(a)], lo[:len(a)], hi[:len(a)]
		si := s[i]
		for l, av := range a {
			bv := b[l] * si
			lo[l], hi[l] = av+bv, av-bv
		}
	}
}

// idct8Lanes is idct8 over L lanes (x is 8 x L).
func idct8Lanes(x []float32, L int) {
	x0, x1, x2, x3 := x[:L], x[L:2*L], x[2*L:3*L], x[3*L:4*L]
	x4, x5, x6, x7 := x[4*L:5*L], x[5*L:6*L], x[6*L:7*L], x[7*L:8*L]
	x1, x2, x3 = x1[:len(x0)], x2[:len(x0)], x3[:len(x0)]
	x4, x5, x6, x7 = x4[:len(x0)], x5[:len(x0)], x6[:len(x0)], x7[:len(x0)]
	s80, s81, s82, s83 := sec8[0], sec8[1], sec8[2], sec8[3]
	for l := range x0 {
		v0, v2, v4, v6 := x0[l], x2[l], x4[l], x6[l]
		e0, e1 := v0+v4, v0-v4
		eo1 := v6 + v2
		eo0 := v2 * sqrt2
		ep0 := (eo0 + eo1) * sec4a
		ep1 := (eo0 - eo1) * sec4b
		E0, E3 := e0+ep0, e0-ep0
		E1, E2 := e1+ep1, e1-ep1
		v1, v3, v5, v7 := x1[l], x3[l], x5[l], x7[l]
		o0 := v1 * sqrt2
		o1 := v3 + v1
		o2 := v5 + v3
		o3 := v7 + v5
		f0, f1 := o0+o2, o0-o2
		fo1 := o3 + o1
		fo0 := o1 * sqrt2
		fp0 := (fo0 + fo1) * sec4a
		fp1 := (fo0 - fo1) * sec4b
		O0 := (f0 + fp0) * s80
		O3 := (f0 - fp0) * s83
		O1 := (f1 + fp1) * s81
		O2 := (f1 - fp1) * s82
		x0[l], x7[l] = E0+O0, E0-O0
		x1[l], x6[l] = E1+O1, E1-O1
		x2[l], x5[l] = E2+O2, E2-O2
		x3[l], x4[l] = E3+O3, E3-O3
	}
}

// idct8x8 is the DCT8 fast path (transposed storage).
func idct8x8Scalar(c []float32, out []float32, stride int) {
	// both passes run down the columns of an 8x8 array (one base pointer,
	// constant offsets, no bounds checks) with a transpose between them
	cc := (*[64]float32)(c[:64])
	idct8Cols(cc)
	var t [64]float32
	for a := 0; a < 8; a++ {
		r := cc[a*8 : a*8+8 : a*8+8]
		t[a], t[8+a], t[16+a], t[24+a] = r[0], r[1], r[2], r[3]
		t[32+a], t[40+a], t[48+a], t[56+a] = r[4], r[5], r[6], r[7]
	}
	idct8Cols(&t)
	for y := 0; y < 8; y++ {
		copy(out[y*stride:y*stride+8], t[y*8:y*8+8])
	}
}

// idct8Cols applies idct8 to each column of the row-major 8x8 array x.
func idct8Cols(x *[64]float32) {
	for l := 0; l < 8; l++ {
		v0, v2, v4, v6 := x[l], x[16+l], x[32+l], x[48+l]
		e0, e1 := v0+v4, v0-v4
		eo1 := v6 + v2
		eo0 := v2 * sqrt2
		ep0 := (eo0 + eo1) * sec4a
		ep1 := (eo0 - eo1) * sec4b
		E0, E3 := e0+ep0, e0-ep0
		E1, E2 := e1+ep1, e1-ep1
		v1, v3, v5, v7 := x[8+l], x[24+l], x[40+l], x[56+l]
		o0 := v1 * sqrt2
		o1 := v3 + v1
		o2 := v5 + v3
		o3 := v7 + v5
		f0, f1 := o0+o2, o0-o2
		fo1 := o3 + o1
		fo0 := o1 * sqrt2
		fp0 := (fo0 + fo1) * sec4a
		fp1 := (fo0 - fo1) * sec4b
		O0 := (f0 + fp0) * sec8c0
		O3 := (f0 - fp0) * sec8c3
		O1 := (f1 + fp1) * sec8c1
		O2 := (f1 - fp1) * sec8c2
		x[l], x[56+l] = E0+O0, E0-O0
		x[8+l], x[48+l] = E1+O1, E1-O1
		x[16+l], x[40+l] = E2+O2, E2-O2
		x[24+l], x[32+l] = E3+O3, E3-O3
	}
}

var resampleTab = func() (t [6][]float32) {
	for lg := range t {
		n := 1 << lg
		t[lg] = make([]float32, n)
		for k := range t[lg] {
			t[lg][k] = resampleScaleSlow(n, k)
		}
	}
	return
}()

// resampleScale is DCTTotalResampleScale for an N-point LF DCT upsampled to 8N.
func resampleScale(n, k int) float32 {
	if lg := bits.TrailingZeros(uint(n)); n == 1<<lg && lg < len(resampleTab) && k < n {
		return resampleTab[lg][k]
	}
	return resampleScaleSlow(n, k)
}

func resampleScaleSlow(n, k int) float32 {
	if k == 0 {
		return 1
	}
	// The LF value is the 8x8 block average of a smooth signal, which damps
	// frequency k by sin(pi k/2N) / (8 sin(pi k/16N)); undo that.
	return float32((8 * math.Sin(math.Pi*float64(k)/float64(16*n))) / math.Sin(math.Pi*float64(k)/float64(2*n)))
}

func idct2TopBlock(s int, block []float32) {
	var temp [64]float32
	n := s / 2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			c00 := block[y*8+x]
			c01 := block[y*8+n+x]
			c10 := block[(y+n)*8+x]
			c11 := block[(y+n)*8+n+x]
			temp[y*2*8+x*2] = c00 + c01 + c10 + c11
			temp[y*2*8+x*2+1] = c00 + c01 - c10 - c11
			temp[(y*2+1)*8+x*2] = c00 - c01 + c10 - c11
			temp[(y*2+1)*8+x*2+1] = c00 - c01 - c10 + c11
		}
	}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			block[y*8+x] = temp[y*8+x]
		}
	}
}

func afvIDCT4x4(coeffs []float32, pixels []float32) {
	for i := 0; i < 16; i++ {
		var p float32
		for j := 0; j < 16; j++ {
			p += coeffs[j] * afvBasis[j][i]
		}
		pixels[i] = p
	}
}

func afvTransform(kind int, c []float32, out []float32, stride int, scratch []float32) {
	afvX := kind & 1
	afvY := kind / 2
	b00, b01, b10 := c[0], c[1], c[8]
	dcs := [3]float32{(b00 + b10 + b01) * 4, b00 + b10 - b01, b00 - b10}
	var coeff [16]float32
	coeff[0] = dcs[0]
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 4; ix++ {
			if ix == 0 && iy == 0 {
				continue
			}
			coeff[iy*4+ix] = c[iy*2*8+ix*2]
		}
	}
	var block [32]float32
	afvIDCT4x4(coeff[:], block[:16])
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 4; ix++ {
			yy, xx := iy, ix
			if afvY == 1 {
				yy = 3 - iy
			}
			if afvX == 1 {
				xx = 3 - ix
			}
			out[(iy+afvY*4)*stride+afvX*4+ix] = block[yy*4+xx]
		}
	}
	block[0] = dcs[1]
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 4; ix++ {
			if ix == 0 && iy == 0 {
				continue
			}
			block[iy*4+ix] = c[iy*2*8+ix*2+1]
		}
	}
	xo := 4
	if afvX == 1 {
		xo = 0
	}
	scaledIDCT2D(4, 4, block[:16], out[afvY*4*stride+xo:], stride, scratch)
	block[0] = dcs[2]
	for iy := 0; iy < 4; iy++ {
		for ix := 0; ix < 8; ix++ {
			if ix == 0 && iy == 0 {
				continue
			}
			block[iy*8+ix] = c[(1+iy*2)*8+ix]
		}
	}
	yo := 4
	if afvY == 1 {
		yo = 0
	}
	scaledIDCT2D(4, 8, block[:32], out[yo*stride:], stride, scratch)
}

// transformToPixels converts dequantized coefficients (libjxl layout, with
// LLF already filled) to pixels.
func transformToPixels(t int, c []float32, out []float32, stride int, scratch []float32) {
	switch t {
	case acsIdentity:
		b00, b01, b10, b11 := c[0], c[1], c[8], c[9]
		dcs := [4]float32{b00 + b01 + b10 + b11, b00 + b01 - b10 - b11, b00 - b01 + b10 - b11, b00 - b01 - b10 + b11}
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				bdc := dcs[y*2+x]
				var rs float32
				for iy := 0; iy < 4; iy++ {
					for ix := 0; ix < 4; ix++ {
						if ix == 0 && iy == 0 {
							continue
						}
						rs += c[(y+iy*2)*8+x+ix*2]
					}
				}
				center := bdc - rs*(1.0/16)
				out[(4*y+1)*stride+4*x+1] = center
				for iy := 0; iy < 4; iy++ {
					for ix := 0; ix < 4; ix++ {
						if ix == 1 && iy == 1 {
							continue
						}
						out[(y*4+iy)*stride+x*4+ix] = c[(y+iy*2)*8+x+ix*2] + center
					}
				}
				out[y*4*stride+x*4] = c[(y+2)*8+x+2] + center
			}
		}
	case acsDCT8x4:
		d0, d1 := c[0]+c[8], c[0]-c[8]
		dcs := [2]float32{d0, d1}
		var block [32]float32
		for x := 0; x < 2; x++ {
			block[0] = dcs[x]
			for iy := 0; iy < 4; iy++ {
				for ix := 0; ix < 8; ix++ {
					if ix == 0 && iy == 0 {
						continue
					}
					block[iy*8+ix] = c[(x+iy*2)*8+ix]
				}
			}
			scaledIDCT2D(8, 4, block[:], out[x*4:], stride, scratch)
		}
	case acsDCT4x8:
		d0, d1 := c[0]+c[8], c[0]-c[8]
		dcs := [2]float32{d0, d1}
		var block [32]float32
		for y := 0; y < 2; y++ {
			block[0] = dcs[y]
			for iy := 0; iy < 4; iy++ {
				for ix := 0; ix < 8; ix++ {
					if ix == 0 && iy == 0 {
						continue
					}
					block[iy*8+ix] = c[(y+iy*2)*8+ix]
				}
			}
			scaledIDCT2D(4, 8, block[:], out[y*4*stride:], stride, scratch)
		}
	case acsDCT4:
		b00, b01, b10, b11 := c[0], c[1], c[8], c[9]
		dcs := [4]float32{b00 + b01 + b10 + b11, b00 + b01 - b10 - b11, b00 - b01 + b10 - b11, b00 - b01 - b10 + b11}
		var block [16]float32
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				block[0] = dcs[y*2+x]
				for iy := 0; iy < 4; iy++ {
					for ix := 0; ix < 4; ix++ {
						if ix == 0 && iy == 0 {
							continue
						}
						block[iy*4+ix] = c[(y+iy*2)*8+x+ix*2]
					}
				}
				scaledIDCT2D(4, 4, block[:], out[y*4*stride+x*4:], stride, scratch)
			}
		}
	case acsDCT2:
		var co [64]float32
		copy(co[:], c[:64])
		idct2TopBlock(2, co[:])
		idct2TopBlock(4, co[:])
		idct2TopBlock(8, co[:])
		for y := 0; y < 8; y++ {
			copy(out[y*stride:y*stride+8], co[y*8:y*8+8])
		}
	case 14, 15, 16, 17:
		afvTransform(t-14, c, out, stride, scratch)
	case acsDCT:
		idct8x8(c, out, stride)
	default:
		rows, cols := 8*acsCoveredY[t], 8*acsCoveredX[t]
		scaledIDCT2D(rows, cols, c, out, stride, scratch)
	}
}

// llfFromDC fills the lowest frequency coefficients from the LF image.
func llfFromDC(t int, dc *Plane, bx, by int, c []float32) {
	cx, cy := acsCoveredX[t], acsCoveredY[t]
	if cx == 1 && cy == 1 {
		c[0] = dc.Pix[by*dc.W+bx]
		return
	}
	// natural LF matrix cy x cx: DCT of dc block
	var buf [32 * 32]float32
	var row, tmp [32]float32
	for y := 0; y < cy; y++ {
		dct1dScaled(dc.Pix[(by+y)*dc.W+bx:(by+y)*dc.W+bx+cx], buf[y*cx:(y+1)*cx])
	}
	for x := 0; x < cx; x++ {
		for y := 0; y < cy; y++ {
			row[y] = buf[y*cx+x]
		}
		dct1dScaled(row[:cy], tmp[:cy])
		for y := 0; y < cy; y++ {
			buf[y*cx+x] = tmp[y]
		}
	}
	stride := 8 * max(cx, cy)
	if cy < cx {
		for y := 0; y < cy; y++ {
			for x := 0; x < cx; x++ {
				c[y*stride+x] = buf[y*cx+x] * resampleScale(cy, y) * resampleScale(cx, x)
			}
		}
	} else {
		// transposed: index [kx][ky]
		for kx := 0; kx < cx; kx++ {
			for ky := 0; ky < cy; ky++ {
				c[kx*stride+ky] = buf[ky*cx+kx] * resampleScale(cy, ky) * resampleScale(cx, kx)
			}
		}
	}
}

// recon holds what the dequantisation and transform of groups needs.
type recon struct {
	xdm, bdm, invGS float32
	dqb             [3]*dequantBias
	maxN            int
	ws              []*reconScratch // per worker
}

type reconScratch struct {
	coef    [3][]float32
	scratch []float32
}

// prepareRecon allocates the XYB planes and readies the dequantisation
// tables; after it, groups can be reconstructed (reconGroup) in any order.
func (v *vardctState) prepareRecon() error {
	if v.rc != nil {
		return nil
	}
	fh := v.fh
	ih := v.fd.ih
	for c := 0; c < 3; c++ {
		v.out[c] = getPlane((v.bw*8)>>v.hs[c], (v.bh*8)>>v.vs[c])
	}
	// make sure tables for used strategies are ready
	for t := 0; t < numACS; t++ {
		if v.usedACS&(1<<t) != 0 {
			if _, err := v.qtable(acsQuantTable[t]); err != nil {
				return err
			}
		}
	}
	rc := &recon{
		xdm:   powf(0.8, float32(fh.XQMScale)-2),
		bdm:   powf(0.8, float32(fh.BQMScale)-2),
		invGS: float32(65536.0 / float64(v.globalScale)),
		maxN:  64,
		ws:    make([]*reconScratch, numWorkers()),
	}
	for c := range rc.dqb {
		rc.dqb[c] = newDequantBias(ih.Opsin.QuantBias[c], ih.Opsin.QuantBiasNumerator)
	}
	for t := 0; t < numACS; t++ {
		if v.usedACS&(1<<t) != 0 {
			rc.maxN = max(rc.maxN, 64*acsCoveredX[t]*acsCoveredY[t])
		}
	}
	v.rc = rc
	return nil
}

// reconGroup dequantises and transforms group g into the XYB planes (wk:
// worker index for scratch buffers) and releases its coefficients.
func (v *vardctState) reconGroup(wk, g int) {
	rc := v.rc
	gc := v.groups[g]
	if rc.ws[wk] == nil {
		w := &reconScratch{scratch: make([]float32, 2*rc.maxN+3*256)}
		for c := 0; c < 3; c++ {
			w.coef[c] = make([]float32, rc.maxN)
		}
		rc.ws[wk] = w
	}
	coef := rc.ws[wk].coef
	scratch := rc.ws[wk].scratch
	for vi := range gc.vbs {
		vb := &gc.vbs[vi]
		t := int(vb.typ)
		n := 64 * acsCoveredX[t] * acsCoveredY[t]
		tables := v.qtables[acsQuantTable[t]]
		scale := rc.invGS / float32(vb.qf)
		muls := [3]float32{scale * rc.xdm, scale, scale * rc.bdm}
		// dequantize
		for c := 0; c < 3; c++ {
			if vb.off[c] < 0 {
				continue
			}
			q := gc.coef[c][vb.off[c] : int(vb.off[c])+n]
			m := tables[c][:n]
			o := coef[c][:n]
			mul := muls[c]
			dequantRow(q, m, o, mul, rc.dqb[c])
		}
		if !v.sub {
			tx, ty := int(vb.bx)/8, int(vb.by)/8
			xcc := v.baseX + float32(v.ytox[ty*v.tw+tx])/float32(v.colorFactor)
			bcc := v.baseB + float32(v.ytob[ty*v.tw+tx])/float32(v.colorFactor)
			y := coef[1][:n]
			x := coef[0][:n]
			b := coef[2][:n]
			for k := 0; k < n; k++ {
				x[k] += xcc * y[k]
				b[k] += bcc * y[k]
			}
		}
		for c := 0; c < 3; c++ {
			if vb.off[c] < 0 {
				continue
			}
			sbx, sby := int(vb.bx)>>v.hs[c], int(vb.by)>>v.vs[c]
			llfFromDC(t, v.dc[c], sbx, sby, coef[c])
			op := v.out[c]
			transformToPixels(t, coef[c], op.Pix[sby*8*op.W+sbx*8:], op.W, scratch)
		}
	}
	if gc.pooled != nil {
		v.coefPool.Put(gc.pooled)
	}
	v.groups[g] = nil // coefficients are no longer needed
}

// finish dequantizes and transforms all groups into XYB planes (unless
// they were reconstructed as they were decoded).
func (v *vardctState) finish() error {
	fh := v.fh
	if !v.streamed {
		v.captureAC()
		if err := v.prepareRecon(); err != nil {
			return err
		}
		if err := parallelForW(len(v.groups), func(wk, g int) error {
			v.reconGroup(wk, g)
			return nil
		}); err != nil {
			return err
		}
	}
	v.groups = nil // coefficients are no longer needed
	if fn := os.Getenv("JXLDUMPACS"); fn != "" {
		f, _ := os.Create(fn)
		fmt.Fprintf(f, "%d %d\n", v.bw, v.bh)
		for i, a := range v.acs {
			fmt.Fprintf(f, "%d ", a)
			if (i+1)%v.bw == 0 {
				fmt.Fprintln(f)
			}
		}
		f.Close()
	}
	for c := 0; c < 3; c++ {
		v.fd.color[c] = cropPlane(v.out[c], divCeil(fh.XSize, 1<<v.hs[c]), divCeil(fh.YSize, 1<<v.vs[c]))
	}
	return nil
}
