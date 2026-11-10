package jxl

import (
	"sync"

	"gojxl/internal/bitio"
	"gojxl/internal/entropy"
)

// AC strategy types.
const (
	acsDCT      = 0
	acsIdentity = 1
	acsDCT2     = 2
	acsDCT4     = 3
	acsDCT16    = 4
	acsDCT32    = 5
	acsDCT16x8  = 6
	acsDCT8x16  = 7
	acsDCT32x8  = 8
	acsDCT8x32  = 9
	acsDCT32x16 = 10
	acsDCT16x32 = 11
	acsDCT4x8   = 12
	acsDCT8x4   = 13
	acsAFV0     = 14
	acsAFV3     = 17
	acsDCT64    = 18
	numACS      = 27
	acsInvalid  = 0xff
)

var acsCoveredX = [numACS]int{1, 1, 1, 1, 2, 4, 1, 2, 1, 4, 2, 4, 1, 1, 1, 1, 1, 1, 8, 4, 8, 16, 8, 16, 32, 16, 32}
var acsCoveredY = [numACS]int{1, 1, 1, 1, 2, 4, 2, 1, 4, 1, 4, 2, 1, 1, 1, 1, 1, 1, 8, 8, 4, 16, 16, 8, 32, 32, 16}
var acsLog2Covered = [numACS]int{0, 0, 0, 0, 2, 4, 1, 1, 2, 2, 3, 3, 0, 0, 0, 0, 0, 0, 6, 5, 5, 8, 7, 7, 10, 9, 9}
var acsStrategyOrder = [numACS]int{0, 1, 1, 1, 2, 3, 4, 4, 5, 5, 6, 6, 1, 1, 1, 1, 1, 1, 7, 8, 8, 9, 10, 10, 11, 12, 12}
var acsQuantTable = [numACS]int{0, 1, 2, 3, 4, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 10, 10, 11, 12, 12, 13, 14, 14, 15, 16, 16}

// order id -> (cx, cy) with cx >= cy (in blocks)
var orderDims = [13][2]int{{1, 1}, {1, 1}, {2, 2}, {4, 4}, {2, 1}, {4, 1}, {4, 2}, {8, 8}, {8, 4}, {16, 16}, {16, 8}, {32, 32}, {32, 16}}

var naturalOrders [13][]int32
var naturalOnce [13]sync.Once

// naturalOrder returns coefficient positions (in the wide layout) in
// natural (zig-zag like) order for an order id.
func naturalOrder(id int) []int32 {
	naturalOnce[id].Do(func() {
		cx, cy := orderDims[id][0], orderDims[id][1]
		xs := cx / cy
		xsm := xs - 1
		xss := 0
		for (1 << xss) < xs {
			xss++
		}
		out := make([]int32, 64*cx*cy)
		cur := cx * cy
		n := cx * 8
		for i := 0; i < n; i++ {
			for j := 0; j <= i; j++ {
				x, y := j, i-j
				if i%2 != 0 {
					x, y = y, x
				}
				if y&xsm != 0 {
					continue
				}
				y >>= xss
				var val int
				if x < cx && y < cy {
					val = y*cx + x
				} else {
					val = cur
					cur++
				}
				out[val] = int32(y*n + x)
			}
		}
		for ip := n - 1; ip > 0; ip-- {
			i := ip - 1
			for j := 0; j <= i; j++ {
				x := n - 1 - (i - j)
				y := n - 1 - j
				if i%2 != 0 {
					x, y = y, x
				}
				if y&xsm != 0 {
					continue
				}
				y >>= xss
				out[cur] = int32(y*n + x)
				cur++
			}
		}
		naturalOrders[id] = out
	})
	return naturalOrders[id]
}

type blockCtxMap struct {
	dcThresholds [3][]int32
	qfThresholds []uint32
	ctxMap       []uint8
	numCtx       int
	numDC        int
}

var defaultBlockCtxMap = []uint8{
	0, 1, 2, 2, 3, 3, 4, 5, 6, 6, 6, 6, 6, 7, 8, 9, 9, 10, 11, 12, 13, 14, 14, 14,
	14, 14, 7, 8, 9, 9, 10, 11, 12, 13, 14, 14, 14, 14, 14,
}

type hfPass struct {
	orders [13][3][]int32
	code   *entropy.Code
}

func (p *hfPass) order(id, c int) []int32 {
	if o := p.orders[id][c]; o != nil {
		return o
	}
	return naturalOrder(id)
}

type varblock struct {
	bx, by int32 // frame block coordinates of the top-left block
	typ    uint8
	qf     int32
	off    [3]int32 // coefficient offset per channel, -1 if absent
}

type groupCoeffs struct {
	vbs  []varblock
	coef [3][]int32
}

type vardctState struct {
	fd *frameDecoder
	fh *FrameHeader

	globalScale uint32
	quantLF     uint32
	bctx        blockCtxMap
	colorFactor uint32
	baseX       float32
	baseB       float32
	xFactorLF   int32
	bFactorLF   int32

	bw, bh int    // frame size in blocks
	hs, vs [3]int // channel shifts (X, Y, B)
	sub    bool   // chroma subsampled

	dc  [3]*Plane
	qdc []uint8

	acs   []uint8
	qf    []int32
	sharp []uint8
	ytox  []int8
	ytob  []int8
	tw    int

	qenc    [numQuantTables]quantEncoding
	qtables [numQuantTables][3][]float32
	qtOnce  [numQuantTables]sync.Once
	qtErr   [numQuantTables]error
	presets int
	passes  []hfPass
	usedACS uint32
	acsMu   sync.Mutex
	groups  []*groupCoeffs
	out     [3]*Plane
}

func newVarDCTState(fd *frameDecoder) *vardctState {
	fh := fd.fh
	v := &vardctState{fd: fd, fh: fh}
	for c := 0; c < 3; c++ {
		v.hs[c], v.vs[c] = fh.HShift[c], fh.VShift[c]
	}
	v.sub = fh.MaxHShift > 0 || fh.MaxVShift > 0
	v.bw = divCeil(fh.XSize, 8<<fh.MaxHShift) << fh.MaxHShift
	v.bh = divCeil(fh.YSize, 8<<fh.MaxVShift) << fh.MaxVShift
	n := v.bw * v.bh
	v.qdc = make([]uint8, n)
	v.acs = make([]uint8, n)
	for i := range v.acs {
		v.acs[i] = acsInvalid
	}
	v.qf = make([]int32, n)
	v.sharp = make([]uint8, n)
	v.tw = divCeil(v.bw, 8)
	th := divCeil(v.bh, 8)
	v.ytox = make([]int8, v.tw*th)
	v.ytob = make([]int8, v.tw*th)
	for c := 0; c < 3; c++ {
		v.dc[c] = newPlane(v.bw>>v.hs[c], v.bh>>v.vs[c])
	}
	return v
}

func (v *vardctState) readGlobal(br *bitio.Reader) error {
	v.globalScale = br.U32(bitio.BitsOff(11, 1), bitio.BitsOff(11, 2049), bitio.BitsOff(12, 4097), bitio.BitsOff(16, 8193))
	v.quantLF = br.U32(bitio.Val(16), bitio.BitsOff(5, 1), bitio.BitsOff(8, 1), bitio.BitsOff(16, 1))
	// block context map
	b := &v.bctx
	if br.Bool() {
		b.ctxMap = defaultBlockCtxMap
		b.numCtx = 15
		b.numDC = 1
	} else {
		bsize := 1
		for c := 0; c < 3; c++ {
			n := int(br.Read(4))
			bsize *= n + 1
			for i := 0; i < n; i++ {
				t := br.U32(bitio.Bits(4), bitio.BitsOff(8, 16), bitio.BitsOff(16, 272), bitio.BitsOff(32, 65808))
				b.dcThresholds[c] = append(b.dcThresholds[c], bitio.UnpackSigned(t))
			}
		}
		b.numDC = bsize
		n := int(br.Read(4))
		bsize *= n + 1
		for i := 0; i < n; i++ {
			t := br.U32(bitio.Bits(2), bitio.BitsOff(3, 4), bitio.BitsOff(5, 12), bitio.BitsOff(8, 44))
			b.qfThresholds = append(b.qfThresholds, t+1)
		}
		if bsize > 64 {
			return invalid("block ctx map too large")
		}
		cm, nc, err := entropy.ReadContextMap(br, bsize*39)
		if err != nil {
			return err
		}
		if nc > 16 {
			return invalid("too many block clusters")
		}
		b.ctxMap, b.numCtx = cm, nc
	}
	// LF channel correlation
	v.colorFactor = 84
	v.baseX, v.baseB = 0, 1
	v.xFactorLF, v.bFactorLF = 128, 128
	if !br.Bool() {
		v.colorFactor = br.U32(bitio.Val(84), bitio.Val(256), bitio.BitsOff(8, 2), bitio.BitsOff(16, 258))
		var err error
		if v.baseX, err = br.F16(); err != nil {
			return err
		}
		if v.baseB, err = br.F16(); err != nil {
			return err
		}
		v.xFactorLF = int32(br.Read(8))
		v.bFactorLF = int32(br.Read(8))
	}
	if br.Overflowed() {
		return bitio.ErrEOF
	}
	return nil
}

// mulDC returns the LF dequantization factor for channel c.
func (v *vardctState) mulDC(c int) float32 {
	return float32(65536.0/float64(v.globalScale)/float64(v.quantLF)) * v.fd.lfDequant[c]
}

func (v *vardctState) lfGroupRect(g int) (x0, y0, w, h int) {
	fh := v.fh
	gx, gy := g%fh.XLFGroups, g/fh.XLFGroups
	d := fh.GroupDim // LF group dim in blocks
	x0, y0 = gx*d, gy*d
	w = min(d, v.bw-x0)
	h = min(d, v.bh-y0)
	return
}

func (v *vardctState) decodeLfGroup(br *bitio.Reader, g int) error {
	if v.fh.Flags&FlagUseLFFrame != 0 {
		return nil
	}
	x0, y0, w, h := v.lfGroupRect(g)
	extra := br.Read(2)
	img := &ModularImage{BitDepth: int(v.fd.ih.BitDepth.BitsPerSample)}
	for _, c := range [3]int{1, 0, 2} {
		img.Channels = append(img.Channels, &Channel{W: w >> v.hs[c], H: h >> v.vs[c]})
	}
	if err := decodeModular(br, img, 1+g, &v.fd.global, 0, false); err != nil {
		return err
	}
	if err := img.undoTransforms(); err != nil {
		return err
	}
	mul := float32(1) / float32(uint32(1)<<extra)
	qx, qy, qb := img.Channels[1], img.Channels[0], img.Channels[2]
	if !v.sub {
		fx, fy, fb := v.mulDC(0)*mul, v.mulDC(1)*mul, v.mulDC(2)*mul
		cflX := v.baseX + float32(v.xFactorLF-128)/float32(v.colorFactor)
		cflB := v.baseB + float32(v.bFactorLF-128)/float32(v.colorFactor)
		for y := 0; y < h; y++ {
			dx := v.dc[0].Row(y0 + y)[x0 : x0+w]
			dy := v.dc[1].Row(y0 + y)[x0 : x0+w]
			db := v.dc[2].Row(y0 + y)[x0 : x0+w]
			rx, ry, rb := qx.Row(y), qy.Row(y), qb.Row(y)
			for x := 0; x < w; x++ {
				Y := float32(ry[x]) * fy
				dy[x] = Y
				dx[x] = float32(rx[x])*fx + cflX*Y
				db[x] = float32(rb[x])*fb + cflB*Y
			}
		}
	} else {
		chans := [3]*Channel{qx, qy, qb}
		for c := 0; c < 3; c++ {
			f := v.mulDC(c) * mul
			ch := chans[c]
			cx0, cy0 := x0>>v.hs[c], y0>>v.vs[c]
			for y := 0; y < ch.H; y++ {
				d := v.dc[c].Row(cy0 + y)[cx0 : cx0+ch.W]
				r := ch.Row(y)
				for x := range d {
					d[x] = float32(r[x]) * f
				}
			}
		}
	}
	// LF context buckets
	b := &v.bctx
	if b.numDC > 1 {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var bk [3]int
				vals := [3]int32{
					qx.Pix[(y>>v.vs[0])*qx.W+(x>>v.hs[0])],
					qy.Pix[(y>>v.vs[1])*qy.W+(x>>v.hs[1])],
					qb.Pix[(y>>v.vs[2])*qb.W+(x>>v.hs[2])],
				}
				for c := 0; c < 3; c++ {
					for _, t := range b.dcThresholds[c] {
						if vals[c] > t {
							bk[c]++
						}
					}
				}
				bucket := bk[0]
				bucket *= len(b.dcThresholds[2]) + 1
				bucket += bk[2]
				bucket *= len(b.dcThresholds[1]) + 1
				bucket += bk[1]
				v.qdc[(y0+y)*v.bw+x0+x] = uint8(bucket)
			}
		}
	}
	return nil
}

func (v *vardctState) decodeHFMeta(br *bitio.Reader, g int) error {
	x0, y0, w, h := v.lfGroupRect(g)
	upper := w * h
	count := int(br.Read(uint(ceilLog2(upper)))) + 1
	cw, ch := divCeil(w, 8), divCeil(h, 8)
	img := &ModularImage{BitDepth: int(v.fd.ih.BitDepth.BitsPerSample)}
	img.Channels = []*Channel{
		{W: cw, H: ch, HShift: 3, VShift: 3},
		{W: cw, H: ch, HShift: 3, VShift: 3},
		{W: count, H: 2},
		{W: w, H: h},
	}
	stream := 1 + 2*v.fh.NumLFGroups + g
	if err := decodeModular(br, img, stream, &v.fd.global, 0, false); err != nil {
		return err
	}
	if err := img.undoTransforms(); err != nil {
		return err
	}
	clamp8 := func(x int32) int8 {
		if x < -128 {
			return -128
		}
		if x > 127 {
			return 127
		}
		return int8(x)
	}
	tx0, ty0 := x0>>3, y0>>3
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			v.ytox[(ty0+y)*v.tw+tx0+x] = clamp8(img.Channels[0].Pix[y*cw+x])
			v.ytob[(ty0+y)*v.tw+tx0+x] = clamp8(img.Channels[1].Pix[y*cw+x])
		}
	}
	acsRow := img.Channels[2].Pix[:count]
	qfRow := img.Channels[2].Pix[count : 2*count]
	sharp := img.Channels[3]
	num := 0
	xlim, ylim := min(v.bw, x0+w), min(v.bh, y0+h)
	gd := v.fh.GroupDim / 8
	var used uint32
	for iy := 0; iy < h; iy++ {
		by := y0 + iy
		for ix := 0; ix < w; ix++ {
			bx := x0 + ix
			s := sharp.Pix[iy*w+ix]
			if s < 0 || s >= 8 {
				return invalid("epf sharpness")
			}
			v.sharp[by*v.bw+bx] = uint8(s)
			if v.acs[by*v.bw+bx] != acsInvalid {
				continue
			}
			if num >= count {
				return invalid("ac strategy count")
			}
			t := acsRow[num]
			if t < 0 || t >= numACS {
				return invalid("ac strategy")
			}
			used |= 1 << uint(t)
			cx, cy := acsCoveredX[t], acsCoveredY[t]
			if (cx > 1 || cy > 1) && v.sub {
				return invalid("ac strategy with subsampling")
			}
			nxg := (bx/gd + 1) * gd
			nyg := (by/gd + 1) * gd
			if bx+cx > nxg || bx+cx > xlim || by+cy > nyg || by+cy > ylim {
				return invalid("ac strategy overflow")
			}
			for yy := 0; yy < cy; yy++ {
				for xx := 0; xx < cx; xx++ {
					p := (by+yy)*v.bw + bx + xx
					if v.acs[p] != acsInvalid {
						return invalid("ac strategy overlap")
					}
					first := uint8(0)
					if xx == 0 && yy == 0 {
						first = 1
					}
					v.acs[p] = uint8(t)<<1 | first
				}
			}
			q := qfRow[num]
			if q < 0 {
				q = 0
			}
			if q > 255 {
				q = 255
			}
			v.qf[by*v.bw+bx] = 1 + q
			num++
		}
	}
	v.acsMu.Lock()
	v.usedACS |= used
	v.acsMu.Unlock()
	return nil
}

// adaptiveSmoothing applies the adaptive LF smoothing filter.
func (v *vardctState) adaptiveSmoothing() {
	const w1 = 0.20345139757231578
	const w2 = 0.0334829185968739
	const w0 = 1.0 - 4.0*(w1+w2)
	var f [3]float32
	for c := 0; c < 3; c++ {
		f[c] = v.mulDC(c)
	}
	smooth := func(planes []*Plane, facs []float32) {
		xs, ys := planes[0].W, planes[0].H
		if xs <= 2 || ys <= 2 {
			return
		}
		outs := make([]*Plane, len(planes))
		for i, p := range planes {
			outs[i] = &Plane{W: p.W, H: p.H, Pix: append([]float32(nil), p.Pix...)}
		}
		_ = parallelFor(ys-2, func(yi int) error {
			y := yi + 1
			sm := make([]float32, len(planes))
			mc := make([]float32, len(planes))
			for x := 1; x < xs-1; x++ {
				gap := float32(0.5)
				for i, p := range planes {
					t := p.Pix[(y-1)*xs:]
					m := p.Pix[y*xs:]
					b := p.Pix[(y+1)*xs:]
					c := m[x]
					corner := (t[x-1] + t[x+1]) + (b[x-1] + b[x+1])
					side := (m[x-1] + m[x+1]) + (t[x] + b[x])
					s := corner*w2 + (side*w1 + c*w0)
					sm[i], mc[i] = s, c
					d := (c - s) / facs[i]
					if d < 0 {
						d = -d
					}
					if d > gap {
						gap = d
					}
				}
				factor := 3 - 4*gap
				if factor < 0 {
					factor = 0
				}
				for i := range planes {
					outs[i].Pix[y*xs+x] = (sm[i]-mc[i])*factor + mc[i]
				}
			}
			return nil
		})
		for i, p := range planes {
			p.Pix = outs[i].Pix
		}
	}
	if !v.sub {
		smooth(v.dc[:], f[:])
	} else {
		for c := 0; c < 3; c++ {
			smooth([]*Plane{v.dc[c]}, []float32{f[c]})
		}
	}
}

func (v *vardctState) readHfGlobal(br *bitio.Reader) error {
	fh := v.fh
	// finalize LF
	if fh.Flags&FlagUseLFFrame != 0 {
		lf := v.fd.dec.lf[0]
		if lf == nil {
			return invalid("missing LF frame")
		}
		for c := 0; c < 3; c++ {
			src := lf.color[c]
			dst := v.dc[c]
			for y := 0; y < dst.H && y < src.H; y++ {
				copy(dst.Row(y), src.Row(y)[:min(dst.W, src.W)])
			}
		}
	} else if fh.Flags&FlagSkipAdaptiveLFSmoothing == 0 {
		v.adaptiveSmoothing()
	}
	// dequant matrices
	allDefault := br.Bool()
	for i := 0; i < numQuantTables; i++ {
		if allDefault {
			v.qenc[i] = defaultQuantEncoding(i)
			continue
		}
		q, err := v.fd.readQuantEncoding(br, i)
		if err != nil {
			return err
		}
		v.qenc[i] = q
	}
	v.presets = int(br.Read(uint(ceilLog2(fh.NumGroups)))) + 1
	np := int(fh.Passes.NumPasses)
	v.passes = make([]hfPass, np)
	for p := 0; p < np; p++ {
		pass := &v.passes[p]
		used := br.U32(bitio.Val(0x5F), bitio.Val(0x13), bitio.Val(0), bitio.Bits(13))
		if used != 0 {
			code, err := entropy.ReadCode(br, 8)
			if err != nil {
				return err
			}
			d := code.NewDecoder(br)
			for o := 0; o < 13; o++ {
				if used&(1<<o) == 0 {
					continue
				}
				nat := naturalOrder(o)
				size := len(nat)
				skip := size / 64
				for c := 0; c < 3; c++ {
					perm, err := entropy.ReadPermutation(br, d, size, skip)
					if err != nil {
						return err
					}
					ord := make([]int32, size)
					for i, pi := range perm {
						ord[i] = nat[pi]
					}
					pass.orders[o][c] = ord
				}
			}
			if err := d.Finish(); err != nil {
				return err
			}
		}
		code, err := entropy.ReadCode(br, 495*v.presets*v.bctx.numCtx)
		if err != nil {
			return err
		}
		pass.code = code
	}
	if br.Overflowed() {
		return bitio.ErrEOF
	}
	return nil
}

// qtable returns (computing lazily) the dequant multipliers for table i.
func (v *vardctState) qtable(i int) ([3][]float32, error) {
	v.qtOnce[i].Do(func() {
		v.qtables[i], v.qtErr[i] = cachedQuantTable(&v.qenc[i], i)
	})
	return v.qtables[i], v.qtErr[i]
}

var defaultQTCache [numQuantTables][3][]float32
var defaultQTOnce [numQuantTables]sync.Once
var defaultQTErr [numQuantTables]error

func cachedQuantTable(q *quantEncoding, i int) ([3][]float32, error) {
	isDefault := q.mode != qmRAW && q.mode == defaultQuantEncoding(i).mode && sameEncoding(q, i)
	if isDefault {
		defaultQTOnce[i].Do(func() {
			d := defaultQuantEncoding(i)
			defaultQTCache[i], defaultQTErr[i] = computeQuantTable(&d, i)
		})
		return defaultQTCache[i], defaultQTErr[i]
	}
	return computeQuantTable(q, i)
}

func sameEncoding(q *quantEncoding, i int) bool {
	d := defaultQuantEncoding(i)
	eq := func(a, b []float32) bool {
		if len(a) != len(b) {
			return false
		}
		for k := range a {
			if a[k] != b[k] {
				return false
			}
		}
		return true
	}
	for c := 0; c < 3; c++ {
		if !eq(q.bands[c], d.bands[c]) || !eq(q.bands4x4[c], d.bands4x4[c]) {
			return false
		}
	}
	return q.idw == d.idw && q.dct2w == d.dct2w && q.dct4mul == d.dct4mul && q.dct4x8mul == d.dct4x8mul && q.afvw == d.afvw
}

// allocHF builds per-group varblock lists and coefficient storage.
func (v *vardctState) allocHF(np int) {
	fh := v.fh
	gd := fh.GroupDim / 8
	v.groups = make([]*groupCoeffs, fh.NumGroups)
	for g := 0; g < fh.NumGroups; g++ {
		gx, gy := g%fh.XGroups, g/fh.XGroups
		bx0, by0 := gx*gd, gy*gd
		bx1, by1 := min(bx0+gd, v.bw), min(by0+gd, v.bh)
		gc := &groupCoeffs{}
		var off [3]int32
		for by := by0; by < by1; by++ {
			for bx := bx0; bx < bx1; bx++ {
				a := v.acs[by*v.bw+bx]
				if a == acsInvalid || a&1 == 0 {
					continue
				}
				t := a >> 1
				vb := varblock{bx: int32(bx), by: int32(by), typ: t, qf: v.qf[by*v.bw+bx]}
				n := int32(64 * acsCoveredX[t] * acsCoveredY[t])
				for c := 0; c < 3; c++ {
					lx, ly := bx-bx0, by-by0
					if (lx>>v.hs[c])<<v.hs[c] != lx || (ly>>v.vs[c])<<v.vs[c] != ly {
						vb.off[c] = -1
						continue
					}
					vb.off[c] = off[c]
					off[c] += n
				}
				gc.vbs = append(gc.vbs, vb)
			}
		}
		for c := 0; c < 3; c++ {
			gc.coef[c] = make([]int32, off[c])
		}
		v.groups[g] = gc
	}
}

var coeffFreqCtx = [63]uint8{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 15, 16, 16, 17, 17, 18, 18, 19, 19,
	20, 20, 21, 21, 22, 22, 23, 23, 23, 23, 24, 24, 24, 24, 25, 25, 25, 25, 26, 26, 26, 26, 27,
	27, 27, 27, 28, 28, 28, 28, 29, 29, 29, 29, 30, 30, 30, 30,
}

var coeffNumNonzeroCtx = [63]uint16{
	0, 31, 62, 62, 93, 93, 93, 93, 123, 123, 123, 123, 152, 152, 152, 152, 152, 152, 152, 152,
	180, 180, 180, 180, 180, 180, 180, 180, 180, 180, 180, 180, 206, 206, 206, 206, 206, 206,
	206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206,
	206, 206, 206, 206, 206, 206, 206,
}

func (v *vardctState) decodePassGroup(br *bitio.Reader, pass, g int) error {
	fh := v.fh
	gc := v.groups[g]
	p := &v.passes[pass]
	b := &v.bctx
	hfp := int(br.Read(uint(ceilLog2(v.presets))))
	if hfp >= v.presets {
		return invalid("hf preset")
	}
	nctx := 495 * b.numCtx
	cm := p.code.ContextMap[nctx*hfp : nctx*(hfp+1)]
	dec := p.code.NewDecoder(br)
	shift := uint(0)
	if pass < int(fh.Passes.NumPasses)-1 {
		shift = uint(fh.Passes.Shift[pass])
	}
	gd := fh.GroupDim / 8
	gx, gy := g%fh.XGroups, g/fh.XGroups
	bx0, by0 := gx*gd, gy*gd
	var nzRow [3][]int32
	for c := 0; c < 3; c++ {
		nzRow[c] = make([]int32, (gd>>v.hs[c])+1)
	}
	nqf := len(b.qfThresholds) + 1
	lastRowY := [3]int{-1, -1, -1}
	for vi := range gc.vbs {
		vb := &gc.vbs[vi]
		t := int(vb.typ)
		cx, cy := acsCoveredX[t], acsCoveredY[t]
		nb := int32(cx * cy)
		lg := uint(acsLog2Covered[t])
		ord := acsStrategyOrder[t]
		lfIdx := int(v.qdc[int(vb.by)*v.bw+int(vb.bx)])
		qfIdx := 0
		for _, th := range b.qfThresholds {
			if uint32(vb.qf) > th {
				qfIdx++
			}
		}
		for i, c := range [3]int{1, 0, 2} {
			if vb.off[c] < 0 {
				continue
			}
			sx := (int(vb.bx) - bx0) >> v.hs[c]
			sy := (int(vb.by) - by0) >> v.vs[c]
			_ = lastRowY
			bctx := int(b.ctxMap[((i*13+ord)*nqf+qfIdx)*b.numDC+lfIdx])
			var pred int32
			switch {
			case sy == 0 && sx == 0:
				pred = 32
			case sy == 0:
				pred = nzRow[c][sx-1]
			case sx == 0:
				pred = nzRow[c][sx]
			default:
				pred = (nzRow[c][sx] + nzRow[c][sx-1] + 1) >> 1
			}
			pidx := pred
			if pred >= 8 {
				pidx = 4 + pred/2
			}
			nz := int32(dec.ReadUintClustered(br, cm[bctx+int(pidx)*b.numCtx]))
			if nz > 63<<lg {
				return invalid("too many nonzeros")
			}
			nzv := (nz + nb - 1) >> lg
			for dx := 0; dx < cx; dx++ {
				nzRow[c][sx+dx] = nzv
			}
			if nz == 0 {
				continue
			}
			coef := gc.coef[c][vb.off[c] : vb.off[c]+64*nb]
			order := p.order(ord, c)
			prev := 0
			if nz <= 4*nb {
				prev = 1
			}
			base := bctx*458 + 37*b.numCtx
			n := int(64 * nb)
			for k := int(nb); k < n; k++ {
				ctx := (int(coeffNumNonzeroCtx[(nz-1)>>lg])+int(coeffFreqCtx[(k-int(nb))>>lg]))*2 + prev
				u := dec.ReadUintClustered(br, cm[base+ctx])
				if u == 0 {
					prev = 0
					continue
				}
				coef[order[k]] += bitio.UnpackSigned(u) << shift
				prev = 1
				nz--
				if nz == 0 {
					break
				}
			}
			if nz != 0 {
				return invalid("nonzero count mismatch")
			}
		}
		if br.Overflowed() {
			return bitio.ErrEOF
		}
	}
	return dec.Finish()
}
