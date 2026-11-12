package enc

import (
	"errors"
	"math"
	"math/bits"
	"runtime"
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
	NoSmall    bool    // disable the 8x8 types IDENTITY, DCT2X2, DCT4X4, AFV
	NoPatches  bool    // disable patches for repeated text-like components
	// Tuning of the 8x8 type choice: SmallMask enables the families
	// IDENTITY (1), DCT2X2 (2), DCT4X4 (4), AFV (8) (0: DCT2X2 only, the
	// one that measured better); SmallMul and SplitMul scale the costs of
	// those families and of DCT4x8/8x4 (0: 1.2, as tuned).
	SmallMask int
	SmallMul  [4]float64
	SplitMul  float64
	// Effort (1-7, 0 = 7) trades AC strategy search for speed.
	Effort    int
	RDMu      float64 // rate weight in AC strategy decisions
	Frame     FrameOpts
	LibAQ     bool // libjxl's adaptive quantisation map
	forceType int  // tests: use only vtypes[forceType] (> 0)
}

// TuneLossy, when set, adjusts the options of single-image lossy encodes
// (the command line tool sets it from GOJXL_* variables for tuning).
var TuneLossy func(*LossyOptions)

// candidates are the transform families tried by AC strategy selection.
type candidates struct{ split, rect, dct16, dct32, rect32, small bool }

// selectionFor maps the effort to the candidate set. Selection costs CPU
// in proportion to the coefficients evaluated. On a 5 MP photo at d=1, one
// core (efforts 1-4 also skip adaptive quantisation and Gaborish; sizes as
// BD-rate against effort 1 by SSIMULACRA 2 over five images):
//
//	effort 1:   DCT8 only                    0.45 s
//	effort 2:   + DCT16                      0.64 s  -2..-25% (smooth images most)
//	effort 3:   + split 8x8 (4x8/8x4)        0.81 s  -4..-25% (photos -5%)
//	effort 4:   + DCT32                      0.92 s  about effort 3
//	effort 5-6: + 16x8/8x16, AQ, Gaborish    1.23 s
//	effort 7:   + 32x16/16x32, DCT2X2        1.38 s
//
// DCT2X2 (and the DCT4x8/8x4 costs, scaled by their true pixel energy
// and 1.2) gave -0.7% BD-rate on five images at effort 7: -2.7% on a
// greyscale text screenshot, -1.2% on bike, photos +-0.2%. IDENTITY,
// DCT4X4 and AFV are supported but off: with this cost model (MSE and an
// estimated rate) they made files 1-3% larger, as did choosing 8x8 types
// by a pixel-domain 8-norm like libjxl's (+3.6%).
func selectionFor(opt LossyOptions) candidates {
	e := opt.Effort
	if e <= 0 {
		e = 7
	}
	switch {
	case opt.OnlyDCT8 || e <= 1:
		return candidates{}
	case e == 2:
		// one family: DCT16 gains most on smooth content
		return candidates{dct16: true}
	case e == 3:
		return candidates{split: true, dct16: true}
	case e == 4:
		return candidates{split: true, dct16: true, dct32: true}
	case e <= 6:
		return candidates{split: true, rect: true, dct16: true, dct32: true}
	}
	return candidates{split: true, rect: true, dct16: true, dct32: true, rect32: true, small: !opt.NoSmall}
}

// abs32 clears the sign bit (no branch: the sign is data-dependent).
func abs32(v float32) float32 { return math.Float32frombits(math.Float32bits(v) &^ (1 << 31)) }

// gaborishInverse sharpens a plane so that the decoder's Gaborish blur
// approximately restores it, with libjxl's 5x5 kernel (enc_gaborish.cc:
// tuned for rate-distortion rather than an exact inverse) and mirrored
// borders. It works in place, in strips of rows: each strip first saves
// the two rows above and below it (before any strip is written), and
// writes a computed row back only when the rows below no longer need it.
func gaborishInverse(p []float32, w, h int) {
	kg := [5]float64{-0.09495815671340026, -0.041031725066768575, 0.013710004822696948,
		0.006510206083837737, -0.0014789063378272242}
	norm := 1 / (1 + 4*(kg[0]+kg[1]+kg[2]+kg[4]+2*kg[3]))
	c := float32(norm)
	r, R := float32(norm*kg[0]), float32(norm*kg[2]) // axis distance 1, 2
	d, D := float32(norm*kg[1]), float32(norm*kg[4]) // diagonal (1,1), (2,2)
	L := float32(norm * kg[3])                       // (1,2) and (2,1)
	// row weights for vertical offsets 0, 1, 2: (centre, +-1, +-2)
	wt := [3][3]float32{{c, r, R}, {r, d, L}, {R, L, D}}
	mirror := func(v, n int) int {
		for v < 0 || v >= n {
			if v < 0 {
				v = -v - 1
			} else {
				v = 2*n - 1 - v
			}
		}
		return v
	}
	sh := max(4, (h+4*runtime.GOMAXPROCS(0)-1)/(4*runtime.GOMAXPROCS(0)))
	ns := (h + sh - 1) / sh
	// original rows y0-2, y0-1 and y1, y1+1 of each strip
	saved := make([][4][]float32, ns)
	parallel(ns, func(s int) {
		y0, y1 := s*sh, min(h, (s+1)*sh)
		for i, yy := range [4]int{y0 - 2, y0 - 1, y1, y1 + 1} {
			if yy >= 0 && yy < h {
				saved[s][i] = append([]float32(nil), p[yy*w:yy*w+w]...)
			}
		}
	})
	parallel(ns, func(s int) {
		y0, y1 := s*sh, min(h, (s+1)*sh)
		rowAt := func(yy int) []float32 {
			switch {
			case yy < y0:
				return saved[s][yy-(y0-2)]
			case yy >= y1:
				return saved[s][2+yy-y1]
			}
			return p[yy*w : yy*w+w]
		}
		var bufs [3][]float32
		for i := range bufs {
			bufs[i] = make([]float32, w)
		}
		for y := y0; y < y1; y++ {
			var rows [5][]float32
			for k := -2; k <= 2; k++ {
				rows[k+2] = rowAt(mirror(y+k, h))
			}
			out := bufs[y%3]
			gaborishInverseRow(out, &rows, &wt, w, mirror)
			if y-2 >= y0 {
				copy(p[(y-2)*w:(y-1)*w], bufs[(y-2)%3])
			}
		}
		for y := max(y0, y1-2); y < y1; y++ {
			copy(p[y*w:(y+1)*w], bufs[y%3])
		}
	})
}

// gaborishInverseRow computes one output row from the five input rows
// around it.
func gaborishInverseRow(out []float32, rows *[5][]float32, wt *[3][3]float32, w int, mirror func(v, n int) int) {
	at := func(x, xm1, xp1, xm2, xp2 int) {
		var sum float32
		for k := -2; k <= 2; k++ {
			row := rows[k+2]
			kw := wt[max(k, -k)]
			sum += kw[0]*row[x] + kw[1]*(row[xm1]+row[xp1]) + kw[2]*(row[xm2]+row[xp2])
		}
		out[x] = sum
	}
	for x := 0; x < w; x++ {
		if x == 2 && w > 4 {
			// interior: no mirroring (same arithmetic as at)
			r0, r1, r2, r3, r4 := rows[0][:w], rows[1][:w], rows[2][:w], rows[3][:w], rows[4][:w]
			w0, w1, w2 := wt[0], wt[1], wt[2]
			o := out[:w]
			for ; x < w-2; x++ {
				var sum float32
				sum += w2[0]*r0[x] + w2[1]*(r0[x-1]+r0[x+1]) + w2[2]*(r0[x-2]+r0[x+2])
				sum += w1[0]*r1[x] + w1[1]*(r1[x-1]+r1[x+1]) + w1[2]*(r1[x-2]+r1[x+2])
				sum += w0[0]*r2[x] + w0[1]*(r2[x-1]+r2[x+1]) + w0[2]*(r2[x-2]+r2[x+2])
				sum += w1[0]*r3[x] + w1[1]*(r3[x-1]+r3[x+1]) + w1[2]*(r3[x-2]+r3[x+2])
				sum += w2[0]*r4[x] + w2[1]*(r4[x-1]+r4[x+1]) + w2[2]*(r4[x-2]+r4[x+2])
				o[x] = sum
			}
			if x >= w {
				break
			}
		}
		at(x, mirror(x-1, w), mirror(x+1, w), mirror(x-2, w), mirror(x+2, w))
	}
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

// opsinAbsorbance is the linear sRGB to LMS mix of XYB.
var opsinAbsorbance = [9]float64{
	0.30, 0.622, 0.078,
	0.23, 0.692, 0.078,
	0.24342268924547819, 0.20476744424496821, 0.55180986650955360,
}

// toXYB converts the image to padded XYB planes (X, Y, B). model maps
// the samples to linear sRGB (nil: they are sRGB).
func toXYB(im *Image, pw, ph int, model *jxl.ColourModel) [3][]float32 {
	var out [3][]float32
	for c := range out {
		out[c] = make([]float32, pw*ph)
	}
	maxv := float32(int(1)<<im.BitDepth - 1)
	cb := float32(math.Cbrt(opsinBias))
	ch := im.Chans
	W, H := im.W, im.H
	eight := im.BitDepth == 8
	// per channel decoding tables and the opsin mix, folded with the
	// conversion to linear sRGB
	lin := [3][]float32{srgbToLinear8[:], srgbToLinear8[:], srgbToLinear8[:]}
	var k [9]float32
	for i, v := range opsinAbsorbance {
		k[i] = float32(v)
	}
	if model != nil {
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				var s float64
				for l := 0; l < 3; l++ {
					s += opsinAbsorbance[i*3+l] * model.M[l*3+j]
				}
				k[i*3+j] = float32(s)
			}
		}
	}
	if model != nil || !eight {
		nc := 3
		if model == nil || model.Grey {
			nc = 1 // one curve
		}
		for c := 0; c < nc; c++ {
			t := make([]float32, int(maxv)+1)
			parallel(len(t)/4096+1, func(b int) {
				for v := b * 4096; v < min((b+1)*4096, len(t)); v++ {
					if model == nil {
						t[v] = srgbToLinear(float32(v) / maxv)
					} else {
						t[v] = float32(model.Curves[c](float64(v) / float64(maxv)))
					}
				}
			})
			lin[c] = t
		}
		if nc == 1 {
			lin[1], lin[2] = lin[0], lin[0]
		}
	}
	mask := int32(maxv)
	// HLG: display light = scene light * scale * Y^(gamma-1); the factor
	// is common to the three channels, so it applies after the opsin mix
	var ootfExp, ootfScale float64
	var ootfLum [3]float32
	if model != nil && model.OOTFGamma != 0 {
		ootfExp, ootfScale = model.OOTFGamma-1, model.OOTFScale
		for i, v := range model.OOTFLum {
			ootfLum[i] = float32(v)
		}
	}
	parallel(ph, func(y int) {
		// row slices in locals: through ch and out every sample would
		// reload the slice headers
		sy := min(y, H-1)
		cr := ch[0][sy*W : sy*W+W]
		cg, cbl := cr, cr
		if !im.Grey {
			cg, cbl = ch[1][sy*W:sy*W+W], ch[2][sy*W:sy*W+W]
		}
		o0, o1, o2 := out[0][y*pw:y*pw+pw], out[1][y*pw:y*pw+pw], out[2][y*pw:y*pw+pw]
		l0, l1, l2 := lin[0], lin[1], lin[2]
		k := k
		for x := 0; x < pw; x++ {
			sx := min(x, W-1)
			r, g, b := l0[cr[sx]&mask], l1[cg[sx]&mask], l2[cbl[sx]&mask]
			var m0, m1, m2 float32
			if ootfScale == 0 {
				m0 = k[0]*r + k[1]*g + k[2]*b + opsinBias
				m1 = k[3]*r + k[4]*g + k[5]*b + opsinBias
				m2 = k[6]*r + k[7]*g + k[8]*b + opsinBias
			} else {
				var s float32
				if yl := ootfLum[0]*r + ootfLum[1]*g + ootfLum[2]*b; yl > 0 {
					s = float32(ootfScale * math.Pow(float64(yl), ootfExp))
				}
				m0 = s*(k[0]*r+k[1]*g+k[2]*b) + opsinBias
				m1 = s*(k[3]*r+k[4]*g+k[5]*b) + opsinBias
				m2 = s*(k[6]*r+k[7]*g+k[8]*b) + opsinBias
			}
			m0 = float32(cbrtFast(float64(max(m0, 0)))) - cb
			m1 = float32(cbrtFast(float64(max(m1, 0)))) - cb
			m2 = float32(cbrtFast(float64(max(m2, 0)))) - cb
			o0[x] = (m0 - m1) / 2
			o1[x] = (m0 + m1) / 2
			o2[x] = m2
		}
	})
	return out
}

// cbrtFast is math.Cbrt for v >= 0 (0 otherwise) via a bit-level
// estimate and two Halley steps: ~1e-14 relative error, so the float32
// results match math.Cbrt, at under half the cost.
func cbrtFast(v float64) float64 {
	if v <= 0 {
		return 0
	}
	t := math.Float64frombits(math.Float64bits(v)/3 + 0x2a9f7893782da1ce)
	t3 := t * t * t
	t = t * (t3 + 2*v) / (2*t3 + v)
	t3 = t * t * t
	return t * (t3 + 2*v) / (2*t3 + v)
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

// dqTab[q] is dequantAC(q, 1, 0.95, 0.145) / step for q > 0: the
// multiplier of step (bit-identical, as dequantAC multiplies last).
var dqTab = func() (t [256]float32) {
	t[1] = 0.95
	for q := 2; q < 256; q++ {
		f := float32(q)
		t[q] = f - 0.145/f
	}
	return
}()

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

// vtype is a varblock transform: cx x cy blocks (8x8 pixels each). Types
// with split set are 8x8 blocks made of two half-size DCTs (DCT4x8: top
// and bottom halves, DCT8x4: left and right halves).
type vtype struct {
	acs, cx, cy, table, order int
	split                     bool
}

func (t vtype) rows() int { return 8 * t.cy }
func (t vtype) cols() int { return 8 * t.cx }
func (t vtype) size() int { return 64 * t.cx * t.cy }

// isLLF reports whether coefficient idx (decoder storage layout) is one of
// the cx x cy lowest frequencies, which are derived from the DC image.
func (t vtype) isLLF(idx int) bool {
	if t.cy < t.cx { // natural layout [ky][kx]
		return idx/t.cols() < t.cy && idx%t.cols() < t.cx
	}
	return idx/t.rows() < t.cx && idx%t.rows() < t.cy // transposed [kx][ky]
}

// vtypes are the supported varblock types: DCT8, DCT16, DCT32, then the
// rectangular DCT16x8 (tall), DCT8x16 (wide), DCT32x16 (tall), DCT16x32,
// then the 8x8 types made of smaller transforms ("split": computed from
// pixels by fdct8Kind): DCT4x8, DCT8x4, IDENTITY, DCT2X2, DCT4X4 and the
// four AFV corners. A placement packs the index in 4 bits: at most 16.
var vtypes = [16]vtype{
	{0, 1, 1, 0, 0, false}, {4, 2, 2, 4, 2, false}, {5, 4, 4, 5, 3, false},
	{6, 1, 2, 6, 4, false}, {7, 2, 1, 6, 4, false}, {10, 2, 4, 8, 6, false}, {11, 4, 2, 8, 6, false},
	{12, 1, 1, 9, 1, true}, {13, 1, 1, 9, 1, true},
	{1, 1, 1, 1, 1, true}, {2, 1, 1, 2, 1, true}, {3, 1, 1, 3, 1, true},
	{14, 1, 1, 10, 1, true}, {15, 1, 1, 10, 1, true}, {16, 1, 1, 10, 1, true}, {17, 1, 1, 10, 1, true},
}

// Indices of the split 8x8 types in vtypes.
const (
	vtDCT4x8 = 7 // acs 12: two 4x8 (rows x cols) halves, top and bottom
	vtDCT8x4 = 8 // acs 13: two 8x4 halves, left and right
	vtIdent  = 9 // acs 1, then DCT2X2, DCT4X4, AFV0-3 up to vtAFV3
	vtAFV3   = 15
)

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
	case 4:
		fdct4(x)
		return
	case 8:
		fdct8(x)
		return
	case 16:
		fdct16(x)
		return
	case 32:
		fdct32(x)
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

// fdct4 is fdct1d for n = 4.
func fdct4(x []float32) {
	_ = x[3]
	u0, u1 := x[0]+x[3], x[1]+x[2]
	v0, v1 := (x[0]-x[3])*f4a, (x[1]-x[2])*f4b
	a, b := v0+v1, v0-v1
	x[0], x[2] = u0+u1, u0-u1
	x[1], x[3] = a*math.Sqrt2+b, b
}

// fdct16 and fdct32 are fdct1d for n = 16 and 32 without recursion.
func fdct16(x []float32) {
	_ = x[15]
	var u, v [8]float32
	sh := secTab[16][:8]
	for i := 0; i < 8; i++ {
		a, b := x[i], x[15-i]
		u[i] = a + b
		v[i] = (a - b) * sh[i]
	}
	fdct8(u[:])
	fdct8(v[:])
	for i := 0; i < 8; i++ {
		x[2*i] = u[i]
	}
	x[1] = v[0]*math.Sqrt2 + v[1]
	for i := 1; i < 7; i++ {
		x[2*i+1] = v[i] + v[i+1]
	}
	x[15] = v[7]
}

func fdct32(x []float32) {
	_ = x[31]
	var u, v [16]float32
	sh := secTab[32][:16]
	for i := 0; i < 16; i++ {
		a, b := x[i], x[31-i]
		u[i] = a + b
		v[i] = (a - b) * sh[i]
	}
	fdct16(u[:])
	fdct16(v[:])
	for i := 0; i < 16; i++ {
		x[2*i] = u[i]
	}
	x[1] = v[0]*math.Sqrt2 + v[1]
	for i := 1; i < 15; i++ {
		x[2*i+1] = v[i] + v[i+1]
	}
	x[31] = v[15]
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

// fdctRect is fdctN for a rows x cols block; the output uses the decoder's
// layout: natural [ky][kx] when rows < cols, else transposed [kx][ky].
func fdctRect(src []float32, stride, rows, cols int, out []float32) {
	if rows == cols {
		fdctN(src, stride, rows, out)
		return
	}
	var tmpA [32 * 32]float32
	var col, scr [64]float32
	tmp := tmpA[:rows*cols]
	inv := 1 / float32(rows*cols)
	for y := 0; y < rows; y++ {
		r := tmp[y*cols : y*cols+cols]
		copy(r, src[y*stride:y*stride+cols])
		fdct1d(r, scr[:])
	}
	for kx := 0; kx < cols; kx++ {
		c := col[:rows]
		for y := 0; y < rows; y++ {
			c[y] = tmp[y*cols+kx]
		}
		fdct1d(c, scr[:])
		for ky, v := range c {
			if rows < cols {
				out[ky*cols+kx] = v * inv
			} else {
				out[kx*rows+ky] = v * inv
			}
		}
	}
}

// rowDCTCache holds, for a 32x32 pixel cell and each channel, the
// unnormalised horizontal DCTs of every row over aligned segments of 8, 16
// and 32 pixels, shared by all transform candidates of the cell.
type rowDCTCache struct {
	h [3][4][32 * 32]float32 // [channel][log2(len/8), or 3 for 4][y*32 + x]
}

// rowLens returns which segment lengths (8, 16, 32, 4, as indexed in
// rowDCTCache.h) the candidates read: 8 always (DCT8, 4x8 halves, 16x8),
// 16 for DCT16, 8x16 and 32x16, 32 for DCT32 and 16x32, 4 for 8x4 halves.
func (s candidates) rowLens() [4]bool {
	return [4]bool{true, s.rect || s.dct16 || s.rect32, s.dct32 || s.rect32, s.split}
}

func (r *rowDCTCache) compute(c int, plane []float32, pw, ph, bx0, by0 int, need [4]bool) {
	rows := min(32, ph-by0*8)
	cols := min(32, pw-bx0*8)
	var scr [64]float32
	for li, L := range [4]int{8, 16, 32, 4} {
		if !need[li] {
			continue
		}
		h := &r.h[c][li]
		for y := 0; y < rows; y++ {
			src := plane[(by0*8+y)*pw+bx0*8:]
			for s := 0; (s+1)*L <= cols; s++ {
				seg := h[y*32+s*L : y*32+s*L+L]
				copy(seg, src[s*L:s*L+L])
				fdct1d(seg, scr[:])
			}
		}
	}
}

// coefSplit is fdctSplit of the 8x8 block at pixel offset (ox, oy).
func (r *rowDCTCache) coefSplit(c int, acs int, ox, oy int, out []float32) {
	var col, scr [8]float32
	const inv = 1.0 / 32
	var dc [2]float32
	if acs == 12 { // 4x8 halves: 8-point row DCTs, 4-point columns
		h := &r.h[c][0]
		for y := 0; y < 2; y++ {
			for kx := 0; kx < 8; kx++ {
				for i := 0; i < 4; i++ {
					col[i] = h[(oy+4*y+i)*32+ox+kx]
				}
				fdct1d(col[:4], scr[:])
				for ky := 0; ky < 4; ky++ {
					out[(y+2*ky)*8+kx] = col[ky] * inv
				}
			}
			dc[y] = out[y*8]
		}
	} else { // 8x4 halves: 4-point row DCTs, 8-point columns
		h := &r.h[c][3]
		for x := 0; x < 2; x++ {
			for a := 0; a < 4; a++ {
				for i := 0; i < 8; i++ {
					col[i] = h[(oy+i)*32+ox+4*x+a]
				}
				fdct1d(col[:8], scr[:])
				for ky := 0; ky < 8; ky++ {
					out[(x+2*a)*8+ky] = col[ky] * inv
				}
			}
			dc[x] = out[x*8]
		}
	}
	out[0] = (dc[0] + dc[1]) / 2
	out[8] = (dc[0] - dc[1]) / 2
}

// coef is fdctRect of the vt block at pixel offset (ox, oy) of the cell.
func (r *rowDCTCache) coef(c int, vt vtype, ox, oy int, out []float32) {
	rows, cols := vt.rows(), vt.cols()
	li := bits.TrailingZeros(uint(cols / 8))
	h := &r.h[c][li]
	inv := 1 / float32(rows*cols)
	var col, scr [64]float32
	for kx := 0; kx < cols; kx++ {
		cl := col[:rows]
		for y := 0; y < rows; y++ {
			cl[y] = h[(oy+y)*32+ox+kx]
		}
		fdct1d(cl, scr[:])
		for ky, v := range cl {
			if rows < cols {
				out[ky*cols+kx] = v * inv
			} else {
				out[kx*rows+ky] = v * inv
			}
		}
	}
}

// fdctSplit computes the coefficients of a split 8x8 type in the decoder's
// storage layout: each half's scaled DCT is interleaved into the 8x8 block,
// with the mean of the half DCs at 0 and half their difference at 8.
func fdctSplit(acs int, src []float32, stride int, out []float32) {
	var h [2][32]float32
	if acs == 12 { // halves of 4 rows x 8 cols (natural layout [ky][kx])
		for y := 0; y < 2; y++ {
			fdctRect(src[y*4*stride:], stride, 4, 8, h[y][:])
			for ky := 0; ky < 4; ky++ {
				for kx := 0; kx < 8; kx++ {
					out[(y+2*ky)*8+kx] = h[y][ky*8+kx]
				}
			}
		}
	} else { // halves of 8 rows x 4 cols (transposed layout [kx][ky])
		for x := 0; x < 2; x++ {
			fdctRect(src[x*4:], stride, 8, 4, h[x][:])
			for a := 0; a < 4; a++ {
				for ky := 0; ky < 8; ky++ {
					out[(x+2*a)*8+ky] = h[x][a*8+ky]
				}
			}
		}
	}
	out[0] = (h[0][0] + h[1][0]) / 2
	out[8] = (h[0][0] - h[1][0]) / 2
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

// llfToDC derives the cy x cx block DC values (row-major) that reproduce
// the lowest frequencies of a varblock transform in the decoder.
func llfToDC(coef []float32, t vtype, dc []float32) {
	cx, cy := t.cx, t.cy
	if cx == 1 && cy == 1 {
		dc[0] = coef[0]
		return
	}
	// natural LLF L[ky][kx], unscaled
	var L [16]float32
	for ky := 0; ky < cy; ky++ {
		for kx := 0; kx < cx; kx++ {
			var v float32
			if cy < cx {
				v = coef[ky*t.cols()+kx]
			} else {
				v = coef[kx*t.rows()+ky]
			}
			L[ky*cx+kx] = v / (jxl.ResampleScale(cy, ky) * jxl.ResampleScale(cx, kx))
		}
	}
	// k-point scaled IDCT: x[i] = c0 + sqrt2 sum c_j cos(pi(2i+1)j/2k)
	idct := func(c []float32, out []float32) {
		k := len(c)
		for i := 0; i < k; i++ {
			v := float64(c[0])
			for j := 1; j < k; j++ {
				v += math.Sqrt2 * float64(c[j]) * math.Cos(math.Pi*float64((2*i+1)*j)/float64(2*k))
			}
			out[i] = float32(v)
		}
	}
	var tmp [16]float32
	var row, col, o [4]float32
	for ky := 0; ky < cy; ky++ {
		idct(L[ky*cx:ky*cx+cx], row[:cx])
		copy(tmp[ky*cx:], row[:cx])
	}
	for x := 0; x < cx; x++ {
		for ky := 0; ky < cy; ky++ {
			col[ky] = tmp[ky*cx+x]
		}
		idct(col[:cy], o[:cy])
		for y := 0; y < cy; y++ {
			dc[y*cx+x] = o[y]
		}
	}
}

// rdCost estimates distortion (in units of step^2, pixel domain via
// Parseval) and rate for a block's AC coefficients.
func rdCost(coef, mult []float32, order []int32, t vtype, step float32, cw float64) (float64, float64) {
	st := make([]float32, len(mult))
	inv := make([]float32, len(mult))
	for i, m := range mult {
		st[i] = m * step
		inv[i] = 1 / st[i]
	}
	return rdCostPre(coef, st, inv, order, t, step, cw)
}

// log2Tab[q] = 3.5 + 2*log2(q), the rate estimate of a non-zero |q|.
var log2Tab = func() (t [256]float64) {
	for q := 1; q < 256; q++ {
		t[q] = 3.5 + 2*math.Log2(float64(q))
	}
	return
}()

// rdCostPre is rdCost with precomputed quantisation steps (st) and their
// inverses (inv) per coefficient. The distortion is in pixel energy: a
// DCT's coefficient errors count size() times (Parseval for the scaled
// DCT); types made of smaller transforms weigh each coefficient by the
// pixel energy of its unit error (pixelWeights).
func rdCostPre(coef, st, inv []float32, order []int32, t vtype, step float32, cw float64) (float64, float64) {
	nllf, sz := t.cx*t.cy, t.size()
	var w []float32
	if t.split {
		w = pixelWeights(t.acs)
	}
	var r, d float64
	last := nllf
	for i := nllf; i < sz; i++ {
		p := order[i]
		v := coef[p]
		x := v * inv[p]
		var e float64
		if x < 0.6 && x > -0.6 {
			if w == nil {
				d += float64(v * v)
				continue
			}
			e = float64(v)
		} else {
			// sign-symmetric: work on magnitudes (same result as
			// math.Round and dequantAC on the signed values)
			if x < 0 {
				x, v = -x, -v
			}
			q := int32(float64(x) + 0.5)
			if q < 256 {
				e = float64(v - dqTab[q]*st[p])
				r += log2Tab[q]
			} else {
				e = float64(v - dequantAC(q, st[p], 0.95, 0.145))
				r += 3.5 + 2*math.Log2(float64(q))
			}
			last = i + 1
		}
		if w != nil {
			d += e * e * float64(w[p]) / float64(sz)
		} else {
			d += e * e
		}
	}
	r += 0.35*float64(last-nllf) + 5
	ref := float64(step) * refMult
	return d * float64(sz) * cw / (ref * ref), r
}

// splitWeights holds pixelWeights per AC strategy.
var splitWeights = func() (t [18][]float32) {
	for _, vt := range vtypes {
		if !vt.split {
			continue
		}
		w := make([]float32, 64)
		var c, px [64]float32
		for p := range w {
			c[p] = 1
			jxl.InverseTransform(vt.acs, c[:], px[:], 8)
			c[p] = 0
			var e float64
			for _, v := range px {
				e += float64(v) * float64(v)
			}
			w[p] = float32(e)
		}
		t[vt.acs] = w
	}
	return
}()

// pixelWeights returns the pixel energy of a unit error in each
// coefficient of an 8x8 block of a split type (64 for all of DCT8's).
func pixelWeights(acs int) []float32 { return splitWeights[acs] }

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
	writeModularLocalSamples(w, chs, effort, noWP, 1<<16)
}

// writeModularLocalSamples also sets the tree learning sample budget of
// noWP streams (more samples barely help these streams).
func writeModularLocalSamples(w *bitio.Writer, chs []channel, effort int, noWP bool, maxSamples int) {
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
			lp.maxSamples = maxSamples
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
	walk := func(fn func(ctx int, v uint32)) { walkStream(chs, s, root, fn) }
	data := newHistograms(nleaves, defaultCfg)
	if allZero {
		// the single leaf predicts 0 exactly: no need to walk the pixels
		walk = func(fn func(ctx int, v uint32)) {
			for range n {
				fn(0, 0)
			}
		}
	}
	var dc *ansCode
	if effort >= 4 {
		var cache residCache
		walk(func(ctx int, v uint32) {
			cache.add(ctx, v)
			data.add(ctx, v)
		})
		dc = writeANSCodeTunedCaches(w, []residCache{cache}, data.counts)
		ansWriteCache(w, dc, &cache)
		return
	}
	if allZero {
		data.counts[0][0] = uint64(n) // n zero tokens
	} else {
		walk(data.add)
	}
	dc = writeANSCode(w, data)
	if allZero && dc.tables[0].freq[0] == ansTab {
		// one symbol of probability 1: the state never changes and no
		// bits follow it
		w.Write(32, 0x130000)
		return
	}
	st := &ansStream{c: dc, toks: make([]ansToken, 0, n)}
	walk(st.add)
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
	// Like libjxl, efforts up to 4 use neither the inverse Gaborish filter
	// (nor, then, the decoder's) nor adaptive quantisation: with the
	// smaller transform sets both cost time and, measured with SSIMULACRA
	// 2, made files 1-15% larger at equal quality.
	if e := opt.Effort; e > 0 && e <= 4 {
		opt.NoGaborish, opt.LibAQ, opt.AQ = true, false, 0
	}
	bw, bh := (im.W+7)/8, (im.H+7)/8
	pw, ph := bw*8, bh*8
	model, err := im.colourModel()
	if err != nil {
		return nil, err
	}
	xyb := toXYB(im, pw, ph, model)
	// patches (effort 7, as libjxl): repeated text-like components go to a
	// reference frame and are subtracted before everything else
	var patches *patchPlan
	if e := opt.Effort; (e <= 0 || e >= 7) && !opt.NoPatches && !im.Alpha && opt.Frame.Anim == nil {
		patches = planPatches(xyb, im.W, im.H, pw, d)
	}
	// keep only the alpha plane of the input, so the colour planes can be
	// freed while encoding (when the caller drops its reference)
	{
		rest := *im
		rest.Chans = nil
		if im.Alpha {
			rest.Chans = [][]int32{im.Chans[len(im.Chans)-1]}
		}
		im = &rest
	}
	var aqMap []float32 // per-block quantisation multipliers (libjxl AQ)
	var aqMed float32
	if opt.LibAQ {
		aqMap = adaptiveQuantMap(xyb, pw, ph, d)
		tmp := append([]float32(nil), aqMap...)
		slices.Sort(tmp)
		aqMed = tmp[len(tmp)/2]
	}
	gab := !opt.NoGaborish
	if gab {
		for c := 0; c < 3; c++ {
			gaborishInverse(xyb[c], pw, ph)
		}
	}
	epfIters := opt.EPFIters
	if epfIters < 0 {
		// libjxl's rule: one iteration from d 0.7, two from 1.5, three
		// from 4 (BD-rate: text screenshot -2.7%, photos +0.1..0.3%)
		epfIters = 0
		for _, t := range []float32{0.7, 1.5, 4} {
			if d >= t {
				epfIters++
			}
		}
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
	var tmul [len(vtypes)][3][]float32
	var torder [len(vtypes)][]int32
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
	var selCoef [][]float32 // coefficients of the chosen varblocks
	if opt.forceType > 0 {  // tests: tile with one transform type
		ft := opt.forceType
		vt := vtypes[ft]
		for by := 0; by+vt.cy <= bh; by += vt.cy {
			for bx := 0; bx+vt.cx <= bw; bx += vt.cx {
				if (bx%32)+vt.cx > 32 || (by%32)+vt.cy > 32 {
					continue
				}
				for y := 0; y < vt.cy; y++ {
					for x := 0; x < vt.cx; x++ {
						i := (by+y)*bw + bx + x
						vtyp[i] = int8(ft)
						first[i] = x == 0 && y == 0
					}
				}
			}
		}
	} else if sel := selectionFor(opt); sel != (candidates{}) {
		smallMask := opt.SmallMask
		if smallMask == 0 {
			smallMask = 2
		}
		var smallMul [4]float64
		for i, m := range opt.SmallMul {
			smallMul[i] = m
			if m == 0 {
				smallMul[i] = 1.2
			}
		}
		splitMul := opt.SplitMul
		if splitMul == 0 {
			splitMul = 1.2
		}
		selCoef = make([][]float32, nb)
		var selSt, selInv [len(vtypes)][3][]float32
		for ti := range vtypes {
			for c := 0; c < 3; c++ {
				m := tmul[ti][c]
				selSt[ti][c] = make([]float32, len(m))
				selInv[ti][c] = make([]float32, len(m))
				for i, v := range m {
					selSt[ti][c][i] = v * baseStep
					selInv[ti][c][i] = 1 / (v * baseStep)
				}
			}
		}
		cw4, ch4 := (bw+3)/4, (bh+3)/4
		parallel(ch4, func(cy int) {
			var buf []float32
			var rc rowDCTCache
			for cx := 0; cx < cw4; cx++ {
				bx0, by0 := cx*4, cy*4
				for c := 0; c < 3; c++ {
					rc.compute(c, xyb[c], pw, ph, bx0, by0, sel.rowLens())
				}
				// cost is the RD cost of type ti at (bx, by), or +Inf once mul
				// times the cost of the channels done reaches bound: the
				// channel terms are non-negative, so the candidate cannot
				// then beat the plan costing bound (its comparison has the
				// same outcome). Y goes first (it decides most often); the
				// terms are summed in X, Y, B order, as without a bound.
				cost := func(ti, bx, by int, bound, mul float64) float64 {
					vt := vtypes[ti]
					if bx*8+vt.cols() > pw || by*8+vt.rows() > ph || bx+vt.cx > bw || by+vt.cy > bh {
						return math.Inf(1)
					}
					sz := vt.size()
					if len(buf) < sz {
						buf = make([]float32, sz)
					}
					var t [3]float64
					for k, c := range [3]int{1, 0, 2} {
						if ti >= vtIdent {
							fdct8Kind(vt.acs, xyb[c][(by*8)*pw+bx*8:], pw, buf[:sz])
						} else if vt.split {
							rc.coefSplit(c, vt.acs, (bx-bx0)*8, (by-by0)*8, buf[:sz])
						} else {
							rc.coef(c, vt, (bx-bx0)*8, (by-by0)*8, buf[:sz])
						}
						dd, rr := rdCostPre(buf[:sz], selSt[ti][c], selInv[ti][c], torder[ti], vt, baseStep, chanDistW[c])
						t[c] = dd/64 + mu*rr
						var part float64
						switch k {
						case 0:
							part = t[1]
						case 1:
							part = t[0] + t[1]
						default:
							return t[0] + t[1] + t[2]
						}
						if mul > 0 && part*mul >= bound {
							return math.Inf(1)
						}
					}
					return math.Inf(1) // not reached
				}
				set := func(ti, bx, by int) {
					vt := vtypes[ti]
					for y := 0; y < vt.cy; y++ {
						for x := 0; x < vt.cx; x++ {
							i := (by+y)*bw + bx + x
							vtyp[i] = int8(ti)
							first[i] = x == 0 && y == 0
						}
					}
				}
				// candidates per 2x2 quad: DCT16, or two tall (DCT16x8 /
				// 2 DCT8) or two wide (DCT8x16 / 2 DCT8) halves; per 4x4
				// cell: DCT32, or two tall (DCT32x16 / quads) or two wide
				// (DCT16x32 / quads) halves. DCT8 is the default.
				// a placement packs ti<<4 | (y-by0)<<2 | (x-bx0)
				type plan struct {
					cost float64
					n    int
					ps   [16]uint8
				}
				join := func(a, b plan) plan {
					a.cost += b.cost
					copy(a.ps[a.n:], b.ps[:b.n])
					a.n += b.n
					return a
				}
				better := func(a, b plan) plan {
					if b.cost < a.cost {
						return b
					}
					return a
				}
				// one costs a single varblock against the plan it is compared
				// with (at mul times its cost)
				one := func(ti, x, y int, rival plan, mul float64) plan {
					p := plan{cost: cost(ti, x, y, rival.cost, mul), n: 1}
					p.ps[0] = uint8(ti<<4 | (y-by0)<<2 | (x - bx0))
					return p
				}
				// or replaces a plan by a single varblock if that costs less
				or := func(p plan, ti, x, y int) plan { return better(p, one(ti, x, y, p, 1)) }
				c8 := func(x, y int) plan {
					if x >= bw || y >= bh {
						return plan{}
					}
					p := plan{cost: cost(0, x, y, math.Inf(1), 1)}
					if sel.split {
						for _, ti := range [2]int{vtDCT4x8, vtDCT8x4} {
							q := one(ti, x, y, p, splitMul)
							q.cost *= splitMul
							p = better(p, q)
						}
					}
					if sel.small {
						for ti := vtIdent; ti <= vtAFV3; ti++ {
							if fam := min(ti-vtIdent, 3); smallMask&(1<<fam) != 0 {
								q := one(ti, x, y, p, smallMul[fam])
								q.cost *= smallMul[fam]
								p = better(p, q)
							}
						}
					}
					return p
				}
				quad := func(sx, sy int) plan {
					if sx >= bw || sy >= bh {
						return plan{}
					}
					a, b, c, e := c8(sx, sy), c8(sx+1, sy), c8(sx, sy+1), c8(sx+1, sy+1)
					if !sel.rect {
						p := join(join(a, b), join(c, e))
						if sel.dct16 {
							p = or(p, 1, sx, sy)
						}
						return p
					}
					left := or(join(a, c), 3, sx, sy)
					right := or(join(b, e), 3, sx+1, sy)
					top := or(join(a, b), 4, sx, sy)
					bottom := or(join(c, e), 4, sx, sy+1)
					p := better(join(left, right), join(top, bottom))
					return or(p, 1, sx, sy)
				}
				q := [4]plan{quad(bx0, by0), quad(bx0+2, by0), quad(bx0, by0+2), quad(bx0+2, by0+2)}
				all := join(join(q[0], q[1]), join(q[2], q[3]))
				best := all
				if sel.dct32 {
					best = or(all, 2, bx0, by0)
				}
				if sel.rect32 {
					left := or(join(q[0], q[2]), 5, bx0, by0)
					right := or(join(q[1], q[3]), 5, bx0+2, by0)
					top := or(join(q[0], q[1]), 6, bx0, by0)
					bottom := or(join(q[2], q[3]), 6, bx0, by0+2)
					best = better(better(join(left, right), join(top, bottom)), best)
				}
				// keep the chosen coefficients for pass A
				keep := func(ti, bx, by int) {
					vt := vtypes[ti]
					sz := vt.size()
					cc := make([]float32, 3*sz)
					for c := 0; c < 3; c++ {
						o := cc[c*sz : (c+1)*sz]
						if ti >= vtIdent {
							fdct8Kind(vt.acs, xyb[c][(by*8)*pw+bx*8:], pw, o)
						} else if vt.split {
							rc.coefSplit(c, vt.acs, (bx-bx0)*8, (by-by0)*8, o)
						} else {
							rc.coef(c, vt, (bx-bx0)*8, (by-by0)*8, o)
						}
					}
					selCoef[by*bw+bx] = cc
				}
				var covered [16]bool
				for _, pk := range best.ps[:best.n] {
					ti, px, py := int(pk>>4), bx0+int(pk&3), by0+int(pk>>2&3)
					set(ti, px, py)
					keep(ti, px, py)
					vt := vtypes[ti]
					for y := 0; y < vt.cy; y++ {
						for x := 0; x < vt.cx; x++ {
							covered[(py-by0+y)*4+px-bx0+x] = true
						}
					}
				}
				for y := 0; y < 4 && by0+y < bh; y++ {
					for x := 0; x < 4 && bx0+x < bw; x++ {
						if !covered[y*4+x] {
							keep(0, bx0+x, by0+y)
						}
					}
				}
			}
		})
	}

	// when selection kept every varblock's coefficients, pass A does not
	// read the XYB planes: release them before it allocates its outputs
	if selCoef != nil {
		all := true
		for b, f := range first {
			if f && selCoef[b] == nil {
				all = false
				break
			}
		}
		if all {
			xyb = [3][]float32{}
		}
	}

	// --- pass A: coefficients, AQ, DC (from LLF), Y AC, CfL statistics
	qdc := [3][]int32{make([]int32, nb), make([]int32, nb), make([]int32, nb)}
	bqf := make([]int32, nb)
	vq := make([][]int32, nb)
	vc := make([][2][]float32, nb) // cached X and B coefficients per varblock
	aqStrength := float32(opt.AQ)
	tw := (bw + 7) / 8
	thh := (bh + 7) / 8
	rowSums := make([][]float64, bh)
	// coefOf returns the block's coefficients: the cached ones from AC
	// strategy selection, or computed into scratch
	coefOf := func(ti, bx, by int, out, scratch *[3][]float32) {
		vt := vtypes[ti]
		sz := vt.size()
		if selCoef != nil && selCoef[by*bw+bx] != nil {
			cc := selCoef[by*bw+bx]
			selCoef[by*bw+bx] = nil
			for c := 0; c < 3; c++ {
				out[c] = cc[c*sz : (c+1)*sz : (c+1)*sz]
			}
			return
		}
		for c := 0; c < 3; c++ {
			if len(scratch[c]) < sz {
				scratch[c] = make([]float32, sz)
			}
			out[c] = scratch[c][:sz]
			if vt.split {
				fdct8Kind(vt.acs, xyb[c][(by*8)*pw+bx*8:], pw, out[c])
			} else {
				fdctRect(xyb[c][(by*8)*pw+bx*8:], pw, vt.rows(), vt.cols(), out[c])
			}
		}
	}
	isLLF := func(ti, idx int) bool { return vtypes[ti].isLLF(idx) }
	stepsOf := func(b int) [3]float32 {
		bs := float32(65536.0) / float32(gs) / float32(bqf[b])
		return [3]float32{bs * chanScale[0], bs * chanScale[1], bs * chanScale[2]}
	}
	parallel(bh, func(by int) {
		var coef, scratch [3][]float32
		sums := make([]float64, 3*tw)
		// the outputs of the varblocks starting in this row come from one
		// slab each, not from three allocations per block
		total := 0
		for bx := 0; bx < bw; bx++ {
			if b := by*bw + bx; first[b] {
				total += vtypes[vtyp[b]].size()
			}
		}
		qslab, cslab := make([]int32, 3*total), make([]float32, 2*total)
		for bx := 0; bx < bw; bx++ {
			b := by*bw + bx
			if !first[b] {
				continue
			}
			ti := int(vtyp[b])
			vt := vtypes[ti]
			cxb, cyb, nn := vt.cx, vt.cy, vt.size()
			coefOf(ti, bx, by, &coef, &scratch)
			bq := qf
			if aqMap != nil {
				// AdjustQuantField: max over the covered blocks, mixed
				// with the mean for large transforms at high distances
				mx, mean := float32(0), float32(0)
				for iy := 0; iy < cyb; iy++ {
					for ix := 0; ix < cxb; ix++ {
						v := aqMap[(by+iy)*bw+bx+ix]
						mx = max(mx, v)
						mean += v
					}
				}
				mean /= float32(cxb * cyb)
				if cxb*cyb >= 4 {
					mix := float32(1)
					if d > 1.54138 {
						mix = max(1-(d-1.54138)*0.56391, 0)
					}
					mx = mx*mix + (1-mix)*mean
				}
				bq = int32(math.Round(float64(float32(qf) * mx / aqMed)))
				bq = min(max(bq, 1), 256)
			} else if aqStrength > 0 {
				var act float32
				for i := 0; i < nn; i++ {
					if !isLLF(ti, i) {
						act += abs32(coef[1][i])
					}
				}
				act /= float32(cxb * cyb)
				m := float32(math.Pow(float64(0.02/(act+0.02)), float64(aqStrength)))
				bq = int32(math.Round(float64(float32(qf) * min(max(m*1.6, 0.5), 2.5))))
				bq = min(max(bq, 1), 256)
			}
			bqf[b] = bq
			steps := stepsOf(b)
			// DC values per covered block from the LLF coefficients
			var dcv [3][16]float32
			for c := 0; c < 3; c++ {
				llfToDC(coef[c], vt, dcv[c][:cxb*cyb])
			}
			for iy := 0; iy < cyb; iy++ {
				for ix := 0; ix < cxb; ix++ {
					i := (by+iy)*bw + bx + ix
					j := iy*cxb + ix
					qy := int32(math.Round(float64(dcv[1][j] / mulDC[1])))
					qdc[1][i] = qy
					qdc[0][i] = int32(math.Round(float64(dcv[0][j] / mulDC[0])))
					qdc[2][i] = int32(math.Round(float64((dcv[2][j] - float32(qy)*mulDC[1]) / mulDC[2])))
				}
			}
			q := qslab[: 3*nn : 3*nn]
			qslab = qslab[3*nn:]
			mY := tmul[ti][1]
			t := bx / 8
			for i := 0; i < nn; i++ {
				if isLLF(ti, i) {
					continue
				}
				st := mY[i] * steps[1]
				q[nn+i] = quantAC(coef[1][i], st)
				yd := float64(dequantAC(q[nn+i], st, biases[1], numer))
				sums[3*t] += float64(coef[0][i]) * yd
				sums[3*t+1] += float64(coef[2][i]) * yd
				sums[3*t+2] += yd * yd
			}
			vq[b] = q
			// copies, so the (larger) cached block can be freed
			xc, bc := cslab[:nn:nn], cslab[nn:2*nn:2*nn]
			cslab = cslab[2*nn:]
			copy(xc, coef[0])
			copy(bc, coef[2])
			vc[b] = [2][]float32{xc, bc}
		}
		rowSums[by] = sums
	})
	// the XYB planes and selection results are no longer needed
	xyb, selCoef = [3][]float32{}, nil
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
			nn := vtypes[ti].size()
			coef[0], coef[2] = vc[b][0], vc[b][1]
			steps := stepsOf(b)
			tix := (by/8)*tw + bx/8
			kx := float32(ytox[tix]) / 84
			kb := 1 + float32(ytob[tix])/84
			q := vq[b]
			mm := tmul[ti]
			for i := 0; i < nn; i++ {
				if isLLF(ti, i) {
					continue
				}
				yd := dequantAC(q[nn+i], mm[1][i]*steps[1], biases[1], numer)
				q[i] = quantAC(coef[0][i]-kx*yd, mm[0][i]*steps[0])
				q[2*nn+i] = quantAC(coef[2][i]-kb*yd, mm[2][i]*steps[2])
			}
			vc[b] = [2][]float32{}
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
				nbk := int32(vt.cx * vt.cy)
				lg := uint(bits.TrailingZeros32(uint32(nbk)))
				nn := vt.size()
				ord := torder[ti]
				for i, c := range chanOrder {
					q := vq[b][c*nn : (c+1)*nn]
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
					for k := int(nbk); k < nn; k++ {
						if q[ord[k]] != 0 {
							nz++
						}
					}
					emit(bctx+int(pidx)*numBlockCtx, uint32(nz))
					nzv := (nz + nbk - 1) >> lg
					for dx := 0; dx < vt.cx; dx++ {
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
					for k := int(nbk); k < nn && nz > 0; k++ {
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
	nw := min(runtime.GOMAXPROCS(0), ngroups)
	hs := make([]*histograms, nw) // per worker: counts are order-free
	// the tokens are kept (4 bytes each) for the writer
	hfToks := make([]residCache, ngroups)
	parallelWorkers(ngroups, nw, func(k, g int) {
		if hs[k] == nil {
			hs[k] = newHistograms(nctx, defaultCfg)
		}
		h, c := hs[k], &hfToks[g]
		emitGroup(g, func(ctx int, v uint32) {
			c.add(ctx, v)
			h.add(ctx, v)
		})
	})
	hf := mergeWorkerHists(hs)

	// --- alpha as a lossless modular extra channel
	var alpha *alphaPlan
	if im.Alpha {
		alpha = planAlpha(im.Chans[len(im.Chans)-1], im.W, im.H, xg, yg)
	}

	// --- section writers
	writeLfGlobal := func(w *bitio.Writer) {
		if patches != nil {
			patches.writeDict(w)
		}
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
		var wm bitio.Writer // the metadata stream, written concurrently
		wm.Write(uint(ceilLog2(cw*chh)), uint64(count-1))
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
		done := make(chan struct{})
		go func() {
			writeModularLocalOpts(&wm, []channel{cx, cb, acs, sh}, 4, true)
			close(done)
		}()
		// learned trees (without the weighted predictor) cost far less
		// than the fixed tree's many contexts, notably for small images
		// the fastest efforts learn the DC tree from fewer samples
		lfSamples := 1 << 16
		if opt.Effort > 0 && opt.Effort <= 2 {
			lfSamples = 1 << 14
		}
		writeModularLocalSamples(w, lf, 4, true, lfSamples)
		<-done
		w.AppendWriter(&wm)
	}
	writeHfGlobal := func(w *bitio.Writer) *ansCode {
		w.WriteBool(true)                                           // default dequant matrices
		w.Write(uint(ceilLog2(ngroups)), 0)                         // one HF preset
		w.WriteU32(0, val(0x5F), val(0x13), val(0), bitio.Bits(13)) // natural orders
		return writeANSCodeClustered(w, defaultCfg, hf.counts, 64)
	}
	writeGroup := func(w *bitio.Writer, g int, hc *ansCode) {
		c := &hfToks[g]
		ansWriteCache(w, hc, c)
		hfToks[g] = residCache{}
		if alpha != nil {
			alpha.writeGroup(w, g)
		}
	}

	// --- headers
	out := &bitio.Writer{}
	fo := opt.Frame
	if !fo.NoImageHeader {
		out.Write(16, 0x0aff)
		writeSize(out, im.W, im.H)
		// grey samples keep a grey colour space only with a description
		// (sRGB grey stays the RGB default the decoder renders)
		plain := im.defaultColour()
		if alpha == nil && fo.Anim == nil && plain && im.BitDepth == 8 && im.Orientation <= 1 {
			out.WriteBool(true) // metadata all default: 8-bit sRGB, XYB
		} else {
			out.WriteBool(false) // metadata
			extra := writeExtraFields(out, im.Orientation, fo.Anim, im.intensityTarget() != 0)
			writeBitDepth(out, im.BitDepth)
			// HDR light makes larger LF values
			out.WriteBool(im.BitDepth <= 12 && im.intensityTarget() == 0) // 16-bit buffers suffice
			if alpha != nil {
				out.WriteU32(1, val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
				writeAlphaInfo(out, im.BitDepth)
			} else {
				out.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
			}
			out.WriteBool(true) // xyb
			writeColour(out, im, im.Grey && !plain)
			if extra {
				writeToneMapping(out, im)
			}
			out.WriteU64(0) // extensions
		}
		out.WriteBool(true) // default transform data
		writeICCIfAny(out, im)
		out.ZeroPadToByte()
	}
	if patches != nil {
		out.AppendBytes(patches.frame)
	}
	out.WriteBool(false)
	out.Write(2, 0)       // regular
	out.Write(1, 0)       // VarDCT
	flags := uint64(0x80) // skip adaptive LF smoothing
	if patches != nil {
		flags |= 2
	}
	out.WriteU64(flags)
	out.WriteU32(1, val(1), val(2), val(4), val(8))
	nec := 0
	if alpha != nil {
		nec = 1
		out.WriteU32(1, val(1), val(2), val(4), val(8)) // ec upsampling
	}
	out.Write(3, 3) // x_qm_scale
	out.Write(3, 2) // b_qm_scale
	out.WriteU32(1, val(1), val(2), val(3), bitsOff(3, 4))
	fo.writeCropBlend(out, im.W, im.H, nec)
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
	p.global = stream{w: 1 << 30, h: 1 << 30}
	if W <= groupDim && H <= groupDim {
		p.global.chans, p.global.ctxIdx = []int{0}, []int{0}
	} else {
		// modular group streams of pass 0: 1 + 3*LF groups + 17 + g
		nlf := ((W + groupDim*8 - 1) / (groupDim * 8)) * ((H + groupDim*8 - 1) / (groupDim * 8))
		for gy := 0; gy < yg; gy++ {
			for gx := 0; gx < xg; gx++ {
				p.groups = append(p.groups, stream{id: 1 + 3*nlf + 17 + gy*xg + gx, chans: []int{0}, ctxIdx: []int{0},
					x0: gx * groupDim, y0: gy * groupDim, w: groupDim, h: groupDim})
			}
		}
	}
	// learned MA tree (as lossless effort 4, without WP)
	all := p.groups
	if len(all) == 0 {
		all = []stream{p.global}
	}
	lp := learnParamsFor(4)
	lp.preds = slices.DeleteFunc(slices.Clone(lp.preds), func(q int) bool { return q == 6 })
	root := learnTree(collectSamples(p.chs, all, lp), lp)
	var nl int
	p.nodes, nl = bfs(root)
	p.root = root
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
	p.code = writeANSCodeClustered(w, defaultCfg, p.data.counts, 64)
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
