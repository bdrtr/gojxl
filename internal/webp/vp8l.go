// Package webp decodes WebP images: lossless (VP8L) and lossy (VP8)
// bitstreams, alpha, metadata and animations in the RIFF container.
package webp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrFormat reports a malformed or unsupported file.
var ErrFormat = errors.New("webp: invalid format")

func errorf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrFormat}, a...)...)
}

// MaxPixels bounds the image area accepted (a few bytes may declare a
// huge image).
var MaxPixels = 1 << 28

// ---------------------------------------------------------------- bits

// bitReader reads LSB-first.
type bitReader struct {
	src []byte
	pos int // next byte to load
	buf uint64
	n   uint
	eof bool // read past the end (reads then give zeros)
}

func (b *bitReader) refill() {
	if b.pos+8 <= len(b.src) {
		b.buf |= binary.LittleEndian.Uint64(b.src[b.pos:]) << b.n
		b.pos += int(63-b.n) >> 3
		b.n |= 56
		return
	}
	for b.n <= 56 {
		if b.pos < len(b.src) {
			b.buf |= uint64(b.src[b.pos]) << b.n
		} else {
			b.eof = true
		}
		b.pos++
		b.n += 8
	}
}

// read returns n <= 32 bits.
func (b *bitReader) read(n uint) uint32 {
	if b.n < n {
		b.refill()
	}
	v := uint32(b.buf & (1<<n - 1))
	b.buf >>= n
	b.n -= n
	return v
}

// overrun reports whether more bits were consumed than the input has.
func (b *bitReader) overrun() bool {
	return b.eof && b.pos-int(b.n>>3) > len(b.src)
}

// ---------------------------------------------------------------- prefix codes

const rootBits = 8

// huffman is a canonical prefix code as a two-level table of entries
// sym<<8 | len (len <= 15), or for long codes in the root table
// offset<<8 | 0x80 | subBits.
type huffman struct {
	t      []uint32
	single bool // one symbol, read with no bits
	sym    uint32
}

// build makes the code from code lengths (0 = unused).
func (h *huffman) build(lens []uint8) error {
	var count [16]int
	n, last := 0, 0
	for s, l := range lens {
		if l > 15 {
			return errorf("code length %d", l)
		}
		if l > 0 {
			count[l]++
			n++
			last = s
		}
	}
	h.t = h.t[:0]
	h.single = false
	if n == 0 {
		return errorf("empty prefix code")
	}
	if n == 1 {
		h.single, h.sym = true, uint32(last)
		return nil
	}
	// completeness (Kraft sum exactly 1)
	left := 1
	for l := 1; l < 16; l++ {
		left = left<<1 - count[l]
		if left < 0 {
			return errorf("over-subscribed prefix code")
		}
	}
	if left != 0 {
		return errorf("incomplete prefix code")
	}
	// symbols sorted by (length, symbol): canonical codes in that order
	var pos [16]int
	for l, off := 1, 0; l < 16; l++ {
		pos[l] = off
		off += count[l]
	}
	sorted := make([]uint16, n)
	for s, l := range lens {
		if l > 0 {
			sorted[pos[l]] = uint16(s)
			pos[l]++
		}
	}
	// sub-table sizes: the longest code under each root prefix
	var subBits [1 << rootBits]uint8
	code := uint32(0)
	for l := rootBits + 1; l < 16; l++ {
		if l == rootBits+1 {
			// the first code of length rootBits+1
			code = 0
			for k := 1; k <= rootBits; k++ {
				code = (code + uint32(count[k])) << 1
			}
		}
		for k := 0; k < count[l]; k++ {
			p := reverse(code>>(uint(l)-rootBits), rootBits)
			subBits[p] = max(subBits[p], uint8(l-rootBits))
			code++
		}
		code <<= 1
	}
	h.t = append(h.t, make([]uint32, 1<<rootBits)...)
	var subStart [1 << rootBits]int
	code = 0
	i := 0
	for l := 1; l < 16; l++ {
		for k := 0; k < count[l]; k++ {
			s := uint32(sorted[i])
			i++
			if l <= rootBits {
				for x := reverse(code, uint(l)); x < 1<<rootBits; x += 1 << uint(l) {
					h.t[x] = s<<8 | uint32(l)
				}
			} else {
				p := reverse(code>>(uint(l)-rootBits), rootBits)
				sb := uint(subBits[p])
				o := subStart[p]
				if o == 0 {
					o = len(h.t)
					subStart[p] = o
					h.t = append(h.t, make([]uint32, 1<<sb)...)
					h.t[p] = uint32(o)<<8 | 0x80 | uint32(sb)
				}
				rest := uint(l) - rootBits
				for x := reverse(code&(1<<rest-1), rest); x < 1<<sb; x += 1 << rest {
					h.t[o+int(x)] = s<<8 | uint32(l)
				}
			}
			code++
		}
		code <<= 1
	}
	return nil
}

func reverse(v uint32, n uint) uint32 {
	r := uint32(0)
	for i := uint(0); i < n; i++ {
		r = r<<1 | v&1
		v >>= 1
	}
	return r
}

// decode reads one symbol.
func (h *huffman) decode(b *bitReader) uint32 {
	if h.single {
		return h.sym
	}
	if b.n < 15 {
		b.refill()
	}
	e := h.t[b.buf&(1<<rootBits-1)]
	if e&0x80 != 0 {
		sb := uint(e & 0x7f)
		e = h.t[int(e>>8)+int(b.buf>>rootBits&(1<<sb-1))]
	}
	l := uint(e & 0xff)
	b.buf >>= l
	b.n -= l
	return e >> 8
}

var codeLengthCodeOrder = [19]uint8{17, 18, 0, 1, 2, 3, 4, 5, 16, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

// readCode reads a prefix code for an alphabet of the given size.
func readCode(b *bitReader, h *huffman, alphabet int) error {
	lens := make([]uint8, alphabet)
	if b.read(1) == 1 { // simple code
		nsym := b.read(1) + 1
		s0 := b.read(uint(1 + 7*b.read(1)))
		if int(s0) >= alphabet {
			return errorf("symbol %d", s0)
		}
		if nsym == 1 {
			h.single, h.sym, h.t = true, s0, h.t[:0]
			return nil
		}
		s1 := b.read(8)
		if int(s1) >= alphabet || s1 == s0 {
			return errorf("simple code symbols")
		}
		lens[s0], lens[s1] = 1, 1
		return h.build(lens)
	}
	var clens [19]uint8
	nc := int(b.read(4)) + 4
	for i := 0; i < nc; i++ {
		clens[codeLengthCodeOrder[i]] = uint8(b.read(3))
	}
	var lc huffman
	if err := lc.build(clens[:]); err != nil {
		return err
	}
	max := alphabet
	if b.read(1) == 1 {
		nbits := 2 + 2*b.read(3)
		max = 2 + int(b.read(uint(nbits)))
		if max > alphabet {
			return errorf("code length count")
		}
	}
	prev := uint8(8)
	for s := 0; s < alphabet && max > 0; max-- {
		c := lc.decode(b)
		switch {
		case c < 16:
			lens[s] = uint8(c)
			s++
			if c != 0 {
				prev = uint8(c)
			}
		default:
			rep, v := 0, uint8(0)
			switch c {
			case 16:
				rep, v = 3+int(b.read(2)), prev
			case 17:
				rep = 3 + int(b.read(3))
			default:
				rep = 11 + int(b.read(7))
			}
			if s+rep > alphabet {
				return errorf("code length repeat")
			}
			for ; rep > 0; rep-- {
				lens[s] = v
				s++
			}
		}
		if b.overrun() {
			return errorf("truncated")
		}
	}
	return h.build(lens)
}

// ---------------------------------------------------------------- VP8L

type transform struct {
	kind  int
	xsize int // image width when the transform was read
	bits  int
	data  []uint32 // predictor / colour transform sub-image, or palette
}

type group [5]huffman

type vp8l struct {
	b *bitReader
}

// DecodeVP8L decodes a VP8L bitstream (with its 5-byte header) to ARGB
// pixels.
func DecodeVP8L(data []byte) (w, h int, pix []uint32, hasAlpha bool, err error) {
	if len(data) < 5 || data[0] != 0x2f {
		return 0, 0, nil, false, errorf("VP8L signature")
	}
	b := &bitReader{src: data[1:]}
	w = int(b.read(14)) + 1
	h = int(b.read(14)) + 1
	hasAlpha = b.read(1) == 1
	if b.read(3) != 0 {
		return 0, 0, nil, false, errorf("VP8L version")
	}
	if w*h > MaxPixels {
		return 0, 0, nil, false, errorf("image too large")
	}
	pix, err = decodeImage(b, w, h)
	return w, h, pix, hasAlpha, err
}

// decodeVP8LStream decodes a header-less VP8L image stream of known size
// (alpha planes).
func decodeVP8LStream(data []byte, w, h int) ([]uint32, error) {
	return decodeImage(&bitReader{src: data}, w, h)
}

// decodeImage decodes the main (level 0) image with its transforms.
func decodeImage(b *bitReader, w, h int) ([]uint32, error) {
	var ts []transform
	xsize := w
	seen := 0
	for b.read(1) == 1 {
		kind := int(b.read(2))
		if seen&(1<<kind) != 0 {
			return nil, errorf("repeated transform")
		}
		seen |= 1 << kind
		t := transform{kind: kind, xsize: xsize}
		switch kind {
		case 0, 1: // predictor, colour transform
			t.bits = int(b.read(3)) + 2
			sub, err := decodeEntropyCoded(b, divUp(xsize, t.bits), divUp(h, t.bits), false)
			if err != nil {
				return nil, err
			}
			t.data = sub
		case 3: // colour indexing
			n := int(b.read(8)) + 1
			pal, err := decodeEntropyCoded(b, n, 1, false)
			if err != nil {
				return nil, err
			}
			for i := 1; i < n; i++ {
				pal[i] = addPixels(pal[i], pal[i-1])
			}
			full := make([]uint32, 256)
			copy(full, pal)
			t.data = full
			switch {
			case n <= 2:
				t.bits = 3
			case n <= 4:
				t.bits = 2
			case n <= 16:
				t.bits = 1
			}
			xsize = divUp(xsize, t.bits)
		}
		ts = append(ts, t)
	}
	pix, err := decodeEntropyCoded(b, xsize, h, true)
	if err != nil {
		return nil, err
	}
	for i := len(ts) - 1; i >= 0; i-- {
		pix = inverse(&ts[i], pix, h)
	}
	return pix, nil
}

func divUp(v, bits int) int { return (v + 1<<bits - 1) >> bits }

// decodeEntropyCoded decodes an entropy-coded image (the main one with
// meta prefix codes when level0).
func decodeEntropyCoded(b *bitReader, w, h int, level0 bool) ([]uint32, error) {
	if w*h > MaxPixels {
		return nil, errorf("image too large")
	}
	cacheBits := 0
	if b.read(1) == 1 {
		cacheBits = int(b.read(4))
		if cacheBits < 1 || cacheBits > 11 {
			return nil, errorf("colour cache bits %d", cacheBits)
		}
	}
	var meta []uint32
	metaBits, metaW := 0, 0
	ngroups := 1
	if level0 && b.read(1) == 1 {
		metaBits = int(b.read(3)) + 2
		metaW = divUp(w, metaBits)
		m, err := decodeEntropyCoded(b, metaW, divUp(h, metaBits), false)
		if err != nil {
			return nil, err
		}
		for i, v := range m {
			g := v >> 8 & 0xffff
			m[i] = g
			ngroups = max(ngroups, int(g)+1)
		}
		meta = m
	}
	if ngroups > 1<<16 {
		return nil, errorf("prefix code groups")
	}
	cacheSize := 0
	if cacheBits > 0 {
		cacheSize = 1 << cacheBits
	}
	groups := make([]group, ngroups)
	for i := range groups {
		for k, size := range [5]int{256 + 24 + cacheSize, 256, 256, 256, 40} {
			if err := readCode(b, &groups[i][k], size); err != nil {
				return nil, err
			}
		}
	}
	if b.overrun() {
		return nil, errorf("truncated")
	}
	pix := make([]uint32, w*h)
	var cache []uint32
	if cacheBits > 0 {
		cache = make([]uint32, cacheSize)
	}
	shift := 32 - uint(cacheBits)
	// the cache holds every pixel written, in order: lastCached is the
	// count of pixels inserted so far (inserted lazily before lookups)
	lastCached := 0
	flush := func(upto int) {
		for ; lastCached < upto; lastCached++ {
			v := pix[lastCached]
			cache[(0x1e35a7bd*v)>>shift] = v
		}
	}
	g := &groups[0]
	mask := 0
	if meta != nil {
		mask = 1<<metaBits - 1
	}
	x, y := 0, 0 // position of pixel p
	regroup := meta != nil
	for p := 0; p < len(pix); {
		if regroup || meta != nil && x&mask == 0 {
			g = &groups[meta[(y>>metaBits)*metaW+x>>metaBits]]
			regroup = false
		}
		s := g[0].decode(b)
		switch {
		case s < 256:
			r := g[1].decode(b)
			bl := g[2].decode(b)
			a := g[3].decode(b)
			pix[p] = a<<24 | r<<16 | s<<8 | bl
			p++
			if x++; x == w {
				x = 0
				y++
			}
			if b.overrun() {
				return nil, errorf("truncated")
			}
			continue
		case s < 256+24:
			length := prefixValue(b, s-256)
			dsym := g[4].decode(b)
			dist := distanceMap(w, prefixValue(b, dsym))
			if dist > p || p+length > len(pix) {
				return nil, errorf("backward reference")
			}
			for i := 0; i < length; i++ {
				pix[p+i] = pix[p+i-dist]
			}
			p += length
			x += length
			for x >= w {
				x -= w
				y++
			}
			regroup = meta != nil
		default:
			i := int(s) - 256 - 24
			if cache == nil || i >= cacheSize {
				return nil, errorf("colour cache index")
			}
			flush(p)
			pix[p] = cache[i]
			p++
			if x++; x == w {
				x = 0
				y++
			}
		}
		if b.overrun() {
			return nil, errorf("truncated")
		}
	}
	return pix, nil
}

// prefixValue decodes an LZ77 length or distance from its prefix symbol.
func prefixValue(b *bitReader, s uint32) int {
	if s < 4 {
		return int(s) + 1
	}
	extra := (s - 2) >> 1
	off := (2 + s&1) << extra
	return int(off+b.read(uint(extra))) + 1
}

var distanceMapTable = [120]uint8{
	0x18, 0x07, 0x17, 0x19, 0x28, 0x06, 0x27, 0x29, 0x16, 0x1a,
	0x26, 0x2a, 0x38, 0x05, 0x37, 0x39, 0x15, 0x1b, 0x36, 0x3a,
	0x25, 0x2b, 0x48, 0x04, 0x47, 0x49, 0x14, 0x1c, 0x35, 0x3b,
	0x46, 0x4a, 0x24, 0x2c, 0x58, 0x45, 0x4b, 0x34, 0x3c, 0x03,
	0x57, 0x59, 0x13, 0x1d, 0x56, 0x5a, 0x23, 0x2d, 0x44, 0x4c,
	0x55, 0x5b, 0x33, 0x3d, 0x68, 0x02, 0x67, 0x69, 0x12, 0x1e,
	0x66, 0x6a, 0x22, 0x2e, 0x54, 0x5c, 0x43, 0x4d, 0x65, 0x6b,
	0x32, 0x3e, 0x78, 0x01, 0x77, 0x79, 0x53, 0x5d, 0x11, 0x1f,
	0x64, 0x6c, 0x42, 0x4e, 0x76, 0x7a, 0x21, 0x2f, 0x75, 0x7b,
	0x31, 0x3f, 0x63, 0x6d, 0x52, 0x5e, 0x00, 0x74, 0x7c, 0x41,
	0x4f, 0x10, 0x20, 0x62, 0x6e, 0x30, 0x73, 0x7d, 0x51, 0x5f,
	0x40, 0x72, 0x7e, 0x61, 0x6f, 0x50, 0x71, 0x7f, 0x60, 0x70,
}

// distanceMap turns a distance code into a pixel distance (codes up to
// 120 are 2D neighbourhood offsets).
func distanceMap(w, code int) int {
	if code > 120 {
		return code - 120
	}
	c := int(distanceMapTable[code-1])
	if d := (c>>4)*w + 8 - c&0xf; d >= 1 {
		return d
	}
	return 1
}

// ---------------------------------------------------------------- transforms

func addPixels(a, b uint32) uint32 {
	ag := (a & 0xff00ff00) + (b & 0xff00ff00)
	rb := (a & 0x00ff00ff) + (b & 0x00ff00ff)
	return ag&0xff00ff00 | rb&0x00ff00ff
}

func average2(a, b uint32) uint32 {
	return (((a ^ b) & 0xfefefefe) >> 1) + (a & b)
}

func clamp255(v int32) uint32 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint32(v)
}

func clampAddSubFull(a, b, c uint32) uint32 {
	var r uint32
	for s := 0; s < 32; s += 8 {
		v := int32(a>>s&0xff) + int32(b>>s&0xff) - int32(c>>s&0xff)
		r |= clamp255(v) << s
	}
	return r
}

func clampAddSubHalf(a, b uint32) uint32 {
	var r uint32
	for s := 0; s < 32; s += 8 {
		x, y := int32(a>>s&0xff), int32(b>>s&0xff)
		r |= clamp255(x+(x-y)/2) << s
	}
	return r
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func selectPred(l, t, tl uint32) uint32 {
	var pl, pt int32
	for s := 0; s < 32; s += 8 {
		p := int32(l>>s&0xff) + int32(t>>s&0xff) - int32(tl>>s&0xff)
		pl += abs32(p - int32(l>>s&0xff))
		pt += abs32(p - int32(t>>s&0xff))
	}
	if pl < pt {
		return l
	}
	return t
}

func predict(mode uint32, l, t, tr, tl uint32) uint32 {
	switch mode {
	case 0:
		return 0xff000000
	case 1:
		return l
	case 2:
		return t
	case 3:
		return tr
	case 4:
		return tl
	case 5:
		return average2(average2(l, tr), t)
	case 6:
		return average2(l, tl)
	case 7:
		return average2(l, t)
	case 8:
		return average2(tl, t)
	case 9:
		return average2(t, tr)
	case 10:
		return average2(average2(l, tl), average2(t, tr))
	case 11:
		return selectPred(l, t, tl)
	case 12:
		return clampAddSubFull(l, t, tl)
	case 13:
		return clampAddSubHalf(average2(l, t), tl)
	}
	return 0xff000000 // modes 14 and 15 act as mode 0 (libwebp)
}

// inverse undoes transform t on pix (width t.xsize after it).
func inverse(t *transform, pix []uint32, h int) []uint32 {
	w := t.xsize
	switch t.kind {
	case 0: // predictor
		bw := divUp(w, t.bits)
		// first row: black then left
		pix[0] = addPixels(pix[0], 0xff000000)
		for x := 1; x < w; x++ {
			pix[x] = addPixels(pix[x], pix[x-1])
		}
		for y := 1; y < h; y++ {
			row := pix[y*w : (y+1)*w]
			up := pix[(y-1)*w : y*w]
			row[0] = addPixels(row[0], up[0])
			modes := t.data[(y>>t.bits)*bw:]
			for x := 1; x < w; x++ {
				m := modes[x>>t.bits] >> 8 & 0xf
				tr := pix[(y-1)*w+x+1] // for the last column: first pixel of this row
				p := predict(m, row[x-1], up[x], tr, up[x-1])
				row[x] = addPixels(row[x], p)
			}
		}
	case 1: // colour transform
		bw := divUp(w, t.bits)
		for y := 0; y < h; y++ {
			row := pix[y*w : (y+1)*w]
			els := t.data[(y>>t.bits)*bw:]
			for x := range row {
				e := els[x>>t.bits]
				g2r, g2b, r2b := int8(e), int8(e>>8), int8(e>>16)
				v := row[x]
				g := int8(v >> 8)
				r := int32(v>>16&0xff) + int32(g2r)*int32(g)>>5
				bl := int32(v&0xff) + int32(g2b)*int32(g)>>5
				bl += int32(r2b) * int32(int8(r)) >> 5
				row[x] = v&0xff00ff00 | uint32(r&0xff)<<16 | uint32(bl&0xff)
			}
		}
	case 2: // subtract green
		for i, v := range pix {
			g := v >> 8 & 0xff
			pix[i] = v&0xff00ff00 | (v+g<<16)&0x00ff0000 | (v+g)&0xff
		}
	case 3: // colour indexing
		pal := t.data
		if t.bits == 0 {
			for i, v := range pix {
				pix[i] = pal[v>>8&0xff]
			}
			return pix
		}
		pw := divUp(w, t.bits)
		out := make([]uint32, w*h)
		bpp := uint(8 >> t.bits)
		per := 1 << t.bits
		m := uint32(1)<<bpp - 1
		for y := 0; y < h; y++ {
			src := pix[y*pw : (y+1)*pw]
			dst := out[y*w : (y+1)*w]
			for x := range dst {
				v := src[x>>t.bits] >> 8
				dst[x] = pal[v>>(uint(x&(per-1))*bpp)&m]
			}
		}
		return out
	}
	return pix
}
