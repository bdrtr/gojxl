package entropy

import "gojxl/internal/bitio"

const (
	maxPrefixBits = 15
	prefixTopBits = 10
)

// prefixHistogram is a canonical (Brotli style) prefix code.
type prefixHistogram struct {
	// top-level table indexed by the next topBits bits:
	// entry = symbol<<8 | length (length <= topBits) or, for longer codes,
	// length field == 0xff meaning "slow path".
	top     []uint32
	topBits uint
	single  int // >=0 if the code has a single symbol (0 bits)
	// slow path data (canonical decoding)
	maxLen  uint
	count   [maxPrefixBits + 1]uint16
	symbols []uint16 // sorted by (length, symbol)
}

func reverseBits(v uint32, n uint) uint32 {
	var r uint32
	for i := uint(0); i < n; i++ {
		r = r<<1 | (v & 1)
		v >>= 1
	}
	return r
}

func newPrefixSingle(sym int) *prefixHistogram {
	return &prefixHistogram{single: sym}
}

func newPrefixFromLengths(lengths []uint8) (*prefixHistogram, error) {
	h := &prefixHistogram{single: -1}
	nonzero := 0
	for s, l := range lengths {
		if l > 0 {
			nonzero++
			h.count[l]++
			if uint(l) > h.maxLen {
				h.maxLen = uint(l)
			}
			_ = s
		}
	}
	if nonzero == 0 {
		return nil, errInvalidHisto
	}
	if nonzero == 1 {
		for s, l := range lengths {
			if l > 0 {
				return newPrefixSingle(s), nil
			}
		}
	}
	// Kraft check
	space := 0
	for l := 1; l <= maxPrefixBits; l++ {
		space += int(h.count[l]) << (maxPrefixBits - l)
	}
	if space != 1<<maxPrefixBits {
		return nil, errInvalidHisto
	}
	// sorted symbols
	var offs [maxPrefixBits + 2]int
	for l := 1; l <= maxPrefixBits; l++ {
		offs[l+1] = offs[l] + int(h.count[l])
	}
	h.symbols = make([]uint16, nonzero)
	for s, l := range lengths {
		if l > 0 {
			h.symbols[offs[l]] = uint16(s)
			offs[l]++
		}
	}
	h.topBits = min(h.maxLen, prefixTopBits)
	h.top = make([]uint32, 1<<h.topBits)
	// assign canonical codes
	code := uint32(0)
	idx := 0
	for l := uint(1); l <= h.maxLen; l++ {
		for k := 0; k < int(h.count[l]); k++ {
			sym := uint32(h.symbols[idx])
			idx++
			if l <= h.topBits {
				rev := reverseBits(code, l)
				for fill := rev; fill < uint32(len(h.top)); fill += 1 << l {
					h.top[fill] = sym<<8 | uint32(l)
				}
			} else {
				rev := reverseBits(code>>(l-h.topBits), h.topBits)
				h.top[rev] = 0xff
			}
			code++
		}
		code <<= 1
	}
	return h, nil
}

func (h *prefixHistogram) readSymbol(br *bitio.Reader) uint32 {
	if h.single >= 0 {
		return uint32(h.single)
	}
	br.Refill()
	e := h.top[br.Peek(h.topBits)]
	if l := e & 0xff; l != 0xff {
		br.Consume(uint(l))
		return e >> 8
	}
	// slow canonical decode
	code := uint32(0)
	first := uint32(0)
	index := 0
	for l := uint(1); l <= h.maxLen; l++ {
		code |= uint32(br.Read(1))
		cnt := uint32(h.count[l])
		if code-first < cnt {
			return uint32(h.symbols[index+int(code-first)])
		}
		index += int(cnt)
		first = (first + cnt) << 1
		code <<= 1
	}
	return 0
}

func readPrefixHistogram(br *bitio.Reader, alphabetSize uint32) (*prefixHistogram, error) {
	if alphabetSize == 1 {
		return newPrefixSingle(0), nil
	}
	if alphabetSize > 1<<maxPrefixBits {
		return nil, errInvalidHisto
	}
	hskip := br.Read(2)
	if hskip == 1 {
		return readPrefixSimple(br, alphabetSize)
	}
	return readPrefixComplex(br, alphabetSize, hskip)
}

func ceilLog2(x uint32) uint {
	n := uint(0)
	for (uint32(1) << n) < x {
		n++
	}
	return n
}

func readPrefixSimple(br *bitio.Reader, alphabetSize uint32) (*prefixHistogram, error) {
	bits := ceilLog2(alphabetSize)
	nsym := br.Read(2) + 1
	var syms [4]uint32
	var lens [4]uint8
	switch nsym {
	case 1:
		s := br.Read(bits)
		if s >= alphabetSize {
			return nil, errInvalidHisto
		}
		return newPrefixSingle(int(s)), nil
	case 2:
		syms[0], syms[1] = br.Read(bits), br.Read(bits)
		lens[0], lens[1] = 1, 1
	case 3:
		syms[0], syms[1], syms[2] = br.Read(bits), br.Read(bits), br.Read(bits)
		lens[0], lens[1], lens[2] = 1, 2, 2
	case 4:
		for i := range 4 {
			syms[i] = br.Read(bits)
		}
		if br.Bool() {
			lens = [4]uint8{1, 2, 3, 3}
		} else {
			lens = [4]uint8{2, 2, 2, 2}
		}
	}
	lengths := make([]uint8, alphabetSize)
	for i := 0; i < int(nsym); i++ {
		if syms[i] >= alphabetSize || lengths[syms[i]] != 0 {
			return nil, errInvalidHisto
		}
		lengths[syms[i]] = lens[i]
	}
	return newPrefixFromLengths(lengths)
}

var codeLengthOrder = [18]int{1, 2, 3, 4, 0, 5, 17, 6, 16, 7, 8, 9, 10, 11, 12, 13, 14, 15}

func readPrefixComplex(br *bitio.Reader, alphabetSize, hskip uint32) (*prefixHistogram, error) {
	var clcl [18]uint8
	bitacc := 0
	nonzero := 0
	nonzeroSym := 0
	for _, idx := range codeLengthOrder[hskip:] {
		var l uint8
		switch br.Read(2) {
		case 0:
			l = 0
		case 1:
			l = 4
		case 2:
			l = 3
		default:
			if br.Bool() {
				if br.Bool() {
					l = 5
				} else {
					l = 1
				}
			} else {
				l = 2
			}
		}
		clcl[idx] = l
		if l != 0 {
			nonzero++
			nonzeroSym = idx
			bitacc += 32 >> l
			if bitacc == 32 {
				break
			}
			if bitacc > 32 {
				return nil, errInvalidHisto
			}
		}
	}
	var clh *prefixHistogram
	if nonzero == 1 {
		clh = newPrefixSingle(nonzeroSym)
	} else if bitacc != 32 {
		return nil, errInvalidHisto
	} else {
		var err error
		clh, err = newPrefixFromLengths(clcl[:])
		if err != nil {
			return nil, err
		}
	}

	lengths := make([]uint8, alphabetSize)
	acc := 0
	prevSym := uint32(8)
	lastNonzero := uint8(8)
	lastRepeat := 0
	repeat := 0
	repeatSym := uint8(0)
	for i := range lengths {
		if repeat > 0 {
			lengths[i] = repeatSym
			repeat--
		} else {
			sym := clh.readSymbol(br)
			switch {
			case sym == 0:
			case sym <= 15:
				lengths[i] = uint8(sym)
				lastNonzero = uint8(sym)
			case sym == 16:
				repeat = int(br.Read(2)) + 3
				if prevSym == 16 {
					repeat += lastRepeat*3 - 8
					lastRepeat += repeat
				} else {
					lastRepeat = repeat
				}
				repeatSym = lastNonzero
				lengths[i] = repeatSym
				repeat--
			case sym == 17:
				repeat = int(br.Read(3)) + 3
				if prevSym == 17 {
					repeat += lastRepeat*7 - 16
					lastRepeat += repeat
				} else {
					lastRepeat = repeat
				}
				repeatSym = 0
				lengths[i] = 0
				repeat--
			}
			prevSym = sym
		}
		if lengths[i] != 0 {
			acc += 1 << (maxPrefixBits - int(lengths[i]))
			if acc > 1<<maxPrefixBits {
				return nil, errInvalidHisto
			}
			if acc == 1<<maxPrefixBits && repeat == 0 {
				break
			}
		}
	}
	if acc != 1<<maxPrefixBits || repeat > 0 {
		return nil, errInvalidHisto
	}
	return newPrefixFromLengths(lengths)
}

// PrefixCode is a Brotli-style prefix code (as used by both JPEG XL and
// Brotli streams).
type PrefixCode struct{ h *prefixHistogram }

// ReadPrefixCode reads a simple or complex prefix code description.
func ReadPrefixCode(br *bitio.Reader, alphabetSize uint32) (PrefixCode, error) {
	h, err := readPrefixHistogram(br, alphabetSize)
	return PrefixCode{h}, err
}

// Read decodes one symbol.
func (p PrefixCode) Read(br *bitio.Reader) uint32 { return p.h.readSymbol(br) }
