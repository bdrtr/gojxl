package jxl

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
		pad, pw := padPlane(p, 1)
		out := p.Pix
		W := p.W
		parallelRows(p.H, func(lo, hi int) {
			for y := lo; y < hi; y++ {
				t := pad[y*pw:]
				m := pad[(y+1)*pw:]
				b := pad[(y+2)*pw:]
				o := out[y*W : (y+1)*W]
				for x := 0; x < W; x++ {
					sum1 := (m[x] + m[x+2]) + (t[x+1] + b[x+1])
					sum2 := (t[x] + t[x+2]) + (b[x] + b[x+2])
					o[x] = sum2*w2 + (sum1*w1 + m[x+1]*w0)
				}
			}
		})
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

var epf0Offsets = [12][2]int{{-2, 0}, {-1, -1}, {-1, 0}, {-1, 1}, {0, -2}, {0, -1}, {0, 1}, {0, 2}, {1, -1}, {1, 0}, {1, 1}, {2, 0}}
var epf1Offsets = [4][2]int{{-1, 0}, {0, -1}, {0, 1}, {1, 0}}
var plusOffsets = [5][2]int{{0, 0}, {-1, 0}, {0, -1}, {1, 0}, {0, 1}}

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
	run := func(step int) {
		const B = 3
		var pads [3][]float32
		var pw int
		for c := 0; c < 3; c++ {
			pads[c], pw = padPlane(fd.color[c], B)
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
		scale := f.EPFChannelScale
		_ = parallelFor(H, func(y int) error {
			var rowMul [8]float32
			if y%8 == 0 || y%8 == 7 {
				for i := range rowMul {
					rowMul[i] = bsm
				}
			} else {
				rowMul = [8]float32{bsm, sm, sm, sm, sm, sm, sm, bsm}
			}
			srow := sig[(y/8)*sbw:]
			at := func(c, dy, x int) float32 { return pads[c][(y+B+dy)*pw+x+B] }
			for x := 0; x < W; x++ {
				is := srow[x/8]
				if is < minSigma {
					for c := 0; c < 3; c++ {
						fd.color[c].Pix[y*W+x] = at(c, 0, x)
					}
					continue
				}
				invSigma := is * rowMul[x%8]
				wsum := float32(1)
				X, Y, Bv := at(0, 0, x), at(1, 0, x), at(2, 0, x)
				cc := [3]float32{X, Y, Bv}
				acc := cc
				add := func(dy, dx int, sad float32) {
					w := sad*invSigma + 1
					if w < 0 {
						w = 0
					}
					wsum += w
					for c := 0; c < 3; c++ {
						acc[c] += w * at(c, dy, x+dx)
					}
				}
				switch step {
				case 0, 1:
					offs := epf1Offsets[:]
					if step == 0 {
						offs = epf0Offsets[:]
					}
					for _, o := range offs {
						var sad float32
						for c := 0; c < 3; c++ {
							var s float32
							for _, po := range plusOffsets {
								a := at(c, po[0], x+po[1])
								b := at(c, o[0]+po[0], x+o[1]+po[1])
								d := a - b
								if d < 0 {
									d = -d
								}
								s += d
							}
							sad += s * scale[c]
						}
						add(o[0], o[1], sad)
					}
				default:
					for _, o := range epf1Offsets {
						var sad float32
						for c := 0; c < 3; c++ {
							d := at(c, o[0], x+o[1]) - cc[c]
							if d < 0 {
								d = -d
							}
							sad += d * scale[c]
						}
						add(o[0], o[1], sad)
					}
				}
				inv := 1 / wsum
				for c := 0; c < 3; c++ {
					fd.color[c].Pix[y*W+x] = acc[c] * inv
				}
			}
			return nil
		})
	}
	if f.EPFIters == 3 {
		run(0)
	}
	run(1)
	if f.EPFIters >= 2 {
		run(2)
	}
}
