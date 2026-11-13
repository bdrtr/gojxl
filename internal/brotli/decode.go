// Package brotli is a Brotli (RFC 7932) decompressor, used for the
// compressed parts of JPEG XL containers (jbrd and brob boxes).
package brotli

import (
	_ "embed"
	"errors"

	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/entropy"
)

//go:embed dictionary.bin
var dictionary []byte

var dictSizeBits = [25]uint{0, 0, 0, 0, 10, 10, 11, 11, 10, 10, 10, 10, 10, 9, 9, 8, 7, 7, 8, 7, 7, 6, 6, 5, 5}
var dictOffsets = [25]int{0, 0, 0, 0, 0, 4096, 9216, 21504, 35840, 44032, 53248, 63488, 74752, 87040, 93696, 100864, 104704, 106752, 108928, 113536, 115968, 118528, 119872, 121280, 122016}

var errCorrupt = errors.New("brotli: corrupt stream")

type lenCode struct {
	base  uint32
	nbits uint
}

var blockLenCodes = [26]lenCode{
	{1, 2}, {5, 2}, {9, 2}, {13, 2}, {17, 3}, {25, 3}, {33, 3}, {41, 3}, {49, 4}, {65, 4}, {81, 4}, {97, 4}, {113, 5},
	{145, 5}, {177, 5}, {209, 5}, {241, 6}, {305, 6}, {369, 7}, {497, 8}, {753, 9}, {1265, 10}, {2289, 11}, {4337, 12},
	{8433, 13}, {16625, 24},
}

var insertCodes = [24]lenCode{
	{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 1}, {8, 1}, {10, 2}, {14, 2}, {18, 3}, {26, 3}, {34, 4}, {50, 4},
	{66, 5}, {98, 5}, {130, 6}, {194, 7}, {322, 8}, {578, 9}, {1090, 10}, {2114, 12}, {6210, 14}, {22594, 24},
}

var copyCodes = [24]lenCode{
	{2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 0}, {7, 0}, {8, 0}, {9, 0}, {10, 1}, {12, 1}, {14, 2}, {18, 2}, {22, 3}, {30, 3},
	{38, 4}, {54, 4}, {70, 5}, {102, 5}, {134, 6}, {198, 7}, {326, 8}, {582, 9}, {1094, 10}, {2118, 24},
}

// insert & copy code cells: (insert base code, copy base code)
var cmdCells = [11][2]uint32{{0, 0}, {0, 8}, {0, 0}, {0, 8}, {8, 0}, {8, 8}, {0, 16}, {16, 0}, {8, 16}, {16, 8}, {16, 16}}

type decoder struct {
	br   *bitio.Reader
	data []byte
	out  []byte
	max  int // maximum output size
}

func (d *decoder) bits(n uint) uint32 {
	if n == 0 {
		return 0
	}
	return d.br.Read(n)
}

func (d *decoder) varLenUint8() uint32 {
	if d.br.Read(1) == 0 {
		return 0
	}
	n := d.br.Read(3)
	if n == 0 {
		return 1
	}
	return 1<<n + d.br.Read(uint(n))
}

type blockSwitch struct {
	n           uint32
	types       entropy.PrefixCode
	counts      entropy.PrefixCode
	cur, prev   uint32
	left        uint32
	initialized bool
}

func (d *decoder) blockCount(c entropy.PrefixCode) (uint32, error) {
	s := c.Read(d.br)
	if s >= 26 {
		return 0, errCorrupt
	}
	return blockLenCodes[s].base + d.bits(blockLenCodes[s].nbits), nil
}

func (d *decoder) readBlockSwitch(b *blockSwitch) error {
	b.n = d.varLenUint8() + 1
	b.cur, b.prev = 0, 1
	b.left = 1 << 28
	if b.n < 2 {
		return nil
	}
	var err error
	if b.types, err = entropy.ReadPrefixCode(d.br, b.n+2); err != nil {
		return err
	}
	if b.counts, err = entropy.ReadPrefixCode(d.br, 26); err != nil {
		return err
	}
	b.left, err = d.blockCount(b.counts)
	return err
}

func (d *decoder) switchBlock(b *blockSwitch) error {
	t := b.types.Read(d.br)
	var nt uint32
	switch t {
	case 0:
		nt = b.prev
	case 1:
		nt = (b.cur + 1) % b.n
	default:
		nt = t - 2
	}
	if nt >= b.n {
		return errCorrupt
	}
	b.prev, b.cur = b.cur, nt
	var err error
	b.left, err = d.blockCount(b.counts)
	return err
}

func (d *decoder) contextMap(size int, ntrees uint32) ([]uint8, error) {
	m := make([]uint8, size)
	if ntrees < 2 {
		return m, nil
	}
	rleMax := uint32(0)
	if d.br.Read(1) == 1 {
		rleMax = d.br.Read(4) + 1
	}
	code, err := entropy.ReadPrefixCode(d.br, ntrees+rleMax)
	if err != nil {
		return nil, err
	}
	for i := 0; i < size; {
		s := code.Read(d.br)
		switch {
		case s == 0:
			m[i] = 0
			i++
		case s <= rleMax:
			n := int(1<<s + d.br.Read(uint(s)))
			if i+n > size {
				return nil, errCorrupt
			}
			for ; n > 0; n-- {
				m[i] = 0
				i++
			}
		default:
			v := s - rleMax
			if v >= ntrees {
				return nil, errCorrupt
			}
			m[i] = uint8(v)
			i++
		}
		if d.br.Overflowed() {
			return nil, errCorrupt
		}
	}
	if d.br.Read(1) == 1 { // inverse move-to-front
		var mtf [256]uint8
		for i := range mtf {
			mtf[i] = uint8(i)
		}
		for i, v := range m {
			x := mtf[v]
			m[i] = x
			copy(mtf[1:v+1], mtf[:v])
			mtf[0] = x
		}
	}
	return m, nil
}

func toUpper(p []byte) int {
	if p[0] < 0xc0 {
		if p[0] >= 'a' && p[0] <= 'z' {
			p[0] ^= 32
		}
		return 1
	}
	if p[0] < 0xe0 {
		if len(p) > 1 {
			p[1] ^= 32
		}
		return 2
	}
	if len(p) > 2 {
		p[2] ^= 5
	}
	return 3
}

func transformWord(word []byte, t int) []byte {
	pre := prefixSuffix[prefixSuffixMap[transformsData[3*t]]:]
	typ := int(transformsData[3*t+1])
	suf := prefixSuffix[prefixSuffixMap[transformsData[3*t+2]]:]
	out := append([]byte(nil), pre[1:1+pre[0]]...)
	switch {
	case typ <= 9:
		word = word[:max(0, len(word)-typ)]
	case typ >= 12 && typ <= 20:
		skip := typ - 11
		if skip > len(word) {
			skip = len(word)
		}
		word = word[skip:]
	}
	start := len(out)
	out = append(out, word...)
	w := out[start:]
	if typ == 10 && len(w) > 0 {
		toUpper(w)
	} else if typ == 11 {
		for len(w) > 0 {
			n := toUpper(w)
			if n > len(w) {
				n = len(w)
			}
			w = w[n:]
		}
	}
	return append(out, suf[1:1+suf[0]]...)
}

// Decode decompresses a complete Brotli stream. maxSize bounds the output.
func Decode(data []byte, maxSize int) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, errCorrupt
		}
	}()
	d := &decoder{br: bitio.NewReader(data), data: data, max: maxSize}
	// window size
	wbits := uint(16)
	if d.br.Read(1) == 1 {
		if n := d.br.Read(3); n != 0 {
			wbits = 17 + uint(n)
		} else if n := d.br.Read(3); n != 0 {
			if n == 1 {
				return nil, errCorrupt // large window extension
			}
			wbits = 8 + uint(n)
		} else {
			wbits = 17
		}
	}
	maxBackward := 1<<wbits - 16
	dist := [4]int{16, 15, 11, 4} // dist[3] is the last distance
	for {
		last := d.br.Read(1) == 1
		if last && d.br.Read(1) == 1 {
			break // ISLASTEMPTY
		}
		mn := d.br.Read(2)
		if mn == 3 { // metadata block
			if d.br.Read(1) != 0 {
				return nil, errCorrupt
			}
			nb := d.br.Read(2)
			skip := uint32(0)
			if nb > 0 {
				skip = d.br.Read(uint(8*nb)) + 1
			}
			if err := d.br.ZeroPadToByte(); err != nil {
				return nil, errCorrupt
			}
			pos := d.br.BytePos() + int(skip)
			if pos > len(d.data) {
				return nil, errCorrupt
			}
			d.data = d.data[pos:]
			d.br = bitio.NewReader(d.data)
			if last {
				break
			}
			continue
		}
		mlen := int(d.br.Read(uint(4*(mn+4)))) + 1
		if len(d.out)+mlen > d.max {
			return nil, errors.New("brotli: output too large")
		}
		if !last && d.br.Read(1) == 1 { // uncompressed
			if err := d.br.ZeroPadToByte(); err != nil {
				return nil, errCorrupt
			}
			pos := d.br.BytePos()
			if pos+mlen > len(d.data) {
				return nil, errCorrupt
			}
			d.out = append(d.out, d.data[pos:pos+mlen]...)
			d.data = d.data[pos+mlen:]
			d.br = bitio.NewReader(d.data)
			continue
		}
		if err := d.metaBlock(mlen, maxBackward, &dist); err != nil {
			return nil, err
		}
		if d.br.Overflowed() {
			return nil, errCorrupt
		}
		if last {
			break
		}
	}
	if d.br.Overflowed() {
		return nil, errCorrupt
	}
	return d.out, nil
}

func (d *decoder) metaBlock(mlen, maxBackward int, dist *[4]int) error {
	var bs [3]blockSwitch
	for i := range bs {
		if err := d.readBlockSwitch(&bs[i]); err != nil {
			return err
		}
	}
	npostfix := uint(d.br.Read(2))
	ndirect := d.br.Read(4) << npostfix
	modes := make([]uint8, bs[0].n)
	for i := range modes {
		modes[i] = uint8(d.br.Read(2))
	}
	ntreesL := d.varLenUint8() + 1
	cmapL, err := d.contextMap(64*int(bs[0].n), ntreesL)
	if err != nil {
		return err
	}
	ntreesD := d.varLenUint8() + 1
	cmapD, err := d.contextMap(4*int(bs[2].n), ntreesD)
	if err != nil {
		return err
	}
	lit := make([]entropy.PrefixCode, ntreesL)
	for i := range lit {
		if lit[i], err = entropy.ReadPrefixCode(d.br, 256); err != nil {
			return err
		}
	}
	cmd := make([]entropy.PrefixCode, bs[1].n)
	for i := range cmd {
		if cmd[i], err = entropy.ReadPrefixCode(d.br, 704); err != nil {
			return err
		}
	}
	distAlpha := 16 + ndirect + 48<<npostfix
	dst := make([]entropy.PrefixCode, ntreesD)
	for i := range dst {
		if dst[i], err = entropy.ReadPrefixCode(d.br, distAlpha); err != nil {
			return err
		}
	}
	end := len(d.out) + mlen
	postMask := uint32(1)<<npostfix - 1
	for len(d.out) < end {
		if d.br.Overflowed() {
			return errCorrupt
		}
		if bs[1].left == 0 {
			if err := d.switchBlock(&bs[1]); err != nil {
				return err
			}
		}
		bs[1].left--
		c := cmd[bs[1].cur].Read(d.br)
		if c >= 704 {
			return errCorrupt
		}
		cell := cmdCells[c>>6]
		ic := cell[0] + (c>>3)&7
		cc := cell[1] + c&7
		ilen := int(insertCodes[ic].base + d.bits(insertCodes[ic].nbits))
		clen := int(copyCodes[cc].base + d.bits(copyCodes[cc].nbits))
		for k := 0; k < ilen; k++ {
			if bs[0].left == 0 {
				if err := d.switchBlock(&bs[0]); err != nil {
					return err
				}
			}
			bs[0].left--
			var p1, p2 byte
			if n := len(d.out); n > 0 {
				p1 = d.out[n-1]
				if n > 1 {
					p2 = d.out[n-2]
				}
			}
			mode := int(modes[bs[0].cur])
			ctx := contextLUT[mode<<9+int(p1)] | contextLUT[mode<<9+256+int(p2)]
			tree := cmapL[64*int(bs[0].cur)+int(ctx)]
			d.out = append(d.out, byte(lit[tree].Read(d.br)))
			if len(d.out) > end {
				return errCorrupt
			}
		}
		if len(d.out) >= end {
			break
		}
		// distance
		var distance int
		push := true
		if c < 128 {
			distance = dist[3]
			push = false
		} else {
			if bs[2].left == 0 {
				if err := d.switchBlock(&bs[2]); err != nil {
					return err
				}
			}
			bs[2].left--
			dctx := 3
			if clen <= 4 {
				dctx = clen - 2
			}
			code := dst[cmapD[4*int(bs[2].cur)+dctx]].Read(d.br)
			switch {
			case code < 16:
				if code == 0 {
					push = false
				}
				ref := [16]int{3, 2, 1, 0, 3, 3, 3, 3, 3, 3, 2, 2, 2, 2, 2, 2}[code]
				delta := [16]int{0, 0, 0, 0, -1, 1, -2, 2, -3, 3, -1, 1, -2, 2, -3, 3}[code]
				distance = dist[ref] + delta
				if distance <= 0 {
					return errCorrupt
				}
			case code < 16+ndirect:
				distance = int(code) - 15
			default:
				x := code - ndirect - 16
				ndist := 1 + uint(x>>(npostfix+1))
				hcode := x >> npostfix
				lcode := x & postMask
				offset := (2 + int(hcode&1)) << ndist
				offset -= 4
				distance = ((offset+int(d.bits(ndist)))<<npostfix + int(lcode) + int(ndirect) + 1)
			}
		}
		maxDist := min(len(d.out), maxBackward)
		if distance > maxDist {
			// static dictionary reference
			if clen < 4 || clen > 24 {
				return errCorrupt
			}
			wid := distance - maxDist - 1
			nb := dictSizeBits[clen]
			idx := wid & (1<<nb - 1)
			t := wid >> nb
			if t >= 121 {
				return errCorrupt
			}
			off := dictOffsets[clen] + idx*clen
			w := transformWord(dictionary[off:off+clen], t)
			d.out = append(d.out, w...)
			if len(d.out) > end {
				return errCorrupt
			}
			continue
		}
		if push {
			dist[0], dist[1], dist[2], dist[3] = dist[1], dist[2], dist[3], distance
		}
		if len(d.out)+clen > end {
			return errCorrupt
		}
		src := len(d.out) - distance
		for k := 0; k < clen; k++ {
			d.out = append(d.out, d.out[src+k])
		}
	}
	return nil
}

// ContextID returns the literal context for mode (0 LSB6, 1 MSB6, 2 UTF8,
// 3 signed) given the two previous bytes.
func ContextID(mode int, p1, p2 byte) uint8 {
	return contextLUT[mode<<9+int(p1)] | contextLUT[mode<<9+256+int(p2)]
}
