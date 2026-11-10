package enc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
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
	// Effort selects the context model size (1 = fastest).
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

// walkStream visits every sample of a stream calling fn(ctx, residual).
func walkStream(chs []channel, s stream, root []*tnode, fn func(ctx int, v uint32)) {
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

func streamSamples(chs []channel, s stream) int {
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
		if !im.Grey {
			forwardYCoCg(src[0], src[1], src[2])
			transforms = func(w *bitio.Writer) {
				w.WriteU32(1, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
				w.Write(2, 0) // RCT
				w.WriteU32(0, bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
				w.WriteU32(6, val(6), bitio.Bits(2), bitsOff(4, 2), bitsOff(6, 10))
			}
		} else {
			transforms = func(w *bitio.Writer) { w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18)) }
		}
		for _, c := range src {
			chs = append(chs, channel{w: im.W, h: im.H, pix: c})
		}
	}
	ntree := len(chs)
	root := buildTree(ntree, im.W*im.H, opt.Effort)
	nodes, nleaves := bfs(root)
	roots := channelRoots(root, ntree)

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
	all := append([]stream{global}, groups...)

	// pass 1: histograms
	hs := make([]*histograms, len(all))
	parallel(len(all), func(i int) {
		h := newHistograms(nleaves, defaultCfg)
		walkStream(chs, all[i], roots, h.add)
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
	treeH := newHistograms(6, defaultCfg)
	treeTokens(nodes, treeH.add)

	// LfGlobal section
	lfg := &bitio.Writer{}
	lfg.WriteBool(true) // LF channel dequant all default
	lfg.WriteBool(true) // global tree present
	tc := writeCode(lfg, treeH)
	treeTokens(nodes, func(ctx int, v uint32) { tc.write(lfg, ctx, v) })
	dc := writeANSCode(lfg, data)
	lfg.WriteBool(true) // use global tree
	lfg.WriteBool(true) // default WP header
	transforms(lfg)
	gs := &ansStream{c: dc, toks: make([]ansToken, 0, streamSamples(chs, global))}
	walkStream(chs, global, roots, gs.add)
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
			w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
			st := &ansStream{c: dc, toks: make([]ansToken, 0, streamSamples(chs, groups[i]))}
			walkStream(chs, groups[i], roots, st.add)
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
