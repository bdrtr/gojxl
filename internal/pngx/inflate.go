package pngx

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

// A table-driven DEFLATE decoder for the case where the whole compressed
// stream is in memory and the decompressed size is known in advance (PNG
// image data): a 64-bit bit buffer refilled eight bytes at a time,
// one-lookup Huffman tables with length/distance bases folded into the
// entries, and back-references copied with doubling copies.

var errInflate = errors.New("png: corrupt deflate stream")

// Huffman table entries: bits 0-4 code length to consume, bits 5-7 kind,
// bits 8-11 extra bits (or subtable index bits), bits 16-31 value.
const (
	kLiteral = 0 << 5
	kLength  = 1 << 5 // also used for distances
	kEnd     = 2 << 5
	kSub     = 3 << 5
	kBad     = 4 << 5

	litBits  = 10
	distBits = 8
)

type huffTable struct {
	bits int
	t    []uint32
}

var (
	lenBase   = [29]uint16{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
	lenExtra  = [29]uint8{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	distBase  = [30]uint16{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
	distExtra = [30]uint8{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
	clOrder   = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
)

// symEntry is the table entry (without the length) of a symbol.
type symEntry func(sym int) uint32

func litEntry(sym int) uint32 {
	switch {
	case sym < 256:
		return kLiteral | uint32(sym)<<16
	case sym == 256:
		return kEnd
	case sym < 286:
		i := sym - 257
		return kLength | uint32(lenExtra[i])<<8 | uint32(lenBase[i])<<16
	}
	return kBad
}

func distEntry(sym int) uint32 {
	if sym < 30 {
		return kLength | uint32(distExtra[sym])<<8 | uint32(distBase[sym])<<16
	}
	return kBad
}

// build fills h for the code lengths (canonical code, LSB-first lookup).
func (h *huffTable) build(lens []uint8, primary int, entry symEntry) error {
	var count [16]int
	for _, l := range lens {
		count[l]++
	}
	count[0] = 0
	var next [16]int
	code, left := 0, 1
	for l := 1; l < 16; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
		left = left<<1 - count[l]
		if left < 0 {
			return errInflate // over-subscribed
		}
	}
	// subtable sizes per primary prefix
	var need [1 << litBits]uint8
	mask := 1<<primary - 1
	codes := make([]int, len(lens))
	for s, l := range lens {
		if l == 0 {
			continue
		}
		codes[s] = next[l]
		next[l]++
		if int(l) > primary {
			r := int(bits.Reverse16(uint16(codes[s]))) >> (16 - int(l))
			p := r & mask
			need[p] = max(need[p], l-uint8(primary))
		}
	}
	size := 1 << primary
	var off [1 << litBits]int
	for p := 0; p <= mask; p++ {
		if need[p] > 0 {
			off[p] = size
			size += 1 << need[p]
		}
	}
	if cap(h.t) < size {
		h.t = make([]uint32, size)
	}
	h.t = h.t[:size]
	for i := range h.t {
		h.t[i] = kBad
	}
	h.bits = primary
	for p := 0; p <= mask; p++ {
		if need[p] > 0 {
			h.t[p] = uint32(primary) | kSub | uint32(need[p])<<8 | uint32(off[p])<<16
		}
	}
	for s, l := range lens {
		if l == 0 {
			continue
		}
		L := int(l)
		r := int(bits.Reverse16(uint16(codes[s]))) >> (16 - L)
		e := entry(s)
		if L <= primary {
			for i := r; i <= mask; i += 1 << L {
				h.t[i] = e | uint32(L)
			}
			continue
		}
		p, rest, sl := r&mask, r>>primary, L-primary
		nb := int(need[p])
		for i := rest; i < 1<<nb; i += 1 << sl {
			h.t[off[p]+i] = e | uint32(sl)
		}
	}
	return nil
}

var fixedLit, fixedDist huffTable

func init() {
	var l [288]uint8
	for i := range l {
		switch {
		case i < 144:
			l[i] = 8
		case i < 256:
			l[i] = 9
		case i < 280:
			l[i] = 7
		default:
			l[i] = 8
		}
	}
	if err := fixedLit.build(l[:], litBits, litEntry); err != nil {
		panic(err)
	}
	var d [30]uint8
	for i := range d {
		d[i] = 5
	}
	if err := fixedDist.build(d[:], distBits, distEntry); err != nil {
		panic(err)
	}
}

type bitReader struct {
	src []byte
	pos int // next byte to load (may run past len(src) by zero padding)
	buf uint64
	n   uint
}

// refill makes at least 56 bits available.
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
		}
		b.pos++
		b.n += 8
	}
}

func (b *bitReader) take(n uint) uint64 {
	v := b.buf & (1<<n - 1)
	b.buf >>= n
	b.n -= n
	return v
}

// decode reads one symbol's table entry (refill done by the caller).
func (h *huffTable) decode(b *bitReader) uint32 {
	e := h.t[b.buf&(1<<uint(h.bits)-1)]
	if e&0xe0 == kSub {
		b.buf >>= uint(h.bits)
		b.n -= uint(h.bits)
		e = h.t[int(e>>16)+int(b.buf&(1<<(e>>8&15)-1))]
	}
	l := uint(e & 31)
	b.buf >>= l
	b.n -= l
	return e
}

// inflateZlib decompresses a zlib stream into dst, which must have exactly
// the decompressed size. The Adler-32 checksum is not verified.
// If progress is non-nil, the number of bytes completed is sent to it at
// least every 1<<progressShift bytes (it needs that much buffer space).
func inflateZlib(src, dst []byte, progress chan<- int) error {
	if len(src) < 2 || src[0]&15 != 8 || (uint16(src[0])<<8|uint16(src[1]))%31 != 0 || src[1]&0x20 != 0 {
		return errors.New("png: bad zlib header")
	}
	return inflate(src[2:], dst, progress)
}

const progressShift = 16

func inflate(src, dst []byte, progress chan<- int) error {
	b := &bitReader{src: src}
	var lit, dist huffTable
	o := 0
	next := len(dst) + 1 // next progress report
	if progress != nil {
		next = 1 << progressShift
	}
	for {
		b.refill()
		if b.pos > len(src)+16 {
			return errInflate
		}
		final := b.take(1)
		typ := b.take(2)
		var lt, dt *huffTable
		switch typ {
		case 0: // stored
			b.take(b.n & 7)
			p := b.pos - int(b.n>>3)
			b.buf, b.n = 0, 0
			if p+4 > len(src) {
				return errInflate
			}
			n := int(binary.LittleEndian.Uint16(src[p:]))
			if uint16(n) != ^binary.LittleEndian.Uint16(src[p+2:]) || p+4+n > len(src) || o+n > len(dst) {
				return errInflate
			}
			copy(dst[o:], src[p+4:p+4+n])
			o += n
			if o >= next {
				progress <- o
				next = o + 1<<progressShift
			}
			b.pos = p + 4 + n
			if final == 1 {
				goto done
			}
			continue
		case 1:
			lt, dt = &fixedLit, &fixedDist
		case 2:
			if err := readDynamic(b, &lit, &dist); err != nil {
				return err
			}
			lt, dt = &lit, &dist
		default:
			return errInflate
		}
		for {
			if o >= next {
				progress <- o
				next = o + 1<<progressShift
			}
			b.refill()
			e := lt.decode(b)
			switch e & 0xe0 {
			case kLiteral:
				if o >= len(dst) {
					return errInflate
				}
				dst[o] = byte(e >> 16)
				o++
				// a second literal without refilling (>= 41 bits left)
				e2 := lt.t[b.buf&(1<<litBits-1)]
				if e2&0xe0 == kLiteral && o < len(dst) {
					l := uint(e2 & 31)
					b.buf >>= l
					b.n -= l
					dst[o] = byte(e2 >> 16)
					o++
				}
				continue
			case kEnd:
			case kLength:
				length := int(e>>16) + int(b.take(uint(e>>8&15)))
				d := dt.decode(b)
				if d&0xe0 != kLength {
					return errInflate
				}
				dd := int(d>>16) + int(b.take(uint(d>>8&15)))
				if dd > o || o+length > len(dst) {
					return errInflate
				}
				if dd >= length {
					copy(dst[o:o+length], dst[o-dd:])
				} else if dd == 1 {
					v := dst[o-1]
					for i := o; i < o+length; i++ {
						dst[i] = v
					}
				} else {
					for w := 0; w < length; {
						w += copy(dst[o+w:o+length], dst[o-dd:o+w])
					}
				}
				o += length
				continue
			default:
				return errInflate
			}
			break
		}
		if b.pos-int(b.n>>3) > len(src) {
			return errInflate // ran past the input
		}
		if final == 1 {
			break
		}
	}
done:
	if o != len(dst) {
		return errInflate
	}
	if progress != nil {
		progress <- o
	}
	return nil
}

func readDynamic(b *bitReader, lit, dist *huffTable) error {
	b.refill()
	nlit := int(b.take(5)) + 257
	ndist := int(b.take(5)) + 1
	ncl := int(b.take(4)) + 4
	if nlit > 286 || ndist > 30 {
		return errInflate
	}
	var cl [19]uint8
	for i := 0; i < ncl; i++ {
		if b.n < 3 {
			b.refill()
		}
		cl[clOrder[i]] = uint8(b.take(3))
	}
	var clt huffTable
	if err := clt.build(cl[:], 7, func(s int) uint32 { return uint32(s) << 16 }); err != nil {
		return err
	}
	var lens [286 + 30]uint8
	for i := 0; i < nlit+ndist; {
		b.refill()
		e := clt.decode(b)
		if e&0xe0 == kBad {
			return errInflate
		}
		s := int(e >> 16)
		if s < 16 {
			lens[i] = uint8(s)
			i++
			continue
		}
		var rep int
		var v uint8
		switch s {
		case 16:
			if i == 0 {
				return errInflate
			}
			v, rep = lens[i-1], 3+int(b.take(2))
		case 17:
			rep = 3 + int(b.take(3))
		default:
			rep = 11 + int(b.take(7))
		}
		if i+rep > nlit+ndist {
			return errInflate
		}
		for ; rep > 0; rep-- {
			lens[i] = v
			i++
		}
	}
	if lens[256] == 0 {
		return errInflate
	}
	if err := lit.build(lens[:nlit], litBits, litEntry); err != nil {
		return err
	}
	return dist.build(lens[nlit:nlit+ndist], distBits, distEntry)
}

// InflateZlib decompresses a zlib stream into dst, which must have exactly
// the decompressed size (other packages' entry to the fast inflater).
func InflateZlib(src, dst []byte) error { return inflateZlib(src, dst, nil) }
