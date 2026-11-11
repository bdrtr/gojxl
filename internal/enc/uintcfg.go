package enc

import (
	"math"
	"math/bits"

	"gojxl/internal/bitio"
)

// Per-cluster hybrid uint configuration search: after contexts are
// clustered, value statistics of each cluster are gathered and the
// (split_exponent, msb_in_token, lsb_in_token) triple with the lowest
// estimated cost is chosen.

const vhSmall = 4096

type valueHist struct {
	small [vhSmall]uint32
	large [33]uint64 // by bit length, for values >= vhSmall
	maxV  uint32
}

func (h *valueHist) add(v uint32) {
	if v < vhSmall {
		h.small[v]++
	} else {
		h.large[bits.Len32(v)]++
	}
	h.maxV = max(h.maxV, v)
}

func (h *valueHist) merge(o *valueHist) {
	for i, v := range o.small {
		h.small[i] += v
	}
	for i, v := range o.large {
		h.large[i] += v
	}
	h.maxV = max(h.maxV, o.maxV)
}

var cfgCandidates = func() []uintCfg {
	var cs []uintCfg
	for se := uint32(0); se <= 8; se++ {
		for msb := uint32(0); msb <= min(se, 3); msb++ {
			for lsb := uint32(0); lsb <= min(se-msb, 3); lsb++ {
				cs = append(cs, uintCfg{se, msb, lsb})
			}
		}
	}
	return cs
}()

// bestCfg picks the cheapest config for a value histogram whose tokens fit
// in maxTok.
func bestCfg(h *valueHist, maxTok uint32) uintCfg {
	best, bestCost := defaultCfg, math.Inf(1)
	counts := make([]uint64, 512)
	type vn struct{ v, n uint32 }
	var nzv []vn
	for v, n := range h.small {
		if n != 0 {
			nzv = append(nzv, vn{uint32(v), n})
		}
	}
	for _, c := range cfgCandidates {
		if t, _, _ := c.encode(h.maxV); t >= maxTok {
			continue
		}
		clear(counts)
		var extra float64
		for _, e := range nzv {
			v, n := e.v, e.n
			t, nb, _ := c.encode(v)
			counts[t] += uint64(n)
			extra += float64(n) * float64(nb)
		}
		for bl, n := range h.large {
			if n == 0 {
				continue
			}
			// representative value of that bit length
			t, nb, _ := c.encode(uint32(1)<<(bl-1) | 1)
			counts[t] += n
			extra += float64(n) * float64(nb)
		}
		cost := histCost(counts) + extra
		for _, n := range counts {
			if n != 0 {
				cost += 6 // rough histogram signalling cost per symbol
			}
		}
		if cost < bestCost {
			best, bestCost = c, cost
		}
	}
	return best
}

// writeANSCodeTuned clusters the contexts, picks a uint config per cluster
// from value statistics (two extra passes over the streams) and writes the
// resulting ANS code.
func writeANSCodeTuned(w *bitio.Writer, nstreams int, visit func(i int, fn func(ctx int, v uint32)), counts [][]uint64) *ansCode {
	return writeANSCodeTunedLZ(w, nstreams, visit, counts, nil)
}

// writeANSCodeTunedLZ is writeANSCodeTuned with optional LZ77: visit then
// also yields copy lengths (lzLenFlag | length-minLen) in the context of
// the copy's first symbol, and distances in the extra, last context.
func writeANSCodeTunedLZ(w *bitio.Writer, nstreams int, visit func(i int, fn func(ctx int, v uint32)), counts [][]uint64, lz *lz77Params) *ansCode {
	maxTok := uint32(256)
	if lz != nil {
		maxTok = lz.minSym
	}
	cm, cl := clusterHistograms(counts, 128)
	ncl := len(cl)
	vhs := make([][]valueHist, nstreams)
	parallel(nstreams, func(i int) {
		vh := make([]valueHist, ncl)
		visit(i, func(ctx int, v uint32) {
			if v&lzLenFlag == 0 {
				vh[cm[ctx]].add(v)
			}
		})
		vhs[i] = vh
	})
	vh := make([]valueHist, ncl)
	for _, v := range vhs {
		for c := range vh {
			vh[c].merge(&v[c])
		}
	}
	vhs = nil
	cfgs := make([]uintCfg, ncl)
	parallel(ncl, func(c int) { cfgs[c] = bestCfg(&vh[c], maxTok) })
	parts := make([][][]uint64, nstreams)
	parallel(nstreams, func(i int) {
		cnt := make([][]uint64, ncl)
		for c := range cnt {
			cnt[c] = make([]uint64, 256)
		}
		visit(i, func(ctx int, v uint32) {
			c := cm[ctx]
			if v&lzLenFlag != 0 {
				t, _, _ := lz.lenCfg.encode(v &^ lzLenFlag)
				cnt[c][lz.minSym+t]++
				return
			}
			t, _, _ := cfgs[c].encode(v)
			cnt[c][t]++
		})
		parts[i] = cnt
	})
	final := make([][]uint64, ncl)
	for c := range final {
		final[c] = make([]uint64, 256)
		for _, p := range parts {
			for t, v := range p[c] {
				final[c][t] += v
			}
		}
	}
	ac := writeANSCodeLZ(w, &histograms{cfg: defaultCfg, cfgs: cfgs, counts: final}, cm, lz)
	ac.lz = lz
	return ac
}
