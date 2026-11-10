// Package enc implements a JPEG XL encoder (lossless modular mode).
package enc

import (
	"container/heap"
	"sort"

	"gojxl/internal/bitio"
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
	// code length code over symbols 0..15 (no RLE)
	var clf [18]uint64
	for s := 0; s <= last; s++ {
		clf[lens[s]]++
	}
	cll := huffmanLengths(clf[:], 5)
	nz := 0
	for _, l := range cll {
		if l > 0 {
			nz++
		}
	}
	w.Write(2, 0) // hskip 0 (complex)
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
		return pc
	}
	clcodes := canonicalCodes(cll)
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
	for s := 0; s <= last; s++ {
		l := lens[s]
		w.Write(uint(cll[l]), uint64(clcodes[l]))
	}
	return pc
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
