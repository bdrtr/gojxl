package jxl

import (
	"gojxl/internal/bitio"
	"gojxl/internal/entropy"
	"math/bits"
	"sync"
)

// Channel is a single modular channel of 32-bit integers.
type Channel struct {
	W, H           int
	HShift, VShift int
	Pix            []int32 // row-major, stride W
}

func newChannel(w, h, hs, vs int) *Channel {
	return &Channel{W: w, H: h, HShift: hs, VShift: vs, Pix: make([]int32, w*h)}
}

// Row returns row y.
func (c *Channel) Row(y int) []int32 { return c.Pix[y*c.W : (y+1)*c.W] }

// WPHeader holds weighted predictor parameters.
type WPHeader struct {
	P1C, P2C, P3Ca, P3Cb, P3Cc, P3Cd, P3Ce int64
	W                                      [4]uint32
}

func defaultWPHeader() WPHeader {
	return WPHeader{P1C: 16, P2C: 10, P3Ca: 7, P3Cb: 7, P3Cc: 7, P3Cd: 0, P3Ce: 0, W: [4]uint32{0xd, 0xc, 0xc, 0xc}}
}

func readWPHeader(br *bitio.Reader) WPHeader {
	if br.Bool() {
		return defaultWPHeader()
	}
	var w WPHeader
	w.P1C = int64(br.Read(5))
	w.P2C = int64(br.Read(5))
	w.P3Ca = int64(br.Read(5))
	w.P3Cb = int64(br.Read(5))
	w.P3Cc = int64(br.Read(5))
	w.P3Cd = int64(br.Read(5))
	w.P3Ce = int64(br.Read(5))
	for i := range w.W {
		w.W[i] = br.Read(4)
	}
	return w
}

// Transform ids.
const (
	TrRCT     = 0
	TrPalette = 1
	TrSqueeze = 2
)

// SqueezeParams is one squeeze step.
type SqueezeParams struct {
	Horizontal bool
	InPlace    bool
	BeginC     int
	NumC       int
}

// Transform is a modular transform.
type Transform struct {
	ID        int
	BeginC    int
	RCTType   int
	NumC      int
	NbColours int
	NbDeltas  int
	DPred     int
	Squeezes  []SqueezeParams
}

func readTransform(br *bitio.Reader) (Transform, error) {
	var t Transform
	t.ID = int(br.U32(bitio.Val(0), bitio.Val(1), bitio.Val(2), bitio.Val(3)))
	if t.ID == 3 {
		return t, invalid("transform id")
	}
	if t.ID == TrRCT || t.ID == TrPalette {
		t.BeginC = int(br.U32(bitio.Bits(3), bitio.BitsOff(6, 8), bitio.BitsOff(10, 72), bitio.BitsOff(13, 1096)))
	}
	switch t.ID {
	case TrRCT:
		t.RCTType = int(br.U32(bitio.Val(6), bitio.Bits(2), bitio.BitsOff(4, 2), bitio.BitsOff(6, 10)))
		if t.RCTType >= 42 {
			return t, invalid("rct type")
		}
	case TrPalette:
		t.NumC = int(br.U32(bitio.Val(1), bitio.Val(3), bitio.Val(4), bitio.BitsOff(13, 1)))
		t.NbColours = int(br.U32(bitio.BitsOff(8, 0), bitio.BitsOff(10, 256), bitio.BitsOff(12, 1280), bitio.BitsOff(16, 5376)))
		t.NbDeltas = int(br.U32(bitio.Val(0), bitio.BitsOff(8, 1), bitio.BitsOff(10, 257), bitio.BitsOff(16, 1281)))
		t.DPred = int(br.Read(4))
		if t.DPred >= 14 {
			return t, invalid("palette predictor")
		}
	case TrSqueeze:
		n := br.U32(bitio.Val(0), bitio.BitsOff(4, 1), bitio.BitsOff(6, 9), bitio.BitsOff(8, 41))
		t.Squeezes = make([]SqueezeParams, n)
		for i := range t.Squeezes {
			s := &t.Squeezes[i]
			s.Horizontal = br.Bool()
			s.InPlace = br.Bool()
			s.BeginC = int(br.U32(bitio.Bits(3), bitio.BitsOff(6, 8), bitio.BitsOff(10, 72), bitio.BitsOff(13, 1096)))
			s.NumC = int(br.U32(bitio.Val(1), bitio.Val(2), bitio.Val(3), bitio.BitsOff(4, 4)))
		}
	}
	return t, nil
}

// ModularHeader is the GroupHeader of a modular stream.
type ModularHeader struct {
	UseGlobalTree bool
	WP            WPHeader
	Transforms    []Transform
}

func readModularHeader(br *bitio.Reader) (ModularHeader, error) {
	var h ModularHeader
	h.UseGlobalTree = br.Bool()
	h.WP = readWPHeader(br)
	n := br.U32(bitio.Val(0), bitio.Val(1), bitio.BitsOff(4, 2), bitio.BitsOff(8, 18))
	if n > 512 {
		return h, invalid("too many transforms")
	}
	h.Transforms = make([]Transform, n)
	for i := range h.Transforms {
		var err error
		if h.Transforms[i], err = readTransform(br); err != nil {
			return h, err
		}
	}
	return h, nil
}

// ModularImage is a set of channels plus transform metadata.
type ModularImage struct {
	Channels   []*Channel
	NbMeta     int
	BitDepth   int
	Transforms []Transform
	WP         WPHeader
	pooled     bool // channel buffers from the group pool (see release)
}

func defaultSqueezeParams(img *ModularImage) []SqueezeParams {
	var params []SqueezeParams
	nbc := len(img.Channels) - img.NbMeta
	first := img.Channels[img.NbMeta]
	w, h := first.W, first.H
	if nbc > 2 && img.Channels[img.NbMeta+1].W == w && img.Channels[img.NbMeta+1].H == h {
		params = append(params,
			SqueezeParams{Horizontal: true, InPlace: false, BeginC: img.NbMeta + 1, NumC: 2},
			SqueezeParams{Horizontal: false, InPlace: false, BeginC: img.NbMeta + 1, NumC: 2})
	}
	p := SqueezeParams{BeginC: img.NbMeta, NumC: nbc, InPlace: true}
	if w <= h && h > 8 {
		p.Horizontal = false
		params = append(params, p)
		h = (h + 1) / 2
	}
	for w > 8 || h > 8 {
		if w > 8 {
			p.Horizontal = true
			params = append(params, p)
			w = (w + 1) / 2
		}
		if h > 8 {
			p.Horizontal = false
			params = append(params, p)
			h = (h + 1) / 2
		}
	}
	return params
}

// metaApply updates the channel list for a transform (forward direction of
// the channel bookkeeping).
func (img *ModularImage) metaApply(t *Transform) error {
	switch t.ID {
	case TrRCT:
		b := t.BeginC
		if b+2 >= len(img.Channels) {
			return invalid("rct channels")
		}
		c0 := img.Channels[b]
		for i := 1; i < 3; i++ {
			c := img.Channels[b+i]
			if c.W != c0.W || c.H != c0.H || c.HShift != c0.HShift || c.VShift != c0.VShift {
				return invalid("rct channel mismatch")
			}
		}
	case TrPalette:
		b := t.BeginC
		end := b + t.NumC - 1
		if t.NumC < 1 || end >= len(img.Channels) {
			return invalid("palette channels")
		}
		c0 := img.Channels[b]
		for i := b + 1; i <= end; i++ {
			c := img.Channels[i]
			if c.W != c0.W || c.H != c0.H || c.HShift != c0.HShift || c.VShift != c0.VShift {
				return invalid("palette channel mismatch")
			}
		}
		if b >= img.NbMeta {
			img.NbMeta++
		} else {
			if end >= img.NbMeta {
				return invalid("palette meta")
			}
			img.NbMeta += 2 - t.NumC
		}
		chs := make([]*Channel, 0, len(img.Channels)-t.NumC+2)
		chs = append(chs, &Channel{W: t.NbColours + t.NbDeltas, H: t.NumC, HShift: -1, VShift: -1})
		chs = append(chs, img.Channels[:b+1]...)
		chs = append(chs, img.Channels[end+1:]...)
		img.Channels = chs
	case TrSqueeze:
		if len(t.Squeezes) == 0 {
			if len(img.Channels) <= img.NbMeta {
				return invalid("squeeze without channels")
			}
			t.Squeezes = defaultSqueezeParams(img)
		}
		for _, s := range t.Squeezes {
			begin := s.BeginC
			end := begin + s.NumC - 1
			if s.NumC < 1 || end >= len(img.Channels) {
				return invalid("squeeze channels")
			}
			if begin < img.NbMeta {
				if end >= img.NbMeta || !s.InPlace {
					return invalid("squeeze meta")
				}
				img.NbMeta += s.NumC
			}
			offset := len(img.Channels)
			if s.InPlace {
				offset = end + 1
			}
			var res []*Channel
			for c := begin; c <= end; c++ {
				ch := img.Channels[c]
				if ch.HShift > 30 || ch.VShift > 30 {
					return invalid("squeeze shift")
				}
				w, h := ch.W, ch.H
				if w == 0 || h == 0 {
					return invalid("squeeze empty channel")
				}
				nc := &Channel{W: w, H: h, HShift: ch.HShift, VShift: ch.VShift}
				var r *Channel
				if s.Horizontal {
					nc.W = (w + 1) / 2
					if nc.HShift >= 0 {
						nc.HShift++
					}
					r = &Channel{W: w - nc.W, H: h, HShift: nc.HShift, VShift: nc.VShift}
				} else {
					nc.H = (h + 1) / 2
					if nc.VShift >= 0 {
						nc.VShift++
					}
					r = &Channel{W: w, H: h - nc.H, HShift: nc.HShift, VShift: nc.VShift}
				}
				img.Channels[c] = nc
				res = append(res, r)
			}
			chs := make([]*Channel, 0, len(img.Channels)+len(res))
			chs = append(chs, img.Channels[:offset]...)
			chs = append(chs, res...)
			chs = append(chs, img.Channels[offset:]...)
			img.Channels = chs
		}
	}
	return nil
}

// allocate makes sure all channels have pixel storage (from the pool of
// group-sized buffers for images marked pooled).
func (img *ModularImage) allocate() {
	for _, c := range img.Channels {
		if len(c.Pix) != c.W*c.H {
			if img.pooled {
				c.Pix = getPix(c.W * c.H)
			} else {
				c.Pix = make([]int32, c.W*c.H)
			}
		}
	}
}

// Group images are decoded, copied into the frame's image and dropped:
// their channel buffers are reused, by power-of-two size class.
var pixPools [32]sync.Pool

func getPix(n int) []int32 {
	if n == 0 {
		return nil
	}
	k := bits.Len(uint(n - 1))
	if v, ok := pixPools[k].Get().(*[]int32); ok {
		b := (*v)[:n]
		clear(b)
		return b
	}
	return make([]int32, n, 1<<k)
}

// release returns the channel buffers of a pooled image.
func (img *ModularImage) release() {
	if !img.pooled {
		return
	}
	for _, c := range img.Channels {
		if k := cap(c.Pix); k > 0 && k&(k-1) == 0 {
			b := c.Pix[:0]
			pixPools[bits.Len(uint(k))-1].Put(&b)
		}
		c.Pix = nil
	}
}

// MA tree node.
type maNode struct {
	property  int32 // -1 for leaf
	splitVal  int32
	lchild    int32 // for leaves: context id
	rchild    int32
	predictor uint8
	offset    int32
	mult      uint32
}

// MATree is a decoded meta-adaptive tree.
type MATree struct {
	nodes     []maNode
	maxProp   int
	usesWP    bool
	numLeaves int
}

const (
	ctxSplitVal = 0
	ctxProperty = 1
	ctxPredict  = 2
	ctxOffset   = 3
	ctxMulLog   = 4
	ctxMulBits  = 5
)

func readMATree(br *bitio.Reader, limit int) (*MATree, error) {
	code, err := entropy.ReadCode(br, 6)
	if err != nil {
		return nil, err
	}
	d := code.NewDecoder(br)
	t := &MATree{}
	toDecode := 1
	leaf := 0
	for toDecode > 0 {
		if len(t.nodes) > limit {
			return nil, invalid("MA tree too large")
		}
		toDecode--
		p := d.ReadUint(br, ctxProperty)
		if p > 256 {
			return nil, invalid("MA tree property")
		}
		prop := int32(p) - 1
		if prop == -1 {
			pred := d.ReadUint(br, ctxPredict)
			if pred >= 14 {
				return nil, invalid("MA tree predictor")
			}
			off := bitio.UnpackSigned(d.ReadUint(br, ctxOffset))
			mulLog := d.ReadUint(br, ctxMulLog)
			if mulLog >= 31 {
				return nil, invalid("MA tree mul log")
			}
			mulBits := d.ReadUint(br, ctxMulBits)
			if uint64(mulBits) >= (uint64(1)<<(31-mulLog))-1 {
				return nil, invalid("MA tree mul bits")
			}
			if pred == 6 {
				t.usesWP = true
			}
			t.nodes = append(t.nodes, maNode{property: -1, lchild: int32(leaf), predictor: uint8(pred), offset: off, mult: (mulBits + 1) << mulLog})
			leaf++
			continue
		}
		if int(prop) > t.maxProp {
			t.maxProp = int(prop)
		}
		if prop == 15 {
			t.usesWP = true
		}
		sv := bitio.UnpackSigned(d.ReadUint(br, ctxSplitVal))
		n := int32(len(t.nodes))
		t.nodes = append(t.nodes, maNode{property: prop, splitVal: sv, lchild: n + int32(toDecode) + 1, rchild: n + int32(toDecode) + 2})
		toDecode += 2
	}
	if err := d.Finish(); err != nil {
		return nil, err
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	t.numLeaves = leaf
	return t, nil
}

// ModularGlobal holds a global tree and its entropy code.
type ModularGlobal struct {
	Tree *MATree
	Code *entropy.Code
}

// decodeModular decodes a modular stream into img. img must have its
// channel list set up (sizes). If maxChanSize > 0, decoding stops at the
// first non-meta channel bigger than that (global image case).
func decodeModular(br *bitio.Reader, img *ModularImage, streamID int, global *ModularGlobal, maxChanSize int, skipHeader bool) error {
	if len(img.Channels) == 0 {
		return nil
	}
	hdr, err := readModularHeader(br)
	if err != nil {
		return err
	}
	if br.Overflowed() {
		return bitio.ErrEOF
	}
	img.WP = hdr.WP
	img.Transforms = hdr.Transforms
	dbg("modular stream %d: globaltree=%v transforms=%+v nch=%d", streamID, hdr.UseGlobalTree, hdr.Transforms, len(img.Channels))
	for i := range img.Transforms {
		if err := img.metaApply(&img.Transforms[i]); err != nil {
			return err
		}
	}
	// validate sizes
	for _, c := range img.Channels {
		if c.W > 1<<24 || c.H > 1<<24 || c.W*c.H > 1<<30 {
			return invalid("modular channel too large")
		}
	}
	img.allocate()

	limitIdx := len(img.Channels)
	numChans := 0
	distMult := 0
	treeLimit := 1024
	for i, c := range img.Channels {
		if maxChanSize > 0 && i >= img.NbMeta && (c.W > maxChanSize || c.H > maxChanSize) {
			limitIdx = i
			break
		}
		treeLimit += c.W * c.H
		if c.W == 0 || c.H == 0 {
			continue
		}
		distMult = max(distMult, c.W)
		numChans++
	}
	if numChans == 0 {
		return nil
	}
	if treeLimit > 1<<20 {
		treeLimit = 1 << 20
	}
	var tree *MATree
	var code *entropy.Code
	if !hdr.UseGlobalTree {
		if tree, err = readMATree(br, treeLimit); err != nil {
			return err
		}
		if code, err = entropy.ReadCode(br, tree.numLeaves); err != nil {
			return err
		}
	} else {
		if global == nil || global.Tree == nil {
			return invalid("global tree requested but absent")
		}
		tree, code = global.Tree, global.Code
	}
	dbg("  tree nodes=%d leaves=%d lz77=%v", len(tree.nodes), tree.numLeaves, code.UsesLZ77())
	dec := code.NewDecoder(br)
	dec.SetDistMultiplier(uint32(distMult))
	for i := 0; i < limitIdx; i++ {
		c := img.Channels[i]
		if c.W == 0 || c.H == 0 {
			continue
		}
		if err := decodeChannel(br, dec, tree, &hdr.WP, img, i, streamID); err != nil {
			return err
		}
		if br.Overflowed() {
			return bitio.ErrEOF
		}
	}
	return dec.Finish()
}
