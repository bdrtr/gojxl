package enc

import (
	"github.com/bdrtr/gojxl/internal/par"
	"math"
	"sort"

	"github.com/bdrtr/gojxl/internal/bitio"
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
	return f * fastLog2(f)
}

// log2 of 1+i/256 and 1/(1+i/256), for fastLog2
var lgTab, lgInv = func() (t, inv [256]float64) {
	for i := range t {
		m := 1 + float64(i)/256
		t[i], inv[i] = math.Log2(m), 1/m
	}
	return
}()

// fastLog2 is log2(x) for normal positive x, to about 1e-10: the exponent,
// a table entry for the top 8 mantissa bits and a cubic in the rest.
func fastLog2(x float64) float64 {
	const c1, c2, c3 = 1 / math.Ln2, -1 / (2 * math.Ln2), 1 / (3 * math.Ln2)
	b := math.Float64bits(x)
	e := float64(int(b>>52) - 1023)
	i := b >> 44 & 0xff
	u := float64(b&(1<<44-1)) * 0x1p-52 * lgInv[i]
	return e + lgTab[i] + u*(c1+u*(c2+u*c3))
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
	// clNl[k][t] caches nlog2n(cl[k][t])
	var clNl [][]float64
	var clTotNl []float64 // nlog2n(clTot[k]): the same for every context tried
	addCost := func(k int, e *ent) float64 {
		h, nl := cl[k], clNl[k]
		c := counts[e.i]
		d := nlog2n(clTot[k]+e.tot) - clTotNl[k]
		for _, t := range e.nz {
			d -= nlog2n(h[t]+c[t]) - nl[t]
		}
		return d
	}
	// remCost is the cost decrease of removing e from cluster k.
	remCost := func(k int, e *ent) float64 {
		h, nl := cl[k], clNl[k]
		c := counts[e.i]
		d := clTotNl[k] - nlog2n(clTot[k]-e.tot)
		for _, t := range e.nz {
			d -= nl[t] - nlog2n(h[t]-c[t])
		}
		return d
	}
	move := func(e *ent, k int, sign int) {
		h, nl := cl[k], clNl[k]
		c := counts[e.i]
		for _, t := range e.nz {
			if sign > 0 {
				h[t] += c[t]
			} else {
				h[t] -= c[t]
			}
			nl[t] = nlog2n(h[t])
		}
		if sign > 0 {
			clTot[k] += e.tot
		} else {
			clTot[k] -= e.tot
		}
		clTotNl[k] = nlog2n(clTot[k])
	}
	// greedy assignment, largest contexts first. With many contexts, the
	// costs against the clusters there are at the start of a batch are
	// computed in parallel; applying the batch in order then recomputes
	// only the costs for clusters changed (or created) within it, so the
	// choices are exactly those of the plain sequential pass.
	const batch = 32
	inPar := par.Workers() > 1 && len(es) >= 8*batch
	var bc []float64 // batch costs: [context in batch][cluster < k0]
	var touched []bool
	for b0 := 0; b0 < len(es); b0 += batch {
		b1 := min(b0+batch, len(es))
		k0 := 0
		if inPar && len(cl) > 0 {
			k0 = len(cl)
			if cap(bc) < (b1-b0)*k0 {
				bc = make([]float64, batch*maxClusters)
			}
			bc = bc[:(b1-b0)*k0]
			parallel(b1-b0, func(j int) {
				e := &es[b0+j]
				row := bc[j*k0 : (j+1)*k0]
				for k := range row {
					row[k] = addCost(k, e)
				}
			})
			touched = append(touched[:0], make([]bool, k0)...)
		}
		for ei := b0; ei < b1; ei++ {
			e := &es[ei]
			c := counts[e.i]
			alone := histCost(c)
			best, bestInc := -1, math.Inf(1)
			for k := range cl {
				var inc float64
				if k < k0 && !touched[k] {
					inc = bc[(ei-b0)*k0+k]
				} else {
					inc = addCost(k, e)
				}
				if inc < bestInc {
					best, bestInc = k, inc
				}
			}
			if best < 0 || (len(cl) < maxClusters && alone+sigBits(c) < bestInc) {
				cl = append(cl, make([]uint64, alpha))
				clNl = append(clNl, make([]float64, alpha))
				clTot = append(clTot, 0)
				clTotNl = append(clTotNl, 0)
				best = len(cl) - 1
			}
			move(e, best, 1)
			if best < k0 {
				touched[best] = true
			}
			cm[e.i] = uint8(best)
		}
	}
	if len(cl) == 0 {
		return cm, [][]uint64{make([]uint64, 1)}
	}
	// refinement: move each context to the cluster where it costs least;
	// targets are searched in parallel against the current clusters, and
	// moves are applied in order, each rechecked against the live state
	target := make([]int32, len(es))
	for round := 0; round < 3; round++ {
		parallelChunks(len(es), 64, func(lo, hi int) {
			for ei := lo; ei < hi; ei++ {
				e := &es[ei]
				cur := int(cm[e.i])
				best, bestCost := cur, remCost(cur, e)
				for k := range cl {
					if k == cur {
						continue
					}
					if inc := addCost(k, e); inc < bestCost-1e-6 {
						best, bestCost = k, inc
					}
				}
				target[ei] = int32(best)
			}
		})
		moved := 0
		for ei := range es {
			e := &es[ei]
			cur, best := int(cm[e.i]), int(target[ei])
			if best == cur || addCost(best, e) >= remCost(cur, e)-1e-6 {
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
	// contexts without samples may map anywhere: repeat the previous
	// mapping so the context map compresses well
	used := make([]bool, len(counts))
	for _, e := range es {
		used[e.i] = true
	}
	for i := 1; i < len(cm); i++ {
		if !used[i] {
			cm[i] = cm[i-1]
		}
	}
	return cm, out
}

// writeContextMap writes an arbitrary context map.
func writeContextMap(w *bitio.Writer, cm []uint8, nclusters int) {
	var best *bitio.Writer
	try := func(t *bitio.Writer) {
		if best == nil || t.BitsWritten() < best.BitsWritten() {
			best = t
		}
	}
	if nclusters <= 8 {
		t := &bitio.Writer{}
		t.WriteBool(true)
		nb := ceilLog2(nclusters)
		t.Write(2, uint64(nb))
		for _, v := range cm {
			t.Write(uint(nb), uint64(v))
		}
		try(t)
	}
	// complex: entropy coded (LZ77 allowed for more than two contexts),
	// with or without move-to-front
	ctxs := make([]int, len(cm))
	for _, mtf := range []bool{false, true} {
		vals := make([]uint32, len(cm))
		if mtf {
			var order [256]uint8
			for i := range order {
				order[i] = uint8(i)
			}
			for i, v := range cm {
				k := 0
				for order[k] != v {
					k++
				}
				vals[i] = uint32(k)
				copy(order[1:k+1], order[:k])
				order[0] = v
			}
		} else {
			for i, v := range cm {
				vals[i] = uint32(v)
			}
		}
		t := &bitio.Writer{}
		t.WriteBool(false) // complex
		t.WriteBool(mtf)
		if len(cm) > 2 {
			writeStreamLZ(t, ctxs, vals, 1, defaultCfg, 1)
		} else {
			h := newHistograms(1, uintCfg{8, 0, 0})
			for _, v := range vals {
				h.add(0, v)
			}
			c := writeCode(t, h)
			for _, v := range vals {
				c.write(t, 0, v)
			}
		}
		try(t)
	}
	w.AppendWriter(best)
}

// writeANSCodeClustered writes an ANS code for len(counts) contexts,
// clustering them. The returned code maps contexts through the map.
func writeANSCodeClustered(w *bitio.Writer, cfg uintCfg, counts [][]uint64, maxClusters int) *ansCode {
	cm, cl := clusterHistograms(counts, maxClusters)
	h := &histograms{cfg: cfg, counts: cl}
	ac := writeANSCodeCM(w, h, cm)
	return ac
}
