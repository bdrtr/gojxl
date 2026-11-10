// Package jpeg parses JPEG files down to quantised DCT coefficients
// (baseline and progressive Huffman coding) and records everything needed
// to reproduce the file bit-exactly from those coefficients (the data that
// JPEG XL stores in its 'jbrd' box). The parsing logic follows libjxl's
// JPEG reader so that the recorded data has the same meaning.
package jpeg

import (
	"errors"
	"fmt"
)

// ErrUnsupported reports JPEG features that are not handled.
var ErrUnsupported = errors.New("jpeg: unsupported feature")

var errInvalid = errors.New("jpeg: invalid file")

// NaturalOrder maps zigzag index -> natural (row-major) index.
var NaturalOrder = [64 + 16]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
	// extra entries for safety in the decoding loops
	63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63, 63,
}

// Component is one colour component with its quantised coefficients.
type Component struct {
	ID       int
	H, V     int // sampling factors
	Tq       int // quantisation table id (0..3) from SOF
	QuantIdx int // index into Image.Quants
	BW, BH   int // blocks (MCU padded)
	// Coef holds BW*BH blocks of 64 coefficients in natural (row-major,
	// coef[v*8+u]) order.
	Coef []int16
}

// QuantTable is one DQT table as it appeared in the file.
type QuantTable struct {
	Index     int
	Precision int
	IsLast    bool // last table of its DQT marker
	Values    [64]uint16
}

// HuffCode is one DHT table as it appeared in the file, with libjxl's
// sentinel symbol 256 appended (all-ones code at the maximum length).
type HuffCode struct {
	SlotID int // table id, +16 for AC tables
	IsLast bool
	Counts [17]int
	Values []int
}

// ScanComp is one component of a scan.
type ScanComp struct{ CompIdx, DCTbl, ACTbl int }

// ExtraZeroRun records zero runs (ZRL symbols) before an end of block.
type ExtraZeroRun struct{ Block, N int }

// Scan describes one SOS.
type Scan struct {
	Comps          []ScanComp
	Ss, Se, Ah, Al int
	ResetPoints    []int
	ExtraZeroRuns  []ExtraZeroRun
}

// Image is a parsed JPEG.
type Image struct {
	W, H        int
	Comps       []Component
	Quant       [4][64]uint16 // natural order, by table id (first definition)
	QuantUsed   [4]bool
	Progressive bool
	ICC         []byte
	EXIF        []byte
	Adobe       bool // APP14 "Adobe" present
	AdobeTransf int
	JFIF        bool // an APP0 marker is present
	Hmax, Vmax  int

	// Reconstruction data
	MarkerOrder     []byte
	AppData         [][]byte // full segments: marker byte, length, payload
	ComData         [][]byte
	InterMarker     [][]byte
	Tail            []byte
	Quants          []QuantTable
	Huff            []HuffCode
	Scans           []Scan
	RestartInterval int
	PaddingBits     []byte
	HasZeroPadding  bool
}

// RGB reports whether the components hold RGB rather than YCbCr (libjxl's
// rule: no APP0, and either an Adobe marker with transform 0 or component
// ids 'R','G','B').
func (img *Image) RGB() bool {
	if len(img.Comps) != 3 {
		return false
	}
	for _, m := range img.MarkerOrder {
		if m == 0xe0 {
			return false
		}
	}
	app := 0
	for _, m := range img.MarkerOrder {
		if m&0xf0 == 0xe0 {
			if m == 0xee {
				d := img.AppData[app]
				if len(d) == 15 && string(d[3:8]) == "Adobe" {
					return d[14] == 0
				}
			}
			app++
		}
	}
	return img.Comps[0].ID == 'R' && img.Comps[1].ID == 'G' && img.Comps[2].ID == 'B'
}

// ---------------------------------------------------------------- huffman

type huffTable struct {
	maxcode [18]int32
	valptr  [17]int32
	mincode [17]int32
	values  []int
	defined bool
}

func buildTable(h *HuffCode) huffTable {
	t := huffTable{values: h.Values, defined: true}
	code, k := int32(0), int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(h.Counts[l])
		t.valptr[l] = k
		t.mincode[l] = code
		code += n
		k += n
		if n > 0 {
			t.maxcode[l] = code - 1
		} else {
			t.maxcode[l] = -1
		}
		code <<= 1
	}
	t.maxcode[17] = 0x7fffffff
	return t
}

// ---------------------------------------------------------------- bits

// bitReader mirrors libjxl's BitReaderState.
type bitReader struct {
	data          []byte
	pos           int
	val           uint64
	bitsLeft      int
	nextMarkerPos int
}

func newBitReader(data []byte, pos int) *bitReader {
	b := &bitReader{data: data}
	b.reset(pos)
	return b
}

func (b *bitReader) reset(pos int) {
	b.pos = pos
	b.val = 0
	b.bitsLeft = 0
	b.nextMarkerPos = len(b.data) - 2
	b.fill()
}

func (b *bitReader) nextByte() byte {
	if b.pos >= b.nextMarkerPos {
		b.pos++
		return 0
	}
	c := b.data[b.pos]
	b.pos++
	if c == 0xff {
		if b.data[b.pos] == 0 {
			b.pos++
		} else {
			b.nextMarkerPos = b.pos - 1
		}
	}
	return c
}

func (b *bitReader) fill() {
	if b.bitsLeft <= 16 {
		for b.bitsLeft <= 56 {
			b.val = b.val<<8 | uint64(b.nextByte())
			b.bitsLeft += 8
		}
	}
}

func (b *bitReader) bits(n int) int {
	if n == 0 {
		return 0
	}
	b.fill()
	v := int(b.val>>(b.bitsLeft-n)) & (1<<n - 1)
	b.bitsLeft -= n
	return v
}

func (b *bitReader) symbol(t *huffTable) int {
	b.fill()
	code := int32(0)
	for l := 1; l <= 16; l++ {
		code = code<<1 | int32(b.val>>(b.bitsLeft-1))&1
		b.bitsLeft--
		if code <= t.maxcode[l] {
			return t.values[t.valptr[l]+code-t.mincode[l]]
		}
	}
	return 0xffff
}

// finish records the padding bits and returns the stream position.
func (b *bitReader) finish(img *Image) (int, error) {
	npad := b.bitsLeft & 7
	if npad > 0 {
		mask := uint64(1)<<npad - 1
		pad := (b.val >> (b.bitsLeft - npad)) & mask
		if pad != mask {
			img.HasZeroPadding = true
		}
		for i := npad - 1; i >= 0; i-- {
			img.PaddingBits = append(img.PaddingBits, byte(pad>>i&1))
		}
	}
	unused := b.bitsLeft >> 3
	for ; unused > 0; unused-- {
		b.pos--
		if b.pos < b.nextMarkerPos && b.data[b.pos] == 0 && b.data[b.pos-1] == 0xff {
			b.pos--
		}
	}
	if b.pos > b.nextMarkerPos {
		return 0, errors.New("jpeg: unexpected end of scan")
	}
	return b.pos, nil
}

func extend(x, s int) int {
	if x >= 1<<(s-1) {
		return x
	}
	return x - (1 << s) + 1
}

// ---------------------------------------------------------------- parsing

type parser struct {
	img   *Image
	data  []byte
	dc    [4]huffTable
	ac    [4]huffTable
	prog  [4][64]uint16 // scan progression masks
	sof   bool
	dri   bool
	isPrg bool
}

func u16(d []byte, p int) int { return int(d[p])<<8 | int(d[p+1]) }

var validMarker = [64]bool{
	true, true, true, false, true, false, false, false, false, false, false, false, false, false, false, false,
	true, true, true, true, true, true, true, true, false, true, true, true, false, true, false, false,
	true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
	false, false, false, false, false, false, false, false, false, false, false, false, false, false, true, false,
}

func findNextMarker(d []byte, pos int) int {
	n := 0
	for pos+1 < len(d) && (d[pos] != 0xff || d[pos+1] < 0xc0 || !validMarker[d[pos+1]-0xc0]) {
		pos++
		n++
	}
	return n
}

// Parse decodes a JPEG file to coefficients and reconstruction data.
func Parse(data []byte) (*Image, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, errors.New("jpeg: not a JPEG file")
	}
	p := &parser{img: &Image{}, data: data}
	img := p.img
	pos := 2
	sos := false
	for {
		if n := findNextMarker(data, pos); n > 0 {
			img.MarkerOrder = append(img.MarkerOrder, 0xff)
			img.InterMarker = append(img.InterMarker, append([]byte(nil), data[pos:pos+n]...))
			pos += n
		}
		if pos+2 > len(data) || data[pos] != 0xff {
			return nil, errors.New("jpeg: marker expected")
		}
		m := data[pos+1]
		pos += 2
		var err error
		switch {
		case m == 0xc0 || m == 0xc1 || m == 0xc2:
			p.isPrg = m == 0xc2
			img.Progressive = p.isPrg
			err = p.sofSeg(&pos)
		case m == 0xc4:
			err = p.dht(&pos)
		case m >= 0xd0 && m <= 0xd7, m == 0xd9:
		case m == 0xda:
			if !p.sof {
				return nil, errors.New("jpeg: SOS before SOF")
			}
			err = p.scan(&pos)
			sos = true
		case m == 0xdb:
			err = p.dqt(&pos)
		case m == 0xdd:
			if p.dri {
				return nil, errors.New("jpeg: duplicate DRI")
			}
			p.dri = true
			if pos+4 > len(data) || u16(data, pos) != 4 {
				return nil, errInvalid
			}
			img.RestartInterval = u16(data, pos+2)
			pos += 4
		case m >= 0xe0 && m <= 0xef, m == 0xfe:
			if pos+2 > len(data) {
				return nil, errInvalid
			}
			l := u16(data, pos)
			if l < 2 || pos+l > len(data) {
				return nil, errInvalid
			}
			seg := append([]byte(nil), data[pos-1:pos+l]...)
			if m == 0xfe {
				img.ComData = append(img.ComData, seg)
			} else {
				img.AppData = append(img.AppData, seg)
			}
			pos += l
		default:
			return nil, fmt.Errorf("%w: marker 0x%02x", ErrUnsupported, m)
		}
		if err != nil {
			return nil, err
		}
		img.MarkerOrder = append(img.MarkerOrder, m)
		if m == 0xd9 {
			break
		}
		if len(img.MarkerOrder) > 16384 {
			return nil, errors.New("jpeg: too many markers")
		}
	}
	if !p.sof || !sos {
		return nil, errors.New("jpeg: missing frame or scan")
	}
	if pos < len(data) {
		img.Tail = append([]byte(nil), data[pos:]...)
	}
	// component quant tables: first table with the id
	for i := range img.Comps {
		c := &img.Comps[i]
		found := false
		for j, q := range img.Quants {
			if q.Index == c.Tq {
				c.QuantIdx = j
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("jpeg: missing quantisation table")
		}
	}
	for j := len(img.Quants) - 1; j >= 0; j-- {
		q := img.Quants[j]
		img.Quant[q.Index] = q.Values
		img.QuantUsed[q.Index] = true
	}
	if len(img.Huff) == 0 {
		return nil, errors.New("jpeg: no Huffman tables")
	}
	p.metadata()
	return img, nil
}

func (p *parser) metadata() {
	img := p.img
	var iccChunks [][]byte
	for _, a := range img.AppData {
		pl := a[3:]
		switch a[0] {
		case 0xe1:
			if len(pl) >= 6 && string(pl[:6]) == "Exif\x00\x00" && img.EXIF == nil {
				img.EXIF = pl[6:]
			}
		case 0xe2:
			if len(pl) >= 14 && string(pl[:12]) == "ICC_PROFILE\x00" {
				iccChunks = append(iccChunks, pl)
			}
		case 0xee:
			if len(pl) >= 12 && string(pl[:5]) == "Adobe" {
				img.Adobe = true
				img.AdobeTransf = int(pl[11])
			}
		}
	}
	for _, m := range img.MarkerOrder {
		if m == 0xe0 {
			img.JFIF = true
		}
	}
	if len(iccChunks) > 0 {
		total := int(iccChunks[0][13])
		parts := make([][]byte, total+1)
		for _, c := range iccChunks {
			if seq := int(c[12]); seq >= 1 && seq <= total {
				parts[seq] = c[14:]
			}
		}
		for _, pt := range parts[1:] {
			img.ICC = append(img.ICC, pt...)
		}
	}
}

func (p *parser) sofSeg(pos *int) error {
	d, img := p.data, p.img
	if p.sof {
		return errors.New("jpeg: duplicate SOF")
	}
	p.sof = true
	s := *pos
	if s+8 > len(d) {
		return errInvalid
	}
	l := u16(d, s)
	if d[s+2] != 8 {
		return fmt.Errorf("%w: %d-bit precision", ErrUnsupported, d[s+2])
	}
	img.H, img.W = u16(d, s+3), u16(d, s+5)
	nc := int(d[s+7])
	if img.W < 1 || img.H < 1 {
		return fmt.Errorf("%w: zero dimension", ErrUnsupported)
	}
	if nc < 1 || nc > 4 || s+8+3*nc > len(d) || l != 8+3*nc {
		return errInvalid
	}
	if nc != 1 && nc != 3 {
		return fmt.Errorf("%w: %d components", ErrUnsupported, nc)
	}
	img.Hmax, img.Vmax = 1, 1
	seen := map[int]bool{}
	for i := 0; i < nc; i++ {
		o := s + 8 + 3*i
		c := Component{ID: int(d[o]), H: int(d[o+1] >> 4), V: int(d[o+1] & 15), Tq: int(d[o+2])}
		if seen[c.ID] || c.H < 1 || c.H > 2 || c.V < 1 || c.V > 2 || c.Tq > 3 {
			return fmt.Errorf("%w: component parameters", ErrUnsupported)
		}
		seen[c.ID] = true
		img.Hmax, img.Vmax = max(img.Hmax, c.H), max(img.Vmax, c.V)
		img.Comps = append(img.Comps, c)
	}
	mx := (img.W + 8*img.Hmax - 1) / (8 * img.Hmax)
	my := (img.H + 8*img.Vmax - 1) / (8 * img.Vmax)
	if mx*my*img.Hmax*img.Vmax > 1<<24 {
		return fmt.Errorf("%w: image too large", ErrUnsupported)
	}
	for i := range img.Comps {
		c := &img.Comps[i]
		if img.Hmax%c.H != 0 || img.Vmax%c.V != 0 {
			return fmt.Errorf("%w: non-integral subsampling", ErrUnsupported)
		}
		c.BW, c.BH = mx*c.H, my*c.V
		c.Coef = make([]int16, c.BW*c.BH*64)
	}
	*pos = s + l
	return nil
}

func (p *parser) dqt(pos *int) error {
	d, img := p.data, p.img
	s := *pos
	if s+2 > len(d) {
		return errInvalid
	}
	l := u16(d, s)
	if l == 2 || s+l > len(d) {
		return errInvalid
	}
	q := s + 2
	for q < s+l && len(img.Quants) < 4 {
		pq, tq := int(d[q]>>4), int(d[q]&15)
		q++
		if pq > 1 || tq > 3 || q+64*(pq+1) > s+l {
			return errInvalid
		}
		t := QuantTable{Index: tq, Precision: pq}
		for k := 0; k < 64; k++ {
			var v int
			if pq == 1 {
				v = u16(d, q)
				q += 2
			} else {
				v = int(d[q])
				q++
			}
			if v == 0 {
				return errInvalid
			}
			t.Values[NaturalOrder[k]] = uint16(v)
		}
		t.IsLast = q == s+l
		img.Quants = append(img.Quants, t)
	}
	if q != s+l {
		return errInvalid
	}
	*pos = s + l
	return nil
}

func (p *parser) dht(pos *int) error {
	d, img := p.data, p.img
	s := *pos
	if s+2 > len(d) {
		return errInvalid
	}
	l := u16(d, s)
	if s+l > len(d) {
		return errInvalid
	}
	if l == 2 {
		img.Huff = append(img.Huff, HuffCode{IsLast: true})
		*pos = s + l
		return nil
	}
	q := s + 2
	for q < s+l {
		if q+17 > s+l {
			return errInvalid
		}
		h := HuffCode{SlotID: int(d[q])}
		isAC := h.SlotID&0x10 != 0
		idx := h.SlotID & 0xf
		if h.SlotID&0xe0 != 0 || idx > 3 {
			return errInvalid
		}
		total, maxDepth := 0, 1
		space := 1 << 16
		for i := 1; i <= 16; i++ {
			c := int(d[q+i])
			if c != 0 {
				maxDepth = i
			}
			h.Counts[i] = c
			total += c
			space -= c << (16 - i)
		}
		q += 17
		if (isAC && total > 256) || (!isAC && total > 12) || q+total > s+l {
			return errInvalid
		}
		seen := [256]bool{}
		for i := 0; i < total; i++ {
			v := int(d[q+i])
			if (!isAC && v >= 12) || seen[v] {
				return errInvalid
			}
			seen[v] = true
			h.Values = append(h.Values, v)
		}
		q += total
		h.Counts[maxDepth]++
		h.Values = append(h.Values, 256)
		space -= 1 << (16 - maxDepth)
		if space < 0 {
			return errors.New("jpeg: invalid Huffman code lengths")
		}
		h.IsLast = q == s+l
		t := buildTable(&h)
		if isAC {
			p.ac[idx] = t
		} else {
			p.dc[idx] = t
		}
		img.Huff = append(img.Huff, h)
	}
	*pos = s + l
	return nil
}

func (p *parser) scan(pos *int) error {
	d, img := p.data, p.img
	s := *pos
	if s+3 > len(d) {
		return errInvalid
	}
	l := u16(d, s)
	ns := int(d[s+2])
	if ns < 1 || ns > len(img.Comps) || l != 6+2*ns || s+l > len(d) {
		return errInvalid
	}
	sc := Scan{}
	seen := map[int]bool{}
	for i := 0; i < ns; i++ {
		id := int(d[s+3+2*i])
		if seen[id] {
			return errInvalid
		}
		seen[id] = true
		ci := -1
		for j := range img.Comps {
			if img.Comps[j].ID == id {
				ci = j
			}
		}
		if ci < 0 {
			return errInvalid
		}
		t := int(d[s+4+2*i])
		sc.Comps = append(sc.Comps, ScanComp{CompIdx: ci, DCTbl: t >> 4, ACTbl: t & 15})
		if t>>4 > 3 || t&15 > 3 {
			return errInvalid
		}
	}
	o := s + 3 + 2*ns
	sc.Ss, sc.Se = int(d[o]), int(d[o+1])
	sc.Ah, sc.Al = int(d[o+2]>>4), int(d[o+2]&15)
	if sc.Ss > 63 || sc.Se < sc.Ss || sc.Se > 63 {
		return errInvalid
	}
	for _, c := range sc.Comps {
		if sc.Ss == 0 && !p.dc[c.DCTbl].defined || sc.Se > 0 && !p.ac[c.ACTbl].defined {
			return errors.New("jpeg: missing Huffman table")
		}
	}
	img.Scans = append(img.Scans, sc)
	si := &img.Scans[len(img.Scans)-1]
	*pos = s + l

	Al, Ah, Ss, Se := 0, 0, 0, 63
	if p.isPrg {
		Al, Ah, Ss, Se = sc.Al, sc.Ah, sc.Ss, sc.Se
	}
	scanMask := uint16(1) << Al
	if Ah == 0 {
		scanMask = 0xffff << Al
	}
	refMask := uint16(1)<<Al - 1
	for _, c := range sc.Comps {
		for k := Ss; k <= Se; k++ {
			if p.prog[c.CompIdx][k]&scanMask != 0 || p.prog[c.CompIdx][k]&refMask != 0 {
				return errors.New("jpeg: invalid scan progression")
			}
			p.prog[c.CompIdx][k] |= scanMask
		}
	}
	if Al > 10 {
		return fmt.Errorf("%w: Al=%d", ErrUnsupported, Al)
	}
	interleaved := ns > 1
	mcuRows := (img.H + 8*img.Vmax - 1) / (8 * img.Vmax)
	mcuCols := (img.W + 8*img.Hmax - 1) / (8 * img.Hmax)
	if !interleaved {
		c := &img.Comps[sc.Comps[0].CompIdx]
		mcuCols = (img.W*c.H + 8*img.Hmax - 1) / (8 * img.Hmax)
		mcuRows = (img.H*c.V + 8*img.Vmax - 1) / (8 * img.Vmax)
	}
	var lastDC [4]int
	br := newBitReader(d, *pos)
	restartsToGo := img.RestartInterval
	nextRST := 0
	eobrun := -1
	blockIdx := 0
	for my := 0; my < mcuRows; my++ {
		for mx := 0; mx < mcuCols; mx++ {
			if img.RestartInterval > 0 {
				if restartsToGo == 0 {
					rp, err := br.finish(img)
					if err != nil {
						return err
					}
					if rp+2 > len(d) || d[rp] != 0xff || int(d[rp+1]) != 0xd0+nextRST {
						return errors.New("jpeg: missing restart marker")
					}
					br.reset(rp + 2)
					nextRST = (nextRST + 1) & 7
					restartsToGo = img.RestartInterval
					lastDC = [4]int{}
					if eobrun > 0 {
						return errors.New("jpeg: EOB run too long")
					}
					eobrun = -1
				}
				restartsToGo--
			}
			for i, c := range sc.Comps {
				comp := &img.Comps[c.CompIdx]
				nby, nbx := 1, 1
				if interleaved {
					nby, nbx = comp.V, comp.H
				}
				for iy := 0; iy < nby; iy++ {
					for ix := 0; ix < nbx; ix++ {
						by, bx := my*nby+iy, mx*nbx+ix
						if bx >= comp.BW || by >= comp.BH {
							return errInvalid
						}
						coef := comp.Coef[(by*comp.BW+bx)*64 : (by*comp.BW+bx)*64+64]
						reset := false
						nzr := 0
						var err error
						if Ah == 0 {
							err = p.decodeBlock(br, &p.dc[c.DCTbl], &p.ac[c.ACTbl], Ss, Se, Al, &eobrun, &reset, &nzr, &lastDC[i], coef)
						} else {
							err = p.refineBlock(br, &p.ac[c.ACTbl], Ss, Se, Al, &eobrun, &reset, coef)
						}
						if err != nil {
							return err
						}
						if reset {
							si.ResetPoints = append(si.ResetPoints, blockIdx)
						}
						if nzr > 0 {
							si.ExtraZeroRuns = append(si.ExtraZeroRuns, ExtraZeroRun{blockIdx, nzr})
						}
						blockIdx++
					}
				}
			}
		}
	}
	if eobrun > 0 {
		return errors.New("jpeg: EOB run too long")
	}
	np, err := br.finish(img)
	if err != nil {
		return err
	}
	if np > len(d) {
		return errors.New("jpeg: unexpected end of file")
	}
	*pos = np
	return nil
}

func (p *parser) decodeBlock(br *bitReader, dct, act *huffTable, Ss, Se, Al int, eobrun *int, reset *bool, nzr *int, lastDC *int, coef []int16) error {
	eobAllowed := Ss > 0
	if Ss == 0 {
		s := br.symbol(dct)
		if s >= 12 {
			return errors.New("jpeg: invalid DC symbol")
		}
		diff := 0
		if s > 0 {
			diff = extend(br.bits(s), s)
		}
		v := diff + *lastDC
		dc := v << Al
		if int(int16(dc)) != dc {
			return errors.New("jpeg: invalid DC coefficient")
		}
		coef[0] = int16(dc)
		*lastDC = v
		Ss++
	}
	if Ss > Se {
		return nil
	}
	if *eobrun > 0 {
		*eobrun--
		return nil
	}
	*nzr = 0
	for k := Ss; k <= Se; k++ {
		sr := br.symbol(act)
		if sr >= 256 {
			return errors.New("jpeg: invalid AC symbol")
		}
		r, s := sr>>4, sr&15
		if s > 0 {
			k += r
			if k > Se {
				return errors.New("jpeg: out-of-band coefficient")
			}
			if s+Al >= 12 {
				return errors.New("jpeg: AC coefficient out of range")
			}
			coef[NaturalOrder[k]] = int16(extend(br.bits(s), s) << Al)
			*nzr = 0
		} else if r == 15 {
			k += 15
			*nzr++
		} else {
			if eobAllowed && k == Ss && *eobrun == 0 {
				*reset = true
			}
			*eobrun = 1 << r
			if r > 0 {
				if !eobAllowed {
					return errors.New("jpeg: EOB run crossing DC")
				}
				*eobrun += br.bits(r)
			}
			break
		}
	}
	*eobrun--
	return nil
}

func (p *parser) refineBlock(br *bitReader, act *huffTable, Ss, Se, Al int, eobrun *int, reset *bool, coef []int16) error {
	am := 1 << Al
	eobAllowed := Ss > 0
	if Ss == 0 {
		if br.bits(1) != 0 {
			coef[0] |= int16(am)
		}
		Ss++
	}
	if Ss > Se {
		return nil
	}
	p1, m1 := int16(am), int16(-am)
	k := Ss
	inZeroRun := false
	if *eobrun <= 0 {
		for ; k <= Se; k++ {
			sym := br.symbol(act)
			if sym >= 256 {
				return errors.New("jpeg: invalid AC symbol")
			}
			r, s := sym>>4, sym&15
			var val int16
			if s != 0 {
				if s != 1 {
					return errors.New("jpeg: invalid refinement symbol")
				}
				if br.bits(1) != 0 {
					val = p1
				} else {
					val = m1
				}
				inZeroRun = false
			} else {
				if r != 15 {
					if eobAllowed && k == Ss && *eobrun == 0 {
						*reset = true
					}
					*eobrun = 1 << r
					if r > 0 {
						if !eobAllowed {
							return errors.New("jpeg: EOB run crossing DC")
						}
						*eobrun += br.bits(r)
					}
					break
				}
				inZeroRun = true
			}
			for {
				z := NaturalOrder[k]
				c := coef[z]
				if c != 0 {
					if br.bits(1) != 0 && c&p1 == 0 {
						if c >= 0 {
							c += p1
						} else {
							c += m1
						}
					}
					coef[z] = c
				} else {
					r--
					if r < 0 {
						break
					}
				}
				k++
				if k > Se {
					break
				}
			}
			if s != 0 {
				if k > Se {
					return errors.New("jpeg: out-of-band coefficient")
				}
				coef[NaturalOrder[k]] = val
			}
		}
	}
	if inZeroRun {
		return errors.New("jpeg: extra zero run before EOB")
	}
	if *eobrun > 0 {
		for ; k <= Se; k++ {
			z := NaturalOrder[k]
			c := coef[z]
			if c != 0 {
				if br.bits(1) != 0 && c&p1 == 0 {
					if c >= 0 {
						c += p1
					} else {
						c += m1
					}
				}
				coef[z] = c
			}
		}
	}
	*eobrun--
	return nil
}
