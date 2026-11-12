package bitio

import "encoding/binary"

// Fast is a bit writer for hot loops: it keeps up to 31 pending bits and
// flushes 32 at a time. Take it with (*Writer).Fast, write with Put and
// hand it back with (*Writer).SetFast before using the Writer again.
type Fast struct {
	buf []byte
	acc uint64
	n   uint
}

// Fast returns the writer's state as a Fast writer.
func (w *Writer) Fast() Fast { return Fast{w.buf, w.acc, w.n} }

// SetFast stores f back into w.
func (w *Writer) SetFast(f Fast) {
	for f.n >= 8 {
		f.buf = append(f.buf, byte(f.acc))
		f.acc >>= 8
		f.n -= 8
	}
	w.buf, w.acc, w.n = f.buf, f.acc, f.n
}

// Put writes the n low bits of v; v must have no bits above them and n
// must be at most 32.
func (f *Fast) Put(n uint, v uint64) {
	f.acc |= v << f.n
	f.n += n
	if f.n >= 32 {
		f.buf = binary.LittleEndian.AppendUint32(f.buf, uint32(f.acc))
		f.acc >>= 32
		f.n -= 32
	}
}
