// Package bitio implements the LSB-first bit reader used by the JPEG XL
// codestream (ISO/IEC 18181-1, section 4/9).
package bitio

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrEOF is reported when the reader runs past the end of its data.
var ErrEOF = errors.New("jxl: unexpected end of bitstream")

// ErrInvalid is a generic "invalid value" error for header fields.
var ErrInvalid = errors.New("jxl: invalid bitstream")

// Reader reads bits LSB-first from an in-memory byte slice.
//
// Reading past the end yields zero bits and sets an overflow flag which is
// reported by Err(). This keeps the hot paths branch-light; callers check Err()
// at section boundaries.
type Reader struct {
	data  []byte
	pos   int    // next byte of data to be loaded into buf
	buf   uint64 // bit buffer, LSB = next bit
	nbits uint   // number of valid bits in buf
	over  int    // number of bytes "read" past the end (zeros)
}

// NewReader returns a reader over data.
func NewReader(data []byte) *Reader {
	r := &Reader{data: data}
	r.refill()
	return r
}

// Reset re-targets the reader at new data.
func (r *Reader) Reset(data []byte) {
	*r = Reader{data: data}
	r.refill()
}

func (r *Reader) refill() {
	if r.pos+8 <= len(r.data) {
		v := binary.LittleEndian.Uint64(r.data[r.pos:])
		r.buf |= v << r.nbits
		n := (63 - r.nbits) >> 3
		r.pos += int(n)
		r.nbits += n << 3
		return
	}
	for r.nbits <= 56 {
		var b byte
		if r.pos < len(r.data) {
			b = r.data[r.pos]
		} else {
			r.over++
		}
		r.pos++
		r.buf |= uint64(b) << r.nbits
		r.nbits += 8
	}
}

// Refill makes sure at least 56 bits are buffered.
func (r *Reader) Refill() {
	if r.nbits < 56 {
		r.refill()
	}
}

// Peek returns the next n bits (n <= 56) without consuming them.
// The caller must make sure enough bits are buffered (via Refill).
func (r *Reader) Peek(n uint) uint64 {
	return r.buf & ((1 << n) - 1)
}

// PeekRefill returns the next n bits (n <= 56), refilling if needed.
func (r *Reader) PeekRefill(n uint) uint64 {
	if r.nbits < n {
		r.refill()
	}
	return r.buf & ((1 << n) - 1)
}

// Consume drops n buffered bits.
func (r *Reader) Consume(n uint) {
	r.buf >>= n
	r.nbits -= n
}

// Read reads n bits (0 <= n <= 32).
func (r *Reader) Read(n uint) uint32 {
	if r.nbits < n {
		r.refill()
	}
	n &= 63
	v := r.buf & (1<<n - 1)
	r.buf >>= n
	r.nbits -= n
	return uint32(v)
}

// State exposes the raw bit buffer for hot decoding loops: callers may
// consume bits from buf (LSB first) as long as they keep nbits in sync and
// call SetState afterwards. Refill must be called before taking the state
// to guarantee at least 56 valid bits.
func (r *Reader) State() (buf uint64, nbits uint) { return r.buf, r.nbits }

// SetState stores back a buffer state obtained from State.
func (r *Reader) SetState(buf uint64, nbits uint) { r.buf, r.nbits = buf, nbits }

// Read64 reads up to 64 bits.
func (r *Reader) Read64(n uint) uint64 {
	if n <= 32 {
		return uint64(r.Read(n))
	}
	lo := uint64(r.Read(32))
	hi := uint64(r.Read(n - 32))
	return lo | hi<<32
}

// Bool reads a single bit.
func (r *Reader) Bool() bool { return r.Read(1) == 1 }

// BitsRead returns the number of bits consumed so far.
func (r *Reader) BitsRead() int { return r.pos*8 - int(r.nbits) }

// ZeroPadToByte skips to the next byte boundary.
func (r *Reader) ZeroPadToByte() error {
	n := uint(r.BitsRead() & 7)
	if n != 0 {
		if r.Read(8-n) != 0 {
			return ErrInvalid
		}
	}
	return nil
}

// SkipBits skips n bits in constant time; skipping past the end of the
// data leaves the reader overflowed (reading zeros).
func (r *Reader) SkipBits(n uint64) {
	if n <= uint64(r.nbits) {
		r.Consume(uint(n))
		return
	}
	n -= uint64(r.nbits)
	r.buf, r.nbits = 0, 0
	if n/8 > uint64(max(0, len(r.data)-r.pos))+8 {
		r.pos = max(r.pos, len(r.data)) + 8
		r.over += 8
		return
	}
	r.pos += int(n / 8)
	r.refill()
	r.Consume(uint(n % 8))
}

// BytePos returns the byte offset (must be byte aligned).
func (r *Reader) BytePos() int { return r.BitsRead() >> 3 }

// Data returns the underlying slice.
func (r *Reader) Data() []byte { return r.data }

// Err reports whether the reader ran out of data.
func (r *Reader) Err() error {
	if r.BitsRead() > len(r.data)*8 {
		return ErrEOF
	}
	return nil
}

// Overflowed reports whether more bits were consumed than available.
func (r *Reader) Overflowed() bool { return r.BitsRead() > len(r.data)*8 }

// U32Dist describes one of the four distributions of a U32 field.
// If Bits == 0 the value is the constant Off, otherwise Off + u(Bits).
type U32Dist struct {
	Off  uint32
	Bits uint8
}

// Val is a constant distribution.
func Val(v uint32) U32Dist { return U32Dist{Off: v} }

// Bits is u(n).
func Bits(n uint8) U32Dist { return U32Dist{Bits: n} }

// BitsOff is off + u(n).
func BitsOff(n uint8, off uint32) U32Dist { return U32Dist{Off: off, Bits: n} }

// U32 reads a U32 field with the given four distributions.
func (r *Reader) U32(d0, d1, d2, d3 U32Dist) uint32 {
	var d U32Dist
	switch r.Read(2) {
	case 0:
		d = d0
	case 1:
		d = d1
	case 2:
		d = d2
	default:
		d = d3
	}
	if d.Bits == 0 {
		return d.Off
	}
	return d.Off + r.Read(uint(d.Bits))
}

// U64 reads a U64 field.
func (r *Reader) U64() uint64 {
	switch r.Read(2) {
	case 0:
		return 0
	case 1:
		return 1 + uint64(r.Read(4))
	case 2:
		return 17 + uint64(r.Read(8))
	}
	v := uint64(r.Read(12))
	shift := uint(12)
	for r.Bool() {
		if shift == 60 {
			v |= uint64(r.Read(4)) << shift
			break
		}
		v |= uint64(r.Read(8)) << shift
		shift += 8
	}
	return v
}

// F16 reads a half precision float. NaN/Inf are rejected.
func (r *Reader) F16() (float32, error) {
	bits := r.Read(16)
	mant := bits & 0x3ff
	exp := (bits >> 10) & 0x1f
	sign := bits >> 15
	if exp == 31 {
		return 0, ErrInvalid
	}
	var v float32
	if exp == 0 {
		v = float32(mant) / (1 << 24)
	} else {
		v = math.Float32frombits(((exp + 112) << 23) | (mant << 13))
	}
	if sign != 0 {
		v = -v
	}
	return v, nil
}

// Enum reads an enum value.
func (r *Reader) Enum() (uint32, error) {
	v := r.U32(Val(0), Val(1), BitsOff(4, 2), BitsOff(6, 18))
	if v >= 64 {
		return 0, ErrInvalid
	}
	return v, nil
}

// UnpackSigned maps the unsigned zig-zag encoding to signed.
func UnpackSigned(v uint32) int32 {
	return int32(v>>1) ^ -int32(v&1)
}

// UnpackSigned64 maps the unsigned zig-zag encoding to signed (64 bit).
func UnpackSigned64(v uint64) int64 {
	return int64(v>>1) ^ -int64(v&1)
}
