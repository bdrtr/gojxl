package enc

import (
	"math"
	"sort"

	"gojxl/internal/bitio"
)

// nlog2n returns n*log2(n) (table for small n).
var nlogTab = func() []float64 {
	t := make([]float64, 1<<16)
	for i := 1; i < len(t); i++ {
		t[i] = float64(i) * math.Log2(float64(i))
	}
	return t
}()

func nlog2n(n uint64) float64 {
	if n < uint64(len(nlogTab)) {
		return nlogTab[n]
	}
	f := float64(n)
	return f * math.Log2(f)
}

// histCost is the ideal coded size (bits) of a histogram.
func histCost(c []uint64) float64 {
	var tot uint64
	var s float64
	for _, v := range c {
		if v != 0 {
			tot += v
			s += nlog2n(v)
		}
	}
	return nlog2n(tot) - s
}

// mergedCost is histCost of a+b without allocating.
func mergedCost(a, b []uint64) float64 {
	if len(a) < len(b) {
		a, b = b, a
	}
	var tot uint64
	var s float64
	for i, v := range a {
		if i < len(b) {
			v += b[i]
		}
		if v != 0 {
			tot += v
			s += nlog2n(v)
		}
	}
	return nlog2n(tot) - s
}

// sigBits estimates the cost of signalling a histogram.
func sigBits(c []uint64) float64 {
	n := 0
	for _, v := range c {
		if v != 0 {
			n++
		}
	}
	return 16 + 9*float64(n)
}

// clusterHistograms groups contexts into at most maxClusters histograms:
// a greedy pass (largest contexts first) followed by a few rounds of
// reassignment to the cheapest cluster. It returns the context map and the
// merged histograms.
func clusterHistograms(counts [][]uint64, maxClusters int) ([]uint8, [][]uint64) {
	maxClusters = min(maxClusters, 256)
	type ent struct {
		i   int
		tot uint64
		nz  []int32 // non-zero symbols
	}
	var es []ent
	alpha := 0
	for i, c := range counts {
		var t uint64
		var nz []int32
		for k, v := range c {
			if v != 0 {
				t += v
				nz = append(nz, int32(k))
			}
		}
		if t > 0 {
			es = append(es, ent{i, t, nz})
		}
		alpha = max(alpha, len(c))
	}
	sort.Slice(es, func(a, b int) bool { return es[a].tot > es[b].tot })
	cm := make([]uint8, len(counts))
	var cl [][]uint64
	var clTot []uint64
	// addCost is the cost increase of adding context e to cluster k
	// (computed over the context's non-zero symbols only).
	addCost := func(k int, e *ent) float64 {
		h := cl[k]
		c := counts[e.i]
		d := nlog2n(clTot[k]+e.tot) - nlog2n(clTot[k])
		for _, t := range e.nz {
			d -= nlog2n(h[t]+c[t]) - nlog2n(h[t])
		}
		return d
	}
	// remCost is the cost decrease of removing e from cluster k.
	remCost := func(k int, e *ent) float64 {
		h := cl[k]
		c := counts[e.i]
		d := nlog2n(clTot[k]) - nlog2n(clTot[k]-e.tot)
		for _, t := range e.nz {
			d -= nlog2n(h[t]) - nlog2n(h[t]-c[t])
		}
		return d
	}
	move := func(e *ent, k int, sign int) {
		h := cl[k]
		c := counts[e.i]
		for _, t := range e.nz {
			if sign > 0 {
				h[t] += c[t]
			} else {
				h[t] -= c[t]
			}
		}
		if sign > 0 {
			clTot[k] += e.tot
		} else {
			clTot[k] -= e.tot
		}
	}
	for ei := range es {
		e := &es[ei]
		c := counts[e.i]
		alone := histCost(c)
		best, bestInc := -1, math.Inf(1)
		for k := range cl {
			if inc := addCost(k, e); inc < bestInc {
				best, bestInc = k, inc
			}
		}
		if best < 0 || (len(cl) < maxClusters && alone+sigBits(c) < bestInc) {
			cl = append(cl, make([]uint64, alpha))
			clTot = append(clTot, 0)
			best = len(cl) - 1
		}
		move(e, best, 1)
		cm[e.i] = uint8(best)
	}
	if len(cl) == 0 {
		return cm, [][]uint64{make([]uint64, 1)}
	}
	// refinement: move each context to the cluster where it costs least
	for round := 0; round < 3; round++ {
		moved := 0
		for ei := range es {
			e := &es[ei]
			cur := int(cm[e.i])
			stay := remCost(cur, e)
			best, bestCost := cur, stay
			for k := range cl {
				if k == cur {
					continue
				}
				if inc := addCost(k, e); inc < bestCost-1e-6 {
					best, bestCost = k, inc
				}
			}
			if best == cur {
				continue
			}
			moved++
			move(e, cur, -1)
			move(e, best, 1)
			cm[e.i] = uint8(best)
		}
		if moved == 0 {
			break
		}
	}
	// drop clusters that became empty
	remap := make([]int, len(cl))
	var out [][]uint64
	for k, h := range cl {
		if clTot[k] == 0 {
			remap[k] = -1
			continue
		}
		remap[k] = len(out)
		out = append(out, h)
	}
	for _, e := range es {
		cm[e.i] = uint8(remap[cm[e.i]])
	}
	return cm, out
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
