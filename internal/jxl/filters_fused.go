package jxl

// Gaborish followed by the EPF iterations in one pass over rows. The
// separate passes copy every plane several times (Gaborish in place, EPF
// tile pads and band buffers) and recompute the difference rows that
// overlapping tiles share; here each worker runs a chain of stages
// (source rows, Gaborish applied; then each EPF step) whose rows wait in
// small rings that stay in cache, each row computed once.

// gabEPFOk reports whether gabEPF handles the frame's filters.
func (fd *frameDecoder) gabEPFOk() bool {
	f := &fd.fh.Filter
	if f.EPFIters < 1 || f.EPFIters > 3 || fd.color[0] == nil || fd.color[1] == fd.color[0] {
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

// noFusedFilters selects the separate Gaborish and EPF passes (tests).
var noFusedFilters = false

// epfRadius is how many input rows above and below an output row an EPF
// step reads (offsets of up to 2 rows, plus-shaped sums of 1 more).
func epfRadius(step int) int {
	switch step {
	case 0:
		return 3
	case 1:
		return 2
	}
	return 1
}

// epfStage is one EPF step of a worker's chain. Its difference rows D
// and summed rows S (plus-shaped sums of D, or D itself in step 2) are
// kept per offset in rings indexed by virtual row (rows past the image
// edges compare mirrored input rows, so they differ from the mirrored
// rows); its output rows by real row.
type epfStage struct {
	offs         [][2]int
	plus         bool
	maxdy        int
	sm, bsm      float32
	d, s         [][][]float32 // [offset][ring slot]
	dNext, sNext int           // next virtual row of d and s
	out          [][3][]float32
	next         int // next real row to produce
}

const (
	fusedB  = 5 // columns of mirrored padding in the row buffers
	ringD   = 8 // D rows live from y-maxdy-1 to y+1
	ringS   = 4 // S rows from y-maxdy to y
	ringOut = 8 // stage outputs are read from y-r to y+r (r <= 3)
)

// gabEPF applies Gaborish (when gab) and then the frame's EPF iterations.
// The result equals applyGaborish followed by applyEPF.
func (fd *frameDecoder) gabEPF(gab bool) {
	f := &fd.fh.Filter
	sig, sbw, _ := fd.epfSigmas()
	planes := fd.color
	W, H := planes[0].W, planes[0].H
	const B = fusedB
	pw := W + 2*B
	var steps []int
	if f.EPFIters == 3 {
		steps = append(steps, 0)
	}
	steps = append(steps, 1)
	if f.EPFIters >= 2 {
		steps = append(steps, 2)
	}
	R0 := 0 // rows the EPF steps reach beyond a row of the first stage
	for _, st := range steps {
		R0 += epfRadius(st)
	}
	R := R0 // source rows read beyond a chunk
	if gab {
		R++
	}
	var gw [3][3]float32
	for c := range gw {
		w1, w2 := f.GabWeights[c][0], f.GabWeights[c][1]
		div := 1 + 4*(w1+w2)
		gw[c] = [3]float32{1 / div, w1 / div, w2 / div}
	}
	sc := f.EPFChannelScale
	s0, s1, s2 := sc[0], sc[1], sc[2]

	nw := numWorkers()
	// chunk borders are computed twice (R0 rows of the first stage)
	nch := min(2*nw, max(1, H/(8*R0+16)))
	per := (H + nch - 1) / nch
	nch = (H + per - 1) / per
	// source rows next to a chunk are read after the neighbouring chunk
	// may have written them: copies are taken first
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
	rowsPer := 3 * ringOut // first stage
	for _, st := range steps {
		no := 2
		if st == 0 {
			no = len(epfOffsets0)
		}
		rowsPer += no*(ringD+ringS) + 3*ringOut
	}
	need := rowsPer * pw
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
		take := func() []float32 {
			s := buf[:pw:pw]
			buf = buf[pw:]
			return s
		}
		takeOut := func() [][3][]float32 {
			o := make([][3][]float32, ringOut)
			for i := range o {
				for c := range o[i] {
					o[i][c] = take()
				}
			}
			return o
		}
		padCols := func(row []float32) {
			m := row[B : B+W]
			for i := 0; i < B; i++ {
				row[i] = m[mirror(i-B, W)]
				row[B+W+i] = m[mirror(W+i, W)]
			}
		}

		// first stage: source rows, Gaborish applied
		src := takeOut()
		srcNext := max(0, Y0-R0)
		produceSrc := func(y int) {
			rows := &src[y&(ringOut-1)]
			for c := 0; c < 3; c++ {
				dst := rows[c]
				m := dst[B : B+W]
				s := rowAt(c, y)
				if gab {
					t, b := rowAt(c, mirror(y-1, H)), rowAt(c, mirror(y+1, H))
					w0, w1, w2 := gw[c][0], gw[c][1], gw[c][2]
					px := func(x int) float32 {
						xl, xr := mirror(x-1, W), mirror(x+1, W)
						sum1 := (s[xl] + s[xr]) + (t[x] + b[x])
						sum2 := (t[xl] + t[xr]) + (b[xl] + b[xr])
						return sum2*w2 + (sum1*w1 + s[x]*w0)
					}
					m[0] = px(0)
					gabRow(m, t, s, b, w0, w1, w2)
					m[W-1] = px(W - 1)
				} else {
					copy(m, s)
				}
				padCols(dst)
			}
		}

		stages := make([]*epfStage, len(steps))
		reach := R0
		for k, st := range steps {
			reach -= epfRadius(st)
			e := &epfStage{plus: st != 2}
			switch st {
			case 0:
				e.offs, e.sm = epfOffsets0[:], f.EPFPass0Sigma*1.65
			case 1:
				e.offs, e.sm = [][2]int{{1, 0}, {0, 1}}, 1.65
			default:
				e.offs, e.sm = [][2]int{{1, 0}, {0, 1}}, f.EPFPass2Sigma*1.65
			}
			e.bsm = e.sm * f.EPFBorderSadMul
			for _, o := range e.offs {
				e.maxdy = max(e.maxdy, o[0])
			}
			e.d, e.s = make([][][]float32, len(e.offs)), make([][][]float32, len(e.offs))
			for i := range e.offs {
				e.d[i], e.s[i] = make([][]float32, ringD), make([][]float32, ringS)
				for j := range e.d[i] {
					e.d[i][j] = take()
				}
				for j := range e.s[i] {
					e.s[i][j] = take()
				}
			}
			if k < len(steps)-1 {
				e.out = takeOut()
			}
			e.next = max(0, Y0-reach)
			e.sNext = e.next - e.maxdy
			e.dNext = e.sNext
			if e.plus {
				e.dNext--
			}
			stages[k] = e
		}

		// rowOf returns stage k's padded output rows for virtual row v
		// (stage -1: the first stage), producing rows as needed.
		var rowOf func(k, v int) *[3][]float32
		var produce func(k, y int)
		rowOf = func(k, v int) *[3][]float32 {
			y := mirror(v, H)
			if k < 0 {
				for ; srcNext <= y; srcNext++ {
					produceSrc(srcNext)
				}
				return &src[y&(ringOut-1)]
			}
			e := stages[k]
			for ; e.next <= y; e.next++ {
				produce(k, e.next)
			}
			return &e.out[y&(ringOut-1)]
		}
		var rowMul [8]float32
		produce = func(k, y int) {
			e := stages[k]
			// difference and summed rows up to virtual row y (D one row
			// further for the plus-shaped sums)
			dTo := y
			if e.plus {
				dTo++
			}
			for ; e.dNext <= dTo; e.dNext++ {
				u := e.dNext
				a := rowOf(k-1, u)
				for i, o := range e.offs {
					b := rowOf(k-1, u+o[0])
					dx := o[1]
					i0, i1 := max(0, -dx), pw-max(0, dx)
					epfDiffRow(e.d[i][u&(ringD-1)][i0:i1], a[0][i0:i1], b[0][i0+dx:i1+dx], a[1][i0:i1], b[1][i0+dx:i1+dx], a[2][i0:i1], b[2][i0+dx:i1+dx], s0, s1, s2)
				}
			}
			for ; e.sNext <= y; e.sNext++ {
				v := e.sNext
				for i := range e.offs {
					if e.plus {
						plusRow(e.s[i][v&(ringS-1)], e.d[i][(v-1)&(ringD-1)], e.d[i][v&(ringD-1)], e.d[i][(v+1)&(ringD-1)])
					} else {
						copy(e.s[i][v&(ringS-1)], e.d[i][v&(ringD-1)])
					}
				}
			}
			if y%8 == 0 || y%8 == 7 {
				rowMul = [8]float32{e.bsm, e.bsm, e.bsm, e.bsm, e.bsm, e.bsm, e.bsm, e.bsm}
			} else {
				rowMul = [8]float32{e.bsm, e.sm, e.sm, e.sm, e.sm, e.sm, e.sm, e.bsm}
			}
			var o [3][]float32
			last := k == len(stages)-1
			if last {
				// the source row y was last read by the first stage's
				// row y+1, already produced
				for c := range o {
					o[c] = planes[c].Row(y)
				}
			} else {
				out := &e.out[y&(ringOut-1)]
				for c := range o {
					o[c] = out[c][B : B+W]
				}
			}
			cur := rowOf(k-1, y)
			srow := sig[(y/8)*sbw:]
			if len(e.offs) == 2 {
				up, dn := rowOf(k-1, y-1), rowOf(k-1, y+1)
				sv, svu := e.s[0][y&(ringS-1)], e.s[0][(y-1)&(ringS-1)]
				sh := e.s[1][y&(ringS-1)]
				epfRow4(&epf4Args{
					o:  o,
					c:  [3][]float32{cur[0][B : B+W], cur[1][B : B+W], cur[2][B : B+W]},
					l:  [3][]float32{cur[0][B-1 : B-1+W], cur[1][B-1 : B-1+W], cur[2][B-1 : B-1+W]},
					r:  [3][]float32{cur[0][B+1 : B+1+W], cur[1][B+1 : B+1+W], cur[2][B+1 : B+1+W]},
					u:  [3][]float32{up[0][B : B+W], up[1][B : B+W], up[2][B : B+W]},
					d:  [3][]float32{dn[0][B : B+W], dn[1][B : B+W], dn[2][B : B+W]},
					sv: sv[B : B+W], svu: svu[B : B+W], sh: sh[B : B+W], shl: sh[B-1 : B-1+W],
					sig: srow, rowMul: &rowMul, n: W,
				})
			} else {
				epfRowN(e, k, y, cur, o, srow, &rowMul, rowOf, B, W)
			}
			if !last {
				out := &e.out[y&(ringOut-1)]
				for c := range out {
					padCols(out[c])
				}
			}
		}
		for y := Y0; y < Y1; y++ {
			produce(len(stages)-1, y)
		}
		return nil
	})
}

// epfRowN is the EPF output of row y for a step with any offsets (step 0),
// summing the weights in the order of epfStep.
func epfRowN(e *epfStage, k, y int, cur *[3][]float32, o [3][]float32, srow []float32, rowMul *[8]float32, rowOf func(k, v int) *[3][]float32, B, W int) {
	type side struct {
		s          []float32 // summed row, offset so that index x matches
		p0, p1, p2 []float32 // input rows, likewise
	}
	var pos, neg [len(epfOffsets0)]side
	no := len(e.offs)
	for i, of := range e.offs {
		dy, dx := of[0], of[1]
		sp := e.s[i][y&(ringS-1)]
		pp := rowOf(k-1, y+dy)
		pos[i] = side{sp[B : B+W], pp[0][B+dx : B+dx+W], pp[1][B+dx : B+dx+W], pp[2][B+dx : B+dx+W]}
		sn := e.s[i][(y-dy)&(ringS-1)]
		pn := rowOf(k-1, y-dy)
		neg[i] = side{sn[B-dx : B-dx+W], pn[0][B-dx : B-dx+W], pn[1][B-dx : B-dx+W], pn[2][B-dx : B-dx+W]}
	}
	c0, c1, c2 := cur[0][B:B+W], cur[1][B:B+W], cur[2][B:B+W]
	o0, o1, o2 := o[0][:W], o[1][:W], o[2][:W]
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
		for i := 0; i < no; i++ {
			p := &pos[i]
			w := relu32(p.s[x]*inv + 1)
			wsum += w
			ax += w * p.p0[x]
			ay += w * p.p1[x]
			ab += w * p.p2[x]
			n := &neg[i]
			w = relu32(n.s[x]*inv + 1)
			wsum += w
			ax += w * n.p0[x]
			ay += w * n.p1[x]
			ab += w * n.p2[x]
		}
		iw := 1 / wsum
		o0[x], o1[x], o2[x] = ax*iw, ay*iw, ab*iw
	}
}
