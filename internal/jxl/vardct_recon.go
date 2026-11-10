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

// dct1dScaled computes the forward scaled DCT (c[0] = mean) directly.
func dct1dScaled(x []float32, out []float32) {
	n := len(x)
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
// in is clobbered.
func scaledIDCT2D(rows, cols int, in []float32, out []float32, stride int, scratch []float32) {
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

// idct8x8 is the DCT8 fast path (transposed storage).
func idct8x8(c []float32, out []float32, stride int) {
	_ = c[63]
	for a := 0; a < 8; a++ {
		idct8(c[a*8 : a*8+8])
	}
	var cl [8]float32
	for y := 0; y < 8; y++ {
		cl[0], cl[1], cl[2], cl[3] = c[y], c[8+y], c[16+y], c[24+y]
		cl[4], cl[5], cl[6], cl[7] = c[32+y], c[40+y], c[48+y], c[56+y]
		idct8(cl[:])
		o := out[y*stride : y*stride+8 : y*stride+8]
		o[0], o[1], o[2], o[3] = cl[0], cl[1], cl[2], cl[3]
		o[4], o[5], o[6], o[7] = cl[4], cl[5], cl[6], cl[7]
	}
}

// resampleScale is DCTTotalResampleScale for an N-point LF DCT upsampled to 8N.
func resampleScale(n, k int) float32 {
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

// finish dequantizes and transforms all groups into XYB planes.
func (v *vardctState) finish() error {
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
	xdm := powf(0.8, float32(fh.XQMScale)-2)
	bdm := powf(0.8, float32(fh.BQMScale)-2)
	invGS := float32(65536.0 / float64(v.globalScale))
	biases := ih.Opsin.QuantBias
	numer := ih.Opsin.QuantBiasNumerator
	type wscratch struct {
		coef    [3][]float32
		scratch []float32
	}
	ws := make([]*wscratch, numWorkers())
	maxN := 64
	for t := 0; t < numACS; t++ {
		if v.usedACS&(1<<t) != 0 {
			maxN = max(maxN, 64*acsCoveredX[t]*acsCoveredY[t])
		}
	}
	err := parallelForW(len(v.groups), func(wk, g int) error {
		gc := v.groups[g]
		if ws[wk] == nil {
			w := &wscratch{scratch: make([]float32, maxN+3*256)}
			for c := 0; c < 3; c++ {
				w.coef[c] = make([]float32, maxN)
			}
			ws[wk] = w
		}
		coef := ws[wk].coef
		scratch := ws[wk].scratch
		for vi := range gc.vbs {
			vb := &gc.vbs[vi]
			t := int(vb.typ)
			n := 64 * acsCoveredX[t] * acsCoveredY[t]
			tables := v.qtables[acsQuantTable[t]]
			scale := invGS / float32(vb.qf)
			muls := [3]float32{scale * xdm, scale, scale * bdm}
			// dequantize
			for c := 0; c < 3; c++ {
				if vb.off[c] < 0 {
					continue
				}
				q := gc.coef[c][vb.off[c] : int(vb.off[c])+n]
				m := tables[c][:n]
				o := coef[c][:n]
				mul := muls[c]
				bias := biases[c]
				for k, qv := range q {
					switch {
					case qv == 0:
						o[k] = 0
					case qv == 1:
						o[k] = bias * m[k] * mul
					case qv == -1:
						o[k] = -bias * m[k] * mul
					default:
						f := float32(qv)
						o[k] = (f - numer/f) * m[k] * mul
					}
				}
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
		return nil
	})
	if err != nil {
		return err
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
