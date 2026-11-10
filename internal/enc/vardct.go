package enc

import (
	"errors"
	"math"
	"math/bits"
	"slices"

	"gojxl/internal/bitio"
	"gojxl/internal/jxl"
)

// LossyOptions controls VarDCT encoding.
type LossyOptions struct {
	// Distance is the target perceptual distance (1.0 ~ visually lossless).
	Distance float32
	// NoGaborish / EPFIters tune the decoder-side restoration filters.
	NoGaborish bool
	EPFIters   int     // -1: default
	Sharpness  int     // EPF sharpness per block (0-7), -1: default
	AQ         float64 // adaptive quantisation strength (0 = off)
	NoCfL      bool    // disable per-tile chroma-from-luma
	OnlyDCT8   bool    // disable larger transforms
	RDMu       float64 // rate weight in AC strategy decisions
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// gaborishInverse sharpens a plane with a truncated Neumann series of the
// inverse Gaborish kernel so that the decoder blur restores the input.
func gaborishInverse(p []float32, w, h int) {
	const w1, w2 = 0.115169525, 0.061248592
	div := float32(1 + 4*(w1+w2))
	k0, k1, k2 := 1/div, float32(w1)/div, float32(w2)/div
	blur := func(src []float32) []float32 {
		out := make([]float32, len(src))
		parallel(h, func(y int) {
			t := src[max(y-1, 0)*w:]
			m := src[y*w:]
			b := src[min(y+1, h-1)*w:]
			for x := 0; x < w; x++ {
				xl, xr := max(x-1, 0), min(x+1, w-1)
				out[y*w+x] = k0*m[x] + k1*(m[xl]+m[xr]+t[x]+b[x]) + k2*(t[xl]+t[xr]+b[xl]+b[xr])
			}
		})
		return out
	}
	// y = x + r + r2 + r3 with r_{n+1} = r_n - K r_n
	r := append([]float32(nil), p...)
	acc := append([]float32(nil), p...)
	for it := 0; it < 3; it++ {
		kr := blur(r)
		for i := range r {
			r[i] -= kr[i]
			acc[i] += r[i]
		}
	}
	copy(p, acc)
}

const (
	opsinBias   = 0.0037930732552754493
	numBlockCtx = 15
)

var srgbToLinear8 = func() (t [256]float32) {
	for i := range t {
		v := float64(i) / 255
		if v <= 0.04045 {
			t[i] = float32(v / 12.92)
		} else {
			t[i] = float32(math.Pow((v+0.055)/1.055, 2.4))
		}
	}
	return
}()

func srgbToLinear(v float32) float32 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return float32(math.Pow((float64(v)+0.055)/1.055, 2.4))
}

// toXYB converts the image to padded XYB planes (X, Y, B).
func toXYB(im *Image, pw, ph int) [3][]float32 {
	var out [3][]float32
	for c := range out {
		out[c] = make([]float32, pw*ph)
	}
	maxv := float32(int(1)<<im.BitDepth - 1)
	cb := float32(math.Cbrt(opsinBias))
	ch := im.Chans
	parallel(ph, func(y int) {
		sy := min(y, im.H-1)
		for x := 0; x < pw; x++ {
			sx := min(x, im.W-1)
			i := sy*im.W + sx
			var r, g, b float32
			if im.BitDepth == 8 {
				r = srgbToLinear8[ch[0][i]]
				if im.Grey {
					g, b = r, r
				} else {
					g, b = srgbToLinear8[ch[1][i]], srgbToLinear8[ch[2][i]]
				}
			} else {
				r = srgbToLinear(float32(ch[0][i]) / maxv)
				if im.Grey {
					g, b = r, r
				} else {
					g, b = srgbToLinear(float32(ch[1][i])/maxv), srgbToLinear(float32(ch[2][i])/maxv)
				}
			}
			m0 := 0.30*r + 0.622*g + 0.078*b + opsinBias
			m1 := 0.23*r + 0.692*g + 0.078*b + opsinBias
			m2 := 0.24342268924547819*r + 0.20476744424496821*g + 0.55180986650955360*b + opsinBias
			m0 = float32(math.Cbrt(float64(max(m0, 0)))) - cb
			m1 = float32(math.Cbrt(float64(max(m1, 0)))) - cb
			m2 = float32(math.Cbrt(float64(max(m2, 0)))) - cb
			o := y*pw + x
			out[0][o] = (m0 - m1) / 2
			out[1][o] = (m0 + m1) / 2
			out[2][o] = m2
		}
	})
	return out
}

// dct8 basis: c[k] = 1/8 sum x[n] * (k==0 ? 1 : sqrt2 cos(pi(2n+1)k/16))
var dctBasis = func() (t [8][8]float32) {
	for k := 0; k < 8; k++ {
		for n := 0; n < 8; n++ {
			v := 1.0 / 8
			if k > 0 {
				v *= math.Sqrt2 * math.Cos(math.Pi*float64((2*n+1)*k)/16)
			}
			t[k][n] = float32(v)
		}
	}
	return
}()

// fdct8x8 computes the scaled DCT of an 8x8 block (row-major input) and
// returns it in the decoder's (transposed) storage layout: s[kx*8+ky].
func fdct8x8(in *[64]float32, s *[64]float32) {
	var tmp [64]float32
	// rows: horizontal frequencies
	for y := 0; y < 8; y++ {
		for k := 0; k < 8; k++ {
			var a float32
			for n := 0; n < 8; n++ {
				a += dctBasis[k][n] * in[y*8+n]
			}
			tmp[y*8+k] = a // tmp[y][kx]
		}
	}
	for kx := 0; kx < 8; kx++ {
		for ky := 0; ky < 8; ky++ {
			var a float32
			for n := 0; n < 8; n++ {
				a += dctBasis[ky][n] * tmp[n*8+kx]
			}
			s[kx*8+ky] = a
		}
	}
}

func quantAC(v, step float32) int32 {
	r := v / step
	if r < 0.6 && r > -0.6 {
		return 0
	}
	return int32(math.Round(float64(r)))
}

// dequantAC mirrors the decoder (AdjustQuantBias).
func dequantAC(q int32, step, bias, numer float32) float32 {
	switch {
	case q == 0:
		return 0
	case q == 1:
		return bias * step
	case q == -1:
		return -bias * step
	}
	f := float32(q)
	return (f - numer/f) * step
}

type vtype struct{ acs, n, table, order int }

// vtypes are the supported varblock types: DCT8, DCT16, DCT32.
var vtypes = [3]vtype{{0, 1, 0, 0}, {4, 2, 4, 2}, {5, 4, 5, 3}}

var chanScale = [3]float32{0.8, 1, 1} // x_qm_scale 3, b_qm_scale 2

// chanDistW weights channel errors by their visibility (from the DC-ish
// quantisation weights of the default DCT8 table).
var chanDistW = [3]float64{31.6, 1, 0.84}

// refMult is the Y DCT8 multiplier of the first AC coefficient, used to
// normalise distortion.
var refMult = float64(jxl.DCT8Multipliers()[1][1])

var dctBases [65][]float32

func dctBasisN(n int) []float32 {
	if b := dctBases[n]; b != nil {
		return b
	}
	return nil
}

func init() {
	for _, n := range []int{2, 4, 8, 16, 32} {
		b := make([]float32, n*n)
		for k := 0; k < n; k++ {
			for i := 0; i < n; i++ {
				v := 1.0 / float64(n)
				if k > 0 {
					v *= math.Sqrt2 * math.Cos(math.Pi*float64((2*i+1)*k)/float64(2*n))
				}
				b[k*n+i] = float32(v)
			}
		}
		dctBases[n] = b
	}
}

var secTab = func() (t [65][]float32) {
	for n := 2; n <= 64; n *= 2 {
		t[n] = make([]float32, n/2)
		for i := range t[n] {
			t[n][i] = float32(1 / (2 * math.Cos((float64(i)+0.5)*math.Pi/float64(n))))
		}
	}
	return
}()

// fdct1d computes M^T x (unnormalised), the transpose of the decoder's
// scaled IDCT; dividing by N gives the scaled forward DCT.
func fdct1d(x, tmp []float32) {
	n := len(x)
	switch n {
	case 1:
		return
	case 2:
		a, b := x[0], x[1]
		x[0], x[1] = a+b, a-b
		return
	case 8:
		fdct8(x)
		return
	}
	h := n / 2
	sh := secTab[n]
	u, v := tmp[:h], tmp[h:n]
	for i := 0; i < h; i++ {
		a, b := x[i], x[n-1-i]
		u[i] = a + b
		v[i] = (a - b) * sh[i]
	}
	fdct1d(u, x[:h])
	fdct1d(v, x[h:n])
	// B^T on the odd part
	for i := 0; i < h; i++ {
		x[2*i] = u[i]
	}
	for i := 0; i < h; i++ {
		w := v[i]
		if i == 0 {
			w *= math.Sqrt2
		}
		if i+1 < h {
			w += v[i+1]
		}
		x[2*i+1] = w
	}
}

var (
	f4a = secTab[4][0]
	f4b = secTab[4][1]
	f8  = secTab[8]
)

// fdct8 is the unrolled transpose of the decoder's idct8 (unnormalised).
func fdct8(x []float32) {
	_ = x[7]
	const r2 = math.Sqrt2
	E0, O0 := x[0]+x[7], x[0]-x[7]
	E1, O1 := x[1]+x[6], x[1]-x[6]
	E2, O2 := x[2]+x[5], x[2]-x[5]
	E3, O3 := x[3]+x[4], x[3]-x[4]
	P0, P1, P2, P3 := O0*f8[0], O1*f8[1], O2*f8[2], O3*f8[3]
	f0, fp0 := P0+P3, P0-P3
	f1, fp1 := P1+P2, P1-P2
	G0, G1 := fp0*f4a, fp1*f4b
	fo0, fo1 := G0+G1, G0-G1
	o0, o1, o2, o3 := f0+f1, fo0*r2+fo1, f0-f1, fo1
	e0, ep0 := E0+E3, E0-E3
	e1, ep1 := E1+E2, E1-E2
	H0, H1 := ep0*f4a, ep1*f4b
	eo0, eo1 := H0+H1, H0-H1
	x[0], x[4] = e0+e1, e0-e1
	x[2], x[6] = eo0*r2+eo1, eo1
	x[1], x[3], x[5], x[7] = o0*r2+o1, o1+o2, o2+o3, o3
}

// fdctN computes the scaled NxN DCT of the block at src (row stride) and
// stores it in the decoder's transposed layout out[kx*n+ky].
func fdctN(src []float32, stride, n int, out []float32) {
	var tmpA [32 * 32]float32
	var col, scr [64]float32
	tmp := tmpA[:n*n]
	inv := 1 / float32(n*n)
	for y := 0; y < n; y++ {
		r := tmp[y*n : y*n+n]
		copy(r, src[y*stride:y*stride+n])
		fdct1d(r, scr[:])
	}
	for kx := 0; kx < n; kx++ {
		c := col[:n]
		for y := 0; y < n; y++ {
			c[y] = tmp[y*n+kx]
		}
		fdct1d(c, scr[:])
		o := out[kx*n : kx*n+n]
		for ky := range o {
			o[ky] = c[ky] * inv
		}
	}
}

// fdctNRef is the direct O(N^3) reference.
func fdctNRef(src []float32, stride, n int, out []float32) {
	bas := dctBasisN(n)
	tmp := make([]float32, n*n)
	for y := 0; y < n; y++ {
		row := src[y*stride : y*stride+n]
		for k := 0; k < n; k++ {
			bk := bas[k*n : k*n+n]
			var a float32
			for i, v := range row {
				a += bk[i] * v
			}
			tmp[y*n+k] = a
		}
	}
	for kx := 0; kx < n; kx++ {
		for ky := 0; ky < n; ky++ {
			bk := bas[ky*n : ky*n+n]
			var a float32
			for i := 0; i < n; i++ {
				a += bk[i] * tmp[i*n+kx]
			}
			out[kx*n+ky] = a
		}
	}
}

// llfToDC derives the k x k block DC values (row-major) that reproduce the
// lowest frequencies of an n=8k transform in the decoder.
func llfToDC(coef []float32, k int, dc []float32) {
	n := 8 * k
	if k == 1 {
		dc[0] = coef[0]
		return
	}
	// natural LLF L[ky][kx] = coef[kx*n+ky], unscaled
	var L [16]float32
	for ky := 0; ky < k; ky++ {
		for kx := 0; kx < k; kx++ {
			L[ky*k+kx] = coef[kx*n+ky] / (jxl.ResampleScale(k, ky) * jxl.ResampleScale(k, kx))
		}
	}
	// k x k scaled IDCT: x[i] = c0 + sqrt2 sum c_j cos(pi(2i+1)j/2k)
	idct := func(c []float32, out []float32) {
		for i := 0; i < k; i++ {
			v := float64(c[0])
			for j := 1; j < k; j++ {
				v += math.Sqrt2 * float64(c[j]) * math.Cos(math.Pi*float64((2*i+1)*j)/float64(2*k))
			}
			out[i] = float32(v)
		}
	}
	var tmp [16]float32
	var row, col [4]float32
	for ky := 0; ky < k; ky++ {
		idct(L[ky*k:ky*k+k], row[:k])
		copy(tmp[ky*k:], row[:k])
	}
	for x := 0; x < k; x++ {
		for ky := 0; ky < k; ky++ {
			col[ky] = tmp[ky*k+x]
		}
		var o [4]float32
		idct(col[:k], o[:k])
		for y := 0; y < k; y++ {
			dc[y*k+x] = o[y]
		}
	}
}

// rdCost estimates distortion (in units of step^2, pixel domain via
// Parseval) and rate for a block's AC coefficients.
func rdCost(coef, mult []float32, order []int32, k int, step float32, cw float64) (float64, float64) {
	n := 8 * k
	var r, d float64
	last := k * k
	for i := k * k; i < n*n; i++ {
		p := order[i]
		st := mult[p] * step
		q := quantAC(coef[p], st)
		e := float64(coef[p] - dequantAC(q, st, 0.95, 0.145))
		d += e * e
		if q != 0 {
			if q < 0 {
				q = -q
			}
			r += 3.5 + 2*math.Log2(float64(q))
			last = i + 1
		}
	}
	r += 0.35*float64(last-k*k) + 5
	ref := float64(step) * refMult
	return d * float64(n*n) * cw / (ref * ref), r
}

// writeModularLocal writes a self-contained modular stream (own tree).
func writeModularLocal(w *bitio.Writer, chs []channel) {
	writeModularLocalEffort(w, chs, 2)
}

// writeModularLocalEffort is writeModularLocal with a choice of context
// model: effort >= 4 learns the tree from the data.
func writeModularLocalEffort(w *bitio.Writer, chs []channel, effort int) {
	writeModularLocalOpts(w, chs, effort, false)
}

// writeModularLocalOpts can exclude the (slow) weighted predictor.
func writeModularLocalOpts(w *bitio.Writer, chs []channel, effort int, noWP bool) {
	w.WriteBool(false) // local tree
	w.WriteBool(true)  // default WP
	w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	n := 0
	for _, c := range chs {
		n += c.w * c.h
	}
	s := stream{w: 1 << 30, h: 1 << 30}
	for i := range chs {
		s.chans = append(s.chans, i)
		s.ctxIdx = append(s.ctxIdx, i)
	}
	allZero := true
	for _, c := range chs {
		for _, v := range c.pix {
			if v != 0 {
				allZero = false
				break
			}
		}
	}
	var root *tnode
	if allZero {
		root = &tnode{leaf: true, pred: 0}
	} else if effort >= 4 && n >= 64 {
		lp := learnParamsFor(effort)
		if noWP {
			lp.preds = slices.DeleteFunc(slices.Clone(lp.preds), func(p int) bool { return p == 6 })
		}
		root = learnTree(collectSamples(chs, []stream{s}, lp), lp)
	} else {
		root = buildTree(len(chs), n, 2)
	}
	nodes, nleaves := bfs(root)

	treeH := newHistograms(6, defaultCfg)
	treeTokens(nodes, treeH.add)
	tc := writeCode(w, treeH)
	treeTokens(nodes, func(ctx int, v uint32) { tc.write(w, ctx, v) })
	data := newHistograms(nleaves, defaultCfg)
	var dc *ansCode
	if effort >= 4 {
		var cache residCache
		walkStream(chs, s, root, func(ctx int, v uint32) {
			cache.ctx = append(cache.ctx, uint16(ctx))
			cache.v = append(cache.v, v)
			data.add(ctx, v)
		})
		visit := func(_ int, fn func(ctx int, v uint32)) {
			for k, v := range cache.v {
				fn(int(cache.ctx[k]), v)
			}
		}
		dc = writeANSCodeTuned(w, 1, visit, data.counts)
		st := &ansStream{c: dc, toks: make([]ansToken, 0, n)}
		visit(0, st.add)
		st.flush(w)
		return
	}
	walkStream(chs, s, root, data.add)
	dc = writeANSCode(w, data)
	st := &ansStream{c: dc, toks: make([]ansToken, 0, n)}
	walkStream(chs, s, root, st.add)
	st.flush(w)
}

// EncodeVarDCT encodes an 8/16-bit sRGB image lossily.
func EncodeVarDCT(im *Image, opt LossyOptions) ([]byte, error) {
	if im.W <= 0 || im.H <= 0 {
		return nil, errors.New("jxl: empty image")
	}
	d := opt.Distance
	if d <= 0 {
		d = 1
	}
	bw, bh := (im.W+7)/8, (im.H+7)/8
	pw, ph := bw*8, bh*8
	xyb := toXYB(im, pw, ph)
	gab := !opt.NoGaborish
	if gab {
		for c := 0; c < 3; c++ {
			gaborishInverse(xyb[c], pw, ph)
		}
	}
	epfIters := opt.EPFIters
	if epfIters < 0 {
		epfIters = 0
	}
	sharp := opt.Sharpness
	if sharp < 0 {
		sharp = 4
	}

	// quantiser parameters
	const gs = 2048
	qf := int32(math.Round(float64(18 / d)))
	qf = min(max(qf, 1), 256)
	qlf := int(math.Round(float64(64 / d)))
	qlf = min(max(qlf, 16), 1024)
	scale := float32(65536.0) / float32(gs) / float32(qf)
	_ = scale
	lfDeq := [3]float32{1.0 / 32 / 128, 1.0 / 4 / 128, 1.0 / 2 / 128}
	var mulDC [3]float32
	for c := 0; c < 3; c++ {
		mulDC[c] = float32(65536.0/float64(gs)/float64(qlf)) * lfDeq[c]
	}
	op := jxl.OpsinInverseDefault()
	biases, numer := op.QuantBias, op.QuantBiasNumerator
	var tmul [3][3][]float32
	var torder [3][]int32
	for i, t := range vtypes {
		tmul[i] = jxl.QuantMultipliers(t.table)
		torder[i] = jxl.NaturalOrder(t.order)
	}
	nb := bw * bh
	baseStep := float32(65536.0) / float32(gs) / float32(qf)

	// --- AC strategy selection on 4x4 block cells
	vtyp := make([]int8, nb)
	first := make([]bool, nb)
	for i := range first {
		first[i] = true
	}
	mu := opt.RDMu
	if mu <= 0 {
		mu = 0.5
	}
	if !opt.OnlyDCT8 {
		cw4, ch4 := (bw+3)/4, (bh+3)/4
		parallel(ch4, func(cy int) {
			var buf []float32
			for cx := 0; cx < cw4; cx++ {
				bx0, by0 := cx*4, cy*4
				cost := func(ti, bx, by int) float64 {
					n := vtypes[ti].n * 8
					if bx*8+n > pw || by*8+n > ph {
						return math.Inf(1)
					}
					if len(buf) < n*n {
						buf = make([]float32, n*n)
					}
					var j float64
					for c := 0; c < 3; c++ {
						fdctN(xyb[c][(by*8)*pw+bx*8:], pw, n, buf[:n*n])
						dd, rr := rdCost(buf[:n*n], tmul[ti][c], torder[ti], vtypes[ti].n, baseStep, chanDistW[c])
						j += dd/64 + mu*rr
					}
					return j
				}
				set := func(ti, bx, by int) {
					n := vtypes[ti].n
					for y := 0; y < n; y++ {
						for x := 0; x < n; x++ {
							i := (by+y)*bw + bx + x
							vtyp[i] = int8(ti)
							first[i] = x == 0 && y == 0
						}
					}
				}
				var sub [4]float64
				var subT [4]int
				for k := 0; k < 4; k++ {
					sx, sy := bx0+2*(k%2), by0+2*(k/2)
					if sx >= bw || sy >= bh {
						continue
					}
					c8 := 0.0
					for j := 0; j < 4; j++ {
						x, y := sx+j%2, sy+j/2
						if x < bw && y < bh {
							c8 += cost(0, x, y)
						}
					}
					c16 := math.Inf(1)
					if sx+2 <= bw && sy+2 <= bh {
						c16 = cost(1, sx, sy)
					}
					if c16 < c8 {
						sub[k], subT[k] = c16, 1
					} else {
						sub[k], subT[k] = c8, 0
					}
				}
				if bx0+4 <= bw && by0+4 <= bh && cost(2, bx0, by0) < sub[0]+sub[1]+sub[2]+sub[3] {
					set(2, bx0, by0)
					continue
				}
				for k := 0; k < 4; k++ {
					if subT[k] == 1 {
						set(1, bx0+2*(k%2), by0+2*(k/2))
					}
				}
			}
		})
	}

	// --- pass A: coefficients, AQ, DC (from LLF), Y AC, CfL statistics
	qdc := [3][]int32{make([]int32, nb), make([]int32, nb), make([]int32, nb)}
	bqf := make([]int32, nb)
	vq := make([][]int32, nb)
	vc := make([][]float32, nb) // cached X and B coefficients per varblock
	aqStrength := float32(opt.AQ)
	tw := (bw + 7) / 8
	thh := (bh + 7) / 8
	rowSums := make([][]float64, bh)
	coefOf := func(ti, bx, by int, out *[3][]float32) {
		n := vtypes[ti].n * 8
		for c := 0; c < 3; c++ {
			if len(out[c]) < n*n {
				out[c] = make([]float32, n*n)
			}
			fdctN(xyb[c][(by*8)*pw+bx*8:], pw, n, out[c][:n*n])
		}
	}
	isLLF := func(ti, idx int) bool {
		k := vtypes[ti].n
		n := k * 8
		return idx/n < k && idx%n < k
	}
	stepsOf := func(b int) [3]float32 {
		bs := float32(65536.0) / float32(gs) / float32(bqf[b])
		return [3]float32{bs * chanScale[0], bs * chanScale[1], bs * chanScale[2]}
	}
	parallel(bh, func(by int) {
		var coef [3][]float32
		sums := make([]float64, 3*tw)
		for bx := 0; bx < bw; bx++ {
			b := by*bw + bx
			if !first[b] {
				continue
			}
			ti := int(vtyp[b])
			k := vtypes[ti].n
			n := k * 8
			coefOf(ti, bx, by, &coef)
			bq := qf
			if aqStrength > 0 {
				var act float32
				for i := 0; i < n*n; i++ {
					if !isLLF(ti, i) {
						act += abs32(coef[1][i])
					}
				}
				act /= float32(k * k)
				m := float32(math.Pow(float64(0.02/(act+0.02)), float64(aqStrength)))
				bq = int32(math.Round(float64(float32(qf) * min(max(m*1.6, 0.5), 2.5))))
				bq = min(max(bq, 1), 256)
			}
			bqf[b] = bq
			steps := stepsOf(b)
			// DC values per covered block from the LLF coefficients
			var dcv [3][16]float32
			for c := 0; c < 3; c++ {
				llfToDC(coef[c], k, dcv[c][:k*k])
			}
			for iy := 0; iy < k; iy++ {
				for ix := 0; ix < k; ix++ {
					i := (by+iy)*bw + bx + ix
					j := iy*k + ix
					qy := int32(math.Round(float64(dcv[1][j] / mulDC[1])))
					qdc[1][i] = qy
					qdc[0][i] = int32(math.Round(float64(dcv[0][j] / mulDC[0])))
					qdc[2][i] = int32(math.Round(float64((dcv[2][j] - float32(qy)*mulDC[1]) / mulDC[2])))
				}
			}
			q := make([]int32, 3*n*n)
			mY := tmul[ti][1]
			t := bx / 8
			for i := 0; i < n*n; i++ {
				if isLLF(ti, i) {
					continue
				}
				st := mY[i] * steps[1]
				q[n*n+i] = quantAC(coef[1][i], st)
				yd := float64(dequantAC(q[n*n+i], st, biases[1], numer))
				sums[3*t] += float64(coef[0][i]) * yd
				sums[3*t+1] += float64(coef[2][i]) * yd
				sums[3*t+2] += yd * yd
			}
			vq[b] = q
			cc := make([]float32, 2*n*n)
			copy(cc, coef[0][:n*n])
			copy(cc[n*n:], coef[2][:n*n])
			vc[b] = cc
		}
		rowSums[by] = sums
	})
	// CfL factors per tile
	ytox := make([]int32, tw*thh)
	ytob := make([]int32, tw*thh)
	for ty := 0; ty < thh; ty++ {
		for tx := 0; tx < tw; tx++ {
			var sx, sb, syy float64
			for by := ty * 8; by < min(bh, ty*8+8); by++ {
				r := rowSums[by]
				sx += r[3*tx]
				sb += r[3*tx+1]
				syy += r[3*tx+2]
			}
			if syy > 1e-9 && !opt.NoCfL {
				clamp := func(v float64) int32 { return int32(min(max(math.Round(v), -128), 127)) }
				ytox[ty*tw+tx] = clamp(sx / syy * 84)
				ytob[ty*tw+tx] = clamp((sb/syy - 1) * 84)
			}
		}
	}
	// pass B: X and B AC with tile CfL
	parallel(bh, func(by int) {
		var coef [3][]float32
		for bx := 0; bx < bw; bx++ {
			b := by*bw + bx
			if !first[b] {
				continue
			}
			ti := int(vtyp[b])
			n := vtypes[ti].n * 8
			coef[0], coef[2] = vc[b][:n*n], vc[b][n*n:]
			steps := stepsOf(b)
			tix := (by/8)*tw + bx/8
			kx := float32(ytox[tix]) / 84
			kb := 1 + float32(ytob[tix])/84
			q := vq[b]
			mm := tmul[ti]
			for i := 0; i < n*n; i++ {
				if isLLF(ti, i) {
					continue
				}
				yd := dequantAC(q[n*n+i], mm[1][i]*steps[1], biases[1], numer)
				q[i] = quantAC(coef[0][i]-kx*yd, mm[0][i]*steps[0])
				q[2*n*n+i] = quantAC(coef[2][i]-kb*yd, mm[2][i]*steps[2])
			}
			vc[b] = nil
		}
	})

	gd := groupDim / 8 // blocks per group
	xg, yg := (bw+gd-1)/gd, (bh+gd-1)/gd
	ngroups := xg * yg
	lfd := groupDim // blocks per LF group
	xlf, ylf := (bw+lfd-1)/lfd, (bh+lfd-1)/lfd
	nlf := xlf * ylf

	freqCtx := jxl.CoeffFreqCtx()
	nzCtx := jxl.CoeffNumNonzeroCtx()
	bctxMap := jxl.DefaultBlockCtxMap()
	chanOrder := [3]int{1, 0, 2}

	// emitGroup produces the HF tokens of a group.
	emitGroup := func(g int, emit func(ctx int, v uint32)) {
		gx, gy := g%xg, g/xg
		bx0, by0 := gx*gd, gy*gd
		bx1, by1 := min(bx0+gd, bw), min(by0+gd, bh)
		var nzRow [3][]int32
		for c := range nzRow {
			nzRow[c] = make([]int32, gd+4)
		}
		for by := by0; by < by1; by++ {
			for bx := bx0; bx < bx1; bx++ {
				b := by*bw + bx
				if !first[b] {
					continue
				}
				ti := int(vtyp[b])
				vt := vtypes[ti]
				nbk := int32(vt.n * vt.n)
				lg := uint(bits.TrailingZeros32(uint32(nbk)))
				n := vt.n * 8
				ord := torder[ti]
				for i, c := range chanOrder {
					q := vq[b][c*n*n : (c+1)*n*n]
					bctx := int(bctxMap[i*13+vt.order])
					sx, sy := bx-bx0, by-by0
					var pred int32
					switch {
					case sy == 0 && sx == 0:
						pred = 32
					case sy == 0:
						pred = nzRow[c][sx-1]
					case sx == 0:
						pred = nzRow[c][sx]
					default:
						pred = (nzRow[c][sx] + nzRow[c][sx-1] + 1) >> 1
					}
					pidx := pred
					if pred >= 8 {
						pidx = 4 + pred/2
					}
					nz := int32(0)
					for k := int(nbk); k < n*n; k++ {
						if q[ord[k]] != 0 {
							nz++
						}
					}
					emit(bctx+int(pidx)*numBlockCtx, uint32(nz))
					nzv := (nz + nbk - 1) >> lg
					for dx := 0; dx < vt.n; dx++ {
						nzRow[c][sx+dx] = nzv
					}
					if nz == 0 {
						continue
					}
					base := bctx*458 + 37*numBlockCtx
					prev := 0
					if nz <= 4*nbk {
						prev = 1
					}
					for k := int(nbk); k < n*n && nz > 0; k++ {
						v := q[ord[k]]
						ctx := base + (int(nzCtx[(nz-1)>>lg])+int(freqCtx[(k-int(nbk))>>lg]))*2 + prev
						emit(ctx, bitio.PackSigned(v))
						if v != 0 {
							nz--
							prev = 1
						} else {
							prev = 0
						}
					}
				}
			}
		}
	}

	// HF histograms over all groups
	nctx := 495 * numBlockCtx
	hs := make([]*histograms, ngroups)
	parallel(ngroups, func(g int) {
		h := newHistograms(nctx, defaultCfg)
		emitGroup(g, h.add)
		hs[g] = h
	})
	hf := newHistograms(nctx, defaultCfg)
	for _, h := range hs {
		hf.merge(h)
	}

	// --- alpha as a lossless modular extra channel
	var alpha *alphaPlan
	if im.Alpha {
		alpha = planAlpha(im.Chans[len(im.Chans)-1], im.W, im.H, xg, yg)
	}

	// --- section writers
	writeLfGlobal := func(w *bitio.Writer) {
		w.WriteBool(true) // LF channel dequant default
		w.WriteU32(gs, bitsOff(11, 1), bitsOff(11, 2049), bitsOff(12, 4097), bitsOff(16, 8193))
		w.WriteU32(uint32(qlf), val(16), bitsOff(5, 1), bitsOff(8, 1), bitsOff(16, 1))
		w.WriteBool(true) // default block context map
		w.WriteBool(true) // default LF channel correlation
		if alpha == nil {
			w.WriteBool(false) // no global tree
			return
		}
		alpha.writeGlobal(w)
	}
	writeLfGroup := func(w *bitio.Writer, g int) {
		gx, gy := g%xlf, g/xlf
		x0, y0 := gx*lfd, gy*lfd
		cw, chh := min(lfd, bw-x0), min(lfd, bh-y0)
		w.Write(2, 0) // extra precision
		var lf []channel
		for _, c := range chanOrder {
			ch := channel{w: cw, h: chh, pix: make([]int32, cw*chh)}
			for y := 0; y < chh; y++ {
				copy(ch.pix[y*cw:(y+1)*cw], qdc[c][(y0+y)*bw+x0:])
			}
			lf = append(lf, ch)
		}
		writeModularLocal(w, lf)
		// HF metadata: strategies, quant field, CfL, sharpness
		var acsT, acsQ []int32
		for y := y0; y < y0+chh; y++ {
			for x := x0; x < x0+cw; x++ {
				if b := y*bw + x; first[b] {
					acsT = append(acsT, int32(vtypes[vtyp[b]].acs))
					acsQ = append(acsQ, bqf[b]-1)
				}
			}
		}
		count := len(acsT)
		w.Write(uint(ceilLog2(cw*chh)), uint64(count-1))
		acs := channel{w: count, h: 2, pix: append(acsT, acsQ...)}
		ltw, lth := (cw+7)/8, (chh+7)/8
		cx := channel{w: ltw, h: lth, pix: make([]int32, ltw*lth)}
		cb := channel{w: ltw, h: lth, pix: make([]int32, ltw*lth)}
		for y := 0; y < lth; y++ {
			for x := 0; x < ltw; x++ {
				ti := (y0/8+y)*tw + x0/8 + x
				cx.pix[y*ltw+x] = ytox[ti]
				cb.pix[y*ltw+x] = ytob[ti]
			}
		}
		sh := channel{w: cw, h: chh, pix: make([]int32, cw*chh)}
		for i := range sh.pix {
			sh.pix[i] = int32(sharp)
		}
		writeModularLocal(w, []channel{cx, cb, acs, sh})
	}
	writeHfGlobal := func(w *bitio.Writer) *ansCode {
		w.WriteBool(true)                                           // default dequant matrices
		w.Write(uint(ceilLog2(ngroups)), 0)                         // one HF preset
		w.WriteU32(0, val(0x5F), val(0x13), val(0), bitio.Bits(13)) // natural orders
		return writeANSCodeClustered(w, defaultCfg, hf.counts, 64)
	}
	writeGroup := func(w *bitio.Writer, g int, hc *ansCode) {
		st := &ansStream{c: hc}
		emitGroup(g, st.add)
		st.flush(w)
		if alpha != nil {
			alpha.writeGroup(w, g)
		}
	}

	// --- headers
	out := &bitio.Writer{}
	out.Write(16, 0x0aff)
	writeSize(out, im.W, im.H)
	if alpha == nil {
		out.WriteBool(true) // metadata all default: 8-bit sRGB, XYB
	} else {
		out.WriteBool(false) // metadata
		out.WriteBool(false) // no extra fields
		writeBitDepth(out, 8)
		out.WriteBool(true) // 16-bit buffers suffice
		out.WriteU32(1, val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
		out.WriteBool(true) // default 8-bit alpha
		out.WriteBool(true) // xyb
		out.WriteBool(true) // sRGB
		out.WriteU64(0)     // extensions
	}
	out.WriteBool(true) // default transform data
	out.ZeroPadToByte()
	out.WriteBool(false)
	out.Write(2, 0)    // regular
	out.Write(1, 0)    // VarDCT
	out.WriteU64(0x80) // skip adaptive LF smoothing
	out.WriteU32(1, val(1), val(2), val(4), val(8))
	if alpha != nil {
		out.WriteU32(1, val(1), val(2), val(4), val(8)) // ec upsampling
	}
	out.Write(3, 3) // x_qm_scale
	out.Write(3, 2) // b_qm_scale
	out.WriteU32(1, val(1), val(2), val(3), bitsOff(3, 4))
	out.WriteBool(false) // no crop
	out.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3))
	if alpha != nil {
		out.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3)) // ec blending
	}
	out.WriteBool(true)                                                     // is_last
	out.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48)) // name
	out.WriteBool(false)                                                    // restoration not default
	out.WriteBool(gab)
	if gab {
		out.WriteBool(false) // default weights
	}
	out.Write(2, uint64(epfIters))
	if epfIters > 0 {
		out.WriteBool(false) // default sharpness LUT
		out.WriteBool(false) // default channel scale
		out.WriteBool(false) // default sigma
	}
	out.WriteU64(0)
	out.WriteU64(0) // frame extensions

	writeTOC := func(sizes []int) {
		out.WriteBool(false)
		out.ZeroPadToByte()
		for _, s := range sizes {
			out.WriteU32(uint32(s), bitio.Bits(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
		}
		out.ZeroPadToByte()
	}
	if ngroups == 1 {
		w := &bitio.Writer{}
		writeLfGlobal(w)
		writeLfGroup(w, 0)
		hc := writeHfGlobal(w)
		writeGroup(w, 0, hc)
		sec := w.Bytes()
		writeTOC([]int{len(sec)})
		out.AppendBytes(sec)
		return out.Bytes(), nil
	}
	lfw := &bitio.Writer{}
	writeLfGlobal(lfw)
	lfSecs := make([][]byte, nlf)
	parallel(nlf, func(g int) {
		w := &bitio.Writer{}
		writeLfGroup(w, g)
		lfSecs[g] = w.Bytes()
	})
	hfw := &bitio.Writer{}
	hc := writeHfGlobal(hfw)
	grpSecs := make([][]byte, ngroups)
	parallel(ngroups, func(g int) {
		w := &bitio.Writer{}
		writeGroup(w, g, hc)
		grpSecs[g] = w.Bytes()
	})
	lfSec, hfSec := lfw.Bytes(), hfw.Bytes()
	sizes := []int{len(lfSec)}
	for _, s := range lfSecs {
		sizes = append(sizes, len(s))
	}
	sizes = append(sizes, len(hfSec))
	for _, s := range grpSecs {
		sizes = append(sizes, len(s))
	}
	writeTOC(sizes)
	out.AppendBytes(lfSec)
	for _, s := range lfSecs {
		out.AppendBytes(s)
	}
	out.AppendBytes(hfSec)
	for _, s := range grpSecs {
		out.AppendBytes(s)
	}
	return out.Bytes(), nil
}

// alphaPlan codes an 8-bit alpha channel as a modular extra channel.
type alphaPlan struct {
	chs    []channel
	nodes  []*tnode
	root   *tnode
	global stream
	groups []stream
	data   *histograms
	code   *ansCode
}

func planAlpha(a []int32, W, H, xg, yg int) *alphaPlan {
	p := &alphaPlan{chs: []channel{{w: W, h: H, pix: a}}}
	root := buildTree(1, W*H, 2)
	var nl int
	p.nodes, nl = bfs(root)
	p.root = root
	p.global = stream{w: 1 << 30, h: 1 << 30}
	if W <= groupDim && H <= groupDim {
		p.global.chans, p.global.ctxIdx = []int{0}, []int{0}
	} else {
		for gy := 0; gy < yg; gy++ {
			for gx := 0; gx < xg; gx++ {
				p.groups = append(p.groups, stream{chans: []int{0}, ctxIdx: []int{0},
					x0: gx * groupDim, y0: gy * groupDim, w: groupDim, h: groupDim})
			}
		}
	}
	p.data = newHistograms(nl, defaultCfg)
	walkStream(p.chs, p.global, p.root, p.data.add)
	for _, s := range p.groups {
		walkStream(p.chs, s, p.root, p.data.add)
	}
	return p
}

func (p *alphaPlan) writeGlobal(w *bitio.Writer) {
	w.WriteBool(true) // global tree
	th := newHistograms(6, defaultCfg)
	treeTokens(p.nodes, th.add)
	tc := writeCode(w, th)
	treeTokens(p.nodes, func(ctx int, v uint32) { tc.write(w, ctx, v) })
	p.code = writeANSCode(w, p.data)
	w.WriteBool(true) // use global tree
	w.WriteBool(true) // default WP
	w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	st := &ansStream{c: p.code}
	walkStream(p.chs, p.global, p.root, st.add)
	st.flush(w)
}

func (p *alphaPlan) writeGroup(w *bitio.Writer, g int) {
	if len(p.groups) == 0 {
		return
	}
	w.WriteBool(true)
	w.WriteBool(true)
	w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	st := &ansStream{c: p.code}
	walkStream(p.chs, p.groups[g], p.root, st.add)
	st.flush(w)
}
