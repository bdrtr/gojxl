package enc

import (
	"errors"
	"runtime"
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

// stream is a set of channel rectangles coded together.
type stream struct {
	x0, y0, w, h int
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
func walkStream(im *Image, chans [][]int32, s stream, root []*tnode, fn func(ctx int, v uint32)) {
	W := im.W
	for c, ch := range chans {
		tree := root[c]
		for y := 0; y < s.h; y++ {
			row := ch[(s.y0+y)*W+s.x0 : (s.y0+y)*W+s.x0+s.w]
			var prev []int32
			if y > 0 {
				prev = ch[(s.y0+y-1)*W+s.x0 : (s.y0+y-1)*W+s.x0+s.w]
			}
			for x := 0; x < s.w; x++ {
				var Wv, N, NW int32
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
				if x > 0 && y > 0 {
					NW = prev[x-1]
				} else {
					NW = Wv
				}
				p10 := Wv - NW
				p11 := NW - N
				n := tree
				for !n.leaf {
					var pv int32
					if n.prop == 10 {
						pv = p10
					} else {
						pv = p11
					}
					if pv > n.split {
						n = n.left
					} else {
						n = n.right
					}
				}
				pred := clampedGradient(N, Wv, NW)
				fn(n.ctx, bitio.PackSigned(row[x]-pred))
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
	chans := make([][]int32, nc)
	for i := range chans {
		chans[i] = append([]int32(nil), im.Chans[i]...)
	}
	useRCT := !im.Grey
	if useRCT {
		forwardYCoCg(chans[0], chans[1], chans[2])
	}
	root := buildTree(nc, im.W*im.H, opt.Effort)
	nodes, nleaves := bfs(root)
	roots := channelRoots(root, nc)

	// stream layout
	single := im.W <= groupDim && im.H <= groupDim
	xg := (im.W + groupDim - 1) / groupDim
	yg := (im.H + groupDim - 1) / groupDim
	var streams []stream
	if single {
		streams = []stream{{0, 0, im.W, im.H}}
	} else {
		for gy := 0; gy < yg; gy++ {
			for gx := 0; gx < xg; gx++ {
				x0, y0 := gx*groupDim, gy*groupDim
				streams = append(streams, stream{x0, y0, min(groupDim, im.W-x0), min(groupDim, im.H-y0)})
			}
		}
	}

	// pass 1: histograms
	hs := make([]*histograms, len(streams))
	parallel(len(streams), func(i int) {
		h := newHistograms(nleaves, defaultCfg)
		walkStream(im, chans, streams[i], roots, h.add)
		hs[i] = h
	})
	data := newHistograms(nleaves, defaultCfg)
	for _, h := range hs {
		data.merge(h)
	}
	treeH := newHistograms(6, defaultCfg)
	treeTokens(nodes, treeH.add)

	// LfGlobal section
	lfg := &bitio.Writer{}
	lfg.WriteBool(true) // LF channel dequant all default
	lfg.WriteBool(true) // global tree present
	tc := writeCode(lfg, treeH)
	treeTokens(nodes, func(ctx int, v uint32) { tc.write(lfg, ctx, v) })
	dc := writeCode(lfg, data)
	// global modular group header
	lfg.WriteBool(true) // use global tree
	lfg.WriteBool(true) // default WP header
	if useRCT {
		lfg.WriteU32(1, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
		lfg.Write(2, 0) // RCT
		lfg.WriteU32(0, bitio.Bits(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
		lfg.WriteU32(6, val(6), bitio.Bits(2), bitsOff(4, 2), bitsOff(6, 10))
	} else {
		lfg.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	}

	out := &bitio.Writer{}
	writeImageHeader(out, im)
	writeFrameHeader(out, im)

	if single {
		walkStream(im, chans, streams[0], roots, func(ctx int, v uint32) { dc.write(lfg, ctx, v) })
		sec := lfg.Bytes()
		out.WriteBool(false) // no permutation
		out.ZeroPadToByte()
		out.WriteU32(uint32(len(sec)), bitio.Bits(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
		out.ZeroPadToByte()
		out.AppendBytes(sec)
		return out.Bytes(), nil
	}

	// pass 2: group sections
	secs := make([][]byte, len(streams))
	parallel(len(streams), func(i int) {
		w := &bitio.Writer{}
		w.WriteBool(true) // use global tree
		w.WriteBool(true) // default WP
		w.WriteU32(0, val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
		walkStream(im, chans, streams[i], roots, func(ctx int, v uint32) { dc.write(w, ctx, v) })
		secs[i] = w.Bytes()
	})
	nlf := ((im.W + groupDim*8 - 1) / (groupDim * 8)) * ((im.H + groupDim*8 - 1) / (groupDim * 8))
	sizes := []int{}
	lfSec := lfg.Bytes()
	sizes = append(sizes, len(lfSec))
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
