package webp

import (
	"image"
	"math/bits"
)

// VP8 (lossy) key frame decoding, following libwebp's integer arithmetic
// so that the output matches it exactly: intra prediction from unfiltered
// neighbours, the normal or simple loop filter over the whole frame, and
// libwebp's "fancy" chroma upsampling and YUV to RGB conversion.

// ---------------------------------------------------------------- bool decoder

type boolDec struct {
	src   []byte
	pos   int
	value uint64 // pending bits; the current window is value >> bits
	bits  int    // valid bits below the 8-bit window (negative: load)
	rng   uint32 // range - 1, in [127, 254]
	eof   bool
}

func newBoolDec(src []byte) *boolDec {
	b := &boolDec{src: src, bits: -8, rng: 254}
	b.load()
	return b
}

func (b *boolDec) load() {
	if b.pos+7 <= len(b.src) {
		var v uint64
		for _, c := range b.src[b.pos : b.pos+7] {
			v = v<<8 | uint64(c)
		}
		b.pos += 7
		b.value = b.value<<56 | v
		b.bits += 56
		return
	}
	if b.pos < len(b.src) {
		b.value = b.value<<8 | uint64(b.src[b.pos])
		b.pos++
		b.bits += 8
		return
	}
	if !b.eof { // as libwebp: one byte of zeros, then stop advancing
		b.value <<= 8
		b.bits += 8
		b.eof = true
	} else {
		b.bits = 0
	}
}

func (b *boolDec) bit(prob uint8) bool {
	if b.bits < 0 {
		b.load()
	}
	split := b.rng * uint32(prob) >> 8
	var r uint32
	one := uint32(b.value>>uint(b.bits)) > split
	if one {
		r = b.rng - split
		b.value -= uint64(split+1) << uint(b.bits)
	} else {
		r = split + 1
	}
	// renormalise: r is in [1, 254]
	s := uint(bits.LeadingZeros8(uint8(r)))
	b.rng = r<<s - 1
	b.bits -= int(s)
	return one
}

// fill loads more bits when the window ran out (call before bitFast).
func (b *boolDec) fill() {
	if b.bits < 0 {
		b.load()
	}
}

// bitFast is bit when at least 8 bits are known to be loaded.
func (b *boolDec) bitFast(prob uint8) bool {
	split := b.rng * uint32(prob) >> 8
	r := split + 1
	one := uint32(b.value>>uint(b.bits)) > split
	if one {
		r = b.rng - split
		b.value -= uint64(split+1) << uint(b.bits)
	}
	t := renorm[uint8(r)]
	b.rng = uint32(t & 0xff)
	b.bits -= int(t >> 8)
	return one
}

// renorm[r] is the shift that brings range r (1..254) to [128, 255],
// and the new range - 1: shift<<8 | (r<<shift - 1).
var renorm = func() (t [256]uint16) {
	for r := 1; r < 256; r++ {
		s := bits.LeadingZeros8(uint8(r))
		t[r] = uint16(s)<<8 | uint16(r<<s-1)
	}
	return
}()

func (b *boolDec) literal(n int) int {
	v := 0
	for ; n > 0; n-- {
		v <<= 1
		if b.bit(128) {
			v |= 1
		}
	}
	return v
}

func (b *boolDec) signed(n int) int {
	v := b.literal(n)
	if b.bit(128) {
		return -v
	}
	return v
}

// optSigned reads a flag and, when set, a signed n-bit value.
func (b *boolDec) optSigned(n int) int {
	if !b.bit(128) {
		return 0
	}
	return b.signed(n)
}

// ---------------------------------------------------------------- headers

const (
	predDC = iota
	predTM
	predVE
	predHE
	predRD
	predVR
	predLD
	predVL
	predHD
	predHU
	// DC variants at the picture edges (16x16 and chroma)
	predDCNoTop
	predDCNoLeft
	predDCNoTopLeft
)

type quant struct {
	y1, y2, uv [2]int32 // DC, AC factors
}

type mbInfo struct {
	segment int
	skip    bool
	i4x4    bool
	ymode   int
	imodes  [16]uint8
	uvmode  int
	coeffs  [384]int16 // 16 Y, 4 U, 4 V blocks
	nzY     uint32     // per Y block: any non-zero coefficient (bit n)
	nzU     uint8      // per U block
	nzV     uint8
	inner   bool // the loop filter does inner edges
}

type vp8 struct {
	w, h      int
	mbw, mbh  int
	segmentOn bool
	updateMap bool
	absDelta  bool
	segQuant  [4]int
	segFilter [4]int
	segProb   [3]uint8
	simple    bool
	level     int
	sharpness int
	lfDelta   bool
	refDelta  [4]int
	modeDelta [4]int
	nparts    int
	q         [4]quant
	coef      [4][8][3][11]uint8
	skipProb  uint8
	useSkip   bool
	parts     []*boolDec
	// intra mode contexts
	intraTop  []uint8 // 4 per macroblock column
	intraLeft [4]uint8
	// non-zero contexts
	nzTop    []uint8 // per macroblock column: bits 0-3 Y, 4-5 U, 6-7 V
	nzDCTop  []uint8
	nzLeft   uint8
	nzDCLeft uint8
}

var bandOf = [17]uint8{0, 1, 2, 3, 6, 4, 5, 6, 6, 6, 6, 6, 6, 6, 6, 7, 0}
var zigzag = [16]uint8{0, 1, 4, 8, 5, 2, 3, 6, 9, 12, 13, 10, 7, 11, 14, 15}

// decodeLossy decodes a VP8 bitstream, with the ALPH chunk data if any.
func decodeLossy(data, alph []byte) (*image.NRGBA, bool, error) {
	y, u, v, w, h, err := decodeVP8(data)
	if err != nil {
		return nil, false, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	yuvToRGBA(img, y, u, v, w, h, (w+15)&^15, ((w+15)&^15)/2)
	if alph == nil {
		return img, false, nil
	}
	if err := decodeAlpha(alph, img); err != nil {
		return nil, false, err
	}
	return img, true, nil
}

// decodeVP8 returns the Y, U, V planes (strides rounded up to whole
// macroblocks) of a key frame.
func decodeVP8(data []byte) (yp, up, vp []byte, w, h int, err error) {
	if len(data) < 10 {
		return nil, nil, nil, 0, 0, errorf("VP8 header")
	}
	tag := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
	if tag&1 != 0 {
		return nil, nil, nil, 0, 0, errorf("not a key frame")
	}
	if tag>>1&7 > 3 {
		return nil, nil, nil, 0, 0, errorf("VP8 profile")
	}
	if tag>>4&1 == 0 {
		return nil, nil, nil, 0, 0, errorf("frame not shown")
	}
	part0 := int(tag >> 5)
	if data[3] != 0x9d || data[4] != 0x01 || data[5] != 0x2a {
		return nil, nil, nil, 0, 0, errorf("VP8 start code")
	}
	w = int(uint16(data[6])|uint16(data[7])<<8) & 0x3fff
	h = int(uint16(data[8])|uint16(data[9])<<8) & 0x3fff
	if w == 0 || h == 0 || w*h > MaxPixels {
		return nil, nil, nil, 0, 0, errorf("VP8 size")
	}
	data = data[10:]
	if part0 > len(data) {
		return nil, nil, nil, 0, 0, errorf("VP8 partition size")
	}
	d := &vp8{w: w, h: h, mbw: (w + 15) / 16, mbh: (h + 15) / 16}
	br := newBoolDec(data[:part0])
	if err := d.readHeader(br); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	if err := d.splitPartitions(data[part0:]); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	ys, uvs := d.mbw*16, d.mbw*8
	yp = make([]byte, ys*d.mbh*16)
	up = make([]byte, uvs*d.mbh*8)
	vp = make([]byte, uvs*d.mbh*8)
	d.intraTop = make([]uint8, 4*d.mbw)
	d.nzTop = make([]uint8, d.mbw)
	d.nzDCTop = make([]uint8, d.mbw)
	mbs := make([]mbInfo, d.mbw) // one row
	filters := make([]bool, d.mbw*d.mbh)
	fsegs := make([]uint8, d.mbw*d.mbh)
	fi4 := make([]bool, d.mbw*d.mbh)
	r := &recon{d: d, y: yp, u: up, v: vp, ys: ys, uvs: uvs}
	r.init()
	for my := 0; my < d.mbh; my++ {
		d.intraLeft = [4]uint8{}
		d.nzLeft, d.nzDCLeft = 0, 0
		tb := d.parts[my%len(d.parts)]
		for mx := 0; mx < d.mbw; mx++ {
			mb := &mbs[mx]
			d.readModes(br, mb, mx)
			d.readResiduals(tb, mb, mx)
			i := my*d.mbw + mx
			filters[i] = mb.inner
			fsegs[i] = uint8(mb.segment)
			fi4[i] = mb.i4x4
		}
		// as libwebp: reading past the data (and the one zero byte it
		// allows) means a truncated file
		if br.eof || tb.eof {
			return nil, nil, nil, 0, 0, errorf("truncated VP8 data")
		}
		for mx := 0; mx < d.mbw; mx++ {
			r.mb(&mbs[mx], mx, my)
		}
	}
	if d.level > 0 {
		d.loopFilter(yp, up, vp, ys, uvs, filters, fsegs, fi4)
	}
	return yp, up, vp, w, h, nil
}

func (d *vp8) readHeader(br *boolDec) error {
	br.bit(128) // colour space
	br.bit(128) // clamping type (always clamped here)
	d.segmentOn = br.bit(128)
	if d.segmentOn {
		d.updateMap = br.bit(128)
		if br.bit(128) { // update segment feature data
			d.absDelta = br.bit(128)
			for i := range d.segQuant {
				d.segQuant[i] = br.optSigned(7)
			}
			for i := range d.segFilter {
				d.segFilter[i] = br.optSigned(6)
			}
		}
		if d.updateMap {
			for i := range d.segProb {
				d.segProb[i] = 255
				if br.bit(128) {
					d.segProb[i] = uint8(br.literal(8))
				}
			}
		}
	}
	d.simple = br.bit(128)
	d.level = br.literal(6)
	d.sharpness = br.literal(3)
	d.lfDelta = br.bit(128)
	if d.lfDelta && br.bit(128) {
		for i := range d.refDelta {
			if br.bit(128) {
				d.refDelta[i] = br.signed(6)
			}
		}
		for i := range d.modeDelta {
			if br.bit(128) {
				d.modeDelta[i] = br.signed(6)
			}
		}
	}
	d.nparts = 1 << br.literal(2)
	// quantisers
	base := br.literal(7)
	dqY1DC := br.optSigned(4)
	dqY2DC := br.optSigned(4)
	dqY2AC := br.optSigned(4)
	dqUVDC := br.optSigned(4)
	dqUVAC := br.optSigned(4)
	clip := func(v, m int) int { return min(max(v, 0), m) }
	for s := 0; s < 4; s++ {
		q := base
		if d.segmentOn {
			q = d.segQuant[s]
			if !d.absDelta {
				q += base
			}
		}
		m := &d.q[s]
		m.y1[0] = int32(dcTable[clip(q+dqY1DC, 127)])
		m.y1[1] = int32(acTable[clip(q, 127)])
		m.y2[0] = int32(dcTable[clip(q+dqY2DC, 127)]) * 2
		// x*155/100 is x*101581 >> 16 for the table's range
		m.y2[1] = int32(acTable[clip(q+dqY2AC, 127)]) * 101581 >> 16
		if m.y2[1] < 8 {
			m.y2[1] = 8
		}
		m.uv[0] = int32(dcTable[clip(q+dqUVDC, 117)])
		m.uv[1] = int32(acTable[clip(q+dqUVAC, 127)])
	}
	br.bit(128) // refresh entropy probabilities (single frame: unused)
	d.coef = defaultCoefProb
	for t := range d.coef {
		for b := range d.coef[t] {
			for c := range d.coef[t][b] {
				for p := range d.coef[t][b][c] {
					if br.bit(coefUpdateProb[t][b][c][p]) {
						d.coef[t][b][c][p] = uint8(br.literal(8))
					}
				}
			}
		}
	}
	d.useSkip = br.bit(128)
	if d.useSkip {
		d.skipProb = uint8(br.literal(8))
	}
	return nil
}

func (d *vp8) splitPartitions(data []byte) error {
	n := d.nparts
	if len(data) < 3*(n-1) {
		return errorf("VP8 partitions")
	}
	sizes, data := data[:3*(n-1)], data[3*(n-1):]
	for i := 0; i < n; i++ {
		size := len(data)
		if i < n-1 {
			size = int(sizes[3*i]) | int(sizes[3*i+1])<<8 | int(sizes[3*i+2])<<16
			if size > len(data) {
				// libwebp lets the last partitions run short
				size = len(data)
			}
		}
		d.parts = append(d.parts, newBoolDec(data[:size]))
		data = data[size:]
	}
	return nil
}

func (d *vp8) readModes(br *boolDec, mb *mbInfo, mx int) {
	mb.segment = 0
	if d.updateMap {
		if !br.bit(d.segProb[0]) {
			mb.segment = 0
			if br.bit(d.segProb[1]) {
				mb.segment = 1
			}
		} else {
			mb.segment = 2
			if br.bit(d.segProb[2]) {
				mb.segment = 3
			}
		}
	}
	mb.skip = d.useSkip && br.bit(d.skipProb)
	mb.i4x4 = !br.bit(145)
	top := d.intraTop[4*mx : 4*mx+4]
	if !mb.i4x4 {
		var m int
		if br.bit(156) {
			if br.bit(128) {
				m = predTM
			} else {
				m = predHE
			}
		} else if br.bit(163) {
			m = predVE
		} else {
			m = predDC
		}
		mb.ymode = m
		for i := range top {
			top[i] = uint8(m)
		}
		d.intraLeft = [4]uint8{uint8(m), uint8(m), uint8(m), uint8(m)}
	} else {
		for y := 0; y < 4; y++ {
			left := d.intraLeft[y]
			for x := 0; x < 4; x++ {
				p := &kfBModeProb[top[x]][left]
				var m uint8
				switch {
				case !br.bit(p[0]):
					m = predDC
				case !br.bit(p[1]):
					m = predTM
				case !br.bit(p[2]):
					m = predVE
				case !br.bit(p[3]):
					switch {
					case !br.bit(p[4]):
						m = predHE
					case !br.bit(p[5]):
						m = predRD
					default:
						m = predVR
					}
				case !br.bit(p[6]):
					m = predLD
				case !br.bit(p[7]):
					m = predVL
				case !br.bit(p[8]):
					m = predHD
				default:
					m = predHU
				}
				mb.imodes[y*4+x] = m
				top[x] = m
				left = m
			}
			d.intraLeft[y] = left
		}
	}
	switch {
	case !br.bit(142):
		mb.uvmode = predDC
	case !br.bit(114):
		mb.uvmode = predVE
	case !br.bit(183):
		mb.uvmode = predHE
	default:
		mb.uvmode = predTM
	}
}

var cat3456 = [4][]uint8{
	{173, 148, 140},
	{176, 155, 140, 135},
	{180, 157, 141, 134, 130},
	{254, 254, 243, 230, 196, 177, 153, 140, 133, 130, 129},
}

// coeffs reads the tokens of one block from index n (0 or 1) and returns
// the index past the last non-zero one.
func (d *vp8) coeffs(br *boolDec, typ, ctx int, dq [2]int32, n int, out []int16) int {
	bands := &d.coef[typ]
	p := &bands[bandOf[n]][ctx]
	for ; n < 16; n++ {
		if br.fill(); !br.bitFast(p[0]) {
			return n
		}
		for br.fill(); !br.bitFast(p[1]); br.fill() {
			n++
			if n == 16 {
				return 16
			}
			p = &bands[bandOf[n]][0]
		}
		var v int
		next := &bands[bandOf[n+1]]
		if br.fill(); !br.bitFast(p[2]) {
			v = 1
			p = &next[1]
		} else {
			v = largeValue(br, p)
			p = &next[2]
		}
		if br.fill(); br.bitFast(128) {
			v = -v
		}
		f := dq[1]
		if n == 0 {
			f = dq[0]
		}
		out[zigzag[n]] = int16(int32(v) * f)
	}
	return 16
}

func largeValue(br *boolDec, p *[11]uint8) int {
	if br.fill(); !br.bitFast(p[3]) {
		if br.fill(); !br.bitFast(p[4]) {
			return 2
		}
		v := 3
		if br.fill(); br.bitFast(p[5]) {
			v++
		}
		return v
	}
	if br.fill(); !br.bitFast(p[6]) {
		if br.fill(); !br.bitFast(p[7]) {
			v := 5
			if br.fill(); br.bitFast(159) {
				v++
			}
			return v
		}
		v := 7
		if br.fill(); br.bitFast(165) {
			v += 2
		}
		if br.fill(); br.bitFast(145) {
			v++
		}
		return v
	}
	b1 := 0
	if br.fill(); br.bitFast(p[8]) {
		b1 = 1
	}
	b0 := 0
	if br.fill(); br.bitFast(p[9+b1]) {
		b0 = 1
	}
	cat := 2*b1 + b0
	v := 0
	for _, pr := range cat3456[cat] {
		v += v
		if br.fill(); br.bitFast(pr) {
			v++
		}
	}
	return v + 3 + 8<<cat
}

func (d *vp8) readResiduals(br *boolDec, mb *mbInfo, mx int) {
	q := &d.q[mb.segment]
	mb.coeffs = [384]int16{}
	mb.nzY, mb.nzU, mb.nzV = 0, 0, 0
	if mb.skip {
		d.nzTop[mx], d.nzLeft = 0, 0
		if !mb.i4x4 {
			d.nzDCTop[mx], d.nzDCLeft = 0, 0
		}
		mb.inner = mb.i4x4
		return
	}
	dst := mb.coeffs[:]
	first, ytyp := 0, 3
	if !mb.i4x4 {
		var dc [16]int16
		ctx := int(d.nzDCTop[mx] + d.nzDCLeft)
		nz := d.coeffs(br, 1, ctx, q.y2, 0, dc[:])
		f := uint8(0)
		if nz > 0 {
			f = 1
		}
		d.nzDCTop[mx], d.nzDCLeft = f, f
		if nz > 1 {
			wht(&dc, dst)
		} else {
			dc0 := int16((int32(dc[0]) + 3) >> 3)
			for i := 0; i < 256; i += 16 {
				dst[i] = dc0
			}
		}
		first, ytyp = 1, 0
	}
	tnz := d.nzTop[mx] & 0xf
	lnz := d.nzLeft & 0xf
	any := false
	for y := 0; y < 4; y++ {
		l := lnz & 1
		for x := 0; x < 4; x++ {
			b := dst[(y*4+x)*16 : (y*4+x)*16+16]
			ctx := int(l + tnz&1)
			nz := d.coeffs(br, ytyp, ctx, q.y1, first, b)
			l = 0
			if nz > first {
				l = 1
			}
			tnz = tnz>>1 | l<<7
			if nz > 0 && blockNonZero(b) {
				mb.nzY |= 1 << uint(y*4+x)
				any = true
			}
		}
		tnz >>= 4
		lnz = lnz>>1 | l<<7
	}
	outT := tnz
	outL := lnz >> 4
	for ch := 0; ch < 2; ch++ {
		tnz = d.nzTop[mx] >> (4 + 2*ch)
		lnz = d.nzLeft >> (4 + 2*ch)
		var nzb uint8
		for y := 0; y < 2; y++ {
			l := lnz & 1
			for x := 0; x < 2; x++ {
				k := 16 + ch*4 + y*2 + x
				b := dst[k*16 : k*16+16]
				ctx := int(l + tnz&1)
				nz := d.coeffs(br, 2, ctx, q.uv, 0, b)
				l = 0
				if nz > 0 {
					l = 1
				}
				tnz = tnz>>1 | l<<3
				if nz > 0 && blockNonZero(b) {
					nzb |= 1 << uint(y*2+x)
					any = true
				}
			}
			tnz >>= 2
			lnz = lnz>>1 | l<<5
		}
		if ch == 0 {
			mb.nzU = nzb
		} else {
			mb.nzV = nzb
		}
		outT |= (tnz << 4) << (2 * ch)
		outL |= (lnz & 0xf0) << (2 * ch)
	}
	// the WHT can leave DCs in blocks that read no coefficient
	if !mb.i4x4 {
		for k := 0; k < 16; k++ {
			if dst[k*16] != 0 {
				mb.nzY |= 1 << uint(k)
				any = true
			}
		}
	}
	d.nzTop[mx] = outT
	d.nzLeft = outL
	mb.inner = mb.i4x4 || any
}

func blockNonZero(b []int16) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

func wht(in *[16]int16, out []int16) {
	var tmp [16]int32
	for i := 0; i < 4; i++ {
		a0 := int32(in[i]) + int32(in[12+i])
		a1 := int32(in[4+i]) + int32(in[8+i])
		a2 := int32(in[4+i]) - int32(in[8+i])
		a3 := int32(in[i]) - int32(in[12+i])
		tmp[i] = a0 + a1
		tmp[8+i] = a0 - a1
		tmp[4+i] = a3 + a2
		tmp[12+i] = a3 - a2
	}
	for i := 0; i < 4; i++ {
		dc := tmp[i*4] + 3
		a0 := dc + tmp[3+i*4]
		a1 := tmp[1+i*4] + tmp[2+i*4]
		a2 := tmp[1+i*4] - tmp[2+i*4]
		a3 := dc - tmp[3+i*4]
		o := out[i*64:]
		o[0] = int16((a0 + a1) >> 3)
		o[16] = int16((a3 + a2) >> 3)
		o[32] = int16((a0 - a1) >> 3)
		o[48] = int16((a3 - a2) >> 3)
	}
}
