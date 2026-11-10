package jpeg

import (
	"errors"

	"gojxl/internal/bitio"
)

// Decompressor decompresses the Brotli stream at the end of a jbrd box.
type Decompressor func([]byte) ([]byte, error)

// ICC chunk types in the jbrd APP marker list.
const (
	appUnknown = 0
	appICC     = 1
	appExif    = 2
	appXMP     = 3
)

// ParseJBRD decodes a jbrd box into an Image holding the reconstruction
// data (without coefficients, frame size, sampling factors or quantisation
// values, which come from the JPEG XL codestream). icc is the codestream's
// ICC profile (for APP2 chunks stored by reference); exif and xmp are the
// container's Exif/xml box payloads.
func ParseJBRD(box []byte, brotli Decompressor, icc, exif, xmp []byte) (img *Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			img, err = nil, errors.New("jpeg: corrupt jbrd box")
		}
	}()
	br := bitio.NewReader(box)
	val, bo, bt := bitio.Val, bitio.BitsOff, bitio.Bits
	img = &Image{}
	isGray := br.Bool()
	numApp, numCom, numScans, numIM := 0, 0, 0, 0
	hasDRI := false
	for {
		m := byte(br.Read(6) + 0xc0)
		img.MarkerOrder = append(img.MarkerOrder, m)
		switch {
		case m&0xf0 == 0xe0:
			numApp++
		case m == 0xfe:
			numCom++
		case m == 0xda:
			numScans++
		case m == 0xff:
			numIM++
		case m == 0xdd:
			hasDRI = true
		}
		if m == 0xd9 {
			break
		}
		if len(img.MarkerOrder) > 16384 || br.Overflowed() {
			return nil, errors.New("jpeg: bad jbrd marker list")
		}
	}
	if numScans == 0 {
		return nil, errors.New("jpeg: no scans")
	}
	types := make([]int, numApp)
	img.AppData = make([][]byte, numApp)
	for i := range img.AppData {
		types[i] = int(br.U32(val(0), val(1), bo(1, 2), bo(2, 4)))
		if types[i] > appXMP {
			return nil, errors.New("jpeg: unknown app marker type")
		}
		n := int(br.Read(16)) + 1
		if n < 3 {
			return nil, errors.New("jpeg: bad marker size")
		}
		img.AppData[i] = make([]byte, n)
	}
	img.ComData = make([][]byte, numCom)
	for i := range img.ComData {
		n := int(br.Read(16)) + 1
		if n < 3 {
			return nil, errors.New("jpeg: bad marker size")
		}
		img.ComData[i] = make([]byte, n)
	}
	nq := int(br.U32(val(1), val(2), val(3), val(4)))
	if nq == 4 {
		return nil, errors.New("jpeg: bad quant table count")
	}
	img.Quants = make([]QuantTable, nq)
	for i := range img.Quants {
		img.Quants[i].Precision = int(br.Read(1))
		img.Quants[i].Index = int(br.Read(2))
		img.Quants[i].IsLast = br.Bool()
	}
	ctype := br.Read(2)
	var ids []int
	switch ctype {
	case 0:
		ids = []int{1}
	case 1:
		ids = []int{1, 2, 3}
	case 2:
		ids = []int{'R', 'G', 'B'}
	default:
		n := int(br.U32(val(1), val(2), val(3), val(4)))
		if n != 1 && n != 3 {
			return nil, errors.New("jpeg: bad component count")
		}
		for i := 0; i < n; i++ {
			ids = append(ids, int(br.Read(8)))
		}
	}
	if (len(ids) == 1) != isGray {
		return nil, errors.New("jpeg: inconsistent component count")
	}
	img.Comps = make([]Component, len(ids))
	for i := range img.Comps {
		img.Comps[i].ID = ids[i]
		img.Comps[i].QuantIdx = int(br.Read(2))
		if img.Comps[i].QuantIdx >= nq {
			return nil, errors.New("jpeg: bad quant table index")
		}
	}
	nh := int(br.U32(val(4), bo(3, 2), bo(4, 10), bo(6, 26)))
	img.Huff = make([]HuffCode, nh)
	for i := range img.Huff {
		h := &img.Huff[i]
		isAC := br.Bool()
		h.SlotID = int(br.Read(2))
		if isAC {
			h.SlotID |= 0x10
		}
		h.IsLast = br.Bool()
		n := 0
		for k := 0; k <= 16; k++ {
			h.Counts[k] = int(br.U32(val(0), val(1), bo(3, 2), bt(8)))
			n += h.Counts[k]
		}
		if n == 0 {
			continue
		}
		if n > 257 {
			return nil, errors.New("jpeg: Huffman code too large")
		}
		seen := map[int]bool{}
		for k := 0; k < n; k++ {
			v := int(br.U32(bt(2), bo(2, 4), bo(4, 8), bo(8, 1)))
			if v > 256 || seen[v] || (!isAC && v >= 12 && v != 256) {
				return nil, errors.New("jpeg: bad Huffman symbols")
			}
			seen[v] = true
			h.Values = append(h.Values, v)
		}
		if h.Values[n-1] != 256 {
			return nil, errors.New("jpeg: missing sentinel symbol")
		}
	}
	img.Scans = make([]Scan, numScans)
	for i := range img.Scans {
		s := &img.Scans[i]
		nc := int(br.U32(val(1), val(2), val(3), val(4)))
		if nc >= 4 {
			return nil, errors.New("jpeg: bad scan")
		}
		s.Ss, s.Se = int(br.Read(6)), int(br.Read(6))
		s.Al, s.Ah = int(br.Read(4)), int(br.Read(4))
		for k := 0; k < nc; k++ {
			c := ScanComp{CompIdx: int(br.Read(2))}
			c.ACTbl, c.DCTbl = int(br.Read(2)), int(br.Read(2))
			if c.CompIdx >= len(ids) {
				return nil, errors.New("jpeg: bad scan component")
			}
			s.Comps = append(s.Comps, c)
		}
		br.U32(val(0), val(1), val(2), bo(3, 3)) // last needed pass
	}
	if hasDRI {
		img.RestartInterval = int(br.Read(16))
	}
	for i := range img.Scans {
		s := &img.Scans[i]
		n := int(br.U32(val(0), bo(2, 1), bo(4, 4), bo(16, 20)))
		last := -1
		for k := 0; k < n; k++ {
			b := int(br.U32(val(0), bo(3, 1), bo(5, 9), bo(28, 41))) + last + 1
			if b >= 3<<26 {
				return nil, errors.New("jpeg: bad reset point")
			}
			s.ResetPoints = append(s.ResetPoints, b)
			last = b
		}
		n = int(br.U32(val(0), bo(2, 1), bo(4, 4), bo(16, 20)))
		last = -1
		for k := 0; k < n; k++ {
			z := int(br.U32(val(1), bo(2, 2), bo(4, 5), bo(8, 20)))
			b := int(br.U32(val(0), bo(3, 1), bo(5, 9), bo(28, 41))) + last + 1
			if z > 4 || b > 3<<26 {
				return nil, errors.New("jpeg: bad zero run")
			}
			s.ExtraZeroRuns = append(s.ExtraZeroRuns, ExtraZeroRun{b, z})
			last = b
		}
	}
	img.InterMarker = make([][]byte, numIM)
	for i := range img.InterMarker {
		img.InterMarker[i] = make([]byte, br.Read(16))
	}
	tail := int(br.U32(val(0), bo(8, 1), bo(16, 257), bo(22, 65793)))
	img.HasZeroPadding = br.Bool()
	if img.HasZeroPadding {
		n := int(br.Read(24))
		if n > len(box)*8 {
			return nil, errors.New("jpeg: bad padding bits")
		}
		img.PaddingBits = make([]byte, n)
		for i := range img.PaddingBits {
			if br.Bool() {
				img.PaddingBits[i] = 1
			}
		}
	}
	if err := br.ZeroPadToByte(); err != nil || br.Overflowed() {
		return nil, errors.New("jpeg: truncated jbrd box")
	}
	// Brotli-compressed payloads
	raw, err := brotli(box[br.BytePos():])
	if err != nil {
		return nil, err
	}
	take := func(n int) ([]byte, error) {
		if n > len(raw) {
			return nil, errors.New("jpeg: jbrd payload too short")
		}
		b := raw[:n]
		raw = raw[n:]
		return b, nil
	}
	numICC := 0
	for i, a := range img.AppData {
		if types[i] == appUnknown {
			b, err := take(len(a))
			if err != nil {
				return nil, err
			}
			copy(a, b)
			if int(a[1])<<8|int(a[2]) != len(a)-1 {
				return nil, errors.New("jpeg: bad marker size")
			}
			continue
		}
		n := len(a) - 1
		a[1], a[2] = byte(n>>8), byte(n)
		switch types[i] {
		case appICC:
			if len(a) < 17 {
				return nil, errors.New("jpeg: ICC marker too short")
			}
			a[0] = 0xe2
			copy(a[3:], "ICC_PROFILE\x00")
			numICC++
			a[15] = byte(numICC)
		case appExif, appXMP:
			a[0] = 0xe1
			tag, blob := "Exif\x00\x00", exif
			if types[i] == appXMP {
				tag, blob = "http://ns.adobe.com/xap/1.0/\x00", xmp
			}
			if len(a) < 3+len(tag) || len(blob) != len(a)-3-len(tag) {
				return nil, errors.New("jpeg: Exif/XMP marker does not match box")
			}
			copy(a[3:], tag)
			copy(a[3+len(tag):], blob)
		}
	}
	pos := 0
	for i, a := range img.AppData {
		if types[i] != appICC {
			continue
		}
		a[16] = byte(numICC)
		n := len(a) - 17
		if pos+n > len(icc) {
			return nil, errors.New("jpeg: ICC profile shorter than APP markers")
		}
		copy(a[17:], icc[pos:pos+n])
		pos += n
	}
	if pos != len(icc) && pos != 0 {
		return nil, errors.New("jpeg: ICC profile longer than APP markers")
	}
	for _, c := range img.ComData {
		b, err := take(len(c))
		if err != nil {
			return nil, err
		}
		copy(c, b)
	}
	for _, d := range img.InterMarker {
		b, err := take(len(d))
		if err != nil {
			return nil, err
		}
		copy(d, b)
	}
	t, err := take(tail)
	if err != nil {
		return nil, err
	}
	img.Tail = append([]byte(nil), t...)
	if len(raw) != 0 {
		return nil, errors.New("jpeg: excess jbrd payload")
	}
	return img, nil
}
