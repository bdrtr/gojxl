// Package enc implements a JPEG XL encoder (lossless modular mode).
package enc

import (
	"container/heap"
	"sort"

	"github.com/bdrtr/gojxl/internal/bitio"
)

type hnode struct {
	w           uint64
	sym         int // -1 for internal nodes
	left, right *hnode
}

type hheap []*hnode

func (h hheap) Len() int { return len(h) }
func (h hheap) Less(i, j int) bool {
	if h[i].w != h[j].w {
		return h[i].w < h[j].w
	}
	return h[i].sym < h[j].sym
}
func (h hheap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *hheap) Push(x any)   { *h = append(*h, x.(*hnode)) }
func (h *hheap) Pop() any {
	o := *h
	n := len(o)
	x := o[n-1]
	*h = o[:n-1]
	return x
}

// huffmanLengths computes code lengths limited to maxLen for the given
// frequencies. Symbols with zero frequency get length 0.
func huffmanLengths(freq []uint64, maxLen int) []uint8 {
	n := len(freq)
	lens := make([]uint8, n)
	f := append([]uint64(nil), freq...)
	for {
		h := &hheap{}
		for s, w := range f {
			if w > 0 {
				*h = append(*h, &hnode{w: w, sym: s})
			}
		}
		if h.Len() == 0 {
			return lens
		}
		if h.Len() == 1 {
			lens[(*h)[0].sym] = 1
			return lens
		}
		heap.Init(h)
		for h.Len() > 1 {
			a := heap.Pop(h).(*hnode)
			b := heap.Pop(h).(*hnode)
			heap.Push(h, &hnode{w: a.w + b.w, sym: -1, left: a, right: b})
		}
		root := (*h)[0]
		ok := true
		var walk func(nd *hnode, d int)
		walk = func(nd *hnode, d int) {
			if nd.sym >= 0 {
				if d > maxLen {
					ok = false
				}
				lens[nd.sym] = uint8(d)
				return
			}
			walk(nd.left, d+1)
			walk(nd.right, d+1)
		}
		walk(root, 0)
		if ok {
			return lens
		}
		// flatten the distribution and retry
		for i := range f {
			if f[i] > 0 {
				f[i] = f[i]/2 + 1
			}
		}
	}
}

// canonicalCodes assigns canonical codes (bit-reversed for LSB-first output).
func canonicalCodes(lens []uint8) []uint16 {
	codes := make([]uint16, len(lens))
	var count [17]int
	maxL := 0
	for _, l := range lens {
		if l > 0 {
			count[l]++
			maxL = max(maxL, int(l))
		}
	}
	var next [17]int
	code := 0
	for l := 1; l <= maxL; l++ {
		code = (code + count[l-1]) << 1
		if l == 1 {
			code = 0
		}
		next[l] = code
	}
	// recompute properly: canonical: code for length l starts at
	// (start[l-1] + count[l-1]) << 1
	code = 0
	count[0] = 0
	for l := 1; l <= maxL; l++ {
		next[l] = code
		code = (code + count[l]) << 1
	}
	for s, l := range lens {
		if l == 0 {
			continue
		}
		c := next[l]
		next[l]++
		// reverse l bits
		r := 0
		for i := 0; i < int(l); i++ {
			r = r<<1 | (c>>i)&1
		}
		codes[s] = uint16(r)
	}
	return codes
}

// prefixCode is a written prefix code.
type prefixCode struct {
	lens  []uint8
	codes []uint16
}

var codeLengthOrder = [18]int{1, 2, 3, 4, 0, 5, 17, 6, 16, 7, 8, 9, 10, 11, 12, 13, 14, 15}

// writeCodeLengthCodeLength writes a code length code length (0..5).
func writeCLCL(w *bitio.Writer, l uint8) {
	switch l {
	case 0:
		w.Write(2, 0)
	case 4:
		w.Write(2, 1)
	case 3:
		w.Write(2, 2)
	case 2:
		w.Write(2, 3)
		w.Write(1, 0)
	case 1:
		w.Write(2, 3)
		w.Write(1, 1)
		w.Write(1, 0)
	case 5:
		w.Write(2, 3)
		w.Write(1, 1)
		w.Write(1, 1)
	}
}

func ceilLog2(x int) int {
	n := 0
	for (1 << n) < x {
		n++
	}
	return n
}

// writePrefixCode builds and writes a prefix code for an alphabet of size
// alpha (> 1) with the given symbol frequencies.
func writePrefixCode(w *bitio.Writer, freq []uint64, alpha int) *prefixCode {
	used := 0
	last := -1
	for s := 0; s < alpha; s++ {
		if freq[s] > 0 {
			used++
			last = s
		}
	}
	pc := &prefixCode{lens: make([]uint8, alpha), codes: make([]uint16, alpha)}
	if used <= 1 {
		// simple code with a single symbol: zero bits per symbol
		if last < 0 {
			last = 0
		}
		w.Write(2, 1) // hskip = 1 => simple
		w.Write(2, 0) // NSYM-1 = 0
		w.Write(uint(ceilLog2(alpha)), uint64(last))
		return pc
	}
	lens := huffmanLengths(freq[:alpha], 15)
	copy(pc.lens, lens)
	pc.codes = canonicalCodes(pc.lens)
	// code length symbols with run-length codes 16 (repeat previous
	// non-zero length) and 17 (repeat zero), as libbrotli's WriteHuffmanTree
	toks, extra := codeLengthTokens(lens[:last+1])
	var clf [18]uint64
	for _, t := range toks {
		clf[t]++
	}
	cll := huffmanLengths(clf[:], 5)
	nz := 0
	for _, l := range cll {
		if l > 0 {
			nz++
		}
	}
	w.Write(2, 0) // hskip 0 (complex)
	clcodes := canonicalCodes(cll)
	if nz == 1 {
		// single code length symbol: written with any nonzero length, all
		// other entries zero; it then costs zero bits per symbol length.
		for _, idx := range codeLengthOrder {
			if cll[idx] > 0 {
				writeCLCL(w, 1)
			} else {
				writeCLCL(w, 0)
			}
		}
		cll = make([]uint8, 18)
	} else {
		acc := 0
		for _, idx := range codeLengthOrder {
			writeCLCL(w, cll[idx])
			if cll[idx] > 0 {
				acc += 32 >> cll[idx]
				if acc == 32 {
					break
				}
			}
		}
	}
	for i, t := range toks {
		w.Write(uint(cll[t]), uint64(clcodes[t]))
		switch t {
		case 16:
			w.Write(2, uint64(extra[i]))
		case 17:
			w.Write(3, uint64(extra[i]))
		}
	}
	return pc
}

// codeLengthTokens run-length codes a code length sequence.
func codeLengthTokens(lens []uint8) (toks, extra []uint8) {
	emit := func(t, e uint8) {
		toks = append(toks, t)
		extra = append(extra, e)
	}
	reverse := func(from int) {
		for a, b := from, len(toks)-1; a < b; a, b = a+1, b-1 {
			toks[a], toks[b] = toks[b], toks[a]
			extra[a], extra[b] = extra[b], extra[a]
		}
	}
	prev := uint8(8)
	for i := 0; i < len(lens); {
		v := lens[i]
		reps := 1
		for i+reps < len(lens) && lens[i+reps] == v {
			reps++
		}
		i += reps
		if v == 0 {
			if reps == 11 {
				emit(0, 0)
				reps--
			}
			if reps < 3 {
				for ; reps > 0; reps-- {
					emit(0, 0)
				}
				continue
			}
			reps -= 3
			start := len(toks)
			for {
				emit(17, uint8(reps&7))
				reps >>= 3
				if reps == 0 {
					break
				}
				reps--
			}
			reverse(start)
			continue
		}
		if prev != v {
			emit(v, 0)
			reps--
		}
		prev = v
		if reps == 7 {
			emit(v, 0)
			reps--
		}
		if reps < 3 {
			for ; reps > 0; reps-- {
				emit(v, 0)
			}
			continue
		}
		reps -= 3
		start := len(toks)
		for {
			emit(16, uint8(reps&3))
			reps >>= 2
			if reps == 0 {
				break
			}
			reps--
		}
		reverse(start)
	}
	return
}

// sortedKeys is a helper for deterministic iteration.
func sortedKeys(m map[int]bool) []int {
	k := make([]int, 0, len(m))
	for v := range m {
		k = append(k, v)
	}
	sort.Ints(k)
	return k
}
