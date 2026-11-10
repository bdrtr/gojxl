package enc

import (
	"encoding/binary"
	"errors"
	"fmt"

	"gojxl/internal/bitio"
	"gojxl/internal/jpeg"
)

// JPEG bitstream reconstruction data ('jbrd' box): the information, besides
// the DCT coefficients stored in the codestream, that a decoder needs to
// rebuild the original JPEG file byte for byte.

var errJBRD = errors.New("jpeg: cannot represent file for exact reconstruction")

// brotliStored wraps data in a valid Brotli stream made of uncompressed
// meta-blocks.
func brotliStored(data []byte) []byte {
	w := &bitio.Writer{}
	w.Write(1, 0) // WBITS = 16
	for len(data) > 0 {
		n := min(len(data), 1<<16)
		w.Write(1, 0) // ISLAST
		w.Write(2, 0) // MNIBBLES = 4
		w.Write(16, uint64(n-1))
		w.Write(1, 1) // ISUNCOMPRESSED
		w.ZeroPadToByte()
		w.AppendBytes(data[:n])
		data = data[n:]
	}
	w.Write(1, 1) // ISLAST
	w.Write(1, 1) // ISLASTEMPTY
	w.ZeroPadToByte()
	return w.Bytes()
}

// EncodeJBRD serialises the reconstruction data of a parsed JPEG.
func EncodeJBRD(j *jpeg.Image) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%w (%v)", errJBRD, r)
		}
	}()
	w := &bitio.Writer{}
	bits := func(n uint, v int) {
		if v < 0 || uint64(v) >= 1<<n {
			panic(fmt.Sprintf("value %d does not fit in %d bits", v, n))
		}
		w.Write(n, uint64(v))
	}
	u32 := func(v int, d0, d1, d2, d3 bitio.U32Dist) {
		if v < 0 {
			panic("negative value")
		}
		w.WriteU32(uint32(v), d0, d1, d2, d3)
	}
	val, bo, bt := bitio.Val, bitio.BitsOff, bitio.Bits

	w.WriteBool(len(j.Comps) == 1)
	hasDRI := false
	scans := 0
	for _, m := range j.MarkerOrder {
		bits(6, int(m)-0xc0)
		if m == 0xdd {
			hasDRI = true
		}
		if m == 0xda {
			scans++
		}
	}
	if len(j.MarkerOrder) == 0 || j.MarkerOrder[len(j.MarkerOrder)-1] != 0xd9 || scans != len(j.Scans) {
		return nil, errJBRD
	}
	types := iccMarkerTypes(j)
	for i, a := range j.AppData {
		// 0: stored as is; 1: ICC chunk rebuilt from the codestream profile
		u32(types[i], val(0), val(1), bo(1, 2), bo(2, 4))
		bits(16, len(a)-1)
	}
	for _, c := range j.ComData {
		bits(16, len(c)-1)
	}
	nq := len(j.Quants)
	if nq < 1 || nq > 3 {
		return nil, errJBRD
	}
	u32(nq, val(1), val(2), val(3), val(4))
	for _, q := range j.Quants {
		bits(1, q.Precision)
		bits(2, q.Index)
		w.WriteBool(q.IsLast)
	}
	ids := make([]int, len(j.Comps))
	for i, c := range j.Comps {
		ids[i] = c.ID
	}
	switch {
	case len(ids) == 1 && ids[0] == 1:
		bits(2, 0)
	case len(ids) == 3 && ids[0] == 1 && ids[1] == 2 && ids[2] == 3:
		bits(2, 1)
	case len(ids) == 3 && ids[0] == 'R' && ids[1] == 'G' && ids[2] == 'B':
		bits(2, 2)
	default:
		bits(2, 3)
		u32(len(ids), val(1), val(2), val(3), val(4))
		for _, id := range ids {
			bits(8, id)
		}
	}
	used := 0
	for _, c := range j.Comps {
		bits(2, c.QuantIdx)
		used |= 1 << c.QuantIdx
	}
	for i := range j.Quants {
		if used&(1<<i) == 0 && (i == 0 || j.Quants[i].Values != j.Quants[i-1].Values) {
			return nil, errJBRD // unused, non-trivial quant table
		}
	}
	u32(len(j.Huff), val(4), bo(3, 2), bo(4, 10), bo(6, 26))
	for _, h := range j.Huff {
		w.WriteBool(h.SlotID&0x10 != 0)
		bits(2, h.SlotID&0xf)
		w.WriteBool(h.IsLast)
		n := 0
		for i := 0; i <= 16; i++ {
			u32(h.Counts[i], val(0), val(1), bo(3, 2), bt(8))
			n += h.Counts[i]
		}
		if n == 0 {
			continue
		}
		if n != len(h.Values) {
			return nil, errJBRD
		}
		for _, v := range h.Values {
			u32(v, bt(2), bo(2, 4), bo(4, 8), bo(8, 1))
		}
	}
	for _, s := range j.Scans {
		u32(len(s.Comps), val(1), val(2), val(3), val(4))
		bits(6, s.Ss)
		bits(6, s.Se)
		bits(4, s.Al)
		bits(4, s.Ah)
		for _, c := range s.Comps {
			bits(2, c.CompIdx)
			bits(2, c.ACTbl)
			bits(2, c.DCTbl)
		}
		u32(0, val(0), val(1), val(2), bo(3, 3)) // last needed pass
	}
	if hasDRI {
		bits(16, j.RestartInterval)
	}
	for _, s := range j.Scans {
		u32(len(s.ResetPoints), val(0), bo(2, 1), bo(4, 4), bo(16, 20))
		last := -1
		for _, b := range s.ResetPoints {
			u32(b-(last+1), val(0), bo(3, 1), bo(5, 9), bo(28, 41))
			last = b
		}
		u32(len(s.ExtraZeroRuns), val(0), bo(2, 1), bo(4, 4), bo(16, 20))
		last = -1
		for _, z := range s.ExtraZeroRuns {
			u32(z.N, val(1), bo(2, 2), bo(4, 5), bo(8, 20))
			u32(z.Block-(last+1), val(0), bo(3, 1), bo(5, 9), bo(28, 41))
			last = z.Block
		}
	}
	for _, d := range j.InterMarker {
		bits(16, len(d))
	}
	u32(len(j.Tail), val(0), bo(8, 1), bo(16, 257), bo(22, 65793))
	w.WriteBool(j.HasZeroPadding)
	if j.HasZeroPadding {
		bits(24, len(j.PaddingBits))
		for _, b := range j.PaddingBits {
			w.WriteBool(b != 0)
		}
	}
	w.ZeroPadToByte()
	var blob []byte
	for i, a := range j.AppData {
		if types[i] == 0 {
			blob = append(blob, a...)
		}
	}
	for _, c := range j.ComData {
		blob = append(blob, c...)
	}
	for _, d := range j.InterMarker {
		blob = append(blob, d...)
	}
	blob = append(blob, j.Tail...)
	w.AppendBytes(brotliStored(blob))
	return w.Bytes(), nil
}

// box appends an ISO BMFF box.
func box(out []byte, typ string, payload []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(8+len(payload)))
	out = append(out, typ...)
	return append(out, payload...)
}

// WrapContainer puts a codestream into a JPEG XL container, with an
// optional jbrd box.
func WrapContainer(codestream, jbrd []byte) []byte {
	out := []byte{0, 0, 0, 0xc, 'J', 'X', 'L', ' ', 0xd, 0xa, 0x87, 0xa}
	out = box(out, "ftyp", []byte("jxl \x00\x00\x00\x00jxl "))
	if jbrd != nil {
		out = box(out, "jbrd", jbrd)
	}
	return box(out, "jxlc", codestream)
}

// iccMarkerTypes marks the APP2 ICC chunks (libjxl's DetectIccProfile
// rule) whose concatenation is the profile stored in the codestream.
func iccMarkerTypes(j *jpeg.Image) []int {
	types := make([]int, len(j.AppData))
	if len(j.ICC) < 132 {
		return types
	}
	const tag = "ICC_PROFILE\x00"
	var icc []byte
	n, total := 0, 0
	for i, a := range j.AppData {
		if a[0] != 0xe2 || len(a) < 3+len(tag)+2 || string(a[3:3+len(tag)]) != tag {
			continue
		}
		id, cnt := int(a[3+len(tag)]), int(a[4+len(tag)])
		if id != n+1 {
			continue
		}
		if total == 0 {
			total = cnt
		}
		if cnt != total {
			continue
		}
		n++
		types[i] = 1
		icc = append(icc, a[17:]...)
	}
	if n != total || string(icc) != string(j.ICC) {
		return make([]int, len(j.AppData))
	}
	return types
}
