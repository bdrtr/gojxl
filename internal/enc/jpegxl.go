package enc

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"os"
	"sort"

	"gojxl/internal/bitio"
	"gojxl/internal/jpeg"
	"gojxl/internal/jxl"
)

// Lossless JPEG recompression: the quantised DCT coefficients of a JPEG
// are stored as a VarDCT frame (DCT8 only, YCbCr, JPEG quantisation tables
// as RAW dequantisation matrices), so that decoding yields exactly the
// image a JPEG decoder would reconstruct from the same coefficients.

// writeF16 writes a half float exactly as libjxl does (truncating).
func writeF16(w *bitio.Writer, v float32) {
	b := math.Float32bits(v)
	sign := b >> 31
	exp := int32((b>>23)&0xff) - 127
	mant := b & 0x7fffff
	if exp < -24 {
		w.Write(16, 0)
		return
	}
	var e16, m16 uint32
	if exp < -14 {
		sub := uint32(-14 - exp)
		m16 = (1 << (10 - sub)) + (mant >> (13 + sub))
	} else {
		e16 = uint32(exp + 15)
		m16 = mant >> 13
	}
	w.Write(16, uint64(sign<<15|e16<<10|m16))
}

// jpegUpsamplingCode maps a component's sampling factors to the frame
// header's jpeg_upsampling value.
func jpegUpsamplingCode(h, v int) uint64 {
	switch {
	case h == 2 && v == 2:
		return 1
	case h == 2:
		return 2
	case v == 2:
		return 3
	}
	return 0
}

// EncodeJPEG recompresses a parsed JPEG into a JPEG XL codestream.
func EncodeJPEG(j *jpeg.Image) ([]byte, error) {
	grey := len(j.Comps) == 1
	if !grey && len(j.Comps) != 3 {
		return nil, fmt.Errorf("%w: %d components", jpeg.ErrUnsupported, len(j.Comps))
	}
	// colour transform as libjxl decides it
	rgb := j.RGB()
	// JPEG XL channel c <- JPEG component jc[c]
	jc := [3]int{1, 0, 2} // YCbCr: (Cb, Y, Cr)
	switch {
	case grey:
		jc = [3]int{0, 0, 0}
	case rgb:
		jc = [3]int{0, 1, 2}
	}
	var hs, vs [3]int
	var upCode [3]uint64
	maxHS, maxVS := 0, 0
	for c := 0; c < 3; c++ {
		cp := &j.Comps[jc[c]]
		if j.Hmax%cp.H != 0 || j.Vmax%cp.V != 0 || j.Hmax/cp.H > 2 || j.Vmax/cp.V > 2 || cp.H > 2 || cp.V > 2 {
			return nil, fmt.Errorf("%w: sampling factors", jpeg.ErrUnsupported)
		}
		hs[c] = bits.TrailingZeros(uint(j.Hmax / cp.H))
		vs[c] = bits.TrailingZeros(uint(j.Vmax / cp.V))
		maxHS, maxVS = max(maxHS, hs[c]), max(maxVS, vs[c])
		upCode[c] = jpegUpsamplingCode(cp.H, cp.V)
		if rgb && (hs[c] != 0 || vs[c] != 0) {
			return nil, fmt.Errorf("%w: subsampled RGB JPEG", jpeg.ErrUnsupported)
		}
		if !j.QuantUsed[cp.Tq] {
			return nil, errors.New("jpeg: missing quantisation table")
		}
	}
	W, H := j.W, j.H
	bw := (W + (8 << maxHS) - 1) / (8 << maxHS) << maxHS
	bh := (H + (8 << maxVS) - 1) / (8 << maxVS) << maxVS
	cbw := [3]int{bw >> hs[0], bw >> hs[1], bw >> hs[2]}
	for c := 0; c < 3; c++ {
		cp := &j.Comps[jc[c]]
		if cp.BW < cbw[c] || cp.BH < bh>>vs[c] {
			return nil, errors.New("jpeg: coefficient planes too small")
		}
	}
	var zero [64]int16
	block := func(c, bx, by int) []int16 {
		if grey && c != 1 {
			return zero[:]
		}
		cp := &j.Comps[jc[c]]
		o := (by*cp.BW + bx) * 64
		return cp.Coef[o : o+64]
	}

	// Chroma from luma (4:4:4 colour only), as libjxl does it: per 64x64
	// tile, the integer factor that makes the most chroma coefficients
	// cancel; applied in exact fixed point so it is reversible.
	cfl := !grey && maxHS == 0 && maxVS == 0
	tw, th := (bw+7)/8, (bh+7)/8
	var cmap [3][]int32 // per tile factor for channels 0 and 2
	var sq [3][64]int32 // (2^11 * qY / qC) per natural position
	if cfl {
		qY := &j.Quant[j.Comps[jc[1]].Tq]
		for _, c := range []int{0, 2} {
			qC := &j.Quant[j.Comps[jc[c]].Tq]
			for n := 0; n < 64; n++ {
				r := (int32(1) << 11) * int32(qY[n]) / int32(qC[n])
				if r > 256<<11-1 {
					cfl = false
				}
				sq[c][n] = r
			}
		}
	}
	if cfl {
		const kScale, kOffset = 84.0, 127
		zeroThresh := float32(kScale * 0.5 * 0.9999)
		for _, c := range []int{0, 2} {
			cmap[c] = make([]int32, tw*th)
			parallel(th, func(ty int) {
				for tx := 0; tx < tw; tx++ {
					var dz [257]int32
					for by := ty * 8; by < min(bh, ty*8+8); by++ {
						for bx := tx * 8; bx < min(bw, tx*8+8); bx++ {
							ym, cs := block(1, bx, by), block(c, bx, by)
							for n := 1; n < 64; n++ {
								m := float32(ym[n]) * (1.0 / 2048) * float32(sq[c][n])
								s := kScale*float32(cs[n]) + kOffset*m
								if m > 1e-8 || m < -1e-8 {
									var from, to float32
									if m > 0 {
										from, to = (s-zeroThresh)/m, (s+zeroThresh)/m
									} else {
										from, to = (s+zeroThresh)/m, (s-zeroThresh)/m
									}
									from = max(from, 0)
									to = min(to, 255)
									if from <= to {
										dz[int(math.Ceil(float64(from)))]++
										dz[int(math.Floor(float64(to+1)))]--
									}
								}
							}
						}
					}
					var maxv, v int32
					b0, b1 := 0, 0
					for i := 0; i < 256; i++ {
						v += dz[i]
						if v > maxv {
							maxv, b0 = v, i
						}
						if v == maxv {
							b1 = i
						}
					}
					best := (b0 + b1 + 1) >> 1
					var off int32
					for i := 0; i <= kOffset; i++ {
						off += dz[i]
					}
					if maxv > off+1 {
						cmap[c][ty*tw+tx] = int32(best - kOffset)
					}
				}
			})
		}
	}
	// acBlock returns the AC coefficients of a block of channel c (in
	// channel block coordinates) in the decoder's transposed layout, with
	// chroma from luma removed.
	acBlock := func(c, bx, by int, q *[64]int32) {
		src := block(c, bx, by)
		if !cfl || c == 1 || cmap[c][(by/8)*tw+bx/8] == 0 {
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					q[x*8+y] = int32(src[y*8+x])
				}
			}
			return
		}
		scale := cmap[c][(by/8)*tw+bx/8] * (1 << 11) / 84
		ys := block(1, bx, by)
		for n := 0; n < 64; n++ {
			cs := (scale*sq[c][n] + 1<<10) >> 11
			f := (int32(ys[n])*cs + 1<<10) >> 11
			q[(n%8)*8+n/8] = int32(src[n]) - f
		}
	}

	// dcValue is the quantised DC as stored in the LF image.
	dcValue := func(c, bx, by int) int32 {
		v := int32(block(c, bx, by)[0])
		if rgb {
			v += 1024 / int32(j.Quant[j.Comps[jc[c]].Tq][0])
		}
		return v
	}

	gd := groupDim / 8
	xg, yg := (bw+gd-1)/gd, (bh+gd-1)/gd
	ngroups := xg * yg
	lfd := groupDim
	xlf, ylf := (bw+lfd-1)/lfd, (bh+lfd-1)/lfd
	nlf := xlf * ylf

	nzCtx := jxl.CoeffNumNonzeroCtx()
	freqCtx := jxl.CoeffFreqCtx()

	// Block context map as libjxl builds it for JPEG input: buckets of luma
	// DC (more for larger, higher quality images); one context per luma
	// bucket, chroma contexts per pair of buckets.
	var dcCounts [2048]int
	total := 0
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			dcCounts[min(max(int(dcValue(1, bx, by))+1024, 0), 2047)]++
			total++
		}
	}
	cbT := &j.Quant[j.Comps[jc[0]].Tq]
	qsum := 0
	for y := 1; y <= 5; y++ {
		qsum += int(cbT[8*y]) // transposed entries 1..5 of channel 0
	}
	nth := ceilLog2(total) - ceilLog2(qsum) - 7
	nth = min(max(nth, 1), 7)
	var dcTh []int32
	cum, cut := 0, total/(nth+1)
	for k := 0; k < 2048; k++ {
		cum += dcCounts[k]
		if cum > cut {
			dcTh = append(dcTh, int32(k-1025))
			cut = total * (len(dcTh) + 1) / (nth + 1)
		}
	}
	numDC := len(dcTh) + 1
	ctxMap := make([]uint8, 3*13*numDC)
	for i := 0; i < numDC; i++ {
		ctxMap[i] = uint8(i)
		if grey {
			ctxMap[13*numDC+i] = uint8(numDC)
			ctxMap[2*13*numDC+i] = uint8(numDC)
			continue
		}
		ctxMap[13*numDC+i] = uint8(numDC + i/2)
		ctxMap[2*13*numDC+i] = uint8(numDC + (numDC-1)/2 + 1 + i/2)
	}
	numCtx := 0
	for _, v := range ctxMap {
		numCtx = max(numCtx, int(v)+1)
	}
	dcIdx := func(bx, by int) int {
		v := dcValue(1, bx, by)
		n := 0
		for _, t := range dcTh {
			if v > t {
				n++
			}
		}
		return n
	}
	nat := jxl.NaturalOrder(0)
	chanOrder := [3]int{1, 0, 2}

	// Coefficient order per channel: positions sorted by how often they
	// are non-zero (the DC position stays first).
	var orders [3][]int32
	{
		var nzc [3][64]int
		parallel(3, func(c int) {
			cbh := bh >> vs[c]
			for by := 0; by < cbh; by++ {
				for bx := 0; bx < cbw[c]; bx++ {
					var q [64]int32
					acBlock(c, bx, by, &q)
					for k, v := range q {
						if v != 0 {
							nzc[c][k]++
						}
					}
				}
			}
		})
		for c := 0; c < 3; c++ {
			o := append([]int32(nil), nat...)
			cnt := nzc[c]
			sort.SliceStable(o[1:], func(a, b int) bool { return cnt[o[1+a]] > cnt[o[1+b]] })
			orders[c] = o
		}
	}

	emitGroup := func(g int, emit func(ctx int, v uint32)) {
		gx, gy := g%xg, g/xg
		bx0, by0 := gx*gd, gy*gd
		bx1, by1 := min(bx0+gd, bw), min(by0+gd, bh)
		var nzRow [3][]int32
		for c := range nzRow {
			nzRow[c] = make([]int32, (gd>>hs[c])+2)
		}
		var q [64]int32
		for by := by0; by < by1; by++ {
			for bx := bx0; bx < bx1; bx++ {
				for i, c := range chanOrder {
					lx, ly := bx-bx0, by-by0
					if (lx>>hs[c])<<hs[c] != lx || (ly>>vs[c])<<vs[c] != ly {
						continue
					}
					acBlock(c, bx>>hs[c], by>>vs[c], &q)
					bctx := int(ctxMap[i*13*numDC+dcIdx(bx, by)])
					sx, sy := lx>>hs[c], ly>>vs[c]
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
					order := orders[c]
					for k := 1; k < 64; k++ {
						if q[order[k]] != 0 {
							nz++
						}
					}
					emit(bctx+int(pidx)*numCtx, uint32(nz))
					nzRow[c][sx] = nz
					if nz == 0 {
						continue
					}
					base := bctx*458 + 37*numCtx
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

	nctx := 495 * numCtx
	hs2 := make([]*histograms, ngroups)
	caches := make([]residCache, ngroups)
	parallel(ngroups, func(g int) {
		h := newHistograms(nctx, defaultCfg)
		c := &caches[g]
		emitGroup(g, func(ctx int, v uint32) {
			c.ctx = append(c.ctx, uint16(ctx))
			c.v = append(c.v, v)
			h.add(ctx, v)
		})
		hs2[g] = h
	})
	replay := func(g int, fn func(ctx int, v uint32)) {
		c := &caches[g]
		for k, v := range c.v {
			fn(int(c.ctx[k]), v)
		}
	}
	hf := newHistograms(nctx, defaultCfg)
	for _, h := range hs2 {
		hf.merge(h)
	}

	// dequantisation: DC from the first quantiser entry, AC as a RAW table
	var dcq [3]float32
	for c := 0; c < 3; c++ {
		q0 := float32(j.Quant[j.Comps[jc[c]].Tq][0])
		dcq[c] = 1.0 / (255 * 8.0 / q0)
	}
	writeLfGlobal := func(w *bitio.Writer) {
		w.WriteBool(false) // custom LF channel dequantisation
		for c := 0; c < 3; c++ {
			writeF16(w, dcq[c]*128)
		}
		w.WriteU32(65536, bitsOff(11, 1), bitsOff(11, 2049), bitsOff(12, 4097), bitsOff(16, 8193))
		w.WriteU32(1, val(16), bitsOff(5, 1), bitsOff(8, 1), bitsOff(16, 1))
		w.WriteBool(false) // custom block context map
		for c := 0; c < 3; c++ {
			if c != 1 {
				w.Write(4, 0)
				continue
			}
			w.Write(4, uint64(len(dcTh)))
			for _, t := range dcTh {
				w.WriteU32(bitio.PackSigned(t), bitio.Bits(4), bitsOff(8, 16), bitsOff(16, 272), bitsOff(32, 65808))
			}
		}
		w.Write(4, 0) // no quant field thresholds
		writeContextMap(w, ctxMap, numCtx)
		w.WriteBool(false) // LF channel correlation: no chroma from luma
		w.WriteU32(84, val(84), val(256), bitsOff(8, 2), bitsOff(16, 258))
		writeF16(w, 0)
		writeF16(w, 0)
		w.Write(8, 128)
		w.Write(8, 128)
		w.WriteBool(false) // no global tree
	}
	writeLfGroup := func(w *bitio.Writer, g int) {
		gx, gy := g%xlf, g/xlf
		x0, y0 := gx*lfd, gy*lfd
		cw, chh := min(lfd, bw-x0), min(lfd, bh-y0)
		w.Write(2, 0) // extra precision
		var lf []channel
		for _, c := range chanOrder {
			ww, hh := cw>>hs[c], chh>>vs[c]
			ch := channel{w: ww, h: hh, pix: make([]int32, ww*hh)}
			for y := 0; y < hh; y++ {
				for x := 0; x < ww; x++ {
					ch.pix[y*ww+x] = dcValue(c, (x0>>hs[c])+x, (y0>>vs[c])+y)
				}
			}
			lf = append(lf, ch)
		}
		writeModularLocalOpts(w, lf, 4, true)
		count := cw * chh
		w.Write(uint(ceilLog2(cw*chh)), uint64(count-1))
		acs := channel{w: count, h: 2, pix: make([]int32, 2*count)} // DCT8, quant field 1
		ltw, lth := (cw+7)/8, (chh+7)/8
		cx := channel{w: ltw, h: lth, pix: make([]int32, ltw*lth)}
		cb := channel{w: ltw, h: lth, pix: make([]int32, ltw*lth)}
		if cfl {
			for y := 0; y < lth; y++ {
				for x := 0; x < ltw; x++ {
					t := (y0/8+y)*tw + x0/8 + x
					cx.pix[y*ltw+x] = cmap[0][t]
					cb.pix[y*ltw+x] = cmap[2][t]
				}
			}
		}
		sh := channel{w: cw, h: chh, pix: make([]int32, cw*chh)}
		writeModularLocal(w, []channel{cx, cb, acs, sh})
	}
	writeHfGlobal := func(w *bitio.Writer) *ansCode {
		w.WriteBool(false) // custom dequantisation matrices
		for i := 0; i < 17; i++ {
			if i != 0 {
				w.Write(3, 0) // library
				continue
			}
			w.Write(3, 7) // RAW
			writeF16(w, 1.0/(8*255))
			var qt []channel
			for c := 0; c < 3; c++ {
				t := &j.Quant[j.Comps[jc[c]].Tq]
				ch := channel{w: 8, h: 8, pix: make([]int32, 64)}
				for y := 0; y < 8; y++ {
					for x := 0; x < 8; x++ {
						ch.pix[x*8+y] = int32(t[y*8+x])
					}
				}
				qt = append(qt, ch)
			}
			writeModularLocal(w, qt)
		}
		w.Write(uint(ceilLog2(ngroups)), 0)                         // one HF preset
		w.WriteU32(1, val(0x5F), val(0x13), val(0), bitio.Bits(13)) // DCT8 order coded
		writeCoeffOrders(w, nat, orders[:])
		return writeANSCodeTuned(w, ngroups, replay, hf.counts)
	}
	writeGroup := func(w *bitio.Writer, g int, hc *ansCode) {
		st := &ansStream{c: hc, toks: make([]ansToken, 0, len(caches[g].v))}
		replay(g, st.add)
		st.flush(w)
	}

	// --- headers
	out := &bitio.Writer{}
	out.Write(16, 0x0aff)
	writeSize(out, W, H)
	out.WriteBool(false) // metadata
	out.WriteBool(false) // no extra fields
	writeBitDepth(out, 8)
	out.WriteBool(true)                                            // 16-bit buffers suffice
	out.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(12, 1)) // no extra channels
	out.WriteBool(false)                                           // not XYB
	hasICC := len(j.ICC) >= 132
	switch {
	case hasICC:
		out.WriteBool(false) // colour encoding
		out.WriteBool(true)  // ICC
		if grey {
			out.WriteEnum(1)
		} else {
			out.WriteEnum(0)
		}
	case grey:
		out.WriteBool(false) // colour encoding
		out.WriteBool(false) // no ICC
		out.WriteEnum(1)     // grey
		out.WriteEnum(1)     // D65
		out.WriteBool(false) // no gamma
		out.WriteEnum(13)    // sRGB transfer
		out.WriteEnum(1)     // relative
	default:
		out.WriteBool(true) // sRGB
	}
	out.WriteU64(0)     // extensions
	out.WriteBool(true) // default transform data
	if hasICC {
		writeICC(out, j.ICC)
	}
	out.ZeroPadToByte()
	out.WriteBool(false)
	out.Write(2, 0)     // regular
	out.Write(1, 0)     // VarDCT
	out.WriteU64(0x80)  // skip adaptive LF smoothing
	out.WriteBool(!rgb) // YCbCr
	if !rgb {
		for c := 0; c < 3; c++ {
			out.Write(2, upCode[c])
		}
	}
	out.WriteU32(1, val(1), val(2), val(4), val(8))
	out.WriteU32(1, val(1), val(2), val(3), bitsOff(3, 4)) // passes
	out.WriteBool(false)                                   // no crop
	out.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3)) // blending
	out.WriteBool(true)                                    // is_last
	out.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48))
	out.WriteBool(false) // restoration filter not default
	out.WriteBool(false) // no gaborish
	out.Write(2, 0)      // no EPF
	out.WriteU64(0)
	out.WriteU64(0)

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
	sizes := []int{len(lfw.Bytes())}
	for _, s := range lfSecs {
		sizes = append(sizes, len(s))
	}
	sizes = append(sizes, len(hfw.Bytes()))
	for _, s := range grpSecs {
		sizes = append(sizes, len(s))
	}
	if os.Getenv("GOJXL_STATS") != "" {
		lfs, gs := 0, 0
		for _, s := range lfSecs {
			lfs += len(s)
		}
		for _, s := range grpSecs {
			gs += len(s)
		}
		fmt.Fprintf(os.Stderr, "lfglobal %d, lf groups %d, hfglobal %d, groups %d\n", sizes[0], lfs, len(hfw.Bytes()), gs)
	}
	writeTOC(sizes)
	out.AppendBytes(lfw.Bytes())
	for _, s := range lfSecs {
		out.AppendBytes(s)
	}
	out.AppendBytes(hfw.Bytes())
	for _, s := range grpSecs {
		out.AppendBytes(s)
	}
	return out.Bytes(), nil
}

// writeCoeffOrders writes the permutations (relative to the natural order
// nat) of the given orders as Lehmer codes.
func writeCoeffOrders(w *bitio.Writer, nat []int32, orders [][]int32) {
	pos := map[int32]int{}
	for i, v := range nat {
		pos[v] = i
	}
	size := len(nat)
	skip := size / 64
	ctxOf := func(x uint32) int { return min(7, int(addLog2Ceil(x))) }
	type tok struct {
		ctx int
		v   uint32
	}
	var toks []tok
	for _, ord := range orders {
		perm := make([]int, size)
		for i, v := range ord {
			perm[i] = pos[v]
		}
		lehmer := make([]uint32, size)
		for i := 0; i < size; i++ {
			n := 0
			for j := i + 1; j < size; j++ {
				if perm[j] < perm[i] {
					n++
				}
			}
			lehmer[i] = uint32(n)
		}
		end := 0
		for i := skip; i < size; i++ {
			if lehmer[i] != 0 {
				end = i + 1 - skip
			}
		}
		toks = append(toks, tok{ctxOf(uint32(size)), uint32(end)})
		prev := uint32(0)
		for i := skip; i < skip+end; i++ {
			toks = append(toks, tok{ctxOf(prev), lehmer[i]})
			prev = lehmer[i]
		}
	}
	h := newHistograms(8, defaultCfg)
	for _, t := range toks {
		h.add(t.ctx, t.v)
	}
	c := writeANSCode(w, h)
	st := &ansStream{c: c}
	for _, t := range toks {
		st.add(t.ctx, t.v)
	}
	st.flush(w)
}
