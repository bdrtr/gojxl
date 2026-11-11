package enc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"

	"gojxl/internal/bitio"
)

// Image is a planar integer image to be encoded losslessly.
type Image struct {
	W, H     int
	BitDepth int       // 1..16
	Grey     bool      // one colour channel
	Alpha    bool      // last channel is alpha
	Chans    [][]int32 // colour channels (1 or 3) followed by alpha
}

// Options controls encoding.
type Options struct {
	// Effort selects the context model (1 = fastest; 4+ = learned MA tree).
	Effort int
}

const groupDim = 256

func val(v uint32) bitio.U32Dist              { return bitio.Val(v) }
func bitsOff(n uint8, o uint32) bitio.U32Dist { return bitio.BitsOff(n, o) }

// ---------------------------------------------------------------- headers

func writeSize(w *bitio.Writer, width, height int) {
	w.WriteBool(false) // not small
	d := [4]bitio.U32Dist{bitsOff(9, 1), bitsOff(13, 1), bitsOff(18, 1), bitsOff(30, 1)}
	w.WriteU32(uint32(height), d[0], d[1], d[2], d[3])
	w.Write(3, 0) // ratio 0: explicit width
	w.WriteU32(uint32(width), d[0], d[1], d[2], d[3])
}

func writeBitDepth(w *bitio.Writer, bd int) {
	w.WriteBool(false) // integer
	w.WriteU32(uint32(bd), val(8), val(10), val(12), bitsOff(6, 1))
}

func writeImageHeader(w *bitio.Writer, im *Image) {
	w.Write(16, 0x0aff)
	writeSize(w, im.W, im.H)
	w.WriteBool(false) // metadata not all default
	w.WriteBool(false) // no extra fields
	writeBitDepth(w, im.BitDepth)
	w.WriteBool(im.BitDepth <= 12) // modular_16bit_buffers
	nec := 0
	if im.Alpha {
		nec = 1
	}
	w.WriteU32(uint32(nec), val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
	if im.Alpha {
		if im.BitDepth == 8 {
			w.WriteBool(true) // default alpha
		} else {
			w.WriteBool(false)
			w.WriteEnum(0) // alpha
			writeBitDepth(w, im.BitDepth)
			w.WriteU32(0, val(0), val(3), val(4), bitsOff(3, 1)) // dim shift
			w.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48))
			w.WriteBool(false) // not premultiplied
		}
	}
	w.WriteBool(false) // xyb_encoded
	// colour encoding
	if !im.Grey {
		w.WriteBool(true) // sRGB
	} else {
		w.WriteBool(false)
		w.WriteBool(false) // want_icc
		w.WriteEnum(1)     // grey
		w.WriteEnum(1)     // D65
		w.WriteBool(false) // no gamma
		w.WriteEnum(13)    // sRGB transfer
		w.WriteEnum(1)     // relative intent
	}
	w.WriteU64(0)     // extensions
	w.WriteBool(true) // default transform data
	w.ZeroPadToByte()
}

func writeFrameHeader(w *bitio.Writer, im *Image) {
	w.WriteBool(false) // not all default
	w.Write(2, 0)      // regular frame
	w.Write(1, 1)      // modular
	w.WriteU64(0)      // flags
	w.WriteBool(false) // no YCbCr
	w.WriteU32(1, val(1), val(2), val(4), val(8))
	if im.Alpha {
		w.WriteU32(1, val(1), val(2), val(4), val(8))
	}
	w.Write(2, 1)                                        // group_size_shift: 256
	w.WriteU32(1, val(1), val(2), val(3), bitsOff(3, 4)) // one pass
	w.WriteBool(false)                                   // no crop
	w.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3)) // blend replace
	if im.Alpha {
		w.WriteU32(0, val(0), val(1), val(2), bitsOff(2, 3))
	}
	w.WriteBool(true)                                                     // is_last
	w.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48)) // name
	// restoration filter: no gaborish, no EPF
	w.WriteBool(false)
	w.WriteBool(false)
	w.Write(2, 0)
	w.WriteU64(0)
	w.WriteU64(0) // frame extensions
}

// ---------------------------------------------------------------- MA tree

type tnode struct {
	leaf        bool
	prop        int
	split       int32
	left, right *tnode
	pred        int
	ctx         int
}

func bucketTree(prop int, th []int32, mk func() *tnode) *tnode {
	if len(th) == 0 {
		return mk()
	}
	m := len(th) / 2
	return &tnode{prop: prop, split: th[m],
		left:  bucketTree(prop, th[m+1:], mk),
		right: bucketTree(prop, th[:m], mk)}
}

// buildTree builds a fixed context tree: channel, then local gradients.
func buildTree(nc int, pixels int, effort int) *tnode {
	var th []int32
	switch {
	case effort <= 1 || pixels < 1<<14:
		th = []int32{-4, 0, 3}
	case pixels < 1<<17:
		th = []int32{-16, -4, 0, 3, 15}
	default:
		th = []int32{-40, -12, -3, 0, 2, 11, 39}
	}
	leaf := func() *tnode { return &tnode{leaf: true, pred: 5} }
	chanTree := func() *tnode {
		return bucketTree(10, th, func() *tnode { return bucketTree(11, th, leaf) })
	}
	cut := []int32{-500, -392, -255, -191, -127, -95, -63, -47, -31, -23, -15,
		-11, -7, -4, -3, -1, 0, 1, 3, 5, 7, 11,
		15, 23, 31, 47, 63, 95, 127, 191, 255, 392, 500}
	if pixels < 1<<15 {
		cut = []int32{-63, -15, -4, -1, 0, 1, 3, 15, 63}
	}
	if effort >= 3 {
		chanTree = func() *tnode { return bucketTree(15, cut, func() *tnode { return &tnode{leaf: true, pred: 6} }) }
	}
	var byChan func(lo, hi int) *tnode
	byChan = func(lo, hi int) *tnode {
		if hi-lo == 1 {
			return chanTree()
		}
		mid := (lo + hi) / 2
		return &tnode{prop: 0, split: int32(mid - 1), left: byChan(mid, hi), right: byChan(lo, mid)}
	}
	return byChan(0, nc)
}

// bfs returns nodes in decoder order and assigns leaf contexts.
func bfs(root *tnode) ([]*tnode, int) {
	q := []*tnode{root}
	leaves := 0
	for i := 0; i < len(q); i++ {
		n := q[i]
		if n.leaf {
			n.ctx = leaves
			leaves++
			continue
		}
		q = append(q, n.left, n.right)
	}
	return q, leaves
}

func treeTokens(nodes []*tnode, emit func(ctx int, v uint32)) {
	for _, n := range nodes {
		if n.leaf {
			emit(1, 0)
			emit(2, uint32(n.pred))
			emit(3, 0) // offset
			emit(4, 0) // mul log
			emit(5, 0) // mul bits
			continue
		}
		emit(1, uint32(n.prop+1))
		emit(0, bitio.PackSigned(n.split))
	}
}

// ---------------------------------------------------------------- streams

type channel struct {
	w, h int
	pix  []int32
}

// stream is a set of channel rectangles coded together.
type stream struct {
	chans  []int // channel indices (into the modular image)
	x0, y0 int   // rect origin (same for all channels)
	w, h   int   // rect size (clipped per channel)
	ctxIdx []int // property 0 value per channel in this stream
	// local, if set, replaces the image channels: the stream's own
	// (transformed) channels, with local transforms written by tws.
	local []channel
	tws   []func(w *bitio.Writer)
}

func clampedGradient(n, w, l int32) int32 {
	mn, mx := min(n, w), max(n, w)
	if l < mn {
		return mx
	}
	if l > mx {
		return mn
	}
	return n + w - l
}

func streamSamples(chs []channel, s stream) int {
	if s.local != nil {
		chs = s.local
	}
	n := 0
	for _, ci := range s.chans {
		c := chs[ci]
		n += max(0, min(s.w, c.w-s.x0)) * max(0, min(s.h, c.h-s.y0))
	}
	return n
}

func usesProp(n *tnode, p int) bool {
	if n.leaf {
		return false
	}
	return n.prop == p || usesProp(n.left, p) || usesProp(n.right, p)
}

func usesWP(n *tnode) bool {
	if n.leaf {
		return n.pred == 6
	}
	return n.prop == 15 || usesWP(n.left) || usesWP(n.right)
}

// buildPalette returns a sorted palette and index channel if the image has
// few enough distinct colours.
func buildPalette(chans [][]int32, limit int) ([][]int32, []int32, bool) {
	nc := len(chans)
	n := len(chans[0])
	if nc > 4 {
		return nil, nil, false
	}
	key := func(i int) uint64 {
		var k uint64
		for c := 0; c < nc; c++ {
			k = k<<16 | uint64(uint16(chans[c][i]))
		}
		return k
	}
	set := make(map[uint64]int32)
	for i := 0; i < n; i++ {
		k := key(i)
		if _, ok := set[k]; !ok {
			if len(set) >= limit {
				return nil, nil, false
			}
			set[k] = 0
		}
	}
	keys := make([]uint64, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	comp := func(k uint64, c int) int32 { return int32(uint16(k >> (16 * uint(nc-1-c)))) }
	lum := func(k uint64) int64 {
		if nc >= 3 {
			return 299*int64(comp(k, 0)) + 587*int64(comp(k, 1)) + 114*int64(comp(k, 2))
		}
		return int64(comp(k, 0))
	}
	sort.Slice(keys, func(a, b int) bool {
		la, lb := lum(keys[a]), lum(keys[b])
		if la != lb {
			return la < lb
		}
		return keys[a] < keys[b]
	})
	pal := make([][]int32, nc)
	for c := range pal {
		pal[c] = make([]int32, len(keys))
	}
	for i, k := range keys {
		set[k] = int32(i)
		for c := 0; c < nc; c++ {
			pal[c][i] = comp(k, c)
		}
	}
	idx := make([]int32, n)
	for i := 0; i < n; i++ {
		idx[i] = set[key(i)]
	}
	return pal, idx, true
}

// rctPixel applies RCT id to one pixel.
func rctPixel(id int, p [3]int32) (int32, int32, int32) {
	perm, typ := id/7, id%7
	a, B, C := p[perm%3], p[(perm+1+perm/3)%3], p[(perm+2-perm/3)%3]
	if typ == 6 {
		Co := a - C
		tmp := C + (Co >> 1)
		Cg := B - tmp
		return tmp + (Cg >> 1), Co, Cg
	}
	bb, c := B, C
	switch typ >> 1 {
	case 1:
		bb = B - a
	case 2:
		bb = B - ((a + C) >> 1)
	}
	if typ&1 != 0 {
		c = C - a
	}
	return a, bb, c
}

// chooseRCT estimates, on a subsample of rows, the cost of coding the
// colour channels under each RCT (gradient residuals in a few activity
// contexts per channel) and returns the cheapest.
func chooseRCT(src [][]int32, w, h, effort int) int {
	var ids []int
	for id := 0; id < 42; id++ {
		if id%7 == 0 && id != 0 {
			continue // pure permutations
		}
		ids = append(ids, id)
	}
	step := max(2, h/64)
	costs := make([]float64, len(ids))
	parallel(len(ids), func(k int) {
		id := ids[k]
		const nctx, npr = 4, 4
		var hist [npr][3][nctx][]uint64
		for p := range hist {
			for c := range hist[p] {
				for q := range hist[p][c] {
					hist[p][c][q] = make([]uint64, 64)
				}
			}
		}
		var extra [npr][3]float64
		prev := make([][3]int32, w)
		cur := make([][3]int32, w)
		for y0 := 1; y0 < h; y0 += step {
			for dy := -1; dy <= 0; dy++ {
				y := y0 + dy
				for x := 0; x < w; x++ {
					i := y*w + x
					a, b, c := rctPixel(id, [3]int32{src[0][i], src[1][i], src[2][i]})
					cur[x] = [3]int32{a, b, c}
				}
				if dy == 0 {
					for x := 1; x+1 < w; x++ {
						for c := 0; c < 3; c++ {
							N, W, NW, NE := prev[x][c], cur[x-1][c], prev[x-1][c], prev[x+1][c]
							act := iabs32(W-NW) + iabs32(N-NW) + iabs32(N-NE)
							q := 0
							switch {
							case act > 96:
								q = 3
							case act > 18:
								q = 2
							case act > 3:
								q = 1
							}
							preds := [npr]int32{clampedGradient(N, W, NW), N, W, (N + NE) >> 1}
							for p, pr := range preds {
								t, nb, _ := defaultCfg.encode(bitio.PackSigned(cur[x][c] - pr))
								h := hist[p][c][q]
								if int(t) >= len(h) {
									t = uint32(len(h) - 1)
								}
								h[t]++
								extra[p][c] += float64(nb)
							}
						}
					}
				}
				prev, cur = cur, prev
			}
		}
		var cost float64
		for c := 0; c < 3; c++ {
			best := math.Inf(1)
			for p := 0; p < npr; p++ {
				pc := extra[p][c]
				for q := range hist[p][c] {
					pc += histCost(hist[p][c][q])
				}
				best = min(best, pc)
			}
			cost += best
		}
		costs[k] = cost
	})
	best := 0
	for k := range ids {
		if costs[k] < costs[best] || (costs[k] == costs[best] && ids[k] == 6) {
			best = k
		}
	}
	if effort < 5 {
		return ids[best]
	}
	// Refine: learn a small tree on row strips for the most promising
	// candidates (plus identity and YCoCg) and use its cost estimate.
	order := make([]int, len(ids))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return costs[order[a]] < costs[order[b]] })
	cands := []int{0, 6}
	for _, k := range order[:min(3, len(order))] {
		if !slices.Contains(cands, ids[k]) {
			cands = append(cands, ids[k])
		}
	}
	strips := stripRows(h, 1<<18/max(w, 1))
	sh := len(strips)
	tc := make([]float64, len(cands))
	for k, id := range cands {
		chs := make([]channel, 3)
		for c := range chs {
			chs[c] = channel{w: w, h: sh, pix: make([]int32, w*sh)}
		}
		for r, y := range strips {
			for x := 0; x < w; x++ {
				i := y*w + x
				a, b, c := rctPixel(id, [3]int32{src[0][i], src[1][i], src[2][i]})
				o := r*w + x
				chs[0].pix[o], chs[1].pix[o], chs[2].pix[o] = a, b, c
			}
		}
		s := stream{chans: []int{0, 1, 2}, ctxIdx: []int{0, 1, 2}, w: w, h: sh}
		lp := learnParamsFor(4)
		lp.maxSamples = 1 << 17
		_, tc[k] = learnTreeCost(collectSamples(chs, []stream{s}, lp), lp)
	}
	bk := 0
	for k := range cands {
		if tc[k] < tc[bk] {
			bk = k
		}
	}
	return cands[bk]
}

// stripRows selects up to n rows as strips of 16 consecutive rows spread
// over the image.
func stripRows(h, n int) []int {
	if n >= h {
		r := make([]int, h)
		for i := range r {
			r[i] = i
		}
		return r
	}
	const sl = 16
	ns := max(1, n/sl)
	var r []int
	for s := 0; s < ns; s++ {
		y0 := s * (h - sl) / max(1, ns-1)
		if ns == 1 {
			y0 = (h - sl) / 2
		}
		for y := max(0, y0); y < min(h, y0+sl); y++ {
			r = append(r, y)
		}
	}
	return r
}

func iabs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// forwardRCT applies RCT id (permutation*7 + type) in place, the inverse
// of the decoder's transform.
func forwardRCT(r, g, b []int32, id int) {
	perm, typ := id/7, id%7
	orig := [3][]int32{r, g, b}
	in0, in1, in2 := orig[perm%3], orig[(perm+1+perm/3)%3], orig[(perm+2-perm/3)%3]
	n := len(r)
	o0, o1, o2 := make([]int32, n), make([]int32, n), make([]int32, n)
	for i := 0; i < n; i++ {
		a, B, C := in0[i], in1[i], in2[i]
		if typ == 6 {
			Co := a - C
			tmp := C + (Co >> 1)
			Cg := B - tmp
			o0[i], o1[i], o2[i] = tmp+(Cg>>1), Co, Cg
			continue
		}
		bb, c := B, C
		switch typ >> 1 {
		case 1:
			bb = B - a
		case 2:
			bb = B - ((a + C) >> 1)
		}
		if typ&1 != 0 {
			c = C - a
		}
		o0[i], o1[i], o2[i] = a, bb, c
	}
	copy(r, o0)
	copy(g, o1)
	copy(b, o2)
}

// forwardYCoCg applies the lossless RCT type 6.
func forwardYCoCg(r, g, b []int32) {
	for i := range r {
		R, G, B := r[i], g[i], b[i]
		Co := R - B
		tmp := B + (Co >> 1)
		Cg := G - tmp
		Y := tmp + (Cg >> 1)
		r[i], g[i], b[i] = Y, Co, Cg
	}
}

func parallel(n int, fn func(i int)) {
	workers := min(runtime.GOMAXPROCS(0), n)
	var wg sync.WaitGroup
	ch := make(chan int, n)
	for i := 0; i < n; i++ {
		ch <- i
	}
	close(ch)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// EncodeLossless encodes the image as a lossless modular JPEG XL codestream.
func EncodeLossless(im *Image, opt Options) ([]byte, error) {
	if im.W <= 0 || im.H <= 0 {
		return nil, errors.New("jxl: empty image")
	}
	if im.BitDepth < 1 || im.BitDepth > 16 {
		return nil, errors.New("jxl: unsupported bit depth")
	}
	nc := len(im.Chans)
	src := make([][]int32, nc)
	for i := range src {
		src[i] = append([]int32(nil), im.Chans[i]...)
	}
	if b := reducedBitDepth(src, im.BitDepth); b < im.BitDepth {
		// samples are exact expansions of b-bit values: code those
		shrinkBitDepth(src, im.BitDepth, b)
		cp := *im
		cp.BitDepth = b
		im = &cp
	}
	// choose transforms
	var chs []channel
	nbMeta := 0
	var transforms func(w *bitio.Writer)
	limit := min(1024, max(16, im.W*im.H/64))
	if pal, idx, ok := buildPalette(src, limit); ok && opt.Effort >= 2 {
		n := len(pal[0])
		pc := channel{w: n, h: nc, pix: make([]int32, n*nc)}
		for c := 0; c < nc; c++ {
			copy(pc.pix[c*n:], pal[c])
		}
		chs = []channel{pc, {w: im.W, h: im.H, pix: idx}}
		nbMeta = 1
		transforms = func(w *bitio.Writer) {
			w.WriteU32(1, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
			w.Write(2, 1) // palette
			w.WriteU32(0, bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
			w.WriteU32(uint32(nc), val(1), val(3), val(4), bitsOff(13, 1))
			w.WriteU32(uint32(n), bitsOff(8, 0), bitsOff(10, 256), bitsOff(12, 1280), bitsOff(16, 5376))
			w.WriteU32(0, val(0), bitsOff(8, 1), bitsOff(10, 257), bitsOff(16, 1281))
			w.Write(4, 0) // zero predictor
		}
	} else {
		var tws []func(w *bitio.Writer)
		// channel palettes for channels using few of their values
		var metas []channel
		palettized := false
		if opt.Effort >= 2 {
			for k := range src {
				vals, idx, ok := channelPalette(src[k])
				if !ok {
					continue
				}
				begin := uint32(k + len(metas))
				nb := uint32(len(vals))
				tws = append(tws, func(w *bitio.Writer) {
					w.Write(2, 1) // palette
					w.WriteU32(begin, bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
					w.WriteU32(1, val(1), val(3), val(4), bitsOff(13, 1))
					w.WriteU32(nb, bitsOff(8, 0), bitsOff(10, 256), bitsOff(12, 1280), bitsOff(16, 5376))
					w.WriteU32(0, val(0), bitsOff(8, 1), bitsOff(10, 257), bitsOff(16, 1281))
					w.Write(4, 0)
				})
				metas = append(metas, channel{w: len(vals), h: 1, pix: vals})
				src[k] = idx
				palettized = palettized || k < 3
			}
		}
		rct := 6
		if opt.Effort >= 4 && !im.Grey {
			rct = chooseRCT(src, im.W, im.H, opt.Effort)
		}
		if palettized {
			rct = 0 // RCTs on palette indices did not pay off
		}
		if !im.Grey && rct != 0 {
			forwardRCT(src[0], src[1], src[2], rct)
			begin := uint32(len(metas))
			id := uint32(rct)
			tws = append(tws, func(w *bitio.Writer) {
				w.Write(2, 0) // RCT
				w.WriteU32(begin, bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
				w.WriteU32(id, val(6), bitio.Bits(2), bitsOff(4, 2), bitsOff(6, 10))
			})
		}
		transforms = func(w *bitio.Writer) {
			w.WriteU32(uint32(len(tws)), val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
			for _, f := range tws {
				f(w)
			}
		}
		for i := len(metas) - 1; i >= 0; i-- {
			chs = append(chs, metas[i])
		}
		nbMeta = len(metas)
		for _, c := range src {
			chs = append(chs, channel{w: im.W, h: im.H, pix: c})
		}
	}

	// stream layout: global stream gets meta channels and channels that fit
	// in a group; the rest is split over groups.
	global := stream{x0: 0, y0: 0, w: 1 << 30, h: 1 << 30}
	firstGroupCh := len(chs)
	for i, c := range chs {
		if i >= nbMeta && (c.w > groupDim || c.h > groupDim) {
			firstGroupCh = i
			break
		}
		global.chans = append(global.chans, i)
		global.ctxIdx = append(global.ctxIdx, i)
	}
	xg := (im.W + groupDim - 1) / groupDim
	yg := (im.H + groupDim - 1) / groupDim
	single := xg == 1 && yg == 1
	var groups []stream
	if !single {
		for gy := 0; gy < yg; gy++ {
			for gx := 0; gx < xg; gx++ {
				s := stream{x0: gx * groupDim, y0: gy * groupDim, w: groupDim, h: groupDim}
				for i := firstGroupCh; i < len(chs); i++ {
					s.chans = append(s.chans, i)
					s.ctxIdx = append(s.ctxIdx, i-firstGroupCh)
				}
				groups = append(groups, s)
			}
		}
	}
	if opt.Effort >= 4 {
		parallel(len(groups), func(i int) { localizeGroup(chs, &groups[i]) })
	}
	all := append([]stream{global}, groups...)

	var root *tnode
	if opt.Effort >= 4 {
		lp := learnParamsFor(opt.Effort)
		root = learnTree(collectSamples(chs, all, lp), lp)
	} else {
		root = buildTree(len(chs), im.W*im.H, opt.Effort)
	}
	nodes, nleaves := bfs(root)
	// With learned trees, the residuals of the first pass are kept so the
	// later passes need not walk the tree again.
	var caches []residCache
	if opt.Effort >= 4 && nleaves <= 1<<16 {
		caches = make([]residCache, len(all))
	}
	visit := func(i int, fn func(ctx int, v uint32)) {
		if caches != nil {
			c := &caches[i]
			for k, v := range c.v {
				fn(int(c.ctx[k]), v)
			}
			return
		}
		walkStream(chs, all[i], root, fn)
	}

	// pass 1: histograms
	hs := make([]*histograms, len(all))
	parallel(len(all), func(i int) {
		h := newHistograms(nleaves, defaultCfg)
		if caches != nil {
			c := &caches[i]
			n := streamSamples(chs, all[i])
			c.ctx, c.v = make([]uint16, 0, n), make([]uint32, 0, n)
			walkStream(chs, all[i], root, func(ctx int, v uint32) {
				c.ctx = append(c.ctx, uint16(ctx))
				c.v = append(c.v, v)
				h.add(ctx, v)
			})
		} else {
			walkStream(chs, all[i], root, h.add)
		}
		hs[i] = h
	})
	data := newHistograms(nleaves, defaultCfg)
	for _, h := range hs {
		data.merge(h)
	}
	if os.Getenv("GOJXL_STATS") != "" {
		var bitsIdeal float64
		var nsamp uint64
		for _, c := range data.counts {
			var tot uint64
			for _, v := range c {
				tot += v
			}
			for _, v := range c {
				if v > 0 {
					bitsIdeal += float64(v) * -math.Log2(float64(v)/float64(tot))
				}
			}
			nsamp += tot
		}
		fmt.Fprintf(os.Stderr, "ideal token bytes %.0f (%.3f bits/sample)\n", bitsIdeal/8, bitsIdeal/float64(max(nsamp, 1)))
	}
	emit := func(visit func(i int, fn func(ctx int, v uint32)), counts [][]uint64, lz *lz77Params) ([]byte, error) {
		treeH := newHistograms(6, defaultCfg)
		treeTokens(nodes, treeH.add)

		// LfGlobal section
		lfg := &bitio.Writer{}
		lfg.WriteBool(true) // LF channel dequant all default
		lfg.WriteBool(true) // global tree present
		tc := writeCode(lfg, treeH)
		treeTokens(nodes, func(ctx int, v uint32) { tc.write(lfg, ctx, v) })
		var dc *ansCode
		if opt.Effort >= 4 {
			dc = writeANSCodeTunedLZ(lfg, len(all), visit, counts, lz)
		} else {
			dc = writeANSCodeClustered(lfg, defaultCfg, counts, 64)
		}
		lfg.WriteBool(true) // use global tree
		lfg.WriteBool(true) // default WP header
		transforms(lfg)
		gs := &ansStream{c: dc, toks: make([]ansToken, 0, streamSamples(chs, global))}
		visit(0, gs.add)
		gs.flush(lfg)

		out := &bitio.Writer{}
		writeImageHeader(out, im)
		writeFrameHeader(out, im)
		if single {
			sec := lfg.Bytes()
			out.WriteBool(false) // no permutation
			out.ZeroPadToByte()
			out.WriteU32(uint32(len(sec)), bitio.Bits(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
			out.ZeroPadToByte()
			out.AppendBytes(sec)
			return out.Bytes(), nil
		}
		// pass 2: group sections
		secs := make([][]byte, len(groups))
		parallel(len(groups), func(i int) {
			w := &bitio.Writer{}
			if len(groups[i].chans) > 0 {
				w.WriteBool(true) // use global tree
				w.WriteBool(true) // default WP
				tws := groups[i].tws
				w.WriteU32(uint32(len(tws)), val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
				for _, f := range tws {
					f(w)
				}
				st := &ansStream{c: dc, toks: make([]ansToken, 0, streamSamples(chs, groups[i]))}
				visit(1+i, st.add)
				st.flush(w)
			}
			secs[i] = w.Bytes()
		})
		nlf := ((im.W + groupDim*8 - 1) / (groupDim * 8)) * ((im.H + groupDim*8 - 1) / (groupDim * 8))
		lfSec := lfg.Bytes()
		sizes := []int{len(lfSec)}
		for i := 0; i < nlf; i++ {
			sizes = append(sizes, 0)
		}
		sizes = append(sizes, 0) // HF global
		for _, s := range secs {
			sizes = append(sizes, len(s))
		}
		out.WriteBool(false)
		out.ZeroPadToByte()
		for _, s := range sizes {
			out.WriteU32(uint32(s), bitio.Bits(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
		}
		out.ZeroPadToByte()
		out.AppendBytes(lfSec)
		for _, s := range secs {
			out.AppendBytes(s)
		}
		return out.Bytes(), nil
	}
	res, err := emit(visit, data.counts, nil)
	if err != nil || caches == nil || opt.Effort < lzMinEffort || os.Getenv("GOJXL_NOLZ") != "" {
		return res, err
	}
	// LZ77 copies of residual runs and repeats; kept only if smaller
	if lzv, lzCounts := modularLZ77(chs, all, caches, data.counts, nleaves); lzv != nil {
		r2, err2 := emit(lzv, lzCounts, &lz77Params{lzMinSym, lzMinLen, lzLenCfg})
		if err2 == nil && len(r2) < len(res) {
			return r2, nil
		}
	}
	return res, nil
}

// tokValue returns a representative value for a token (for statistics).
func tokValue(tok uint32) uint32 {
	c := defaultCfg
	if tok < 1<<c.se {
		return tok
	}
	ml := c.msb + c.lsb
	n := c.se - ml + ((tok - (1 << c.se)) >> ml)
	t := tok>>c.lsb&(1<<c.msb-1) | 1<<c.msb
	return t << n << c.lsb
}

// expand maps a b-bit value to the D-bit range the way decoders do.
func expandDepth(v int64, b, d int) int64 {
	mb, md := int64(1)<<b-1, int64(1)<<d-1
	return (v*md + mb/2) / mb
}

// reducedBitDepth returns the smallest bit depth b <= depth such that every
// sample is the expansion of a b-bit value.
func reducedBitDepth(chans [][]int32, depth int) int {
	if depth <= 1 {
		return depth
	}
	md := int64(1)<<depth - 1
	for b := 1; b < depth; b++ {
		mb := int64(1)<<b - 1
		// Values whose exact expansion lies close to a rounding boundary may
		// be expanded differently by float-based decoders: refuse those.
		ambiguous := make([]bool, mb+1)
		for v := int64(0); v <= mb; v++ {
			f := float64(v*md%mb) / float64(mb)
			ambiguous[v] = math.Abs(f-0.5) < 0.02
		}
		ok := true
		for _, c := range chans {
			for _, s := range c {
				v := (int64(s)*mb + md/2) / md
				if expandDepth(v, b, depth) != int64(s) || ambiguous[v] {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if ok {
			return b
		}
	}
	return depth
}

func shrinkBitDepth(chans [][]int32, depth, b int) {
	md, mb := int64(1)<<depth-1, int64(1)<<b-1
	for _, c := range chans {
		for i, s := range c {
			c[i] = int32((int64(s)*mb + md/2) / md)
		}
	}
}

// channelPalette maps a channel to indices into its sorted distinct values
// when it uses at most 3/4 of its value range.
func channelPalette(c []int32) ([]int32, []int32, bool) {
	if len(c) == 0 {
		return nil, nil, false
	}
	mn, mx := c[0], c[0]
	for _, v := range c {
		mn, mx = min(mn, v), max(mx, v)
	}
	rng := int(mx-mn) + 1
	if rng < 4 || rng > 1<<17 {
		return nil, nil, false
	}
	used := make([]int32, rng)
	for _, v := range c {
		used[v-mn] = 1
	}
	n := 0
	for i := range used {
		if used[i] != 0 {
			used[i] = int32(n)
			n++
		} else {
			used[i] = -1
		}
	}
	if n*4 > rng*3 || n > 70000 {
		return nil, nil, false
	}
	vals := make([]int32, 0, n)
	for i, u := range used {
		if u >= 0 {
			vals = append(vals, int32(i)+mn)
		}
	}
	idx := make([]int32, len(c))
	for i, v := range c {
		idx[i] = used[v-mn]
	}
	return vals, idx, true
}

// residCache holds the (context, residual) sequence of a stream.
type residCache struct {
	ctx []uint16
	v   []uint32
}

// paletteTransform returns a writer for a single-channel palette transform.
func paletteTransform(begin, nb int) func(w *bitio.Writer) {
	return func(w *bitio.Writer) {
		w.Write(2, 1) // palette
		w.WriteU32(uint32(begin), bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
		w.WriteU32(1, val(1), val(3), val(4), bitsOff(13, 1))
		w.WriteU32(uint32(nb), bitsOff(8, 0), bitsOff(10, 256), bitsOff(12, 1280), bitsOff(16, 5376))
		w.WriteU32(0, val(0), bitsOff(8, 1), bitsOff(10, 257), bitsOff(16, 1281))
		w.Write(4, 0)
	}
}

// localizeGroup copies the group's channels and applies group-local
// channel palettes where a channel uses few of its values in the group.
func localizeGroup(chs []channel, g *stream) {
	var data []channel
	for _, ci := range g.chans {
		c := chs[ci]
		sw, sh := min(g.w, c.w-g.x0), min(g.h, c.h-g.y0)
		if sw <= 0 || sh <= 0 {
			return // keep the shared layout
		}
		pix := make([]int32, sw*sh)
		for y := 0; y < sh; y++ {
			copy(pix[y*sw:(y+1)*sw], c.pix[(g.y0+y)*c.w+g.x0:])
		}
		data = append(data, channel{w: sw, h: sh, pix: pix})
	}
	var metas []channel
	var tws []func(w *bitio.Writer)
	for k := range data {
		vals, idx, ok := channelPalette(data[k].pix)
		if !ok {
			continue
		}
		w, h := data[k].w, data[k].h
		metaBits := float64(len(vals)) * (math.Log2(float64(vals[len(vals)-1]-vals[0]+1)/float64(len(vals))) + 2)
		if estChannelCost(idx, w, h)+metaBits >= estChannelCost(data[k].pix, w, h) {
			continue
		}
		tws = append(tws, paletteTransform(k+len(metas), len(vals)))
		metas = append(metas, channel{w: len(vals), h: 1, pix: vals})
		data[k].pix = idx
	}
	if len(tws) == 0 {
		return
	}
	var local []channel
	for i := len(metas) - 1; i >= 0; i-- {
		local = append(local, metas[i])
	}
	local = append(local, data...)
	g.local, g.tws = local, tws
	g.chans, g.ctxIdx = make([]int, len(local)), make([]int, len(local))
	for i := range local {
		g.chans[i], g.ctxIdx[i] = i, i
	}
	g.x0, g.y0, g.w, g.h = 0, 0, 1<<30, 1<<30
}

// estChannelCost estimates the coded size (bits) of a channel: gradient
// residuals in four local activity contexts.
func estChannelCost(pix []int32, w, h int) float64 {
	var hist [4][64]uint64
	var extra float64
	for y := 1; y < h; y++ {
		row, prev := pix[y*w:(y+1)*w], pix[(y-1)*w:y*w]
		for x := 1; x+1 < w; x++ {
			N, W, NW, NE := prev[x], row[x-1], prev[x-1], prev[x+1]
			act := iabs32(W-NW) + iabs32(N-NW) + iabs32(N-NE)
			q := 0
			switch {
			case act > 96:
				q = 3
			case act > 18:
				q = 2
			case act > 3:
				q = 1
			}
			t, nb, _ := defaultCfg.encode(bitio.PackSigned(row[x] - clampedGradient(N, W, NW)))
			hist[q][min(t, 63)]++
			extra += float64(nb)
		}
	}
	for q := range hist {
		extra += histCost(hist[q][:])
	}
	return extra
}
