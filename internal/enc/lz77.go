package enc

import "gojxl/internal/bitio"

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
