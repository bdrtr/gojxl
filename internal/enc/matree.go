package enc

import (
	"math"
	"sort"
	"sync"

	"gojxl/internal/bitio"
)

// MA tree learning: samples of (properties, residual tokens per candidate
// predictor) are collected from the image and a tree is grown greedily by
// splitting nodes where the estimated entropy saving exceeds a threshold.

// nbhd holds the causal neighbourhood of a sample (decoder conventions).
type nbhd struct {
	W, N, NW, NE, NN, WW, NEE int64
	wp                        int64
}

func predictOne(p int, nb *nbhd) int64 {
	switch p {
	case 0:
		return 0
	case 1:
		return nb.W
	case 2:
		return nb.N
	case 3:
		return (nb.W + nb.N) / 2
	case 4:
		pp := nb.W + nb.N - nb.NW
		if abs64(pp-nb.W) < abs64(pp-nb.N) {
			return nb.W
		}
		return nb.N
	case 5:
		return int64(clampedGradient(int32(nb.N), int32(nb.W), int32(nb.NW)))
	case 6:
		return nb.wp
	case 7:
		return nb.NE
	case 8:
		return nb.NW
	case 9:
		return nb.WW
	case 10:
		return (nb.W + nb.NW) / 2
	case 11:
		return (nb.N + nb.NW) / 2
	case 12:
		return (nb.N + nb.NE) / 2
	case 13:
		return (6*nb.N - 2*nb.NN + 7*nb.W + nb.WW + nb.NEE + 3*nb.NE + 8) / 16
	}
	return 0
}

// iterStream visits every sample of a stream in decoding order, providing
// the decoder's 16 properties (property 1, the stream id, is left 0) and
// neighbourhood. WP state is maintained when useWP is set.
func iterStream(chs []channel, s stream, useWP bool, nrefs int, full bool, fn func(k, x, y int, props *[maxProps]int32, nb *nbhd, cur int32)) {
	var props [maxProps]int32
	var nb nbhd
	if s.local != nil {
		chs = s.local
	}
	for k, ci := range s.chans {
		chn := chs[ci]
		W := chn.w
		sw, sh := min(s.w, chn.w-s.x0), min(s.h, chn.h-s.y0)
		if sw <= 0 || sh <= 0 {
			continue
		}
		// previous channels of the same size referenced by properties 16+
		var refs []channel
		for j := k - 1; j >= 0 && len(refs) < nrefs; j-- {
			rc := chs[s.chans[j]]
			if min(s.w, rc.w-s.x0) == sw && min(s.h, rc.h-s.y0) == sh {
				refs = append(refs, rc)
			}
		}
		for i := 16; i < maxProps; i++ {
			props[i] = 0
		}
		var wp *wpState
		if useWP {
			wp = newWP(sw)
		}
		props[0] = int32(s.ctxIdx[k])
		ch := chn.pix
		for y := 0; y < sh; y++ {
			base := (s.y0+y)*W + s.x0
			row := ch[base : base+sw]
			var prev, prev2 []int32
			if y > 0 {
				prev = ch[base-W : base-W+sw]
			}
			if y > 1 {
				prev2 = ch[base-2*W : base-2*W+sw]
			}
			props[2] = int32(y)
			props[9] = 0
			for x := 0; x < sw; x++ {
				var Wv, N, NW, NE, NN, WW, NEE int64
				if x > 0 {
					Wv = int64(row[x-1])
				} else if y > 0 {
					Wv = int64(prev[x])
				}
				if y > 0 {
					N = int64(prev[x])
				} else {
					N = Wv
				}
				if x > 0 && y > 0 {
					NW = int64(prev[x-1])
				} else {
					NW = Wv
				}
				if x+1 < sw && y > 0 {
					NE = int64(prev[x+1])
				} else {
					NE = N
				}
				if y > 1 {
					NN = int64(prev2[x])
				} else {
					NN = N
				}
				if x > 1 {
					WW = int64(row[x-2])
				} else {
					WW = Wv
				}
				if x+2 < sw && y > 0 {
					NEE = int64(prev[x+2])
				} else {
					NEE = NE
				}
				nb = nbhd{Wv, N, NW, NE, NN, WW, NEE, 0}
				if wp != nil {
					var mx int64
					nb.wp, mx = wp.predict(x, y, N, Wv, NE, NW, NN)
					props[15] = int32(mx)
				}
				if full {
					props[3] = int32(x)
					props[4] = int32(abs64(N))
					props[5] = int32(abs64(Wv))
					props[6] = int32(N)
					props[7] = int32(Wv)
					props[8] = int32(Wv - int64(props[9]))
					props[12] = int32(N - NE)
					props[13] = int32(N - NN)
					props[14] = int32(Wv - WW)
				}
				props[9] = int32(Wv + N - NW)
				props[10] = int32(Wv - NW)
				props[11] = int32(NW - N)
				for r, rc := range refs {
					rb := (s.y0+y)*rc.w + s.x0
					v := int64(rc.pix[rb+x])
					var vl, vt, vtl int64
					if x > 0 {
						vl = int64(rc.pix[rb+x-1])
					}
					if y > 0 {
						vt = int64(rc.pix[rb-rc.w+x])
					} else {
						vt = vl
					}
					if x > 0 && y > 0 {
						vtl = int64(rc.pix[rb-rc.w+x-1])
					} else {
						vtl = vl
					}
					pr := int64(clampedGradient(int32(vt), int32(vl), int32(vtl)))
					o := 16 + 4*r
					props[o] = int32(abs64(v))
					props[o+1] = int32(v)
					props[o+2] = int32(abs64(v - pr))
					props[o+3] = int32(v - pr)
				}
				fn(k, x, y, &props, &nb, row[x])
				if wp != nil {
					wp.update(int64(row[x]), x, y)
				}
			}
		}
	}
}

// walkStream visits every sample of a stream calling fn(ctx, residual).
func walkStream(chs []channel, s stream, root *tnode, fn func(ctx int, v uint32)) {
	if s.local == nil && isFixedTree(root) {
		walkFixed(chs, s, channelRoots(root, len(chs)), fn)
		return
	}
	if root.leaf && root.pred != 6 {
		walkSingleLeaf(chs, s, root, fn)
		return
	}
	full := treeUses(root, func(p int) bool { return p != 0 && p != 9 && p != 10 && p != 11 && p != 15 })
	iterStream(chs, s, usesWP(root), treeRefs(root), full, func(_, _, _ int, props *[maxProps]int32, nb *nbhd, cur int32) {
		n := root
		for !n.leaf {
			if props[n.prop] > n.split {
				n = n.left
			} else {
				n = n.right
			}
		}
		var pred int64
		switch n.pred {
		case 5:
			pred = int64(clampedGradient(int32(nb.N), int32(nb.W), int32(nb.NW)))
		case 6:
			pred = nb.wp
		default:
			pred = predictOne(n.pred, nb)
		}
		fn(n.ctx, packSigned64(int64(cur)-pred))
	})
}

func walkSingleLeaf(chs []channel, s stream, leaf *tnode, fn func(ctx int, v uint32)) {
	iterStream(chs, s, false, 0, false, func(_, _, _ int, _ *[maxProps]int32, nb *nbhd, cur int32) {
		fn(leaf.ctx, packSigned64(int64(cur)-predictOne(leaf.pred, nb)))
	})
}

func packSigned64(v int64) uint32 {
	return uint32(v<<1) ^ uint32(v>>63)
}

// maxProps is the number of properties supported (two channel references).
const maxProps = 24

// treeUses reports whether any split of the tree uses a property matching f.
func treeUses(n *tnode, f func(int) bool) bool {
	if n.leaf {
		return false
	}
	return f(n.prop) || treeUses(n.left, f) || treeUses(n.right, f)
}

// treeRefs returns the number of referenced previous channels.
func treeRefs(n *tnode) int {
	if n.leaf {
		return 0
	}
	r := 0
	if n.prop >= 16 {
		r = (n.prop-16)/4 + 1
	}
	return max(r, treeRefs(n.left), treeRefs(n.right))
}

// ---------------------------------------------------------------- learning

// baseProps are the properties considered for splits (plus 16+ for
// channel references).
var baseProps = []int{0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

type treeSamples struct {
	n      int
	bucket [][]uint8 // [prop][sample]
	th     [][]int32 // [prop] thresholds: bucket b holds values <= th[b] (> th[b-1])
	tok    [][]uint8 // [pred][sample]
	preds  []int     // candidate predictors
	ntok   int       // token alphabet size
	ebits  []float64 // extra bits per token
	scale  float64   // real pixels per sample
	nlog   []float64 // n*log2(n) table
	cols   [][]byte  // column per learn prop (bucket), then per pred (token)
	props  []int     // property ids of the bucket columns
}

type learnParams struct {
	maxSamples int
	maxBuckets int
	preds      []int
	threshold  float64 // minimum saving (bits) for a split
	maxLeaves  int
	nrefs      int // previous channels referenced (properties 16+)
	splitCands int // predictors split candidates are evaluated with
}

func learnParamsFor(effort int) learnParams {
	p := learnParams{
		maxSamples: 1 << 18, maxBuckets: 32,
		preds:     []int{0, 1, 2, 5, 6},
		threshold: 120, maxLeaves: 1024, nrefs: 1, splitCands: 2,
	}
	if effort >= 5 {
		p.maxSamples, p.maxBuckets = 1<<19, 64
		p.preds = []int{0, 1, 2, 4, 5, 6, 13}
		p.threshold = 96
		p.maxLeaves = 2048
		p.nrefs = 2
	}
	if effort >= 6 {
		p.maxSamples, p.maxBuckets = 1<<20, 96
		p.preds = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
		p.maxLeaves = 4096
	}
	return p
}

// collectSamples gathers samples from all streams (in parallel).
func collectSamples(chs []channel, streams []stream, lp learnParams) *treeSamples {
	total := 0
	for _, s := range streams {
		total += streamSamples(chs, s)
	}
	keep := 1.0
	if total > lp.maxSamples {
		keep = float64(lp.maxSamples) / float64(total)
	}
	thr := uint64(keep * float64(1<<32))
	lpProps := append([]int(nil), baseProps...)
	for i := 16; i < 16+4*lp.nrefs; i++ {
		lpProps = append(lpProps, i)
	}
	np := len(lpProps)
	npred := len(lp.preds)
	type part struct {
		props [][]int32
		tok   [][]uint8
	}
	parts := make([]part, len(streams))
	parallel(len(streams), func(i int) {
		pt := part{props: make([][]int32, np), tok: make([][]uint8, npred)}
		rng := uint64(i)*0x9E3779B97F4A7C15 + 1
		iterStream(chs, streams[i], true, lp.nrefs, true, func(_, _, _ int, props *[maxProps]int32, nb *nbhd, cur int32) {
			rng ^= rng << 13
			rng ^= rng >> 7
			rng ^= rng << 17
			if keep < 1 && rng&0xffffffff >= thr {
				return
			}
			for j, p := range lpProps {
				pt.props[j] = append(pt.props[j], props[p])
			}
			for j, pr := range lp.preds {
				tok, _, _ := defaultCfg.encode(packSigned64(int64(cur) - predictOne(pr, nb)))
				pt.tok[j] = append(pt.tok[j], uint8(min(tok, 255)))
			}
		})
		parts[i] = pt
	})
	ts := &treeSamples{preds: lp.preds, scale: 1 / keep, props: lpProps}
	props := make([][]int32, np)
	ts.tok = make([][]uint8, npred)
	for _, pt := range parts {
		for j := range props {
			props[j] = append(props[j], pt.props[j]...)
		}
		for j := range ts.tok {
			ts.tok[j] = append(ts.tok[j], pt.tok[j]...)
		}
	}
	ts.n = len(props[0])
	for _, t := range ts.tok {
		for _, v := range t {
			ts.ntok = max(ts.ntok, int(v)+1)
		}
	}
	ts.ebits = make([]float64, ts.ntok)
	for t := range ts.ebits {
		ts.ebits[t] = float64(tokExtraBits(uint32(t)))
	}
	// quantise properties into buckets
	ts.bucket = make([][]uint8, np)
	ts.th = make([][]int32, np)
	parallel(np, func(j int) {
		vals := props[j]
		// quantiles from a subsample
		step := max(1, len(vals)/65536)
		sorted := make([]int32, 0, len(vals)/step+1)
		for i := 0; i < len(vals); i += step {
			sorted = append(sorted, vals[i])
		}
		sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
		var th []int32
		nb := lp.maxBuckets
		for b := 1; b < nb; b++ {
			v := sorted[(len(sorted)-1)*b/nb]
			if len(th) == 0 || v > th[len(th)-1] {
				th = append(th, v)
			}
		}
		// small-range properties (channel, palette indices): exact values
		uniq := sorted[:0:0]
		for i, v := range sorted {
			if i == 0 || v != sorted[i-1] {
				uniq = append(uniq, v)
				if len(uniq) > nb {
					break
				}
			}
		}
		if len(uniq) <= nb {
			th = uniq[:len(uniq)-1]
		}
		bk := make([]uint8, len(vals))
		for i, v := range vals {
			bk[i] = uint8(sort.Search(len(th), func(k int) bool { return th[k] >= v }))
		}
		ts.bucket[j] = bk
		ts.th[j] = th
	})
	// column-major sample matrix: buckets then tokens
	ts.cols = append(append([][]byte(nil), ts.bucket...), ts.tok...)
	ts.bucket, ts.tok = nil, nil
	ts.nlog = make([]float64, ts.n+1)
	for i := 1; i <= ts.n; i++ {
		ts.nlog[i] = float64(i) * math.Log2(float64(i))
	}
	return ts
}

// tokExtraBits returns the number of raw bits following a token.
func tokExtraBits(tok uint32) uint {
	c := defaultCfg
	if tok < 1<<c.se {
		return 0
	}
	return uint(c.se - c.msb - c.lsb + ((tok - (1 << c.se)) >> (c.msb + c.lsb)))
}

// cost returns the estimated bits of a histogram (entropy + extra bits).
func (ts *treeSamples) cost(h []uint32) float64 {
	var tot uint32
	var c float64
	for t, v := range h {
		if v != 0 {
			tot += v
			c += float64(v)*ts.ebits[t] - ts.nlog[v]
		}
	}
	if tot == 0 {
		return 0
	}
	return c + ts.nlog[tot]
}

type learner struct {
	ts *treeSamples
	lp learnParams
}

// learnTree grows a tree over the samples.
func learnTree(ts *treeSamples, lp learnParams) *tnode {
	n, _ := learnTreeCost(ts, lp)
	return n
}

// learnTreeCost also returns the estimated coded size (bits) of the samples
// scaled to the whole image.
func learnTreeCost(ts *treeSamples, lp learnParams) (*tnode, float64) {
	l := &learner{ts: ts, lp: lp}
	n, c := l.build(0, ts.n, lp.maxLeaves)
	return n, c * ts.scale
}

// leafInfo returns the best predictor (index) and its cost for samples.
// It also returns the indices of the splitCands cheapest predictors, which
// are the ones split candidates are evaluated with.
func (l *learner) leafInfo(lo, hi int) (int, float64, []int) {
	ts := l.ts
	h := make([]uint32, ts.ntok)
	np := len(ts.props)
	costs := make([]float64, len(ts.preds))
	order := make([]int, len(ts.preds))
	for pi := range ts.preds {
		clear(h)
		for _, t := range ts.cols[np+pi][lo:hi] {
			h[t]++
		}
		costs[pi] = ts.cost(h)
		order[pi] = pi
	}
	sort.SliceStable(order, func(a, b int) bool { return costs[order[a]] < costs[order[b]] })
	k := min(len(order), max(1, l.lp.splitCands))
	return order[0], costs[order[0]], order[:k]
}

type splitChoice struct {
	prop   int // index into ts.props
	bucket int // split: left = buckets > bucket
	gain   float64
}

func (l *learner) bestSplit(lo, hi int, parentCost float64, cands []int) splitChoice {
	ts := l.ts
	np := len(ts.props)
	nc := len(cands)
	n := hi - lo
	// compact the token alphabet of the candidate predictors in this node
	var tmap [256]int32
	for i := range tmap {
		tmap[i] = -1
	}
	T := 0
	for _, pi := range cands {
		for _, t := range ts.cols[np+pi][lo:hi] {
			tmap[t] = 0
		}
	}
	var ebits [256]float64
	for t := range tmap {
		if tmap[t] == 0 {
			tmap[t] = int32(T)
			ebits[T] = ts.ebits[t]
			T++
		}
	}
	res := make([]splitChoice, np)
	eval := func(j int) {
		// buckets present in this node
		var bmap [256]int32
		bc := ts.cols[j][lo:hi]
		for _, b := range bc {
			bmap[b] = 1
		}
		var borig [256]int
		B := 0
		for b := range bmap {
			if bmap[b] != 0 {
				bmap[b] = int32(B)
				borig[B] = b
				B++
			}
		}
		if B < 2 {
			return
		}
		// hist layout: [cand][bucket][token]
		hist := make([]uint32, nc*B*T)
		for ci, pi := range cands {
			hp := hist[ci*B*T : (ci+1)*B*T]
			tc := ts.cols[np+pi][lo:hi]
			tc = tc[:len(bc)]
			for i, b := range bc {
				hp[int(bmap[b])*T+int(tmap[tc[i]])]++
			}
		}
		costR := make([]float64, B)
		costL := make([]float64, B)
		for b := range costR {
			costR[b], costL[b] = math.Inf(1), math.Inf(1)
		}
		r := make([]uint32, T)
		lft := make([]uint32, T)
		for ci := 0; ci < nc; ci++ {
			hc := hist[ci*B*T : (ci+1)*B*T]
			clear(lft)
			var sL, eL float64
			var nL uint32
			for b := 0; b < B; b++ {
				for k, v := range hc[b*T : b*T+T] {
					lft[k] += v
				}
			}
			for k, v := range lft {
				if v != 0 {
					sL += ts.nlog[v]
					eL += float64(v) * ebits[k]
					nL += v
				}
			}
			clear(r)
			var sR, eR float64
			var nR uint32
			for b := 0; b < B-1; b++ {
				for k, h := range hc[b*T : b*T+T] {
					if h == 0 {
						continue
					}
					sR += ts.nlog[r[k]+h] - ts.nlog[r[k]]
					r[k] += h
					sL += ts.nlog[lft[k]-h] - ts.nlog[lft[k]]
					lft[k] -= h
					e := float64(h) * ebits[k]
					eR += e
					eL -= e
					nR += h
					nL -= h
				}
				costR[b] = min(costR[b], ts.nlog[nR]-sR+eR)
				costL[b] = min(costL[b], ts.nlog[nL]-sL+eL)
			}
		}
		bestGain, bestB := 0.0, -1
		for b := 0; b < B-1; b++ {
			if g := parentCost - costR[b] - costL[b]; g > bestGain {
				bestGain, bestB = g, b
			}
		}
		if bestB >= 0 {
			res[j] = splitChoice{prop: j, bucket: borig[bestB], gain: bestGain}
		}
	}
	if n > 1<<14 {
		var wg sync.WaitGroup
		for j := 0; j < np; j++ {
			wg.Add(1)
			go func(j int) { defer wg.Done(); eval(j) }(j)
		}
		wg.Wait()
	} else {
		for j := 0; j < np; j++ {
			eval(j)
		}
	}
	best := splitChoice{prop: -1}
	for _, r := range res {
		if r.gain > best.gain {
			best = r
		}
	}
	return best
}

// build grows the subtree for samples idx with at most budget leaves (the
// budget is shared between children in proportion to their sample counts,
// keeping the result independent of scheduling).
func (l *learner) build(lo, hi, budget int) (*tnode, float64) {
	n := hi - lo
	pi, c, cands := l.leafInfo(lo, hi)
	leaf := &tnode{leaf: true, pred: l.ts.preds[pi]}
	if n < 32 || budget < 2 {
		return leaf, c
	}
	sc := l.bestSplit(lo, hi, c, cands)
	if sc.prop < 0 || sc.gain*l.ts.scale < l.lp.threshold {
		return leaf, c
	}
	// stable partition of every column: right (<= bucket) first
	b := uint8(sc.bucket)
	p := sc.prop
	key := l.ts.cols[p][lo:hi]
	mask := make([]bool, n)
	i := 0
	for k, v := range key {
		mask[k] = v > b
		if !mask[k] {
			i++
		}
	}
	tmp := make([]byte, n-i)
	for _, col := range l.ts.cols {
		c := col[lo:hi]
		w, t := 0, 0
		for k, v := range c {
			if mask[k] {
				tmp[t] = v
				t++
			} else {
				c[w] = v
				w++
			}
		}
		copy(c[w:], tmp)
	}
	nd := &tnode{prop: l.ts.props[p], split: l.ts.th[p][sc.bucket]}
	mid := lo + i
	br := min(max(1, budget*i/n), budget-1)
	bl := budget - br
	var cl, cr float64
	if n > 1<<15 {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); nd.left, cl = l.build(mid, hi, bl) }()
		nd.right, cr = l.build(lo, mid, br)
		wg.Wait()
	} else {
		nd.right, cr = l.build(lo, mid, br)
		nd.left, cl = l.build(mid, hi, bl)
	}
	return nd, cl + cr
}

// isFixedTree reports whether walkFixed can evaluate the tree.
func isFixedTree(n *tnode) bool {
	if n.leaf {
		return n.pred == 5 || n.pred == 6
	}
	switch n.prop {
	case 0, 9, 10, 11, 15:
	default:
		return false
	}
	return isFixedTree(n.left) && isFixedTree(n.right)
}

// walkFixed is the specialised walker for the fixed context trees of
// efforts 1-3 (properties 0, 9, 10, 11, 15; predictors 5 and 6).
func walkFixed(chs []channel, s stream, root []*tnode, fn func(ctx int, v uint32)) {
	for k, ci := range s.chans {
		chn := chs[ci]
		W := chn.w
		sw, sh := min(s.w, chn.w-s.x0), min(s.h, chn.h-s.y0)
		if sw <= 0 || sh <= 0 {
			continue
		}
		tree := root[min(s.ctxIdx[k], len(root)-1)]
		var wp *wpState
		if usesWP(tree) || usesProp(tree, 15) {
			wp = newWP(sw)
		}
		ch := chn.pix
		for y := 0; y < sh; y++ {
			row := ch[(s.y0+y)*W+s.x0 : (s.y0+y)*W+s.x0+sw]
			var prev, prev2 []int32
			if y > 0 {
				prev = ch[(s.y0+y-1)*W+s.x0 : (s.y0+y-1)*W+s.x0+sw]
			}
			if y > 1 {
				prev2 = ch[(s.y0+y-2)*W+s.x0 : (s.y0+y-2)*W+s.x0+sw]
			}
			for x := 0; x < sw; x++ {
				var Wv, N, NW, NE, NN int32
				if x > 0 && y > 0 {
					Wv, N, NW = row[x-1], prev[x], prev[x-1]
				} else {
					if x > 0 {
						Wv = row[x-1]
					} else if y > 0 {
						Wv = prev[x]
					}
					if y > 0 {
						N = prev[x]
					} else {
						N = Wv
					}
					NW = Wv
				}
				var wpPred, maxErr int64
				if wp != nil {
					if x+1 < sw && y > 0 {
						NE = prev[x+1]
					} else {
						NE = N
					}
					if y > 1 {
						NN = prev2[x]
					} else {
						NN = N
					}
					wpPred, maxErr = wp.predict(x, y, int64(N), int64(Wv), int64(NE), int64(NW), int64(NN))
				}
				n := tree
				for !n.leaf {
					var pv int32
					switch n.prop {
					case 10:
						pv = Wv - NW
					case 11:
						pv = NW - N
					case 15:
						pv = int32(maxErr)
					case 9:
						pv = Wv + N - NW
					}
					if pv > n.split {
						n = n.left
					} else {
						n = n.right
					}
				}
				var pred int32
				if n.pred == 6 {
					pred = int32(wpPred)
				} else {
					pred = clampedGradient(N, Wv, NW)
				}
				fn(n.ctx, bitio.PackSigned(row[x]-pred))
				if wp != nil {
					wp.update(int64(row[x]), x, y)
				}
			}
		}
	}
}

// channelRoots returns the per-channel subtree (after the channel splits).
func channelRoots(root *tnode, nc int) []*tnode {
	out := make([]*tnode, nc)
	for c := 0; c < nc; c++ {
		n := root
		for !n.leaf && n.prop == 0 {
			if int32(c) > n.split {
				n = n.left
			} else {
				n = n.right
			}
		}
		out[c] = n
	}
	return out
}
