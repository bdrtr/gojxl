package entropy

import "gojxl/internal/bitio"

const (
	ansLogTabSize = 12
	ansTabSize    = 1 << ansLogTabSize
	ansFinalState = 0x130000
)

// ansBucket is one entry of the alias table.
type ansBucket struct {
	freq0       uint16 // frequency of the "own" symbol (bucket index)
	freq1XorF0  uint16
	offset1     uint16 // offset for the alias symbol (already minus cutoff)
	cutoff      uint16
	aliasSymbol uint16
}

type ansHistogram struct {
	buckets      []ansBucket
	logBucket    uint
	bucketMask   uint32
	singleSymbol int // -1 if none
}

func readU8(br *bitio.Reader) uint32 {
	if br.Bool() {
		n := br.Read(3)
		return (1 << n) + br.Read(uint(n))
	}
	return 0
}

// read the log-count prefix code (7-bit table).
func readLogCount(br *bitio.Reader) uint16 {
	switch br.Read(3) {
	case 0:
		return 10
	case 1:
		for _, v := range [4]uint16{4, 0, 11, 13} {
			if br.Bool() {
				return v
			}
		}
		return 12
	case 2:
		return 7
	case 3:
		if br.Bool() {
			return 1
		}
		return 3
	case 4:
		return 6
	case 5:
		return 8
	case 6:
		return 9
	default:
		if br.Bool() {
			return 2
		}
		return 5
	}
}

func readANSHistogram(br *bitio.Reader, logAlpha uint) (*ansHistogram, error) {
	tableSize := 1 << logAlpha
	logBucket := ansLogTabSize - logAlpha
	bucketSize := uint16(1) << logBucket
	dist := make([]uint16, tableSize)
	var alphabetSize int

	if br.Bool() { // simple
		if br.Bool() {
			v0 := int(readU8(br))
			v1 := int(readU8(br))
			if v0 == v1 {
				return nil, errInvalidHisto
			}
			alphabetSize = max(v0, v1) + 1
			if alphabetSize > tableSize {
				return nil, errInvalidHisto
			}
			p := uint16(br.Read(12))
			dist[v0] = p
			dist[v1] = ansTabSize - p
		} else {
			v := int(readU8(br))
			alphabetSize = v + 1
			if alphabetSize > tableSize {
				return nil, errInvalidHisto
			}
			dist[v] = ansTabSize
		}
	} else if br.Bool() { // flat
		alphabetSize = int(readU8(br)) + 1
		if alphabetSize > tableSize {
			return nil, errInvalidHisto
		}
		base := ansTabSize / alphabetSize
		left := ansTabSize % alphabetSize
		for i := 0; i < alphabetSize; i++ {
			dist[i] = uint16(base)
			if i < left {
				dist[i]++
			}
		}
	} else {
		ln := uint(0)
		for ln < 3 && br.Bool() {
			ln++
		}
		shift := int(br.Read(ln)) + (1 << ln) - 1
		if shift > 13 {
			return nil, errInvalidHisto
		}
		alphabetSize = int(readU8(br)) + 3
		if alphabetSize > tableSize {
			return nil, errInvalidHisto
		}
		type rng struct{ s, e int }
		var repeats []rng
		omitLog, omitPos := -1, -1
		for idx := 0; idx < alphabetSize; {
			dist[idx] = readLogCount(br)
			if dist[idx] == 13 {
				rc := int(readU8(br)) + 4
				if idx+rc > alphabetSize {
					return nil, errInvalidHisto
				}
				repeats = append(repeats, rng{idx, idx + rc})
				idx += rc
				continue
			}
			if int(dist[idx]) > omitLog {
				omitLog = int(dist[idx])
				omitPos = idx
			}
			idx++
		}
		if omitPos < 0 {
			return nil, errInvalidHisto
		}
		if omitPos+1 < tableSize && dist[omitPos+1] == 13 {
			return nil, errInvalidHisto
		}
		ri := 0
		acc := 0
		prev := uint16(0)
		for idx := 0; idx < tableSize; idx++ {
			if ri < len(repeats) && repeats[ri].s <= idx {
				if repeats[ri].e == idx {
					ri++
				} else {
					dist[idx] = prev
					acc += int(prev)
					if acc > ansTabSize {
						return nil, errInvalidHisto
					}
					continue
				}
			}
			code := dist[idx]
			if code == 0 || idx == omitPos {
				prev = 0
				continue
			}
			if code > 1 {
				zeros := int(code) - 1
				bitcount := shift - ((12 - zeros) >> 1)
				if bitcount < 0 {
					bitcount = 0
				}
				if bitcount > zeros {
					bitcount = zeros
				}
				code = uint16(1<<zeros) + uint16(br.Read(uint(bitcount))<<(zeros-bitcount))
			}
			dist[idx] = code
			prev = code
			acc += int(code)
			if acc > ansTabSize {
				return nil, errInvalidHisto
			}
		}
		if acc >= ansTabSize {
			return nil, errInvalidHisto
		}
		dist[omitPos] = uint16(ansTabSize - acc)
	}

	h := &ansHistogram{
		buckets:      make([]ansBucket, tableSize),
		logBucket:    logBucket,
		bucketMask:   uint32(bucketSize) - 1,
		singleSymbol: -1,
	}
	for i, d := range dist {
		if d == ansTabSize {
			h.singleSymbol = i
			for j := range h.buckets {
				h.buckets[j] = ansBucket{
					freq0:       dist[j],
					freq1XorF0:  dist[j] ^ ansTabSize,
					offset1:     bucketSize * uint16(j),
					cutoff:      0,
					aliasSymbol: uint16(i),
				}
			}
			return h, nil
		}
	}

	cutoff := make([]uint16, tableSize)
	alias := make([]uint16, tableSize)
	offs := make([]uint16, tableSize)
	var under, over []int
	for i := 0; i < tableSize; i++ {
		cutoff[i] = dist[i]
		if i < alphabetSize {
			alias[i] = uint16(i)
		}
		if dist[i] < bucketSize {
			under = append(under, i)
		} else if dist[i] > bucketSize {
			over = append(over, i)
		}
	}
	for len(over) > 0 && len(under) > 0 {
		o := over[len(over)-1]
		over = over[:len(over)-1]
		u := under[len(under)-1]
		under = under[:len(under)-1]
		by := bucketSize - cutoff[u]
		cutoff[o] -= by
		alias[u] = uint16(o)
		offs[u] = cutoff[o]
		if cutoff[o] < bucketSize {
			under = append(under, o)
		} else if cutoff[o] > bucketSize {
			over = append(over, o)
		}
	}
	for i := 0; i < tableSize; i++ {
		b := &h.buckets[i]
		b.freq0 = dist[i]
		if cutoff[i] == bucketSize {
			b.aliasSymbol = uint16(i)
			b.offset1 = 0
			b.cutoff = 0
			b.freq1XorF0 = 0
		} else {
			b.aliasSymbol = alias[i]
			b.offset1 = offs[i] - cutoff[i]
			b.cutoff = cutoff[i]
			b.freq1XorF0 = dist[i] ^ dist[alias[i]]
		}
	}
	return h, nil
}

// readSymbol decodes one symbol updating the ANS state.
func (h *ansHistogram) readSymbol(br *bitio.Reader, state *uint32) uint32 {
	s := *state
	idx := s & 0xfff
	i := idx >> h.logBucket
	pos := idx & h.bucketMask
	b := &h.buckets[i]
	freq := uint32(b.freq0)
	sym := i
	off := pos
	if pos >= uint32(b.cutoff) {
		sym = uint32(b.aliasSymbol)
		freq ^= uint32(b.freq1XorF0)
		off += uint32(b.offset1)
	}
	s = (s>>12)*freq + off
	if s < 1<<16 {
		s = s<<16 | br.Read(16)
	}
	*state = s
	return sym
}
