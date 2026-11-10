package enc

import (
	"math"
	"sort"

	"gojxl/internal/bitio"
)

func histCost(c []uint64) float64 {
	var tot uint64
	for _, v := range c {
		tot += v
	}
	if tot == 0 {
		return 0
	}
	ft := float64(tot)
	var b float64
	for _, v := range c {
		if v > 0 {
			b += float64(v) * math.Log2(ft/float64(v))
		}
	}
	return b
}

func mergedCost(a, b []uint64) float64 {
	n := max(len(a), len(b))
	m := make([]uint64, n)
	copy(m, a)
	for i, v := range b {
		m[i] += v
	}
	return histCost(m)
}

// clusterHistograms greedily groups contexts into at most maxClusters
// histograms. It returns the context map and the merged histograms.
func clusterHistograms(counts [][]uint64, maxClusters int) ([]uint8, [][]uint64) {
	type ent struct {
		i   int
		tot uint64
	}
	var es []ent
	for i, c := range counts {
		var t uint64
		for _, v := range c {
			t += v
		}
		if t > 0 {
			es = append(es, ent{i, t})
		}
	}
	sort.Slice(es, func(a, b int) bool { return es[a].tot > es[b].tot })
	cm := make([]uint8, len(counts))
	var cl [][]uint64
	var clCost []float64
	const newClusterBits = 8 * 40 // rough histogram signalling cost
	for _, e := range es {
		c := counts[e.i]
		alone := histCost(c)
		best, bestInc := -1, math.Inf(1)
		for k := range cl {
			inc := mergedCost(cl[k], c) - clCost[k]
			if inc < bestInc {
				best, bestInc = k, inc
			}
		}
		if best < 0 || (len(cl) < maxClusters && alone+newClusterBits < bestInc) {
			cl = append(cl, append([]uint64(nil), c...))
			clCost = append(clCost, alone)
			cm[e.i] = uint8(len(cl) - 1)
			continue
		}
		m := cl[best]
		if len(c) > len(m) {
			nm := make([]uint64, len(c))
			copy(nm, m)
			m = nm
		}
		for i, v := range c {
			m[i] += v
		}
		cl[best] = m
		clCost[best] = histCost(m)
		cm[e.i] = uint8(best)
	}
	if len(cl) == 0 {
		cl = [][]uint64{make([]uint64, 1)}
	}
	return cm, cl
}

// writeContextMap writes an arbitrary context map.
func writeContextMap(w *bitio.Writer, cm []uint8, nclusters int) {
	if nclusters <= 8 {
		w.WriteBool(true)
		nb := ceilLog2(nclusters)
		w.Write(2, uint64(nb))
		for _, v := range cm {
			w.Write(uint(nb), uint64(v))
		}
		return
	}
	w.WriteBool(false) // complex
	w.WriteBool(false) // no MTF
	h := newHistograms(1, uintCfg{8, 0, 0})
	for _, v := range cm {
		h.add(0, uint32(v))
	}
	c := writeCode(w, h)
	for _, v := range cm {
		c.write(w, 0, uint32(v))
	}
}

// writeANSCodeClustered writes an ANS code for len(counts) contexts,
// clustering them. The returned code maps contexts through the map.
func writeANSCodeClustered(w *bitio.Writer, cfg uintCfg, counts [][]uint64, maxClusters int) *ansCode {
	cm, cl := clusterHistograms(counts, maxClusters)
	h := &histograms{cfg: cfg, counts: cl}
	ac := writeANSCodeCM(w, h, cm)
	return ac
}
