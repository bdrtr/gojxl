package enc

import (
	"math/bits"
	"sort"

	"gojxl/internal/bitio"
	"gojxl/internal/entropy"
)

const ansTab = 4096

// writeU8 writes the U8 variable length code used by ANS histograms.
func writeU8(w *bitio.Writer, v int) {
	if v == 0 {
		w.WriteBool(false)
		return
	}
	n := bits.Len(uint(v)) - 1
	w.WriteBool(true)
	w.Write(3, uint64(n))
	w.Write(uint(n), uint64(v-(1<<n)))
}

// writeLogCount writes a log-count with the fixed prefix code.
func writeLogCount(w *bitio.Writer, k int) {
	switch k {
	case 10:
		w.Write(3, 0)
	case 7:
		w.Write(3, 2)
	case 6:
		w.Write(3, 4)
	case 8:
		w.Write(3, 5)
	case 9:
		w.Write(3, 6)
	case 1:
		w.Write(3, 3)
		w.Write(1, 1)
	case 3:
		w.Write(3, 3)
		w.Write(1, 0)
	case 2:
		w.Write(3, 7)
		w.Write(1, 1)
	case 5:
		w.Write(3, 7)
		w.Write(1, 0)
	case 4:
		w.Write(3, 1)
		w.Write(1, 1)
	case 0:
		w.Write(3, 1)
		w.Write(2, 2) // bits: 0 then 1
	case 11:
		w.Write(3, 1)
		w.Write(3, 4) // 0,0,1
	case 12:
		w.Write(3, 1)
		w.Write(4, 0) // 0,0,0,0
	}
}

// normalize scales frequencies to integer counts summing to 4096.
func normalize(freq []uint64) []uint16 {
	var total uint64
	for _, f := range freq {
		total += f
	}
	c := make([]uint16, len(freq))
	if total == 0 {
		return c
	}
	sum := 0
	for i, f := range freq {
		if f == 0 {
			continue
		}
		v := int((f*ansTab + total/2) / total)
		if v < 1 {
			v = 1
		}
		c[i] = uint16(v)
		sum += v
	}
	// fix the sum, adjusting the largest counts first
	order := make([]int, 0, len(freq))
	for i := range freq {
		if c[i] > 0 {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return c[order[a]] > c[order[b]] })
	for sum != ansTab {
		changed := false
		for _, i := range order {
			if sum > ansTab && c[i] > 1 {
				c[i]--
				sum--
				changed = true
			} else if sum < ansTab {
				c[i]++
				sum++
				changed = true
			}
			if sum == ansTab {
				break
			}
		}
		if !changed {
			break
		}
	}
	return c
}

// ansTable is a written ANS histogram with encoder slots.
type ansTable struct {
	freq  []uint16
	slots [][]uint16
}

// writeANSHistogram writes counts (summing to 4096) and returns the table.
func writeANSHistogram(w *bitio.Writer, counts []uint16, logAlpha uint) *ansTable {
	last, nz := -1, 0
	for i, c := range counts {
		if c > 0 {
			last = i
			nz++
		}
	}
	var alpha int
	if nz <= 1 {
		if last < 0 {
			last = 0
		}
		w.WriteBool(true)  // simple
		w.WriteBool(false) // one symbol
		writeU8(w, last)
		alpha = last + 1
		full := make([]uint16, 1<<logAlpha)
		full[last] = ansTab
		return &ansTable{freq: full, slots: entropy.ANSEncodeSlots(full, alpha, logAlpha)}
	}
	alpha = max(last+1, 3)
	w.WriteBool(false) // not simple
	w.WriteBool(false) // not flat
	// shift = 13: three ones then 3 bits (13 + 1 - 8 = 6)
	w.Write(3, 7)
	w.Write(3, 6)
	writeU8(w, alpha-3)
	logc := make([]int, alpha)
	omit, omitK := -1, -1
	for i := 0; i < alpha; i++ {
		c := counts[i]
		k := 0
		if c == 1 {
			k = 1
		} else if c > 1 {
			k = bits.Len16(c)
		}
		logc[i] = k
		if k > omitK {
			omitK, omit = k, i
		}
		writeLogCount(w, k)
	}
	for i := 0; i < alpha; i++ {
		k := logc[i]
		if i == omit || k <= 1 {
			continue
		}
		c := counts[i]
		w.Write(uint(k-1), uint64(c)-(1<<(k-1)))
	}
	full := make([]uint16, 1<<logAlpha)
	copy(full, counts)
	return &ansTable{freq: full, slots: entropy.ANSEncodeSlots(full, alpha, logAlpha)}
}

// ansCode is an ANS data code.
type ansCode struct {
	cfgs   []uintCfg // per histogram
	tables []*ansTable
	cm     []uint8
}

func writeANSCode(w *bitio.Writer, h *histograms) *ansCode {
	return writeANSCodeCM(w, h, nil)
}

// writeANSCodeCM writes an ANS code; cm maps contexts to the histograms in
// h (nil = identity).
func writeANSCodeCM(w *bitio.Writer, h *histograms, cm []uint8) *ansCode {
	n := len(h.counts)
	w.WriteBool(false) // lz77
	if cm != nil {
		if len(cm) > 1 {
			writeContextMap(w, cm, n)
		}
	} else if n > 1 {
		writeIdentityContextMap(w, n)
	}
	w.WriteBool(false) // ANS
	maxAlpha := 3
	for _, c := range h.counts {
		for k := len(c) - 1; k >= 0; k-- {
			if c[k] > 0 {
				maxAlpha = max(maxAlpha, k+1)
				break
			}
		}
	}
	logAlpha := uint(max(5, ceilLog2(maxAlpha)))
	for _, c := range h.cfgs {
		logAlpha = max(logAlpha, uint(c.se))
	}
	w.Write(2, uint64(logAlpha-5))
	cfgs := h.cfgs
	if cfgs == nil {
		cfgs = make([]uintCfg, n)
		for i := range cfgs {
			cfgs[i] = h.cfg
		}
	}
	for i := 0; i < n; i++ {
		cfgs[i].write(w, uint32(logAlpha))
	}
	ac := &ansCode{cfgs: cfgs, tables: make([]*ansTable, n), cm: cm}
	for i, c := range h.counts {
		f := make([]uint64, 1<<logAlpha)
		copy(f, c)
		ac.tables[i] = writeANSHistogram(w, normalize(f), logAlpha)
	}
	return ac
}

type ansToken struct {
	ctx   uint16
	tok   uint16
	nb    uint8
	bits  uint32
	chunk int32 // -1 or 16-bit renormalisation word
}

// ansStream collects tokens of one stream and writes them.
type ansStream struct {
	c    *ansCode
	toks []ansToken
}

func (s *ansStream) add(ctx int, v uint32) {
	if s.c.cm != nil {
		ctx = int(s.c.cm[ctx])
	}
	tok, nb, bv := s.c.cfgs[ctx].encode(v)
	s.toks = append(s.toks, ansToken{ctx: uint16(ctx), tok: uint16(tok), nb: uint8(nb), bits: bv, chunk: -1})
}

// flush encodes the collected tokens into w.
func (s *ansStream) flush(w *bitio.Writer) {
	if len(s.toks) == 0 {
		return
	}
	x := uint32(0x130000)
	for i := len(s.toks) - 1; i >= 0; i-- {
		t := &s.toks[i]
		tb := s.c.tables[t.ctx]
		f := uint32(tb.freq[t.tok])
		if uint64(x) >= uint64(f)<<20 {
			t.chunk = int32(x & 0xffff)
			x >>= 16
		}
		x = ((x / f) << 12) | uint32(tb.slots[t.tok][x%f])
	}
	w.Write(32, uint64(x))
	for i := range s.toks {
		t := &s.toks[i]
		if t.chunk >= 0 {
			w.Write(16, uint64(t.chunk))
		}
		w.Write(uint(t.nb), uint64(t.bits))
	}
}
