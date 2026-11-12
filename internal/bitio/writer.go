package bitio

// Writer writes bits LSB-first.
type Writer struct {
	buf []byte
	acc uint64
	n   uint
}

// Write writes the low n bits of v (n <= 56).
func (w *Writer) Write(n uint, v uint64) {
	if n == 0 {
		return
	}
	w.acc |= (v & (1<<n - 1)) << w.n
	w.n += n
	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

// WriteBool writes one bit.
func (w *Writer) WriteBool(b bool) {
	if b {
		w.Write(1, 1)
	} else {
		w.Write(1, 0)
	}
}

// BitsWritten returns the number of bits written so far.
func (w *Writer) BitsWritten() int { return len(w.buf)*8 + int(w.n) }

// ZeroPadToByte pads with zero bits to a byte boundary.
func (w *Writer) ZeroPadToByte() {
	if w.n > 0 {
		w.Write(8-w.n, 0)
	}
}

// Bytes pads to a byte boundary and returns the data.
func (w *Writer) Bytes() []byte {
	w.ZeroPadToByte()
	return w.buf
}

// Grow makes room for n more bytes.
func (w *Writer) Grow(n int) {
	if cap(w.buf)-len(w.buf) < n {
		nb := make([]byte, len(w.buf), len(w.buf)+n)
		copy(nb, w.buf)
		w.buf = nb
	}
}

// AppendBytes appends byte-aligned data (the writer must be byte aligned).
func (w *Writer) AppendBytes(b []byte) {
	if w.n != 0 {
		for _, c := range b {
			w.Write(8, uint64(c))
		}
		return
	}
	w.buf = append(w.buf, b...)
}

// WriteU32 encodes v with the first matching distribution.
func (w *Writer) WriteU32(v uint32, d0, d1, d2, d3 U32Dist) {
	ds := [4]U32Dist{d0, d1, d2, d3}
	for i, d := range ds {
		if d.Bits == 0 {
			if v == d.Off {
				w.Write(2, uint64(i))
				return
			}
			continue
		}
		if v >= d.Off && uint64(v-d.Off) < 1<<d.Bits {
			w.Write(2, uint64(i))
			w.Write(uint(d.Bits), uint64(v-d.Off))
			return
		}
	}
	panic("bitio: value not representable in U32 distribution")
}

// WriteU64 encodes a U64 field.
func (w *Writer) WriteU64(v uint64) {
	switch {
	case v == 0:
		w.Write(2, 0)
	case v <= 16:
		w.Write(2, 1)
		w.Write(4, v-1)
	case v <= 272:
		w.Write(2, 2)
		w.Write(8, v-17)
	default:
		w.Write(2, 3)
		w.Write(12, v&0xfff)
		v >>= 12
		shift := uint(12)
		for v > 0 {
			w.Write(1, 1)
			if shift == 60 {
				w.Write(4, v&0xf)
				return
			}
			w.Write(8, v&0xff)
			v >>= 8
			shift += 8
		}
		w.Write(1, 0)
	}
}

// WriteEnum writes an enum value.
func (w *Writer) WriteEnum(v uint32) {
	w.WriteU32(v, Val(0), Val(1), BitsOff(4, 2), BitsOff(6, 18))
}

// PackSigned maps signed to the zig-zag unsigned encoding.
func PackSigned(v int32) uint32 {
	return uint32(v<<1) ^ uint32(v>>31)
}

// AppendWriter appends all bits written to o.
func (w *Writer) AppendWriter(o *Writer) {
	for _, c := range o.buf {
		w.Write(8, uint64(c))
	}
	w.Write(o.n, o.acc)
}
