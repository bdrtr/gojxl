package enc

import (
	"math"
	"math/bits"

	"gojxl/internal/bitio"
)

// LZ77 over hybrid-integer symbol streams. A copy is coded as a length
// token (minSym + length token) in the context of its first symbol,
// followed by a distance in the extra, final context.

const (
	lzMinSym  = 224
	lzMinLen  = 3
	lzWindow  = 1 << 20
	lzMaxLen  = 1 << 16
	lzHashLog = 15
)

var lzLenCfg = uintCfg{0, 0, 0}

// lzItem is a literal (length 0) or a copy.
type lzItem struct {
	pos    int32
	length int32
	dist   int32
}

// lzMatches finds greedy-with-lazy-lookahead matches in vals. Positions
// not covered by a copy are literals.
func lzMatches(vals []uint32, chain int) []lzItem {
	n := len(vals)
	head := make([]int32, 1<<lzHashLog)
	for i := range head {
		head[i] = -1
	}
	prev := make([]int32, n)
	hash := func(i int) uint32 {
		h := vals[i]*0x9E3779B1 ^ vals[i+1]*0x85EBCA77 ^ vals[i+2]*0xC2B2AE3D
		return (h * 0x27D4EB2F) >> (32 - lzHashLog)
	}
	inserted := 0
	insert := func(upto int) {
		for ; inserted < upto && inserted+2 < n; inserted++ {
			h := hash(inserted)
			prev[inserted] = head[h]
			head[h] = int32(inserted)
		}
	}
	find := func(i int) (int, int) {
		if i+lzMinLen > n {
			return 0, 0
		}
		insert(i)
		bestLen, bestDist := 0, 0
		maxLen := min(n-i, lzMaxLen)
		cand := head[hash(i)]
		for c := 0; c < chain && cand >= 0 && i-int(cand) <= lzWindow; c++ {
			j := int(cand)
			if vals[j+bestLen] == vals[i+bestLen] || bestLen == 0 {
				l := 0
				for l < maxLen && vals[j+l] == vals[i+l] {
					l++
				}
				if l > bestLen {
					bestLen, bestDist = l, i-j
					if l == maxLen {
						break
					}
				}
			}
			cand = prev[j]
		}
		return bestLen, bestDist
	}
	worth := func(l, d int) bool { return l >= 5 || (l == 4 && d < 1<<12) || (l == 3 && d < 64) }
	var out []lzItem
	for i := 0; i < n; {
		l, d := find(i)
		if !worth(l, d) {
			i++
			continue
		}
		// lazy: prefer a longer match starting at the next position
		if l < 64 {
			if l2, d2 := find(i + 1); l2 > l+1 && worth(l2, d2) {
				i++
				l, d = l2, d2
			}
		}
		out = append(out, lzItem{pos: int32(i), length: int32(l), dist: int32(d)})
		insert(i + l)
		i += l
	}
	return out
}

// writeStreamLZ writes an ANS code and a single stream of (ctx, value)
// symbols, using LZ77 if it makes the result smaller. cfg must code all
// literal values with tokens below lzMinSym.
func writeStreamLZ(w *bitio.Writer, ctxs []int, vals []uint32, nctx int, cfg uintCfg, maxClusters int) {
	plain := &bitio.Writer{}
	{
		h := newHistograms(nctx, cfg)
		for i, v := range vals {
			h.add(ctxs[i], v)
		}
		c := writeANSCodeClustered(plain, cfg, h.counts, maxClusters)
		st := &ansStream{c: c}
		for i, v := range vals {
			st.add(ctxs[i], v)
		}
		st.flush(plain)
	}
	best := plain
	if len(vals) >= 32 {
		if lzw := writeLZ(ctxs, vals, nctx, cfg, maxClusters); lzw != nil && lzw.BitsWritten() < best.BitsWritten() {
			best = lzw
		}
	}
	w.AppendWriter(best)
}

func writeLZ(ctxs []int, vals []uint32, nctx int, cfg uintCfg, maxClusters int) *bitio.Writer {
	items := lzMatches(vals, 128)
	if len(items) == 0 {
		return nil
	}
	dctx := nctx
	counts := make([][]uint64, nctx+1)
	for i := range counts {
		counts[i] = make([]uint64, 256)
	}
	visit := func(lit func(ctx int, v uint32), cp func(ctx int, it lzItem)) {
		p := 0
		for _, it := range items {
			for ; p < int(it.pos); p++ {
				lit(ctxs[p], vals[p])
			}
			cp(ctxs[p], it)
			p += int(it.length)
		}
		for ; p < len(vals); p++ {
			lit(ctxs[p], vals[p])
		}
	}
	visit(func(ctx int, v uint32) {
		t, _, _ := cfg.encode(v)
		counts[ctx][t]++
	}, func(ctx int, it lzItem) {
		t, _, _ := lzLenCfg.encode(uint32(it.length - lzMinLen))
		counts[ctx][lzMinSym+t]++
		t, _, _ = cfg.encode(uint32(it.dist - 1))
		counts[dctx][t]++
	})
	cm, cl := clusterHistograms(counts, maxClusters)
	w := &bitio.Writer{}
	c := writeANSCodeLZ(w, &histograms{cfg: cfg, counts: cl}, cm, &lz77Params{lzMinSym, lzMinLen, lzLenCfg})
	st := &ansStream{c: c}
	visit(func(ctx int, v uint32) { st.add(ctx, v) }, func(ctx int, it lzItem) {
		t, nb, bv := lzLenCfg.encode(uint32(it.length - lzMinLen))
		st.addTok(ctx, lzMinSym+t, nb, bv)
		st.add(dctx, uint32(it.dist-1))
	})
	st.flush(w)
	return w
}

// lzLenFlag marks a copy length in visited (ctx, value) streams.
const lzLenFlag = 1 << 31

// lzMinEffort is the lowest effort that tries LZ77 on modular streams.
const lzMinEffort = 4

// specialDistances are the decoder's 2-D distance codes (dx, dy).
var specialDist = [120][2]int8{
	{0, 1}, {1, 0}, {1, 1}, {-1, 1}, {0, 2}, {2, 0}, {1, 2}, {-1, 2}, {2, 1}, {-2, 1},
	{2, 2}, {-2, 2}, {0, 3}, {3, 0}, {1, 3}, {-1, 3}, {3, 1}, {-3, 1}, {2, 3}, {-2, 3},
	{3, 2}, {-3, 2}, {0, 4}, {4, 0}, {1, 4}, {-1, 4}, {4, 1}, {-4, 1}, {3, 3}, {-3, 3},
	{2, 4}, {-2, 4}, {4, 2}, {-4, 2}, {0, 5}, {3, 4}, {-3, 4}, {4, 3}, {-4, 3}, {5, 0},
	{1, 5}, {-1, 5}, {5, 1}, {-5, 1}, {2, 5}, {-2, 5}, {5, 2}, {-5, 2}, {4, 4}, {-4, 4},
	{3, 5}, {-3, 5}, {5, 3}, {-5, 3}, {0, 6}, {6, 0}, {1, 6}, {-1, 6}, {6, 1}, {-6, 1},
	{2, 6}, {-2, 6}, {6, 2}, {-6, 2}, {4, 5}, {-4, 5}, {5, 4}, {-5, 4}, {3, 6}, {-3, 6},
	{6, 3}, {-6, 3}, {0, 7}, {7, 0}, {1, 7}, {-1, 7}, {5, 5}, {-5, 5}, {7, 1}, {-7, 1},
	{4, 6}, {-4, 6}, {6, 4}, {-6, 4}, {2, 7}, {-2, 7}, {7, 2}, {-7, 2}, {3, 7}, {-3, 7},
	{7, 3}, {-7, 3}, {5, 6}, {-5, 6}, {6, 5}, {-6, 5}, {8, 0}, {4, 7}, {-4, 7}, {7, 4},
	{-7, 4}, {8, 1}, {8, 2}, {6, 6}, {-6, 6}, {8, 3}, {5, 7}, {-5, 7}, {7, 5}, {-7, 5},
	{8, 4}, {6, 7}, {-6, 7}, {7, 6}, {-7, 6}, {8, 5}, {7, 7}, {-7, 7}, {8, 6}, {8, 7},
}

// lzDistCoder maps distances to distance symbols for a distance
// multiplier (row width); mult == 0 means plain distances.
type lzDistCoder struct {
	mult    int
	special []uint8 // special[dist] = code+1, 0 if none
}

func newLZDistCoder(mult int) *lzDistCoder {
	d := &lzDistCoder{mult: mult}
	if mult == 0 {
		return d
	}
	d.special = make([]uint8, 8*mult+9)
	for i, sd := range specialDist {
		v := int(sd[0]) + mult*int(sd[1])
		if v >= 1 && d.special[v] == 0 {
			d.special[v] = uint8(i + 1)
		}
	}
	return d
}

func (d *lzDistCoder) symbol(dist int) uint32 {
	if d.mult == 0 {
		return uint32(dist - 1)
	}
	if dist < len(d.special) && d.special[dist] != 0 {
		return uint32(d.special[dist] - 1)
	}
	return uint32(dist + 119)
}

// lzMatchesCost finds copies in a symbol stream whose estimated cost is
// below that of the literals they replace (litCost[i] is the cost in bits
// of symbol i coded as a literal). Besides hash-chain candidates, it always
// tries distance 1 (runs) and the row above (mult).
func lzMatchesCost(vals []uint32, litCost []float32, dc *lzDistCoder, chain int) ([]lzItem, float64) {
	n := len(vals)
	if n < 2*lzMinLen {
		return nil, 0
	}
	pre := make([]float64, n+1)
	for i, c := range litCost {
		pre[i+1] = pre[i] + float64(c)
	}
	head := make([]int32, 1<<lzHashLog)
	for i := range head {
		head[i] = -1
	}
	prev := make([]int32, n)
	hash := func(i int) uint32 {
		h := vals[i]*0x9E3779B1 ^ vals[i+1]*0x85EBCA77 ^ vals[i+2]*0xC2B2AE3D
		return (h * 0x27D4EB2F) >> (32 - lzHashLog)
	}
	insert := func(i int) {
		if i+2 < n {
			h := hash(i)
			prev[i] = head[h]
			head[h] = int32(i)
		}
	}
	matchLen := func(i, j, maxLen int) int {
		l := 0
		for l < maxLen && vals[j+l] == vals[i+l] {
			l++
		}
		return l
	}
	copyCost := func(l, d int) float64 {
		c := 4 + float64(bits.Len(uint(l-lzMinLen)))*2
		ds := dc.symbol(d)
		if ds < 120 && dc.mult != 0 {
			c += 3 + float64(ds)/16
		} else {
			c += 5 + float64(bits.Len(uint(ds)))
		}
		return c
	}
	// run lengths of copies at distance 1 and at the row above, so these
	// are found in O(1) per position (long runs are common in residuals)
	runLen := func(d int) []int32 {
		r := make([]int32, n+1)
		if d < 1 || d >= n {
			return r
		}
		for i := n - 1; i >= d; i-- {
			if vals[i] == vals[i-d] {
				r[i] = min(r[i+1]+1, lzMaxLen)
			}
		}
		return r
	}
	run1 := runLen(1)
	var runUp []int32
	if dc.mult > 1 {
		runUp = runLen(dc.mult)
	}
	const chainMaxLen = 512
	var out []lzItem
	gain := 0.0
	for i := 0; i < n; {
		best, bestL, bestD := 0.0, 0, 0
		consider := func(l, d int) {
			if l < lzMinLen {
				return
			}
			g := pre[i+l] - pre[i]
			if g-7-float64(2*bits.Len(uint(l-lzMinLen))) <= best {
				return // cannot beat best even with the cheapest distance
			}
			if s := g - copyCost(l, d); s > best {
				best, bestL, bestD = s, l, d
			}
		}
		consider(int(run1[i]), 1)
		if runUp != nil {
			consider(int(runUp[i]), dc.mult)
		}
		// inside runs, chain candidates mostly repeat the run itself
		if bestL < chainMaxLen && run1[i] < 8 && i+2 < n {
			maxLen := min(n-i, chainMaxLen)
			cand := head[hash(i)]
			seenL := max(bestL, lzMinLen-1)
			for c := 0; c < chain && cand >= 0; c++ {
				d := i - int(cand)
				if d > lzWindow {
					break
				}
				// candidates come nearest first, so a farther one only
				// helps if it is longer than any seen so far
				if seenL <= maxLen && vals[int(cand)+seenL-1] == vals[i+seenL-1] {
					l := matchLen(i, int(cand), maxLen)
					seenL = max(seenL, l)
					consider(l, d)
				}
				cand = prev[cand]
			}
		}
		if bestL == 0 {
			insert(i)
			i++
			continue
		}
		gain += best
		out = append(out, lzItem{pos: int32(i), length: int32(bestL), dist: int32(bestD)})
		for k := 0; k < min(bestL, 32); k++ {
			insert(i + k)
		}
		i += bestL
	}
	return out, gain
}

// streamDistMult is the decoder's LZ77 distance multiplier of a modular
// stream: the largest width among its non-empty channels.
func streamDistMult(chs []channel, s stream) int {
	if s.local != nil {
		chs = s.local
	}
	m := 0
	for _, ci := range s.chans {
		c := chs[ci]
		w := max(0, min(s.w, c.w-s.x0))
		h := max(0, min(s.h, c.h-s.y0))
		if w > 0 && h > 0 {
			m = max(m, w)
		}
	}
	return m
}

// modularLZ77 searches copies in the cached residual streams. If the
// estimated size improves, it returns a visit function yielding the LZ77
// symbol streams and their histograms (nleaves+1 contexts).
func modularLZ77(chs []channel, all []stream, caches []residCache, counts [][]uint64, nleaves int) (func(i int, fn func(ctx int, v uint32)), [][]uint64) {
	for i := range caches {
		for _, v := range caches[i].v {
			if v&lzLenFlag != 0 {
				return nil, nil
			}
		}
	}
	// literal cost per (context, token)
	costs := make([][]float32, len(counts))
	for c, h := range counts {
		var tot uint64
		for _, n := range h {
			tot += n
		}
		costs[c] = make([]float32, len(h))
		for t, n := range h {
			costs[c][t] = float32(math.Log2(float64(tot+1) / float64(n+1)))
		}
	}
	lcs := make([][]float32, len(all))
	dcs := make([]*lzDistCoder, len(all))
	sgain := make([]float64, len(all))
	stot := make([]float64, len(all))
	parallel(len(all), func(i int) {
		c := &caches[i]
		lc := make([]float32, len(c.v))
		for k, v := range c.v {
			t, nb, _ := defaultCfg.encode(v)
			cc := costs[c.ctx[k]]
			x := float32(nb)
			if int(t) < len(cc) {
				x += cc[t]
			} else {
				x += 16
			}
			lc[k] = x
		}
		lcs[i] = lc
		dcs[i] = newLZDistCoder(streamDistMult(chs, all[i]))
		// cheap check on 8 evenly spaced chunks (about 1/8 of the stream)
		n := len(c.v)
		cl := max(n/64, 4*dcs[i].mult, 4096)
		for k := 0; k < 8; k++ {
			a := k * n / 8
			b := min(a+cl, n)
			if a >= b {
				continue
			}
			_, g := lzMatchesCost(c.v[a:b], lc[a:b], dcs[i], 16)
			sgain[i] += g
			for _, x := range lc[a:b] {
				stot[i] += float64(x)
			}
		}
	})
	var sg, st float64
	for i := range sgain {
		sg += sgain[i]
		st += stot[i]
	}
	if sg < 0.015*st {
		return nil, nil // photographic content: copies barely help
	}
	items := make([][]lzItem, len(all))
	parallel(len(all), func(i int) {
		items[i], _ = lzMatchesCost(caches[i].v, lcs[i], dcs[i], 16)
		lcs[i] = nil
	})
	n := 0
	for _, it := range items {
		n += len(it)
	}
	if n == 0 {
		return nil, nil
	}
	visit := func(i int, fn func(ctx int, v uint32)) {
		c := &caches[i]
		p := 0
		for _, it := range items[i] {
			for ; p < int(it.pos); p++ {
				fn(int(c.ctx[p]), c.v[p])
			}
			fn(int(c.ctx[p]), lzLenFlag|uint32(it.length-lzMinLen))
			fn(nleaves, dcs[i].symbol(int(it.dist)))
			p += int(it.length)
		}
		for ; p < len(c.v); p++ {
			fn(int(c.ctx[p]), c.v[p])
		}
	}
	lzc := make([][]uint64, nleaves+1)
	for i := range lzc {
		lzc[i] = make([]uint64, 256)
	}
	var oldExtra, newExtra uint64
	for i := range all {
		for _, v := range caches[i].v {
			_, nb, _ := defaultCfg.encode(v)
			oldExtra += uint64(nb)
		}
		visit(i, func(ctx int, v uint32) {
			var t uint32
			var nb uint
			if v&lzLenFlag != 0 {
				t, nb, _ = lzLenCfg.encode(v &^ lzLenFlag)
				t += lzMinSym
			} else {
				t, nb, _ = defaultCfg.encode(v)
			}
			newExtra += uint64(nb)
			lzc[ctx][t]++
		})
	}
	// compare estimated sizes after clustering, as the writer does
	est := func(h [][]uint64) float64 {
		_, cl := clusterHistograms(h, 128)
		s := 0.0
		for _, c := range cl {
			s += histCost(c) + sigBits(c)
		}
		return s
	}
	var old, nw float64
	parallel(2, func(k int) {
		if k == 0 {
			old = est(counts) + float64(oldExtra)
		} else {
			nw = est(lzc) + float64(newExtra)
		}
	})
	// the tuned writer gains less from LZ77 than this estimate suggests,
	// so small estimated gains rarely survive
	if nw >= old*0.97 {
		return nil, nil
	}
	return visit, lzc
}
