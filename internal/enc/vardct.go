package enc

import (
	"errors"
	"math"

	"gojxl/internal/bitio"
	"gojxl/internal/jxl"
)

// LossyOptions controls VarDCT encoding.
type LossyOptions struct {
	// Distance is the target perceptual distance (1.0 ~ visually lossless).
	Distance float32
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

// writeModularLocal writes a self-contained modular stream (own tree).
func writeModularLocal(w *bitio.Writer, chs []channel) {
	w.WriteBool(false) // local tree
	w.WriteBool(true)  // default WP
	w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	n := 0
	for _, c := range chs {
		n += c.w * c.h
	}
	root := buildTree(len(chs), n, 2)
	nodes, nleaves := bfs(root)
	roots := channelRoots(root, len(chs))
	s := stream{w: 1 << 30, h: 1 << 30}
	for i := range chs {
		s.chans = append(s.chans, i)
		s.ctxIdx = append(s.ctxIdx, i)
	}
	treeH := newHistograms(6, defaultCfg)
	treeTokens(nodes, treeH.add)
	tc := writeCode(w, treeH)
	treeTokens(nodes, func(ctx int, v uint32) { tc.write(w, ctx, v) })
	data := newHistograms(nleaves, defaultCfg)
	walkStream(chs, s, roots, data.add)
	dc := writeANSCode(w, data)
	st := &ansStream{c: dc, toks: make([]ansToken, 0, n)}
	walkStream(chs, s, roots, st.add)
	st.flush(w)
}

// EncodeVarDCT encodes an 8/16-bit sRGB image lossily.
func EncodeVarDCT(im *Image, opt LossyOptions) ([]byte, error) {
	if im.W <= 0 || im.H <= 0 {
		return nil, errors.New("jxl: empty image")
	}
	if im.Alpha {
		return nil, errors.New("jxl: lossy alpha not supported yet")
	}
	d := opt.Distance
	if d <= 0 {
		d = 1
	}
	bw, bh := (im.W+7)/8, (im.H+7)/8
	pw, ph := bw*8, bh*8
	xyb := toXYB(im, pw, ph)

	// quantiser parameters
	const gs = 2048
	qf := int32(math.Round(float64(18 / d)))
	qf = min(max(qf, 1), 256)
	qlf := int(math.Round(float64(64 / d)))
	qlf = min(max(qlf, 16), 1024)
	scale := float32(65536.0) / float32(gs) / float32(qf)
	steps := [3]float32{scale * 0.8, scale, scale} // x_qm_scale 3, b_qm_scale 2
	lfDeq := [3]float32{1.0 / 32 / 128, 1.0 / 4 / 128, 1.0 / 2 / 128}
	var mulDC [3]float32
	for c := 0; c < 3; c++ {
		mulDC[c] = float32(65536.0/float64(gs)/float64(qlf)) * lfDeq[c]
	}
	mults := jxl.DCT8Multipliers()
	op := jxl.OpsinInverseDefault()
	biases, numer := op.QuantBias, op.QuantBiasNumerator

	// per block: quantised DC (3) and AC coefficients (3x64)
	nb := bw * bh
	qdc := [3][]int32{make([]int32, nb), make([]int32, nb), make([]int32, nb)}
	qac := make([]int32, nb*192)
	parallel(bh, func(by int) {
		var blk, s [64]float32
		var coef [3][64]float32
		for bx := 0; bx < bw; bx++ {
			b := by*bw + bx
			for c := 0; c < 3; c++ {
				p := xyb[c]
				for y := 0; y < 8; y++ {
					copy(blk[y*8:y*8+8], p[(by*8+y)*pw+bx*8:])
				}
				fdct8x8(&blk, &s)
				coef[c] = s
			}
			// DC: Y, X direct; B relative to dequantised Y
			qy := int32(math.Round(float64(coef[1][0] / mulDC[1])))
			yDC := float32(qy) * mulDC[1]
			qdc[1][b] = qy
			qdc[0][b] = int32(math.Round(float64(coef[0][0] / mulDC[0])))
			qdc[2][b] = int32(math.Round(float64((coef[2][0] - yDC) / mulDC[2])))
			// AC: Y first (B is coded relative to dequantised Y)
			q := qac[b*192 : b*192+192]
			var yDeq [64]float32
			for k := 1; k < 64; k++ {
				st := mults[1][k] * steps[1]
				q[64+k] = quantAC(coef[1][k], st)
				yDeq[k] = dequantAC(q[64+k], st, biases[1], numer)
			}
			for k := 1; k < 64; k++ {
				st := mults[0][k] * steps[0]
				q[k] = quantAC(coef[0][k], st)
				st = mults[2][k] * steps[2]
				q[128+k] = quantAC(coef[2][k]-yDeq[k], st)
			}
		}
	})

	gd := groupDim / 8 // blocks per group
	xg, yg := (bw+gd-1)/gd, (bh+gd-1)/gd
	ngroups := xg * yg
	lfd := groupDim // blocks per LF group
	xlf, ylf := (bw+lfd-1)/lfd, (bh+lfd-1)/lfd
	nlf := xlf * ylf

	order := jxl.NaturalOrderDCT8()
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
			nzRow[c] = make([]int32, gd)
		}
		for by := by0; by < by1; by++ {
			for bx := bx0; bx < bx1; bx++ {
				b := by*bw + bx
				for i, c := range chanOrder {
					q := qac[b*192+c*64 : b*192+c*64+64]
					bctx := int(bctxMap[i*13])
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
					for k := 1; k < 64; k++ {
						if q[order[k]] != 0 {
							nz++
						}
					}
					emit(bctx+int(pidx)*numBlockCtx, uint32(nz))
					nzRow[c][sx] = nz
					if nz == 0 {
						continue
					}
					base := bctx*458 + 37*numBlockCtx
					prev := 0
					if nz <= 4 {
						prev = 1
					}
					for k := 1; k < 64 && nz > 0; k++ {
						v := q[order[k]]
						ctx := base + (int(nzCtx[nz-1])+int(freqCtx[k-1]))*2 + prev
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

	// --- section writers
	writeLfGlobal := func(w *bitio.Writer) {
		w.WriteBool(true) // LF channel dequant default
		w.WriteU32(gs, bitsOff(11, 1), bitsOff(11, 2049), bitsOff(12, 4097), bitsOff(16, 8193))
		w.WriteU32(uint32(qlf), val(16), bitsOff(5, 1), bitsOff(8, 1), bitsOff(16, 1))
		w.WriteBool(true)  // default block context map
		w.WriteBool(true)  // default LF channel correlation
		w.WriteBool(false) // no global tree
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
		// HF metadata: all DCT8, constant quant field, no CfL, no sharpness
		count := cw * chh
		w.Write(uint(ceilLog2(count)), uint64(count-1))
		tw, th := (cw+7)/8, (chh+7)/8
		acs := channel{w: count, h: 2, pix: make([]int32, 2*count)}
		for i := 0; i < count; i++ {
			acs.pix[count+i] = qf - 1
		}
		writeModularLocal(w, []channel{
			{w: tw, h: th, pix: make([]int32, tw*th)},
			{w: tw, h: th, pix: make([]int32, tw*th)},
			acs,
			{w: cw, h: chh, pix: make([]int32, cw*chh)},
		})
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
	}

	// --- headers
	out := &bitio.Writer{}
	out.Write(16, 0x0aff)
	writeSize(out, im.W, im.H)
	out.WriteBool(true) // metadata all default: 8-bit sRGB, XYB
	out.WriteBool(true) // default transform data
	out.ZeroPadToByte()
	out.WriteBool(false)
	out.Write(2, 0)    // regular
	out.Write(1, 0)    // VarDCT
	out.WriteU64(0x80) // skip adaptive LF smoothing
	out.WriteU32(1, val(1), val(2), val(4), val(8))
	out.Write(3, 3) // x_qm_scale
	out.Write(3, 2) // b_qm_scale
	out.WriteU32(1, val(1), val(2), val(3), bitsOff(3, 4))
	out.WriteBool(false) // no crop
	out.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3))
	out.WriteBool(true)                                                     // is_last
	out.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48)) // name
	out.WriteBool(false)                                                    // restoration not default
	out.WriteBool(false)                                                    // no gaborish
	out.Write(2, 0)                                                         // no EPF
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
