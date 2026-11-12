package jxl

import "math"

// Restoration filters: Gaborish and the edge preserving filter (EPF).

// padPlane returns a copy of p with a mirrored border of b pixels.
func padPlane(p *Plane, b int) ([]float32, int) {
	pw := p.W + 2*b
	out := make([]float32, pw*(p.H+2*b))
	parallelRows(p.H+2*b, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			src := p.Row(mirror(y-b, p.H))
			dst := out[y*pw : (y+1)*pw]
			copy(dst[b:b+p.W], src)
			for x := 0; x < b; x++ {
				dst[x] = src[mirror(x-b, p.W)]
				dst[b+p.W+x] = src[mirror(p.W+x, p.W)]
			}
		}
	})
	return out, pw
}

func (fd *frameDecoder) applyGaborish() {
	f := &fd.fh.Filter
	for c := 0; c < 3; c++ {
		p := fd.color[c]
		if p == nil || (c > 0 && p == fd.color[0]) {
			continue
		}
		w1, w2 := f.GabWeights[c][0], f.GabWeights[c][1]
		div := 1 + 4*(w1+w2)
		w0 := 1 / div
		w1 /= div
		w2 /= div
		W, H := p.W, p.H
		inPlaceRows(p, 1, func(rowAt func(y int) []float32, y int, o []float32) {
			t := rowAt(mirror(y-1, H))
			m := rowAt(y)
			b := rowAt(mirror(y+1, H))
			px := func(x int) float32 {
				xl, xr := mirror(x-1, W), mirror(x+1, W)
				sum1 := (m[xl] + m[xr]) + (t[x] + b[x])
				sum2 := (t[xl] + t[xr]) + (b[xl] + b[xr])
				return sum2*w2 + (sum1*w1 + m[x]*w0)
			}
			if W < 3 {
				for x := 0; x < W; x++ {
					o[x] = px(x)
				}
				return
			}
			o[0] = px(0)
			gabRow(o, t, m, b, w0, w1, w2)
			o[W-1] = px(W - 1)
		})
	}
}

// inPlaceRows runs a vertical filter of radius r over plane p in place:
// fn computes output row y into o, reading input rows through rowAt (at
// most r rows away, mirrored at the edges). Chunks of rows run in
// parallel; each first saves the r original rows on each side of it, and
// a row's output is written back only after the r rows below it are
// computed, so every read sees original values.
func inPlaceRows(p *Plane, r int, fn func(rowAt func(y int) []float32, y int, o []float32)) {
	W, H := p.W, p.H
	nch := min(2*numWorkers(), max(1, H/(4*r+4)))
	per := (H + nch - 1) / nch
	nch = (H + per - 1) / per
	type border struct{ above, below [][]float32 }
	saved := make([]border, nch)
	_ = parallelFor(nch, func(ci int) error {
		y0, y1 := ci*per, min(H, (ci+1)*per)
		for y := y0 - r; y < y0; y++ {
			if y >= 0 {
				saved[ci].above = append(saved[ci].above, append([]float32(nil), p.Row(y)...))
			}
		}
		for y := y1; y < y1+r && y < H; y++ {
			saved[ci].below = append(saved[ci].below, append([]float32(nil), p.Row(y)...))
		}
		return nil
	})
	_ = parallelFor(nch, func(ci int) error {
		y0, y1 := ci*per, min(H, (ci+1)*per)
		above, below := saved[ci].above, saved[ci].below
		rowAt := func(y int) []float32 {
			switch {
			case y < y0:
				return above[y-(y0-len(above))]
			case y >= y1:
				return below[y-y1]
			}
			return p.Row(y)
		}
		bufs := make([][]float32, r+1)
		for i := range bufs {
			bufs[i] = make([]float32, W)
		}
		for y := y0; y < y1; y++ {
			fn(rowAt, y, bufs[y%(r+1)])
			if y-r >= y0 {
				copy(p.Row(y-r), bufs[(y-r)%(r+1)])
			}
		}
		for y := max(y0, y1-r); y < y1; y++ {
			copy(p.Row(y), bufs[y%(r+1)])
		}
		return nil
	})
}

const (
	invSigmaNum = -1.1715728752538099024
	minSigma    = -3.90524291751269967465540850526868
)

// epfSigmas computes the per-block inverse sigma image.
func (fd *frameDecoder) epfSigmas() ([]float32, int, int) {
	f := &fd.fh.Filter
	bw, bh := divCeil(fd.fh.XSize, 8), divCeil(fd.fh.YSize, 8)
	if fd.vd != nil {
		bw, bh = fd.vd.bw, fd.vd.bh
	}
	s := make([]float32, bw*bh)
	if fd.vd == nil {
		v := float32(invSigmaNum / float64(f.EPFSigmaModular))
		for i := range s {
			s[i] = v
		}
		return s, bw, bh
	}
	v := fd.vd
	quantScale := float32(v.globalScale) / 65536
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			a := v.acs[by*bw+bx]
			if a == acsInvalid || a&1 == 0 {
				continue
			}
			t := int(a >> 1)
			sq := f.EPFQuantMul / (quantScale * float32(v.qf[by*bw+bx]) * invSigmaNum)
			for iy := 0; iy < acsCoveredY[t]; iy++ {
				for ix := 0; ix < acsCoveredX[t]; ix++ {
					i := (by+iy)*bw + bx + ix
					sg := sq * f.EPFSharpLUT[v.sharp[i]]
					if sg > -1e-4 {
						sg = -1e-4
					}
					s[i] = 1 / sg
				}
			}
		}
	}
	return s, bw, bh
}

var epfOffsets0 = [6][2]int{{2, 0}, {1, 1}, {1, 0}, {1, -1}, {0, 2}, {0, 1}} // (dy, dx); negatives are mirrored pairs

// applyEPF runs the edge preserving filter. SADs are computed from
// per-direction difference images: for an offset o, D_o(p) = sum_c s_c
// |I_c(p) - I_c(p+o)|; the plus-shaped SAD towards +o at q is the plus-sum
// of D_o around q, and towards -o it is the same quantity at q-o.
func (fd *frameDecoder) applyEPF() {
	f := &fd.fh.Filter
	if fd.color[0] == nil || fd.color[1] == fd.color[0] {
		return
	}
	sig, sbw, _ := fd.epfSigmas()
	W, H := fd.color[0].W, fd.color[0].H
	for _, p := range fd.color {
		if p.W != W || p.H != H {
			return // subsampled chroma: EPF is not applied
		}
	}
	if f.EPFIters == 3 {
		fd.epfStep(0, sig, sbw)
	}
	fd.epfStep(1, sig, sbw)
	if f.EPFIters >= 2 {
		fd.epfStep(2, sig, sbw)
	}
}

func (fd *frameDecoder) epfStep(step int, sig []float32, sbw int) {
	f := &fd.fh.Filter
	W, H := fd.color[0].W, fd.color[0].H
	const B = 5 // border
	planes := fd.color
	var sm float32
	switch step {
	case 0:
		sm = f.EPFPass0Sigma * 1.65
	case 1:
		sm = 1.65
	default:
		sm = f.EPFPass2Sigma * 1.65
	}
	bsm := sm * f.EPFBorderSadMul
	sc := f.EPFChannelScale
	// offsets (dy, dx) with positive "half"; each implies its negative too
	var offs [][2]int
	switch step {
	case 0:
		offs = epfOffsets0[:]
	default:
		offs = [][2]int{{1, 0}, {0, 1}}
	}
	no := len(offs)
	const band = 16
	const tileW = 256
	nby := (H + band - 1) / band
	nw := numWorkers()
	scratch := make([][]float32, nw)
	s0, s1, s2 := sc[0], sc[1], sc[2]
	// tile writes the output of rows [y0, y1) x [tx0, tx1) to out (band
	// buffers, row y at (y-y0)*W), reading input rows through rowAt; it
	// copies its whole input (with a border of B) before writing.
	tile := func(wk, y0, y1, tx0, tx1 int, rowAt func(c, y int) []float32, out *[3][]float32) {
		tw := tx1 - tx0
		ybase := y0 - B
		prow := (y1 + B) - ybase
		pw := tw + 2*B
		ry0 := y0 - 3
		rows := y1 - ry0 + 1
		dw := tw + 6
		need := 3*prow*pw + 2*no*rows*dw
		if len(scratch[wk]) < need {
			scratch[wk] = make([]float32, need)
		}
		buf := scratch[wk]
		var pads [3][]float32
		for c := 0; c < 3; c++ {
			pads[c] = buf[c*prow*pw : (c+1)*prow*pw]
			for r := 0; r < prow; r++ {
				srow := rowAt(c, mirror(ybase+r, H))
				dst := pads[c][r*pw : (r+1)*pw]
				// the in-image span is copied, only the few columns past
				// an image edge are mirrored
				lo, hi := max(0, tx0-B), min(W, tx1+B)
				copy(dst[lo-(tx0-B):], srow[lo:hi])
				for i := 0; i < lo-(tx0-B); i++ {
					dst[i] = srow[mirror(tx0-B+i, W)]
				}
				for i := hi - (tx0 - B); i < pw; i++ {
					dst[i] = srow[mirror(tx0-B+i, W)]
				}
			}
		}
		dbuf := buf[3*prow*pw:]
		D := make([][]float32, no)
		for k := range D {
			D[k] = dbuf[:rows*dw]
			dbuf = dbuf[rows*dw:]
		}
		for k, o := range offs {
			dk := D[k]
			for r := 0; r < rows; r++ {
				y := ry0 + r
				ia := (y-ybase)*pw + B - 3
				ib := (y-ybase+o[0])*pw + B - 3 + o[1]
				drow := dk[r*dw : (r+1)*dw]
				n := len(drow)
				a0, b0 := pads[0][ia:ia+n], pads[0][ib:ib+n]
				a1, b1 := pads[1][ia:ia+n], pads[1][ib:ib+n]
				a2, b2 := pads[2][ia:ia+n], pads[2][ib:ib+n]
				epfDiffRow(drow, a0, b0, a1, b1, a2, b2, s0, s1, s2)
			}
		}
		S := D
		if step != 2 {
			P := make([][]float32, no)
			for k := range P {
				pk := dbuf[:rows*dw]
				dbuf = dbuf[rows*dw:]
				dk := D[k]
				for r := 1; r < rows-1; r++ {
					up := dk[(r-1)*dw : r*dw]
					mid := dk[r*dw : (r+1)*dw]
					dn := dk[(r+1)*dw : (r+2)*dw]
					out := pk[r*dw : (r+1)*dw]
					plusRow(out, up, mid, dn)
				}
				P[k] = pk
			}
			S = P
		}
		p0, p1, p2 := pads[0], pads[1], pads[2]
		for y := y0; y < y1; y++ {
			var rowMul [8]float32
			if y%8 == 0 || y%8 == 7 {
				for i := range rowMul {
					rowMul[i] = bsm
				}
			} else {
				rowMul = [8]float32{bsm, sm, sm, sm, sm, sm, sm, bsm}
			}
			srow := sig[(y/8)*sbw:]
			r := y - ry0
			ob := (y - y0) * W
			o0 := out[0][ob+tx0 : ob+tx1]
			o1 := out[1][ob+tx0 : ob+tx1]
			o2 := out[2][ob+tx0 : ob+tx1]
			base := (y-ybase)*pw + B
			c0, c1, c2 := p0[base:base+tw], p1[base:base+tw], p2[base:base+tw]
			if no == 2 {
				SV, SH := S[0], S[1]
				up, dn := base-pw, base+pw
				sv := SV[r*dw+3 : r*dw+3+tw]
				svu := SV[(r-1)*dw+3 : (r-1)*dw+3+tw]
				sh := SH[r*dw+3 : r*dw+3+tw]
				shl := SH[r*dw+2 : r*dw+2+tw]
				l0, l1, l2 := p0[base-1:base-1+tw], p1[base-1:base-1+tw], p2[base-1:base-1+tw]
				rr0, rr1, rr2 := p0[base+1:base+1+tw], p1[base+1:base+1+tw], p2[base+1:base+1+tw]
				u0, u1, u2 := p0[up:up+tw], p1[up:up+tw], p2[up:up+tw]
				d0, d1, d2 := p0[dn:dn+tw], p1[dn:dn+tw], p2[dn:dn+tw]
				epfRow4(&epf4Args{
					o: [3][]float32{o0, o1, o2}, c: [3][]float32{c0, c1, c2},
					l: [3][]float32{l0, l1, l2}, r: [3][]float32{rr0, rr1, rr2},
					u: [3][]float32{u0, u1, u2}, d: [3][]float32{d0, d1, d2},
					sv: sv, svu: svu, sh: sh, shl: shl,
					sig: srow[tx0>>3:], rowMul: &rowMul, n: tw,
				})
				continue
			}
			for lx := 0; lx < tw; lx++ {
				x := tx0 + lx
				is := srow[x>>3]
				X, Y, Bv := c0[lx], c1[lx], c2[lx]
				if is < minSigma {
					o0[lx], o1[lx], o2[lx] = X, Y, Bv
					continue
				}
				inv := is * rowMul[x&7]
				wsum := float32(1)
				ax, ay, ab := X, Y, Bv
				di := r*dw + lx + 3
				for k, o := range offs {
					sk := S[k]
					w := relu32(sk[di]*inv + 1)
					q := base + o[0]*pw + lx + o[1]
					wsum += w
					ax += w * p0[q]
					ay += w * p1[q]
					ab += w * p2[q]
					w = relu32(sk[di-o[0]*dw-o[1]]*inv + 1)
					q = base - o[0]*pw + lx - o[1]
					wsum += w
					ax += w * p0[q]
					ay += w * p1[q]
					ab += w * p2[q]
				}
				iw := 1 / wsum
				o0[lx], o1[lx], o2[lx] = ax*iw, ay*iw, ab*iw
			}
		}
	}
	// In place: chunks of bands run in parallel, the bands of a chunk in
	// order. A band's output waits in a buffer until the next band, which
	// reads up to B rows into it, is done, so the rows a band reads from
	// the planes are unfiltered; rows outside the chunk come from copies
	// taken before any chunk writes.
	nch := min(2*nw, nby)
	per := (nby + nch - 1) / nch
	nch = (nby + per - 1) / per
	type border struct{ above, below [3][][]float32 }
	saved := make([]border, nch)
	_ = parallelFor(nch, func(ci int) error {
		Y0, Y1 := ci*per*band, min(H, (ci+1)*per*band)
		for c := 0; c < 3; c++ {
			for y := max(0, Y0-B); y < Y0; y++ {
				saved[ci].above[c] = append(saved[ci].above[c], append([]float32(nil), planes[c].Row(y)...))
			}
			for y := Y1; y < min(H, Y1+B); y++ {
				saved[ci].below[c] = append(saved[ci].below[c], append([]float32(nil), planes[c].Row(y)...))
			}
		}
		return nil
	})
	bandBufs := make([][2][3][]float32, nw)
	_ = parallelForW(nch, func(wk, ci int) error {
		Y0, Y1 := ci*per*band, min(H, (ci+1)*per*band)
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
		bb := &bandBufs[wk]
		for i := range bb {
			for c := range bb[i] {
				if len(bb[i][c]) < band*W {
					bb[i][c] = make([]float32, band*W)
				}
			}
		}
		flush := func(i, y0, y1 int) {
			for c := 0; c < 3; c++ {
				copy(planes[c].Pix[y0*W:y1*W], bb[i][c][:(y1-y0)*W])
			}
		}
		for y0 := Y0; y0 < Y1; y0 += band {
			cur := (y0 / band) & 1
			for tx0 := 0; tx0 < W; tx0 += tileW {
				tile(wk, y0, min(Y1, y0+band), tx0, min(W, tx0+tileW), rowAt, &bb[cur])
			}
			if y0 > Y0 {
				flush(cur^1, y0-band, y0)
			}
		}
		last := (Y1 - 1) / band
		flush(last&1, last*band, Y1)
		return nil
	})
}

func abs32(v float32) float32 { return math.Float32frombits(math.Float32bits(v) &^ 0x80000000) }

// relu32 returns max(v, 0) without branches (NaN-free inputs).
func relu32(v float32) float32 {
	b := math.Float32bits(v)
	return math.Float32frombits(b &^ uint32(int32(b)>>31))
}
