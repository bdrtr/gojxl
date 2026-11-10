// Package jpeg parses JPEG files down to quantised DCT coefficients
// (baseline and progressive Huffman coding), as needed for lossless
// JPEG -> JPEG XL recompression.
package jpeg

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Component is one colour component with its quantised coefficients.
type Component struct {
	ID     int
	H, V   int // sampling factors
	Tq     int // quantisation table index
	BW, BH int // blocks allocated (MCU padded)
	// Coef holds BW*BH blocks of 64 coefficients in natural (row-major,
	// coef[v*8+u]) order.
	Coef []int16
}

// Image is a parsed JPEG.
type Image struct {
	W, H        int
	Comps       []Component
	Quant       [4][64]uint16 // natural order
	QuantUsed   [4]bool
	Progressive bool
	ICC         []byte
	EXIF        []byte
	Adobe       bool // APP14 present
	AdobeTransf int  // APP14 colour transform
	JFIF        bool
	Hmax, Vmax  int
}

// zigzag maps zigzag index -> natural index.
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

// ErrUnsupported reports JPEG features that are not handled.
var ErrUnsupported = errors.New("jpeg: unsupported feature")

type huff struct {
	// lookup by (length, code): canonical decoding tables
	maxcode [18]int32
	valptr  [17]int32
	mincode [17]int32
	vals    []byte
	look    [1 << 9]uint16 // fast path: (len<<8 | val), 0 = slow
	ok      bool
}

func buildHuff(counts [16]int, vals []byte) (huff, error) {
	var h huff
	h.vals = vals
	code, k := int32(0), int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(counts[l-1])
		h.valptr[l] = k
		h.mincode[l] = code
		code += n
		k += n
		if n > 0 {
			h.maxcode[l] = code - 1
		} else {
			h.maxcode[l] = -1
		}
		if code > 1<<l {
			return h, errors.New("jpeg: bad huffman table")
		}
		code <<= 1
	}
	h.maxcode[17] = 0x7fffffff
	// fast lookup for codes up to 9 bits
	code, k = 0, 0
	for l := 1; l <= 9; l++ {
		for i := 0; i < counts[l-1]; i++ {
			c := int(code) << (9 - l)
			for j := 0; j < 1<<(9-l); j++ {
				h.look[c+j] = uint16(l<<8) | uint16(vals[k])
			}
			code++
			k++
		}
		code <<= 1
	}
	h.ok = true
	return h, nil
}

type bitReader struct {
	data []byte
	pos  int
	acc  uint64
	n    uint
	// marker hit: no more entropy data
	marker bool
}

func (b *bitReader) fill() {
	for b.n <= 56 {
		var c byte
		if !b.marker && b.pos < len(b.data) {
			c = b.data[b.pos]
			if c == 0xff {
				if b.pos+1 < len(b.data) && b.data[b.pos+1] == 0 {
					b.pos += 2
				} else {
					b.marker = true
					c = 0
				}
			} else {
				b.pos++
			}
		}
		b.acc |= uint64(c) << (56 - b.n)
		b.n += 8
	}
}

func (b *bitReader) bits(n uint) int32 {
	if n == 0 {
		return 0
	}
	if b.n < n {
		b.fill()
	}
	v := int32(b.acc >> (64 - n))
	b.acc <<= n
	b.n -= n
	return v
}

func (b *bitReader) bit() int32 { return b.bits(1) }

func (b *bitReader) decode(h *huff) (byte, error) {
	if b.n < 16 {
		b.fill()
	}
	if e := h.look[b.acc>>(64-9)]; e != 0 {
		l := uint(e >> 8)
		b.acc <<= l
		b.n -= l
		return byte(e), nil
	}
	code := int32(0)
	for l := 1; l <= 16; l++ {
		code = code<<1 | int32(b.acc>>63)
		b.acc <<= 1
		b.n--
		if code <= h.maxcode[l] {
			return h.vals[h.valptr[l]+code-h.mincode[l]], nil
		}
	}
	return 0, errors.New("jpeg: bad huffman code")
}

// extend sign-extends a magnitude category value.
func extend(v int32, s uint) int32 {
	if s == 0 {
		return 0
	}
	if v < 1<<(s-1) {
		return v - (1 << s) + 1
	}
	return v
}

// restart realigns after an RSTn marker.
func (b *bitReader) restart() error {
	b.acc, b.n = 0, 0
	// skip to the marker
	for b.pos+1 < len(b.data) && !(b.data[b.pos] == 0xff && b.data[b.pos+1] >= 0xd0 && b.data[b.pos+1] <= 0xd7) {
		b.pos++
	}
	if b.pos+1 >= len(b.data) {
		return errors.New("jpeg: missing restart marker")
	}
	b.pos += 2
	b.marker = false
	return nil
}

type decoder struct {
	img     *Image
	dc, ac  [4]huff
	ri      int
	eobrun  int
	sofSeen bool
}

// Parse decodes a JPEG file to coefficients.
func Parse(data []byte) (*Image, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, errors.New("jpeg: not a JPEG file")
	}
	d := &decoder{img: &Image{}}
	pos := 2
	var iccChunks [][]byte
	for {
		// find marker
		for pos < len(data) && data[pos] != 0xff {
			pos++
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return nil, errors.New("jpeg: unexpected end")
		}
		m := data[pos]
		pos++
		if m == 0xd9 { // EOI
			break
		}
		if m >= 0xd0 && m <= 0xd7 || m == 0x01 {
			continue
		}
		if pos+2 > len(data) {
			return nil, errors.New("jpeg: truncated segment")
		}
		l := int(binary.BigEndian.Uint16(data[pos:]))
		if l < 2 || pos+l > len(data) {
			return nil, errors.New("jpeg: bad segment length")
		}
		seg := data[pos+2 : pos+l]
		pos += l
		switch {
		case m == 0xc0 || m == 0xc1 || m == 0xc2:
			d.img.Progressive = m == 0xc2
			if err := d.parseSOF(seg); err != nil {
				return nil, err
			}
		case m >= 0xc3 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc:
			return nil, fmt.Errorf("%w: SOF%d", ErrUnsupported, m-0xc0)
		case m == 0xc4:
			if err := d.parseDHT(seg); err != nil {
				return nil, err
			}
		case m == 0xdb:
			if err := d.parseDQT(seg); err != nil {
				return nil, err
			}
		case m == 0xdd:
			if len(seg) < 2 {
				return nil, errors.New("jpeg: bad DRI")
			}
			d.ri = int(binary.BigEndian.Uint16(seg))
		case m == 0xda:
			n, err := d.parseSOS(seg, data[pos:])
			if err != nil {
				return nil, err
			}
			pos += n
		case m == 0xe0:
			if len(seg) >= 5 && string(seg[:5]) == "JFIF\x00" {
				d.img.JFIF = true
			}
		case m == 0xe1:
			if len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
				d.img.EXIF = seg[6:]
			}
		case m == 0xe2:
			if len(seg) >= 14 && string(seg[:12]) == "ICC_PROFILE\x00" {
				iccChunks = append(iccChunks, seg)
			}
		case m == 0xee:
			if len(seg) >= 12 && string(seg[:5]) == "Adobe" {
				d.img.Adobe = true
				d.img.AdobeTransf = int(seg[11])
			}
		}
	}
	if !d.sofSeen {
		return nil, errors.New("jpeg: no frame")
	}
	if len(iccChunks) > 0 {
		// order chunks by sequence number
		total := int(iccChunks[0][13])
		parts := make([][]byte, total+1)
		for _, c := range iccChunks {
			if seq := int(c[12]); seq >= 1 && seq <= total {
				parts[seq] = c[14:]
			}
		}
		for _, p := range parts[1:] {
			d.img.ICC = append(d.img.ICC, p...)
		}
	}
	return d.img, nil
}

func (d *decoder) parseSOF(s []byte) error {
	if d.sofSeen {
		return errors.New("jpeg: multiple frames")
	}
	d.sofSeen = true
	if len(s) < 6 {
		return errors.New("jpeg: bad SOF")
	}
	if s[0] != 8 {
		return fmt.Errorf("%w: %d-bit precision", ErrUnsupported, s[0])
	}
	img := d.img
	img.H = int(binary.BigEndian.Uint16(s[1:]))
	img.W = int(binary.BigEndian.Uint16(s[3:]))
	nc := int(s[5])
	if img.W == 0 || img.H == 0 {
		return fmt.Errorf("%w: DNL / zero size", ErrUnsupported)
	}
	if nc != 1 && nc != 3 || len(s) < 6+3*nc {
		return fmt.Errorf("%w: %d components", ErrUnsupported, nc)
	}
	img.Hmax, img.Vmax = 1, 1
	for i := 0; i < nc; i++ {
		c := Component{ID: int(s[6+3*i]), H: int(s[7+3*i] >> 4), V: int(s[7+3*i] & 15), Tq: int(s[8+3*i])}
		if c.H < 1 || c.H > 4 || c.V < 1 || c.V > 4 || c.Tq > 3 {
			return errors.New("jpeg: bad component")
		}
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
		c.BW, c.BH = mx*c.H, my*c.V
		c.Coef = make([]int16, c.BW*c.BH*64)
	}
	return nil
}

func (d *decoder) parseDQT(s []byte) error {
	for len(s) > 0 {
		pq, tq := s[0]>>4, int(s[0]&15)
		if tq > 3 {
			return errors.New("jpeg: bad DQT")
		}
		s = s[1:]
		for k := 0; k < 64; k++ {
			var v uint16
			if pq == 0 {
				if len(s) < 1 {
					return errors.New("jpeg: bad DQT")
				}
				v = uint16(s[0])
				s = s[1:]
			} else {
				if len(s) < 2 {
					return errors.New("jpeg: bad DQT")
				}
				v = binary.BigEndian.Uint16(s)
				s = s[2:]
			}
			if v == 0 {
				return errors.New("jpeg: zero quantiser")
			}
			d.img.Quant[tq][zigzag[k]] = v
		}
		d.img.QuantUsed[tq] = true
	}
	return nil
}

func (d *decoder) parseDHT(s []byte) error {
	for len(s) > 0 {
		if len(s) < 17 {
			return errors.New("jpeg: bad DHT")
		}
		tc, th := s[0]>>4, int(s[0]&15)
		if tc > 1 || th > 3 {
			return errors.New("jpeg: bad DHT")
		}
		var counts [16]int
		n := 0
		for i := 0; i < 16; i++ {
			counts[i] = int(s[1+i])
			n += counts[i]
		}
		if len(s) < 17+n || n > 256 {
			return errors.New("jpeg: bad DHT")
		}
		vals := append([]byte(nil), s[17:17+n]...)
		h, err := buildHuff(counts, vals)
		if err != nil {
			return err
		}
		if tc == 0 {
			d.dc[th] = h
		} else {
			d.ac[th] = h
		}
		s = s[17+n:]
	}
	return nil
}

// parseSOS decodes one scan; rest is the data after the SOS header. It
// returns the number of bytes of entropy coded data consumed.
func (d *decoder) parseSOS(s []byte, rest []byte) (int, error) {
	img := d.img
	if !d.sofSeen || len(s) < 1 {
		return 0, errors.New("jpeg: SOS before SOF")
	}
	ns := int(s[0])
	if ns < 1 || ns > 4 || len(s) < 4+2*ns {
		return 0, errors.New("jpeg: bad SOS")
	}
	type scomp struct {
		c      *Component
		td, ta int
	}
	var sc []scomp
	for i := 0; i < ns; i++ {
		id := int(s[1+2*i])
		var comp *Component
		for k := range img.Comps {
			if img.Comps[k].ID == id {
				comp = &img.Comps[k]
			}
		}
		if comp == nil {
			return 0, errors.New("jpeg: bad scan component")
		}
		sc = append(sc, scomp{comp, int(s[2+2*i] >> 4), int(s[2+2*i] & 15)})
	}
	p := s[1+2*ns:]
	ss, se, ah, al := int(p[0]), int(p[1]), int(p[2]>>4), uint(p[2]&15)
	if !img.Progressive {
		ss, se, ah, al = 0, 63, 0, 0
	}
	if ss > se || se > 63 || (ss == 0 && se != 0 && img.Progressive) || (ss > 0 && ns != 1) || al > 13 {
		return 0, errors.New("jpeg: bad spectral selection")
	}
	for _, c := range sc {
		if c.td > 3 || c.ta > 3 {
			return 0, errors.New("jpeg: bad table index")
		}
		if ss == 0 && ah == 0 && !d.dc[c.td].ok || se > 0 && !d.ac[c.ta].ok {
			return 0, errors.New("jpeg: missing huffman table")
		}
	}
	br := &bitReader{data: rest}
	var pred [4]int32
	d.eobrun = 0
	// MCU geometry
	var mcusX, mcusY int
	single := ns == 1
	if single {
		c := sc[0].c
		cw := (img.W*c.H + img.Hmax - 1) / img.Hmax
		ch := (img.H*c.V + img.Vmax - 1) / img.Vmax
		mcusX, mcusY = (cw+7)/8, (ch+7)/8
	} else {
		mcusX = (img.W + 8*img.Hmax - 1) / (8 * img.Hmax)
		mcusY = (img.H + 8*img.Vmax - 1) / (8 * img.Vmax)
	}
	block := func(sci, bx, by int) error {
		c := sc[sci].c
		blk := c.Coef[(by*c.BW+bx)*64 : (by*c.BW+bx)*64+64]
		if ss == 0 {
			if ah == 0 {
				t, err := br.decode(&d.dc[sc[sci].td])
				if err != nil {
					return err
				}
				if t > 11 {
					return errors.New("jpeg: bad DC")
				}
				diff := extend(br.bits(uint(t)), uint(t))
				pred[sci] += diff
				blk[0] = int16(pred[sci] << al)
			} else if br.bit() != 0 {
				blk[0] |= int16(1 << al)
			}
			if se == 0 {
				return nil
			}
		}
		k0 := max(ss, 1)
		ach := &d.ac[sc[sci].ta]
		if ah == 0 {
			// first AC scan / baseline
			if d.eobrun > 0 {
				d.eobrun--
				return nil
			}
			for k := k0; k <= se; k++ {
				rs, err := br.decode(ach)
				if err != nil {
					return err
				}
				r, s := int(rs>>4), uint(rs&15)
				if s == 0 {
					if r < 15 {
						d.eobrun = (1 << r) - 1
						if r > 0 {
							d.eobrun += int(br.bits(uint(r)))
						}
						if !img.Progressive && d.eobrun != 0 {
							return errors.New("jpeg: bad baseline EOB")
						}
						break
					}
					k += 15
					continue
				}
				k += r
				if k > 63 {
					return errors.New("jpeg: AC index overflow")
				}
				blk[zigzag[k]] = int16(extend(br.bits(s), s) << al)
			}
			return nil
		}
		// successive approximation refinement
		p1 := int16(1 << al)
		m1 := int16(-1) << al
		k := k0
		if d.eobrun <= 0 {
			for ; k <= se; k++ {
				rs, err := br.decode(ach)
				if err != nil {
					return err
				}
				r, s := int(rs>>4), int(rs&15)
				var val int16
				if s != 0 {
					if s != 1 {
						return errors.New("jpeg: bad refinement")
					}
					if br.bit() != 0 {
						val = p1
					} else {
						val = m1
					}
				} else if r != 15 {
					d.eobrun = 1 << r
					if r > 0 {
						d.eobrun += int(br.bits(uint(r)))
					}
					break
				}
				for ; k <= se; k++ {
					z := zigzag[k]
					if blk[z] != 0 {
						if br.bit() != 0 && blk[z]&p1 == 0 {
							if blk[z] >= 0 {
								blk[z] += p1
							} else {
								blk[z] += m1
							}
						}
					} else {
						if r == 0 {
							if val != 0 {
								blk[z] = val
							}
							break
						}
						r--
					}
				}
			}
		}
		if d.eobrun > 0 {
			for ; k <= se; k++ {
				z := zigzag[k]
				if blk[z] != 0 && br.bit() != 0 && blk[z]&p1 == 0 {
					if blk[z] >= 0 {
						blk[z] += p1
					} else {
						blk[z] += m1
					}
				}
			}
			d.eobrun--
		}
		return nil
	}
	n := 0
	for my := 0; my < mcusY; my++ {
		for mx := 0; mx < mcusX; mx++ {
			if d.ri > 0 && n > 0 && n%d.ri == 0 {
				if err := br.restart(); err != nil {
					return 0, err
				}
				pred = [4]int32{}
				d.eobrun = 0
			}
			n++
			if single {
				if err := block(0, mx, my); err != nil {
					return 0, err
				}
				continue
			}
			for i, c := range sc {
				for v := 0; v < c.c.V; v++ {
					for h := 0; h < c.c.H; h++ {
						if err := block(i, mx*c.c.H+h, my*c.c.V+v); err != nil {
							return 0, err
						}
					}
				}
			}
		}
	}
	// position after the entropy coded data: next marker
	pos := br.pos
	for pos+1 < len(rest) && !(rest[pos] == 0xff && rest[pos+1] != 0 && !(rest[pos+1] >= 0xd0 && rest[pos+1] <= 0xd7)) {
		pos++
	}
	return pos, nil
}
