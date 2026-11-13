package jxl

// Gaborish followed by a single EPF iteration (step 1), the usual filter
// setting of lossy files, in one pass over rows. The separate passes copy
// every plane several times (Gaborish in place, EPF tile pads, EPF band
// buffers); here each worker keeps the filtered rows it needs in small
// rings that stay in cache.

// gabEPF1Ok reports whether gabEPF1 handles the frame's filters.
func (fd *frameDecoder) gabEPF1Ok() bool {
	f := &fd.fh.Filter
	if f.EPFIters != 1 || fd.color[0] == nil || fd.color[1] == fd.color[0] {
		return false
	}
	W, H := fd.color[0].W, fd.color[0].H
	for _, p := range fd.color {
		if p == nil || p.W != W || p.H != H {
			return false
		}
	}
	return W >= 8 && H >= 8
}

// gabEPF1 applies Gaborish (when gab) and then EPF step 1. The result
// equals applyGaborish followed by applyEPF.
func (fd *frameDecoder) gabEPF1(gab bool) {
	f := &fd.fh.Filter
	sig, sbw, _ := fd.epfSigmas()
	planes := fd.color
	W, H := planes[0].W, planes[0].H
	const (
		B  = 3 // columns of mirrored padding in the row buffers
		R  = 3 // source rows read beyond a chunk (2 for EPF, 1 for Gaborish)
		nI = 8 // ring sizes (powers of two)
		nD = 4
		nP = 2
		nO = 4 // output rows are written back nO-1 rows late
	)
	pw := W + 2*B
	var gw [3][3]float32
	for c := range gw {
		w1, w2 := f.GabWeights[c][0], f.GabWeights[c][1]
		div := 1 + 4*(w1+w2)
		gw[c] = [3]float32{1 / div, w1 / div, w2 / div}
	}
	const sm = float32(1.65)
	bsm := sm * f.EPFBorderSadMul
	sc := f.EPFChannelScale
	s0, s1, s2 := sc[0], sc[1], sc[2]

	nw := numWorkers()
	nch := min(2*nw, max(1, H/16))
	per := (H + nch - 1) / nch
	nch = (H + per - 1) / per
	// rows next to a chunk are read after the neighbouring chunk may have
	// written them: copies are taken first
	type border struct{ above, below [3][][]float32 }
	saved := make([]border, nch)
	_ = parallelFor(nch, func(ci int) error {
		Y0, Y1 := ci*per, min(H, (ci+1)*per)
		for c := 0; c < 3; c++ {
			for y := max(0, Y0-R); y < Y0; y++ {
				saved[ci].above[c] = append(saved[ci].above[c], append([]float32(nil), planes[c].Row(y)...))
			}
			for y := Y1; y < min(H, Y1+R); y++ {
				saved[ci].below[c] = append(saved[ci].below[c], append([]float32(nil), planes[c].Row(y)...))
			}
		}
		return nil
	})
	need := (3*nI+nD*2+nP+1)*pw + 3*nO*W
	scratch := make([][]float32, nw)
	_ = parallelForW(nch, func(wk, ci int) error {
		Y0, Y1 := ci*per, min(H, (ci+1)*per)
		above, below := &saved[ci].above, &saved[ci].below
		rowAt := func(c, y int) []float32 {
			switch {
			case y < Y0:
				return above[c][y-(Y0-len(above[c]))]
			case y >= Y1:
				return below[c][y-Y1]
			}
			return planes[c].Row(y)
		}
		if len(scratch[wk]) < need {
			scratch[wk] = make([]float32, need)
		}
		buf := scratch[wk]
		take := func(n int) []float32 {
			s := buf[:n:n]
			buf = buf[n:]
			return s
		}
		var ir [nI][3][]float32
		for i := range ir {
			for c := range ir[i] {
				ir[i][c] = take(pw)
			}
		}
		var dv, dh [nD][]float32
		for i := range dv {
			dv[i], dh[i] = take(pw), take(pw)
		}
		var pv [nP][]float32
		for i := range pv {
			pv[i] = take(pw)
		}
		ph := take(pw)
		var or [nO][3][]float32
		for i := range or {
			for c := range or[i] {
				or[i][c] = take(W)
			}
		}
		I := func(v int) *[3][]float32 { return &ir[v&(nI-1)] }
		// fill computes the (Gaborish-filtered) padded rows of virtual
		// row v, which may lie outside the image (mirrored)
		fill := func(v int) {
			y := mirror(v, H)
			rows := I(v)
			for c := 0; c < 3; c++ {
				dst := rows[c]
				m := dst[B : B+W]
				src := rowAt(c, y)
				if gab {
					t, b := rowAt(c, mirror(y-1, H)), rowAt(c, mirror(y+1, H))
					w0, w1, w2 := gw[c][0], gw[c][1], gw[c][2]
					px := func(x int) float32 {
						xl, xr := mirror(x-1, W), mirror(x+1, W)
						sum1 := (src[xl] + src[xr]) + (t[x] + b[x])
						sum2 := (t[xl] + t[xr]) + (b[xl] + b[xr])
						return sum2*w2 + (sum1*w1 + src[x]*w0)
					}
					m[0] = px(0)
					gabRow(m, t, src, b, w0, w1, w2)
					m[W-1] = px(W - 1)
				} else {
					copy(m, src)
				}
				for i := 0; i < B; i++ {
					dst[i] = m[mirror(i-B, W)]
					dst[B+W+i] = m[mirror(W+i, W)]
				}
			}
		}
		// D rows: vertical (v against v+1) and horizontal (x against x+1)
		diffV := func(v int) {
			a, b := I(v), I(v+1)
			epfDiffRow(dv[v&(nD-1)], a[0], b[0], a[1], b[1], a[2], b[2], s0, s1, s2)
		}
		diffH := func(v int) {
			a := I(v)
			n := pw - 1
			epfDiffRow(dh[v&(nD-1)][:n], a[0][:n], a[0][1:], a[1][:n], a[1][1:], a[2][:n], a[2][1:], s0, s1, s2)
		}
		plusV := func(y int) {
			plusRow(pv[y&(nP-1)], dv[(y-1)&(nD-1)], dv[y&(nD-1)], dv[(y+1)&(nD-1)])
		}
		for v := Y0 - 2; v <= Y0+1; v++ {
			fill(v)
		}
		for v := Y0 - 2; v <= Y0; v++ {
			diffV(v)
		}
		diffH(Y0 - 1)
		diffH(Y0)
		plusV(Y0 - 1)
		var rowMul [8]float32
		writeBack := func(y int) {
			o := &or[y&(nO-1)]
			for c := 0; c < 3; c++ {
				copy(planes[c].Row(y), o[c])
			}
		}
		for y := Y0; y < Y1; y++ {
			fill(y + 2)
			diffV(y + 1)
			diffH(y + 1)
			plusV(y)
			plusRow(ph, dh[(y-1)&(nD-1)], dh[y&(nD-1)], dh[(y+1)&(nD-1)])
			if y%8 == 0 || y%8 == 7 {
				rowMul = [8]float32{bsm, bsm, bsm, bsm, bsm, bsm, bsm, bsm}
			} else {
				rowMul = [8]float32{bsm, sm, sm, sm, sm, sm, sm, bsm}
			}
			cur, up, dn := I(y), I(y-1), I(y+1)
			o := &or[y&(nO-1)]
			sv, svu := pv[y&(nP-1)], pv[(y-1)&(nP-1)]
			epfRow4(&epf4Args{
				o:  *o,
				c:  [3][]float32{cur[0][B : B+W], cur[1][B : B+W], cur[2][B : B+W]},
				l:  [3][]float32{cur[0][B-1 : B-1+W], cur[1][B-1 : B-1+W], cur[2][B-1 : B-1+W]},
				r:  [3][]float32{cur[0][B+1 : B+1+W], cur[1][B+1 : B+1+W], cur[2][B+1 : B+1+W]},
				u:  [3][]float32{up[0][B : B+W], up[1][B : B+W], up[2][B : B+W]},
				d:  [3][]float32{dn[0][B : B+W], dn[1][B : B+W], dn[2][B : B+W]},
				sv: sv[B : B+W], svu: svu[B : B+W], sh: ph[B : B+W], shl: ph[B-1 : B-1+W],
				sig: sig[(y/8)*sbw:], rowMul: &rowMul, n: W,
			})
			// the rows below may still read source rows down to y-3
			// (mirrored at the bottom edge)
			if y-(nO-1) >= Y0 {
				writeBack(y - (nO - 1))
			}
		}
		for y := max(Y0, Y1-(nO-1)); y < Y1; y++ {
			writeBack(y)
		}
		return nil
	})
}

// noFusedFilters selects the separate Gaborish and EPF passes (tests).
var noFusedFilters = false
