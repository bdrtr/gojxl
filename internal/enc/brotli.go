package enc

import (
	"math/bits"

	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/brotli"
)

// A small Brotli (RFC 7932) compressor: LZ77 with hash chains and one
// prefix code per alphabet in each meta-block (no block splitting or
// literal context modelling). It is used for the jbrd payload and for
// brob-compressed metadata boxes.

type brLen struct{ base, nbits uint32 }

var brInsert = [24]brLen{
	{0, 0}, {1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 1}, {8, 1}, {10, 2}, {14, 2}, {18, 3}, {26, 3}, {34, 4}, {50, 4},
	{66, 5}, {98, 5}, {130, 6}, {194, 7}, {322, 8}, {578, 9}, {1090, 10}, {2114, 12}, {6210, 14}, {22594, 24},
}

var brCopy = [24]brLen{
	{2, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 0}, {7, 0}, {8, 0}, {9, 0}, {10, 1}, {12, 1}, {14, 2}, {18, 2}, {22, 3}, {30, 3},
	{38, 4}, {54, 4}, {70, 5}, {102, 5}, {134, 6}, {198, 7}, {326, 8}, {582, 9}, {1094, 10}, {2118, 24},
}

var brCells = [11][2]uint32{{0, 0}, {0, 8}, {0, 0}, {0, 8}, {8, 0}, {8, 8}, {0, 16}, {16, 0}, {8, 16}, {16, 8}, {16, 16}}

func brLenCode(t *[24]brLen, v uint32) uint32 {
	c := uint32(23)
	for c > 0 && t[c].base > v {
		c--
	}
	return c
}

// brCommand is one insert-and-copy command; copy == 0 marks the final
// insert-only command of a meta-block.
type brCommand struct {
	insStart, insLen int
	copyLen          int
	dist             int
}

type brSym struct {
	cmd      uint16
	insCode  uint8
	copyCode uint8
	dsym     int16 // -1: implicit distance
	dbits    uint8
	dextra   uint32
}

// BrotliCompress compresses data into a Brotli stream.
func BrotliCompress(data []byte) []byte {
	w := &bitio.Writer{}
	if len(data) == 0 {
		w.Write(1, 0) // WBITS 16
		w.Write(2, 3) // ISLAST, ISLASTEMPTY
		return w.Bytes()
	}
	w.Write(1, 1)
	w.Write(3, 5) // WBITS 22
	vals := make([]uint32, len(data))
	for i, b := range data {
		vals[i] = uint32(b)
	}
	ring := [4]int{16, 15, 11, 4}
	const maxBlock = 1 << 24
	for start := 0; start < len(data); start += maxBlock {
		end := min(start+maxBlock, len(data))
		brMetaBlock(w, data, vals[start:end], start, end == len(data), &ring)
	}
	out := w.Bytes()
	if stored := brotliStored(data); len(stored) <= len(out) {
		return stored
	}
	return out
}

func brMetaBlock(w *bitio.Writer, data []byte, vals []uint32, off int, last bool, ring *[4]int) {
	items := lzMatches(vals, 64)
	var cmds []brCommand
	p := 0
	for _, it := range items {
		cmds = append(cmds, brCommand{off + p, int(it.pos) - p, int(it.length), int(it.dist)})
		p = int(it.pos + it.length)
	}
	if p < len(vals) {
		cmds = append(cmds, brCommand{off + p, len(vals) - p, 0, 0})
	}
	// symbols
	syms := make([]brSym, len(cmds))
	cmdF := make([]uint64, 704)
	distF := make([]uint64, 64)
	for i, c := range cmds {
		s := &syms[i]
		ic := brLenCode(&brInsert, uint32(c.insLen))
		cc := uint32(0)
		if c.copyLen > 0 {
			cc = brLenCode(&brCopy, uint32(c.copyLen))
		}
		s.insCode, s.copyCode = uint8(ic), uint8(cc)
		implicit := c.copyLen == 0 || c.dist == ring[3]
		s.dsym = -1
		if c.copyLen > 0 {
			switch c.dist {
			case ring[3]:
				s.dsym = 0
			case ring[2]:
				s.dsym = 1
			case ring[1]:
				s.dsym = 2
			case ring[0]:
				s.dsym = 3
			default:
				x := uint32(c.dist + 3)
				nb := uint32(bits.Len32(x)) - 2
				lsb := (x >> nb) & 1
				s.dsym = int16(16 + 2*(nb-1) + lsb)
				s.dbits = uint8(nb)
				s.dextra = x & (1<<nb - 1)
			}
			if s.dsym != 0 {
				ring[0], ring[1], ring[2], ring[3] = ring[1], ring[2], ring[3], c.dist
			}
		}
		cell := -1
		if implicit && ic < 8 && cc < 16 {
			cell = int(cc >> 3)
			s.dsym = -1
		} else {
			for k := 2; k < 11; k++ {
				if ic >= brCells[k][0] && ic < brCells[k][0]+8 && cc >= brCells[k][1] && cc < brCells[k][1]+8 {
					cell = k
					break
				}
			}
			if s.dsym < 0 {
				s.dsym = 0 // final command: distance never read
			}
		}
		s.cmd = uint16(cell<<6 | int(ic&7)<<3 | int(cc&7))
		cmdF[s.cmd]++
		if s.dsym >= 0 && c.copyLen > 0 {
			distF[s.dsym]++
		}
	}
	mode, cmap, litF := brLiteralContexts(data, cmds)
	// header
	mlen := len(vals)
	w.WriteBool(last)
	if last {
		w.WriteBool(false) // ISLASTEMPTY
	}
	nib := max(4, (bits.Len(uint(mlen-1))+3)/4)
	w.Write(2, uint64(nib-4))
	w.Write(uint(4*nib), uint64(mlen-1))
	if !last {
		w.WriteBool(false) // ISUNCOMPRESSED
	}
	w.Write(3, 0) // NBLTYPESL, NBLTYPESI, NBLTYPESD = 1
	w.Write(2, 0) // NPOSTFIX
	w.Write(4, 0) // NDIRECT
	w.Write(2, uint64(mode))
	writeVarLenUint8(w, len(litF)-1) // NTREESL
	if len(litF) > 1 {
		w.WriteBool(false) // no RLE
		f := make([]uint64, len(litF))
		for _, v := range cmap {
			f[v]++
		}
		pc := writePrefixCode(w, f, len(litF))
		for _, v := range cmap {
			w.Write(uint(pc.lens[v]), uint64(pc.codes[v]))
		}
		w.WriteBool(false) // no inverse MTF
	}
	w.Write(1, 0) // NTREESD = 1
	lits := make([]*prefixCode, len(litF))
	for i, f := range litF {
		lits[i] = writePrefixCode(w, f, 256)
	}
	cmd := writePrefixCode(w, cmdF, 704)
	dst := writePrefixCode(w, distF, 64)
	for i, c := range cmds {
		s := &syms[i]
		w.Write(uint(cmd.lens[s.cmd]), uint64(cmd.codes[s.cmd]))
		ib := brInsert[s.insCode]
		w.Write(uint(ib.nbits), uint64(uint32(c.insLen)-ib.base))
		cb := brCopy[s.copyCode]
		if c.copyLen > 0 {
			w.Write(uint(cb.nbits), uint64(uint32(c.copyLen)-cb.base))
		} else {
			w.Write(uint(cb.nbits), 0)
		}
		for p := c.insStart; p < c.insStart+c.insLen; p++ {
			lit := lits[cmap[brCtx(data, p, mode)]]
			b := data[p]
			w.Write(uint(lit.lens[b]), uint64(lit.codes[b]))
		}
		if c.copyLen > 0 && s.cmd >= 128 {
			w.Write(uint(dst.lens[s.dsym]), uint64(dst.codes[s.dsym]))
			w.Write(uint(s.dbits), uint64(s.dextra))
		}
	}
}

func writeVarLenUint8(w *bitio.Writer, v int) {
	if v == 0 {
		w.Write(1, 0)
		return
	}
	n := bits.Len(uint(v)) - 1
	w.Write(1, 1)
	w.Write(3, uint64(n))
	w.Write(uint(n), uint64(v-1<<n))
}

func brCtx(data []byte, p, mode int) uint8 {
	var p1, p2 byte
	if p > 0 {
		p1 = data[p-1]
	}
	if p > 1 {
		p2 = data[p-2]
	}
	return brotli.ContextID(mode, p1, p2)
}

// brLiteralContexts picks a literal context mode and clusters the 64
// literal contexts; it returns the mode, the context map and the
// per-cluster literal histograms.
func brLiteralContexts(data []byte, cmds []brCommand) (int, []uint8, [][]uint64) {
	nlit := 0
	for _, c := range cmds {
		nlit += c.insLen
	}
	single := make([]uint64, 256)
	for _, c := range cmds {
		for _, b := range data[c.insStart : c.insStart+c.insLen] {
			single[b]++
		}
	}
	bestCost := histCost(single) + sigBits(single)
	bestMode, bestMap, bestH := 0, make([]uint8, 64), [][]uint64{single}
	if nlit < 1024 {
		return bestMode, bestMap, bestH
	}
	for _, mode := range []int{0, 2, 1} {
		counts := make([][]uint64, 64)
		for i := range counts {
			counts[i] = make([]uint64, 256)
		}
		for _, c := range cmds {
			for p := c.insStart; p < c.insStart+c.insLen; p++ {
				counts[brCtx(data, p, mode)][data[p]]++
			}
		}
		cm, cl := clusterHistograms(counts, 64)
		cost := 64.0 * 2 // context map
		for _, h := range cl {
			cost += histCost(h) + sigBits(h)
		}
		if cost < bestCost {
			bestCost, bestMode, bestMap, bestH = cost, mode, cm, cl
		}
	}
	return bestMode, bestMap, bestH
}
