//go:build goexperiment.simd && amd64

package jxl

import "simd/archsimd"

type v8 = archsimd.Float32x8

var useSIMD = archsimd.X86.AVX2() && archsimd.X86.FMA()

// SIMDActive reports whether vectorised kernels are in use.
func SIMDActive() bool { return useSIMD }

func ld(s []float32) v8 { return archsimd.LoadFloat32x8(s[:8]) }

// vzu clears the upper halves of the vector registers before returning to
// scalar (SSE) code, which otherwise pays for false dependencies on them.
func vzu()                 { archsimd.ClearAVXUpperBits() }
func bc(f float32) v8      { return archsimd.BroadcastFloat32x8(f) }
func st(v v8, s []float32) { v.Store(s[:8]) }

// idct8v performs the 8-point scaled IDCT across eight vectors.
func idct8v(x *[8]v8) {
	s2 := bc(sqrt2)
	sa, sb := bc(sec4a), bc(sec4b)
	e0 := x[0].Add(x[4])
	e1 := x[0].Sub(x[4])
	eo1 := x[6].Add(x[2])
	eo0 := x[2].Mul(s2)
	ep0 := eo0.Add(eo1).Mul(sa)
	ep1 := eo0.Sub(eo1).Mul(sb)
	E0, E3 := e0.Add(ep0), e0.Sub(ep0)
	E1, E2 := e1.Add(ep1), e1.Sub(ep1)
	o0 := x[1].Mul(s2)
	o1 := x[3].Add(x[1])
	o2 := x[5].Add(x[3])
	o3 := x[7].Add(x[5])
	f0, f1 := o0.Add(o2), o0.Sub(o2)
	fo1 := o3.Add(o1)
	fo0 := o1.Mul(s2)
	fp0 := fo0.Add(fo1).Mul(sa)
	fp1 := fo0.Sub(fo1).Mul(sb)
	O0 := f0.Add(fp0).Mul(bc(sec8[0]))
	O3 := f0.Sub(fp0).Mul(bc(sec8[3]))
	O1 := f1.Add(fp1).Mul(bc(sec8[1]))
	O2 := f1.Sub(fp1).Mul(bc(sec8[2]))
	x[0], x[7] = E0.Add(O0), E0.Sub(O0)
	x[1], x[6] = E1.Add(O1), E1.Sub(O1)
	x[2], x[5] = E2.Add(O2), E2.Sub(O2)
	x[3], x[4] = E3.Add(O3), E3.Sub(O3)
}

// transpose8 transposes an 8x8 matrix held in eight row vectors.
func transpose8(r *[8]v8) {
	half := func(a, b, c, d v8) (v8, v8, v8, v8) {
		s0 := a.ConcatPermuteScalarsGrouped(0, 1, 4, 5, b)
		s1 := a.ConcatPermuteScalarsGrouped(2, 3, 6, 7, b)
		s2 := c.ConcatPermuteScalarsGrouped(0, 1, 4, 5, d)
		s3 := c.ConcatPermuteScalarsGrouped(2, 3, 6, 7, d)
		return s0.ConcatPermuteScalarsGrouped(0, 2, 4, 6, s2),
			s0.ConcatPermuteScalarsGrouped(1, 3, 5, 7, s2),
			s1.ConcatPermuteScalarsGrouped(0, 2, 4, 6, s3),
			s1.ConcatPermuteScalarsGrouped(1, 3, 5, 7, s3)
	}
	t0, t1, t2, t3 := half(r[0], r[1], r[2], r[3])
	u0, u1, u2, u3 := half(r[4], r[5], r[6], r[7])
	r[0] = t0.ConcatPermute128Scalars(0, 2, u0)
	r[4] = t0.ConcatPermute128Scalars(1, 3, u0)
	r[1] = t1.ConcatPermute128Scalars(0, 2, u1)
	r[5] = t1.ConcatPermute128Scalars(1, 3, u1)
	r[2] = t2.ConcatPermute128Scalars(0, 2, u2)
	r[6] = t2.ConcatPermute128Scalars(1, 3, u2)
	r[3] = t3.ConcatPermute128Scalars(0, 2, u3)
	r[7] = t3.ConcatPermute128Scalars(1, 3, u3)
}

func idct8x8(c, out []float32, stride int) {
	if !useSIMD {
		idct8x8Scalar(c, out, stride)
		return
	}
	var v [8]v8
	for a := 0; a < 8; a++ {
		v[a] = ld(c[a*8:])
	}
	idct8v(&v) // horizontal: lanes = vertical frequency
	transpose8(&v)
	idct8v(&v) // vertical: lanes = x
	for y := 0; y < 8; y++ {
		st(v[y], out[y*stride:])
	}
	vzu()
}

func dequantRow(q []int32, m, o []float32, mul float32, d *dequantBias) {
	if !useSIMD {
		dequantRowScalar(q, m, o, mul, d)
		return
	}
	bias, numer := d.bias, d.numer
	n := len(q)
	k := 0
	nv, bv, mv, one := bc(numer), bc(bias), bc(mul), bc(1)
	for ; k+8 <= n; k += 8 {
		f := archsimd.LoadInt32x8(q[k : k+8]).ConvertToFloat32()
		gen := f.Sub(nv.Div(f))
		small := f.Mul(bv)
		val := small.IfElse(f.Abs().LessEqual(one), gen)
		st(val.Mul(ld(m[k:])).Mul(mv), o[k:])
	}
	vzu()
	if k < n {
		dequantRowScalar(q[k:], m[k:], o[k:], mul, d)
	}
}

func epfDiffRow(d, a0, b0, a1, b1, a2, b2 []float32, s0, s1, s2 float32) {
	if !useSIMD {
		epfDiffRowScalar(d, a0, b0, a1, b1, a2, b2, s0, s1, s2)
		return
	}
	n := len(d)
	x := 0
	v0, v1, v2 := bc(s0), bc(s1), bc(s2)
	for ; x+8 <= n; x += 8 {
		r := ld(a0[x:]).Sub(ld(b0[x:])).Abs().Mul(v0)
		r = ld(a1[x:]).Sub(ld(b1[x:])).Abs().MulAdd(v1, r)
		r = ld(a2[x:]).Sub(ld(b2[x:])).Abs().MulAdd(v2, r)
		st(r, d[x:])
	}
	vzu()
	if x < n {
		epfDiffRowScalar(d[x:], a0[x:], b0[x:], a1[x:], b1[x:], a2[x:], b2[x:], s0, s1, s2)
	}
}

func plusRow(out, up, mid, dn []float32) {
	if !useSIMD {
		plusRowScalar(out, up, mid, dn)
		return
	}
	n := len(out)
	x := 1
	for ; x+8 <= n-1; x += 8 {
		r := ld(mid[x:]).Add(ld(mid[x-1:])).Add(ld(mid[x+1:])).Add(ld(up[x:])).Add(ld(dn[x:]))
		st(r, out[x:])
	}
	vzu()
	for ; x < n-1; x++ {
		out[x] = mid[x] + mid[x-1] + mid[x+1] + up[x] + dn[x]
	}
}

func gabRow(o, t, m, b []float32, w0, w1, w2 float32) {
	if !useSIMD {
		gabRowScalar(o, t, m, b, w0, w1, w2)
		return
	}
	n := len(o)
	x := 1
	v0, v1, v2 := bc(w0), bc(w1), bc(w2)
	for ; x+8 <= n-1; x += 8 {
		sum1 := ld(m[x-1:]).Add(ld(m[x+1:])).Add(ld(t[x:]).Add(ld(b[x:])))
		sum2 := ld(t[x-1:]).Add(ld(t[x+1:])).Add(ld(b[x-1:]).Add(ld(b[x+1:])))
		st(sum2.MulAdd(v2, sum1.MulAdd(v1, ld(m[x:]).Mul(v0))), o[x:])
	}
	vzu()
	for ; x < n-1; x++ {
		sum1 := (m[x-1] + m[x+1]) + (t[x] + b[x])
		sum2 := (t[x-1] + t[x+1]) + (b[x-1] + b[x+1])
		o[x] = sum2*w2 + (sum1*w1 + m[x]*w0)
	}
}

func epfRow4(a *epf4Args) {
	if !useSIMD {
		epfRow4Scalar(a, 0)
		return
	}
	nb := a.n / 8
	rm := archsimd.LoadFloat32x8Array(a.rowMul)
	one, zero := bc(1), bc(0)
	for bi := 0; bi < nb; bi++ {
		lx := bi * 8
		is := a.sig[bi]
		if is < minSigma {
			for c := 0; c < 3; c++ {
				copy(a.o[c][lx:lx+8], a.c[c][lx:lx+8])
			}
			continue
		}
		inv := bc(is).Mul(rm)
		wd := ld(a.sv[lx:]).MulAdd(inv, one).Max(zero)
		wu := ld(a.svu[lx:]).MulAdd(inv, one).Max(zero)
		wr := ld(a.sh[lx:]).MulAdd(inv, one).Max(zero)
		wl := ld(a.shl[lx:]).MulAdd(inv, one).Max(zero)
		iw := one.Div(one.Add(wd).Add(wu).Add(wr).Add(wl))
		for c := 0; c < 3; c++ {
			acc := ld(a.c[c][lx:])
			acc = wd.MulAdd(ld(a.d[c][lx:]), acc)
			acc = wu.MulAdd(ld(a.u[c][lx:]), acc)
			acc = wr.MulAdd(ld(a.r[c][lx:]), acc)
			acc = wl.MulAdd(ld(a.l[c][lx:]), acc)
			st(acc.Mul(iw), a.o[c][lx:])
		}
	}
	vzu()
	if nb*8 < a.n {
		epfRow4Scalar(a, nb*8)
	}
}

func xybRow(c *xybConv, x, y, b, r, g, bo []float32) {
	if !useSIMD {
		xybRowScalar(c, x, y, b, r, g, bo)
		return
	}
	n := len(r)
	i := 0
	var m [9]v8
	for k := range m {
		m[k] = bc(c.m[k])
	}
	cb0, cb1, cb2 := bc(c.cbrtBias[0]), bc(c.cbrtBias[1]), bc(c.cbrtBias[2])
	b0, b1, b2 := bc(c.bias[0]), bc(c.bias[1]), bc(c.bias[2])
	for ; i+8 <= n; i += 8 {
		X, Y, B := ld(x[i:]), ld(y[i:]), ld(b[i:])
		gr := Y.Add(X).Sub(cb0)
		gg := Y.Sub(X).Sub(cb1)
		gb := B.Sub(cb2)
		mr := gr.Mul(gr).MulAdd(gr, b0)
		mg := gg.Mul(gg).MulAdd(gg, b1)
		mb := gb.Mul(gb).MulAdd(gb, b2)
		st(m[0].Mul(mr).Add(m[1].Mul(mg)).Add(m[2].Mul(mb)), r[i:])
		st(m[3].Mul(mr).Add(m[4].Mul(mg)).Add(m[5].Mul(mb)), g[i:])
		st(m[6].Mul(mr).Add(m[7].Mul(mg)).Add(m[8].Mul(mb)), bo[i:])
	}
	vzu()
	if i < n {
		xybRowScalar(c, x[i:], y[i:], b[i:], r[i:], g[i:], bo[i:])
	}
}

// idctNv performs an N-point scaled IDCT across N vectors (N power of two).
func idctNv(x, tmp []v8) {
	n := len(x)
	switch n {
	case 1:
		return
	case 2:
		a, b := x[0], x[1]
		x[0], x[1] = a.Add(b), a.Sub(b)
		return
	case 8:
		idct8v((*[8]v8)(x))
		return
	}
	h := n / 2
	for i := 0; i < h; i++ {
		tmp[i] = x[2*i]
		tmp[h+i] = x[2*i+1]
	}
	odd := tmp[h:n]
	for i := h - 1; i > 0; i-- {
		odd[i] = odd[i].Add(odd[i-1])
	}
	odd[0] = odd[0].Mul(bc(sqrt2))
	idctNv(tmp[:h], x[:h])
	idctNv(odd, x[h:n])
	sh := secHalf(n)
	for i := 0; i < h; i++ {
		a := tmp[i]
		b := tmp[h+i].Mul(bc(sh[i]))
		x[i] = a.Add(b)
		x[n-1-i] = a.Sub(b)
	}
}

// idctRowsV applies a rowLen-point IDCT to each of nrows contiguous rows,
// eight rows at a time via 8x8 transposes (nrows, rowLen multiples of 8).
func idctRowsV(in []float32, nrows, rowLen int, vbuf, vtmp []v8) {
	v := vbuf[:rowLen]
	for r0 := 0; r0 < nrows; r0 += 8 {
		for x0 := 0; x0 < rowLen; x0 += 8 {
			blk := (*[8]v8)(v[x0 : x0+8])
			for k := 0; k < 8; k++ {
				blk[k] = ld(in[(r0+k)*rowLen+x0:])
			}
			transpose8(blk)
		}
		idctNv(v, vtmp[:rowLen])
		for x0 := 0; x0 < rowLen; x0 += 8 {
			blk := (*[8]v8)(v[x0 : x0+8])
			transpose8(blk)
			for k := 0; k < 8; k++ {
				st(blk[k], in[(r0+k)*rowLen+x0:])
			}
		}
	}
}

func scaledIDCT2D(rows, cols int, in, out []float32, stride int, scratch []float32) {
	if !useSIMD || rows%8 != 0 || cols%8 != 0 {
		scaledIDCT2DScalar(rows, cols, in, out, stride, scratch)
		return
	}
	if max(rows, cols) <= 32 {
		var vbuf, vtmp [32]v8 // zeroed per call: keep small for small blocks
		scaledIDCT2DV(rows, cols, in, out, stride, vbuf[:], vtmp[:])
	} else {
		var vbuf, vtmp [256]v8
		scaledIDCT2DV(rows, cols, in, out, stride, vbuf[:], vtmp[:])
	}
	vzu()
}

func scaledIDCT2DV(rows, cols int, in, out []float32, stride int, vbuf, vtmp []v8) {
	if rows < cols {
		// natural layout: horizontal pass on rows, vertical pass vectorised
		idctRowsV(in, rows, cols, vbuf[:], vtmp[:])
		v := vbuf[:rows]
		for x0 := 0; x0 < cols; x0 += 8 {
			for r := range v {
				v[r] = ld(in[r*cols+x0:])
			}
			idctNv(v, vtmp[:rows])
			for y := range v {
				st(v[y], out[y*stride+x0:])
			}
		}
		return
	}
	// transposed layout in[a*rows+y]: vertical pass on contiguous runs,
	// horizontal pass vectorised over 8 rows, then 8x8 transposes.
	idctRowsV(in, cols, rows, vbuf[:], vtmp[:])
	v := vbuf[:cols]
	for y0 := 0; y0 < rows; y0 += 8 {
		for a := range v {
			v[a] = ld(in[a*rows+y0:])
		}
		idctNv(v, vtmp[:cols])
		for x0 := 0; x0 < cols; x0 += 8 {
			blk := (*[8]v8)(v[x0 : x0+8])
			transpose8(blk)
			for k := 0; k < 8; k++ {
				st(blk[k], out[(y0+k)*stride+x0:])
			}
		}
	}
}
