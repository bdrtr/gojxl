package jpeg

import (
	"errors"
	"math/bits"
)

// Write serialises the image back to a JPEG file, reproducing the original
// byte for byte when the reconstruction data came from Parse (this mirrors
// libjxl's JPEG writer).
func (img *Image) Write() ([]byte, error) {
	w := &jpegWriter{img: img}
	if img.HasZeroPadding {
		w.pad = img.PaddingBits
		w.padOn = true
	}
	w.out = append(w.out, 0xff, 0xd8)
	for _, m := range img.MarkerOrder {
		if err := w.section(m); err != nil {
			return nil, err
		}
	}
	if w.padOn && w.padPos != len(w.pad) {
		return nil, errors.New("jpeg: invalid number of padding bits")
	}
	return w.out, nil
}

type huffEnc struct {
	depth [256]uint8
	code  [256]uint16
	ok    bool
}

type jpegWriter struct {
	img                                   *Image
	out                                   []byte
	dc, ac                                [4]huffEnc
	dhtIdx, dqtIdx, appIdx, comIdx, imIdx int
	scanIdx                               int
	progressive, seenDRI                  bool
	pad                                   []byte
	padPos                                int
	padOn                                 bool

	// entropy coder state
	buf     uint64 // pending bits, MSB first
	nbits   uint
	eobRun  int
	curAC   *huffEnc
	refBits []byte
}

func (w *jpegWriter) section(m byte) error {
	img := w.img
	switch {
	case m == 0xc0 || m == 0xc1 || m == 0xc2:
		w.progressive = m == 0xc2
		n := len(img.Comps)
		l := 8 + 3*n
		w.out = append(w.out, 0xff, m, byte(l>>8), byte(l), 8, byte(img.H>>8), byte(img.H), byte(img.W>>8), byte(img.W), byte(n))
		for _, c := range img.Comps {
			if c.QuantIdx >= len(img.Quants) {
				return errInvalid
			}
			w.out = append(w.out, byte(c.ID), byte(c.H<<4|c.V), byte(img.Quants[c.QuantIdx].Index))
		}
	case m == 0xc4:
		return w.dht()
	case m >= 0xd0 && m <= 0xd7:
		w.out = append(w.out, 0xff, m)
	case m == 0xd9:
		w.out = append(w.out, 0xff, 0xd9)
		w.out = append(w.out, img.Tail...)
	case m == 0xda:
		return w.scan()
	case m == 0xdb:
		return w.dqt()
	case m == 0xdd:
		w.seenDRI = true
		w.out = append(w.out, 0xff, 0xdd, 0, 4, byte(img.RestartInterval>>8), byte(img.RestartInterval))
	case m >= 0xe0 && m <= 0xef:
		if w.appIdx >= len(img.AppData) {
			return errInvalid
		}
		w.out = append(w.out, 0xff)
		w.out = append(w.out, img.AppData[w.appIdx]...)
		w.appIdx++
	case m == 0xfe:
		if w.comIdx >= len(img.ComData) {
			return errInvalid
		}
		w.out = append(w.out, 0xff)
		w.out = append(w.out, img.ComData[w.comIdx]...)
		w.comIdx++
	case m == 0xff:
		if w.imIdx >= len(img.InterMarker) {
			return errInvalid
		}
		w.out = append(w.out, img.InterMarker[w.imIdx]...)
		w.imIdx++
	default:
		return errInvalid
	}
	return nil
}

func (w *jpegWriter) dht() error {
	img := w.img
	l := 2
	for i := w.dhtIdx; i < len(img.Huff); i++ {
		h := &img.Huff[i]
		for _, c := range h.Counts {
			l += c
		}
		if l == 2 {
			break
		}
		l += 16
		if h.IsLast {
			break
		}
	}
	w.out = append(w.out, 0xff, 0xc4, byte(l>>8), byte(l))
	for {
		if w.dhtIdx >= len(img.Huff) {
			return errInvalid
		}
		h := &img.Huff[w.dhtIdx]
		w.dhtIdx++
		total, maxLen := 0, 0
		for i, c := range h.Counts {
			if c != 0 {
				maxLen = i
			}
			total += c
		}
		if total == 0 {
			break
		}
		t := &w.dc[h.SlotID&3]
		if h.SlotID&0x10 != 0 {
			t = &w.ac[h.SlotID&3]
		}
		if err := buildHuffEnc(h, t); err != nil {
			return err
		}
		w.out = append(w.out, byte(h.SlotID))
		for i := 1; i <= 16; i++ {
			c := h.Counts[i]
			if i == maxLen {
				c--
			}
			w.out = append(w.out, byte(c))
		}
		for _, v := range h.Values[:total-1] {
			w.out = append(w.out, byte(v))
		}
		if h.IsLast {
			break
		}
	}
	return nil
}

func buildHuffEnc(h *HuffCode, t *huffEnc) error {
	*t = huffEnc{ok: true}
	for i := range t.depth {
		t.depth[i] = 127
	}
	var sizes []int
	for l := 1; l <= 16; l++ {
		for i := 0; i < h.Counts[l]; i++ {
			sizes = append(sizes, l)
		}
	}
	if len(sizes) == 0 {
		return nil
	}
	if len(sizes) > len(h.Values) {
		return errInvalid
	}
	code := 0
	si := sizes[0]
	for p := 0; p < len(sizes)-1; p++ { // the last (sentinel) gets no code
		for sizes[p] != si {
			code <<= 1
			si++
		}
		v := h.Values[p]
		if v > 255 {
			return errInvalid
		}
		t.depth[v] = uint8(sizes[p])
		t.code[v] = uint16(code)
		code++
	}
	return nil
}

func (w *jpegWriter) dqt() error {
	img := w.img
	l := 2
	for i := w.dqtIdx; i < len(img.Quants); i++ {
		l += 1 + (img.Quants[i].Precision+1)*64
		if img.Quants[i].IsLast {
			break
		}
	}
	w.out = append(w.out, 0xff, 0xdb, byte(l>>8), byte(l))
	for {
		if w.dqtIdx >= len(img.Quants) {
			return errInvalid
		}
		q := &img.Quants[w.dqtIdx]
		w.dqtIdx++
		w.out = append(w.out, byte(q.Precision<<4|q.Index))
		for k := 0; k < 64; k++ {
			v := q.Values[NaturalOrder[k]]
			if q.Precision != 0 {
				w.out = append(w.out, byte(v>>8))
			}
			w.out = append(w.out, byte(v))
		}
		if q.IsLast {
			break
		}
	}
	return nil
}

// ---------------------------------------------------------------- bits

func (w *jpegWriter) emitByte(b byte) {
	w.out = append(w.out, b)
	if b == 0xff {
		w.out = append(w.out, 0)
	}
}

func (w *jpegWriter) writeBits(n uint, v uint64) {
	if n == 0 {
		return
	}
	w.buf = w.buf<<n | (v & (1<<n - 1))
	w.nbits += n
	for w.nbits >= 8 {
		w.nbits -= 8
		w.emitByte(byte(w.buf >> w.nbits))
	}
}

func (w *jpegWriter) symbol(t *huffEnc, s int) error {
	if t.depth[s] > 16 {
		return errors.New("jpeg: symbol without Huffman code")
	}
	w.writeBits(uint(t.depth[s]), uint64(t.code[s]))
	return nil
}

func (w *jpegWriter) byteBoundary() error {
	n := (8 - w.nbits%8) % 8
	var pat uint64
	if !w.padOn {
		pat = 1<<n - 1
	} else {
		for i := uint(0); i < n; i++ {
			if w.padPos >= len(w.pad) {
				return errors.New("jpeg: not enough padding bits")
			}
			b := w.pad[w.padPos]
			w.padPos++
			if b&^1 != 0 {
				return errInvalid
			}
			pat = pat<<1 | uint64(b)
		}
	}
	w.writeBits(n, pat)
	w.buf, w.nbits = 0, 0
	return nil
}

func (w *jpegWriter) flush() error {
	if w.eobRun > 0 {
		nb := bits.Len(uint(w.eobRun)) - 1
		if err := w.symbol(w.curAC, nb<<4); err != nil {
			return err
		}
		w.writeBits(uint(nb), uint64(w.eobRun))
		w.eobRun = 0
	}
	for _, b := range w.refBits {
		w.writeBits(1, uint64(b))
	}
	w.refBits = w.refBits[:0]
	return nil
}

func (w *jpegWriter) bufferEOB(ac *huffEnc, refBits []byte) error {
	if w.eobRun == 0 {
		w.curAC = ac
	}
	w.eobRun++
	w.refBits = append(w.refBits, refBits...)
	if w.eobRun == 0x7fff {
		return w.flush()
	}
	return nil
}

// magnitude returns the bit count and the bits coding v.
func magnitude(v int) (uint, uint64) {
	a := v
	if v < 0 {
		a = -v
		v--
	}
	if a == 0 {
		return 0, 0
	}
	n := uint(bits.Len(uint(a)))
	return n, uint64(v) & (1<<n - 1)
}

// ---------------------------------------------------------------- scans

func (w *jpegWriter) scan() error {
	img := w.img
	if w.scanIdx >= len(img.Scans) {
		return errInvalid
	}
	sc := &img.Scans[w.scanIdx]
	w.scanIdx++
	// SOS
	n := len(sc.Comps)
	l := 6 + 2*n
	w.out = append(w.out, 0xff, 0xda, byte(l>>8), byte(l), byte(n))
	for _, c := range sc.Comps {
		if c.CompIdx >= len(img.Comps) {
			return errInvalid
		}
		w.out = append(w.out, byte(img.Comps[c.CompIdx].ID), byte(c.DCTbl<<4|c.ACTbl))
	}
	w.out = append(w.out, byte(sc.Ss), byte(sc.Se), byte(sc.Ah<<4|sc.Al))

	w.buf, w.nbits, w.eobRun, w.refBits = 0, 0, 0, w.refBits[:0]
	ri := 0
	if w.seenDRI {
		ri = img.RestartInterval
	}
	interleaved := n > 1
	hmax, vmax := 1, 1
	for _, c := range img.Comps {
		hmax, vmax = max(hmax, c.H), max(vmax, c.V)
	}
	hg, vg := 1, 1
	if !interleaved {
		hg, vg = img.Comps[sc.Comps[0].CompIdx].H, img.Comps[sc.Comps[0].CompIdx].V
	}
	mcuCols := (img.W*hg + 8*hmax - 1) / (8 * hmax)
	mcuRows := (img.H*vg + 8*vmax - 1) / (8 * vmax)
	Al, Ah, Ss, Se := 0, 0, 0, 63
	if w.progressive {
		Al, Ah, Ss, Se = sc.Al, sc.Ah, sc.Ss, sc.Se
	}
	mode := 0 // sequential
	if w.progressive && !(Ah == 0 && Al == 0 && Ss == 0 && Se == 63) {
		mode = 1
		if Ah != 0 {
			mode = 2
		}
	}
	wantDC, wantAC := Ss == 0, Ss != 0 || Se != 0
	resetPos, zrPos := 0, 0
	nextReset := -1
	if len(sc.ResetPoints) > 0 {
		nextReset = sc.ResetPoints[0]
		resetPos = 1
	}
	nextZR := -1
	if len(sc.ExtraZeroRuns) > 0 {
		nextZR = sc.ExtraZeroRuns[0].Block
	}
	restartsToGo := ri
	nextRST := 0
	var lastDC [4]int
	blockIdx := 0
	for my := 0; my < mcuRows; my++ {
		for mx := 0; mx < mcuCols; mx++ {
			if ri > 0 && restartsToGo == 0 {
				if err := w.flush(); err != nil {
					return err
				}
				if err := w.byteBoundary(); err != nil {
					return err
				}
				w.out = append(w.out, 0xff, byte(0xd0+nextRST))
				nextRST = (nextRST + 1) & 7
				restartsToGo = ri
				lastDC = [4]int{}
			}
			for _, si := range sc.Comps {
				c := &img.Comps[si.CompIdx]
				dch, ach := &w.dc[si.DCTbl], &w.ac[si.ACTbl]
				if wantDC && mode != 2 && !dch.ok || wantAC && !ach.ok {
					return errors.New("jpeg: missing Huffman table")
				}
				nby, nbx := 1, 1
				if interleaved {
					nby, nbx = c.V, c.H
				}
				for iy := 0; iy < nby; iy++ {
					for ix := 0; ix < nbx; ix++ {
						by, bx := my*nby+iy, mx*nbx+ix
						if bx >= c.BW || by >= c.BH {
							return errInvalid
						}
						if blockIdx == nextReset {
							if err := w.flush(); err != nil {
								return err
							}
							nextReset = -1
							if resetPos < len(sc.ResetPoints) {
								nextReset = sc.ResetPoints[resetPos]
								resetPos++
							}
						}
						nzr := 0
						if blockIdx == nextZR {
							nzr = sc.ExtraZeroRuns[zrPos].N
							zrPos++
							nextZR = -1
							if zrPos < len(sc.ExtraZeroRuns) {
								nextZR = sc.ExtraZeroRuns[zrPos].Block
							}
						}
						coef := c.Coef[(by*c.BW+bx)*64 : (by*c.BW+bx)*64+64]
						var err error
						switch mode {
						case 0:
							err = w.blockSeq(coef, dch, ach, nzr, &lastDC[si.CompIdx])
						case 1:
							err = w.blockProg(coef, dch, ach, Ss, Se, Al, nzr, &lastDC[si.CompIdx])
						default:
							err = w.blockRefine(coef, ach, Ss, Se, Al)
						}
						if err != nil {
							return err
						}
						blockIdx++
					}
				}
			}
			restartsToGo--
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	return w.byteBoundary()
}

func (w *jpegWriter) blockSeq(coef []int16, dch, ach *huffEnc, nzr int, lastDC *int) error {
	d := int(coef[0]) - *lastDC
	*lastDC = int(coef[0])
	nb, bv := magnitude(d)
	if err := w.symbol(dch, int(nb)); err != nil {
		return err
	}
	w.writeBits(nb, bv)
	r := 0
	for k := 1; k < 64; k++ {
		v := int(coef[NaturalOrder[k]])
		if v == 0 {
			r++
			continue
		}
		for r > 15 {
			if err := w.symbol(ach, 0xf0); err != nil {
				return err
			}
			r -= 16
		}
		nb, bv := magnitude(v)
		if err := w.symbol(ach, r<<4|int(nb)); err != nil {
			return err
		}
		w.writeBits(nb, bv)
		r = 0
	}
	for i := 0; i < nzr; i++ {
		if err := w.symbol(ach, 0xf0); err != nil {
			return err
		}
		r -= 16
	}
	if r > 0 {
		return w.symbol(ach, 0)
	}
	return nil
}

func (w *jpegWriter) blockProg(coef []int16, dch, ach *huffEnc, Ss, Se, Al, nzr int, lastDC *int) error {
	eobAllowed := Ss > 0
	if Ss == 0 {
		v := int(coef[0]) >> Al
		d := v - *lastDC
		*lastDC = v
		nb, bv := magnitude(d)
		if err := w.symbol(dch, int(nb)); err != nil {
			return err
		}
		w.writeBits(nb, bv)
		Ss++
	}
	if Ss > Se {
		return nil
	}
	r := 0
	for k := Ss; k <= Se; k++ {
		t := int(coef[NaturalOrder[k]])
		if t == 0 {
			r++
			continue
		}
		var t2 int
		if t < 0 {
			t = (-t) >> Al
			t2 = ^t
		} else {
			t >>= Al
			t2 = t
		}
		if t == 0 {
			r++
			continue
		}
		if err := w.flush(); err != nil {
			return err
		}
		for r > 15 {
			if err := w.symbol(ach, 0xf0); err != nil {
				return err
			}
			r -= 16
		}
		nb := uint(bits.Len(uint(t)))
		if err := w.symbol(ach, r<<4|int(nb)); err != nil {
			return err
		}
		w.writeBits(nb, uint64(t2)&(1<<nb-1))
		r = 0
	}
	if nzr > 0 {
		if err := w.flush(); err != nil {
			return err
		}
		for i := 0; i < nzr; i++ {
			if err := w.symbol(ach, 0xf0); err != nil {
				return err
			}
			r -= 16
		}
	}
	if r > 0 {
		if err := w.bufferEOB(ach, nil); err != nil {
			return err
		}
		if !eobAllowed {
			return w.flush()
		}
	}
	return nil
}

func (w *jpegWriter) blockRefine(coef []int16, ach *huffEnc, Ss, Se, Al int) error {
	eobAllowed := Ss > 0
	if Ss == 0 {
		w.writeBits(1, uint64(int(coef[0])>>Al&1))
		Ss++
	}
	if Ss > Se {
		return nil
	}
	var abs [64]int
	eob := 0
	for k := Ss; k <= Se; k++ {
		a := int(coef[NaturalOrder[k]])
		if a < 0 {
			a = -a
		}
		abs[k] = a >> Al
		if abs[k] == 1 {
			eob = k
		}
	}
	r := 0
	var rb [64]byte
	nrb := 0
	for k := Ss; k <= Se; k++ {
		if abs[k] == 0 {
			r++
			continue
		}
		for r > 15 && k <= eob {
			if err := w.flush(); err != nil {
				return err
			}
			if err := w.symbol(ach, 0xf0); err != nil {
				return err
			}
			r -= 16
			for _, b := range rb[:nrb] {
				w.writeBits(1, uint64(b))
			}
			nrb = 0
		}
		if abs[k] > 1 {
			rb[nrb] = byte(abs[k] & 1)
			nrb++
			continue
		}
		if err := w.flush(); err != nil {
			return err
		}
		if err := w.symbol(ach, r<<4|1); err != nil {
			return err
		}
		bit := uint64(1)
		if coef[NaturalOrder[k]] < 0 {
			bit = 0
		}
		w.writeBits(1, bit)
		for _, b := range rb[:nrb] {
			w.writeBits(1, uint64(b))
		}
		nrb = 0
		r = 0
	}
	if r > 0 || nrb > 0 {
		if err := w.bufferEOB(ach, rb[:nrb]); err != nil {
			return err
		}
		if !eobAllowed {
			return w.flush()
		}
	}
	return nil
}
