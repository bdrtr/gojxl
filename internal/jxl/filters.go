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
		out := getPlane(W, H)
		parallelRows(H, func(lo, hi int) {
			for y := lo; y < hi; y++ {
				t := p.Row(mirror(y-1, H))
				m := p.Row(y)
				b := p.Row(mirror(y+1, H))
				o := out.Row(y)
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
					continue
				}
				o[0] = px(0)
				for x := 1; x < W-1; x++ {
					sum1 := (m[x-1] + m[x+1]) + (t[x] + b[x])
					sum2 := (t[x-1] + t[x+1]) + (b[x-1] + b[x+1])
					o[x] = sum2*w2 + (sum1*w1 + m[x]*w0)
				}
				o[W-1] = px(W - 1)
			}
		})
		putPlane(p)
		fd.color[c] = out
	}
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
	pw := W + 2*B
	in := fd.color
	var outPlanes [3]*Plane
	for c := 0; c < 3; c++ {
		outPlanes[c] = getPlane(W, H)
	}
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
	nb := (H + band - 1) / band
	outs := [3][]float32{outPlanes[0].Pix, outPlanes[1].Pix, outPlanes[2].Pix}
	nw := numWorkers()
	scratch := make([][]float32, nw)
	_ = parallelForW(nb, func(wk, bi int) error {
		y0 := bi * band
		y1 := min(H, y0+band)
		ybase := y0 - B
		prow := (y1 + B) - ybase
		need := 3*prow*pw + 2*len(offs)*(band+4)*(W+6)
		if len(scratch[wk]) < need {
			scratch[wk] = make([]float32, need)
		}
		buf := scratch[wk]
		var pads [3][]float32
		for c := 0; c < 3; c++ {
			pads[c] = buf[c*prow*pw : (c+1)*prow*pw]
			src := in[c]
			for r := 0; r < prow; r++ {
				srow := src.Row(mirror(ybase+r, H))
				dst := pads[c][r*pw : (r+1)*pw]
				copy(dst[B:B+W], srow)
				for x := 0; x < B; x++ {
					dst[x] = srow[mirror(x-B, W)]
					dst[B+W+x] = srow[mirror(W+x, W)]
				}
			}
		}
		dbuf := buf[3*prow*pw:]
		// D rows cover y in [y0-3, y1], x in [-3, W+2]
		ry0 := y0 - 3
		rows := y1 - ry0 + 1
		dw := W + 6
		D := make([][]float32, no)
		for k := range D {
			D[k] = dbuf[:rows*dw]
			dbuf = dbuf[rows*dw:]
		}
		s0, s1, s2 := sc[0], sc[1], sc[2]
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
				for x := range drow {
					drow[x] = abs32(a0[x]-b0[x])*s0 + abs32(a1[x]-b1[x])*s1 + abs32(a2[x]-b2[x])*s2
				}
			}
		}
		// plus sums (for steps 0 and 1)
		var P [][]float32
		if step != 2 {
			P = make([][]float32, no)
			for k := range P {
				pk := dbuf[:rows*dw]
				dbuf = dbuf[rows*dw:]
				dk := D[k]
				for r := 1; r < rows-1; r++ {
					up := dk[(r-1)*dw : r*dw]
					mid := dk[r*dw : (r+1)*dw]
					dn := dk[(r+1)*dw : (r+2)*dw]
					out := pk[r*dw : (r+1)*dw]
					for x := 1; x < dw-1; x++ {
						out[x] = mid[x] + mid[x-1] + mid[x+1] + up[x] + dn[x]
					}
				}
				P[k] = pk
			}
		}
		S := D
		if step != 2 {
			S = P
		}
		if no == 2 {
			// fast path: 4 neighbours (down/up via S[0], right/left via S[1])
			SV, SH := S[0], S[1]
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
				o0 := outs[0][y*W : (y+1)*W]
				o1 := outs[1][y*W : (y+1)*W]
				o2 := outs[2][y*W : (y+1)*W]
				base := (y-ybase)*pw + B
				up, dn := base-pw, base+pw
				sv := SV[r*dw+3 : r*dw+3+W]
				svu := SV[(r-1)*dw+3 : (r-1)*dw+3+W]
				sh := SH[r*dw+3 : r*dw+3+W]
				shl := SH[r*dw+2 : r*dw+2+W]
				c0, c1, c2 := p0[base:base+W], p1[base:base+W], p2[base:base+W]
				l0, l1, l2 := p0[base-1:base-1+W], p1[base-1:base-1+W], p2[base-1:base-1+W]
				rr0, rr1, rr2 := p0[base+1:base+1+W], p1[base+1:base+1+W], p2[base+1:base+1+W]
				u0, u1, u2 := p0[up:up+W], p1[up:up+W], p2[up:up+W]
				d0, d1, d2 := p0[dn:dn+W], p1[dn:dn+W], p2[dn:dn+W]
				for x := 0; x < W; x++ {
					is := srow[x>>3]
					if is < minSigma {
						o0[x], o1[x], o2[x] = c0[x], c1[x], c2[x]
						continue
					}
					inv := is * rowMul[x&7]
					wd := relu32(sv[x]*inv + 1)
					wu := relu32(svu[x]*inv + 1)
					wr := relu32(sh[x]*inv + 1)
					wl := relu32(shl[x]*inv + 1)
					iw := 1 / (1 + wd + wu + wr + wl)
					o0[x] = (c0[x] + wd*d0[x] + wu*u0[x] + wr*rr0[x] + wl*l0[x]) * iw
					o1[x] = (c1[x] + wd*d1[x] + wu*u1[x] + wr*rr1[x] + wl*l1[x]) * iw
					o2[x] = (c2[x] + wd*d2[x] + wu*u2[x] + wr*rr2[x] + wl*l2[x]) * iw
				}
			}
			return nil
		}

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
			o0 := outs[0][y*W : (y+1)*W]
			o1 := outs[1][y*W : (y+1)*W]
			o2 := outs[2][y*W : (y+1)*W]
			c0 := pads[0][(y-ybase)*pw+B:]
			c1 := pads[1][(y-ybase)*pw+B:]
			c2 := pads[2][(y-ybase)*pw+B:]
			for x := 0; x < W; x++ {
				is := srow[x>>3]
				X, Y, Bv := c0[x], c1[x], c2[x]
				if is < minSigma {
					o0[x], o1[x], o2[x] = X, Y, Bv
					continue
				}
				inv := is * rowMul[x&7]
				wsum := float32(1)
				ax, ay, ab := X, Y, Bv
				di := r*dw + x + 3
				for k, o := range offs {
					sk := S[k]
					// towards +o
					w := relu32(sk[di]*inv + 1)
					q := (y-ybase+o[0])*pw + B + x + o[1]
					wsum += w
					ax += w * pads[0][q]
					ay += w * pads[1][q]
					ab += w * pads[2][q]
					// towards -o: value at q-o
					w = relu32(sk[di-o[0]*dw-o[1]]*inv + 1)
					q = (y-ybase-o[0])*pw + B + x - o[1]
					wsum += w
					ax += w * pads[0][q]
					ay += w * pads[1][q]
					ab += w * pads[2][q]
				}
				iw := 1 / wsum
				o0[x], o1[x], o2[x] = ax*iw, ay*iw, ab*iw
			}
		}
		return nil
	})
	for c := 0; c < 3; c++ {
		putPlane(fd.color[c])
		fd.color[c] = outPlanes[c]
	}
}

func abs32(v float32) float32 { return math.Float32frombits(math.Float32bits(v) &^ 0x80000000) }

// relu32 returns max(v, 0) without branches (NaN-free inputs).
func relu32(v float32) float32 {
	b := math.Float32bits(v)
	return math.Float32frombits(b &^ uint32(int32(b)>>31))
}
