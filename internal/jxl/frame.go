package jxl

import (
	"fmt"
	"github.com/bdrtr/gojxl/internal/par"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bdrtr/gojxl/internal/bitio"
)

// Plane is a float32 image plane.
type Plane struct {
	W, H int
	Pix  []float32
}

func newPlane(w, h int) *Plane { return &Plane{W: w, H: h, Pix: make([]float32, w*h)} }

// Row returns row y.
func (p *Plane) Row(y int) []float32 { return p.Pix[y*p.W : (y+1)*p.W] }

// frameDecoder holds the state for decoding a single frame.
type frameDecoder struct {
	dec  *Decoder
	ih   *ImageHeader
	fh   *FrameHeader
	toc  *TOC
	data []byte
	br   *bitio.Reader // shared reader when toc.Single

	lfDequant [3]float32
	global    ModularGlobal
	gmod      *ModularImage // full modular image
	nbColor   int           // number of colour channels in the modular image (0 for VarDCT)

	vd *vardctState // VarDCT state (nil for modular frames)

	noiseLUT [8]float32
	hasNoise bool
	patches  *patchDict
	splines  *splineSet

	// output: colour planes (3) and extra channels, at frame (pre-upsampling) resolution
	color [3]*Plane
	ec    []*Plane
}

func (fd *frameDecoder) section(i int) (*bitio.Reader, error) {
	if fd.toc.Single {
		return fd.br, nil
	}
	off, sz := fd.toc.Offsets[i], fd.toc.Sizes[i]
	if off+sz > len(fd.data) {
		return nil, bitio.ErrEOF
	}
	return bitio.NewReader(fd.data[off : off+sz]), nil
}

func (fd *frameDecoder) numSections() int {
	return 2 + fd.fh.NumLFGroups + fd.fh.NumGroups*int(fd.fh.Passes.NumPasses)
}

// numWorkers returns the number of workers used by parallelForW.
func numWorkers() int { return par.Workers() }

// parallelForW is like parallelFor but also passes a worker index in
// [0, numWorkers()) so callers can keep per-worker scratch buffers.
func parallelForW(n int, fn func(w, i int) error) error {
	workers := min(numWorkers(), n)
	if workers <= 1 {
		for i := 0; i < n; i++ {
			if err := fn(0, i); err != nil {
				return err
			}
		}
		return nil
	}
	var next atomic.Int64
	var firstErr atomic.Value
	var panicked atomic.Value
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked.CompareAndSwap(nil, fmt.Sprint(r))
					next.Store(int64(n)) // stop other workers
				}
			}()
			for {
				i := int(next.Add(1) - 1)
				if i >= n || firstErr.Load() != nil {
					return
				}
				if err := fn(w, i); err != nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if p := panicked.Load(); p != nil {
		panic(p) // re-raise in the calling goroutine
	}
	if e := firstErr.Load(); e != nil {
		return e.(error)
	}
	return nil
}

// parallelFor runs fn(i) for i in [0,n) on up to par.Workers() goroutines.
func parallelFor(n int, fn func(i int) error) error {
	return parallelForW(n, func(_, i int) error { return fn(i) })
}

func (fd *frameDecoder) decode() error {
	fh := fd.fh
	// LfGlobal
	br, err := fd.section(0)
	if err != nil {
		return err
	}
	if err := fd.decodeLfGlobal(br); err != nil {
		return err
	}
	st := startStage()
	// LF groups
	lfg := func(g int) error {
		r, err := fd.section(1 + g)
		if err != nil {
			return err
		}
		return fd.decodeLfGroup(r, g)
	}
	if fd.toc.Single {
		for g := 0; g < fh.NumLFGroups; g++ {
			if err := lfg(g); err != nil {
				return err
			}
		}
	} else if err := parallelFor(fh.NumLFGroups, lfg); err != nil {
		return err
	}
	st.done("lfgroups")
	st = startStage()
	// HF global
	br, err = fd.section(1 + fh.NumLFGroups)
	if err != nil {
		return err
	}
	if err := fd.decodeHfGlobal(br); err != nil {
		return err
	}
	// pass groups
	np := int(fh.Passes.NumPasses)
	// single-pass VarDCT: each group is dequantised and transformed right
	// after its coefficients are decoded, so they are never all in memory
	stream := false
	if fd.vd != nil {
		fd.vd.allocHF(np)
		if np == 1 && fd.dec.capture == nil {
			if err := fd.vd.prepareRecon(); err != nil {
				return err
			}
			stream, fd.vd.streamed = true, true
		}
	}
	for p := 0; p < np; p++ {
		pg := func(wk, g int) error {
			r, err := fd.section(2 + fh.NumLFGroups + p*fh.NumGroups + g)
			if err != nil {
				return err
			}
			if err := fd.decodePassGroup(r, p, g); err != nil {
				return err
			}
			if stream {
				fd.vd.reconGroup(wk, g)
			}
			return nil
		}
		if fd.toc.Single {
			for g := 0; g < fh.NumGroups; g++ {
				if err := pg(0, g); err != nil {
					return err
				}
			}
		} else if err := parallelForW(fh.NumGroups, pg); err != nil {
			return err
		}
	}
	if fd.toc.Single && fd.br.Overflowed() {
		return bitio.ErrEOF
	}
	st.done("passgroups")
	st = startStage()
	defer st.done("finish")
	return fd.finish()
}

func (fd *frameDecoder) decodeLfGlobal(br *bitio.Reader) error {
	fh := fd.fh
	if fh.Flags&FlagPatches != 0 {
		if err := fd.readPatches(br); err != nil {
			return err
		}
	}
	if fh.Flags&FlagSplines != 0 {
		if err := fd.readSplines(br); err != nil {
			return err
		}
	}
	if fh.Flags&FlagNoise != 0 {
		if err := fd.readNoise(br); err != nil {
			return err
		}
	}
	// LF channel dequantization
	fd.lfDequant = [3]float32{1.0 / 32 / 128, 1.0 / 4 / 128, 1.0 / 2 / 128}
	if !br.Bool() {
		for i := 0; i < 3; i++ {
			v, err := br.F16()
			if err != nil {
				return err
			}
			fd.lfDequant[i] = v / 128
		}
	}
	if fh.Encoding == EncVarDCT {
		fd.vd = newVarDCTState(fd)
		if err := fd.vd.readGlobal(br); err != nil {
			return err
		}
	}
	// Global modular
	if br.Bool() {
		nbChans := 3
		if fd.ih.IsGrey() && !fd.ih.XYBEncoded && !fh.DoYCbCr {
			nbChans = 1
		}
		limit := 1024 + fh.XSize*fh.YSize*(nbChans+len(fd.ih.ExtraChannels))/16
		if limit > 1<<22 {
			limit = 1 << 22
		}
		tree, err := readMATree(br, limit)
		if err != nil {
			return err
		}
		code, err := readCodeForTree(br, tree)
		if err != nil {
			return err
		}
		fd.global = ModularGlobal{Tree: tree, Code: code}
	}
	// set up the global modular image
	img := &ModularImage{BitDepth: int(fd.ih.BitDepth.BitsPerSample)}
	if fh.Encoding == EncModular {
		fd.nbColor = 3
		if fd.ih.IsGrey() && !fd.ih.XYBEncoded && !fh.DoYCbCr {
			fd.nbColor = 1
		}
		for c := 0; c < fd.nbColor; c++ {
			hs, vs := 0, 0
			if fh.DoYCbCr {
				hs, vs = fh.HShift[c], fh.VShift[c]
			}
			img.Channels = append(img.Channels, &Channel{W: divCeil(fh.XSize, 1<<hs), H: divCeil(fh.YSize, 1<<vs), HShift: hs, VShift: vs})
		}
	}
	for i := range fd.ih.ExtraChannels {
		up := int(fh.ECUpsampling[i]) << fd.ih.ExtraChannels[i].DimShift
		if up < int(fh.Upsampling) || up > 8 {
			return invalid("ec upsampling")
		}
		fh.ECUpsampling[i] = uint32(up)
		sh := ceilLog2(up) - ceilLog2(int(fh.Upsampling))
		img.Channels = append(img.Channels, &Channel{
			W: divCeil(int(fh.Width), up), H: divCeil(int(fh.Height), up),
			HShift: sh, VShift: sh,
		})
	}
	fd.gmod = img
	if err := decodeModular(br, img, 0, &fd.global, fh.GroupDim, false); err != nil {
		return err
	}
	return nil
}

func ceilLog2(x int) int {
	n := 0
	for (1 << n) < x {
		n++
	}
	return n
}

// decodeModularRect decodes channels of the global modular image that
// belong to the given rect (in frame pixel coordinates, unclipped size
// rectSize) with shifts in [minShift, maxShift].
func (fd *frameDecoder) decodeModularRect(br *bitio.Reader, x0, y0, rectSize, minShift, maxShift, stream int) error {
	full := fd.gmod
	gd := fd.fh.GroupDim
	c := full.NbMeta
	for ; c < len(full.Channels); c++ {
		fc := full.Channels[c]
		if fc.W > gd || fc.H > gd {
			break
		}
	}
	type mapping struct {
		c            int
		x0, y0, w, h int
	}
	var maps []mapping
	sub := &ModularImage{BitDepth: full.BitDepth, pooled: true}
	defer sub.release()
	for ; c < len(full.Channels); c++ {
		fc := full.Channels[c]
		sh := min(fc.HShift, fc.VShift)
		if sh < minShift || sh > maxShift {
			continue
		}
		cx0 := x0 >> fc.HShift
		cy0 := y0 >> fc.VShift
		cw := min(rectSize>>fc.HShift, fc.W-cx0)
		chh := min(rectSize>>fc.VShift, fc.H-cy0)
		if cw <= 0 || chh <= 0 {
			continue
		}
		maps = append(maps, mapping{c, cx0, cy0, cw, chh})
		sub.Channels = append(sub.Channels, &Channel{W: cw, H: chh, HShift: fc.HShift, VShift: fc.VShift})
	}
	if len(sub.Channels) == 0 {
		return nil
	}
	if err := decodeModular(br, sub, stream, &fd.global, 0, false); err != nil {
		return err
	}
	if err := sub.undoTransforms(); err != nil {
		return err
	}
	if len(sub.Channels) != len(maps) {
		return invalid("group channel count mismatch")
	}
	for i, m := range maps {
		sc := sub.Channels[i]
		if sc.W != m.w || sc.H != m.h {
			return invalid("group channel size mismatch")
		}
		fc := full.Channels[m.c]
		for y := 0; y < m.h; y++ {
			copy(fc.Pix[(m.y0+y)*fc.W+m.x0:(m.y0+y)*fc.W+m.x0+m.w], sc.Pix[y*m.w:(y+1)*m.w])
		}
	}
	return nil
}

func (fd *frameDecoder) decodeLfGroup(br *bitio.Reader, g int) error {
	fh := fd.fh
	gx, gy := g%fh.XLFGroups, g/fh.XLFGroups
	lfDim := fh.GroupDim * 8
	if fd.vd != nil {
		if err := fd.vd.decodeLfGroup(br, g); err != nil {
			return err
		}
	}
	stream := 1 + fh.NumLFGroups + g
	if err := fd.decodeModularRect(br, gx*lfDim, gy*lfDim, lfDim, 3, 1000, stream); err != nil {
		return err
	}
	if fd.vd != nil {
		if err := fd.vd.decodeHFMeta(br, g); err != nil {
			return err
		}
	}
	return nil
}

func (fd *frameDecoder) decodeHfGlobal(br *bitio.Reader) error {
	if fd.vd != nil {
		return fd.vd.readHfGlobal(br)
	}
	return nil
}

func (p *Passes) downsamplingBracket(pass int) (minShift, maxShift int) {
	maxShift = 2
	minShift = 3
	for i := 0; ; i++ {
		for j := range p.Downsample {
			if uint32(i) == p.LastPass[j] {
				switch p.Downsample[j] {
				case 8:
					minShift = 3
				case 4:
					minShift = 2
				case 2:
					minShift = 1
				case 1:
					minShift = 0
				}
			}
		}
		if i == int(p.NumPasses)-1 {
			minShift = 0
		}
		if i == pass {
			return
		}
		maxShift = minShift - 1
	}
}

func (fd *frameDecoder) decodePassGroup(br *bitio.Reader, pass, g int) error {
	fh := fd.fh
	gx, gy := g%fh.XGroups, g/fh.XGroups
	if fd.vd != nil {
		if err := fd.vd.decodePassGroup(br, pass, g); err != nil {
			return err
		}
	}
	minS, maxS := fh.Passes.downsamplingBracket(pass)
	stream := 1 + 3*fh.NumLFGroups + 17 + fh.NumGroups*pass + g
	return fd.decodeModularRect(br, gx*fh.GroupDim, gy*fh.GroupDim, fh.GroupDim, minS, maxS, stream)
}

// finish undoes global transforms and converts to float planes.
func (fd *frameDecoder) finish() error {
	fh := fd.fh
	img := fd.gmod
	if err := img.undoTransforms(); err != nil {
		return err
	}
	if fd.vd != nil {
		if cp := fd.dec.capture; cp != nil && cp.CoeffsOnly {
			fd.vd.captureAC()
			return ErrCoeffsCaptured
		}
		if err := fd.vd.finish(); err != nil {
			return err
		}
	}
	nb := fd.nbColor
	if len(img.Channels) != nb+len(fd.ih.ExtraChannels) {
		return invalid("modular channel count")
	}
	// integer channels become float planes in place (same size, both
	// pointer-free); a channel's memory is used once only
	used := map[*int32]bool{}
	plane := func(ch *Channel) *Plane {
		n := ch.W * ch.H
		if n > 0 && len(ch.Pix) >= n && !used[&ch.Pix[0]] {
			used[&ch.Pix[0]] = true
			return &Plane{W: ch.W, H: ch.H, Pix: unsafe.Slice((*float32)(unsafe.Pointer(&ch.Pix[0])), n)}
		}
		return newPlane(ch.W, ch.H)
	}
	if nb > 0 {
		xyb := fd.ih.XYBEncoded
		// with XYB, B adds Y (channel 0): convert channel 0 (c = 1) last
		for _, c := range [3]int{0, 2, 1} {
			cin := c
			if xyb && c < 2 {
				cin = 1 - c
			} else if nb == 1 {
				cin = 0
			}
			ch := img.Channels[cin]
			if nb == 1 && c > 0 {
				fd.color[c] = fd.color[0]
				continue
			}
			p := plane(ch)
			bd := fd.ih.BitDepth
			switch {
			case xyb:
				f := fd.lfDequant[c]
				if c == 2 {
					y := img.Channels[0].Pix
					for i, v := range ch.Pix {
						p.Pix[i] = float32(v+y[i]) * f
					}
				} else {
					for i, v := range ch.Pix {
						p.Pix[i] = float32(v) * f
					}
				}
			case bd.Float:
				for i, v := range ch.Pix {
					p.Pix[i] = intToFloat(v, bd)
				}
			default:
				f := 1.0 / float64(uint64(1)<<bd.BitsPerSample-1)
				for i, v := range ch.Pix {
					p.Pix[i] = float32(float64(v) * f)
				}
			}
			fd.color[c] = p
		}
		_ = fh
	}
	fd.ec = make([]*Plane, len(fd.ih.ExtraChannels))
	for i, e := range fd.ih.ExtraChannels {
		ch := img.Channels[nb+i]
		p := plane(ch)
		if e.BitDepth.Float {
			for k, v := range ch.Pix {
				p.Pix[k] = intToFloat(v, e.BitDepth)
			}
		} else {
			f := 1.0 / float64(uint64(1)<<e.BitDepth.BitsPerSample-1)
			for k, v := range ch.Pix {
				p.Pix[k] = float32(float64(v) * f)
			}
		}
		fd.ec[i] = p
	}
	return nil
}

// intToFloat reinterprets a modular integer as a custom float.
func intToFloat(v int32, bd BitDepth) float32 {
	bits := bd.BitsPerSample
	expBits := bd.ExpBits
	if bits == 32 && expBits == 8 {
		return math.Float32frombits(uint32(v))
	}
	mantBits := bits - expBits - 1
	u := uint32(v)
	sign := (u >> (bits - 1)) & 1
	exp := int32((u >> mantBits) & ((1 << expBits) - 1))
	mant := u & ((1 << mantBits) - 1)
	bias := int32(1)<<(expBits-1) - 1
	var f float32
	if exp == 0 && mant == 0 {
		f = 0
	} else if exp == 0 {
		// subnormal
		f = float32(math.Ldexp(float64(mant), int(1-bias-int32(mantBits))))
	} else if exp == (1<<expBits)-1 {
		if mant == 0 {
			f = float32(math.Inf(1))
		} else {
			f = float32(math.NaN())
		}
	} else {
		f = float32(math.Ldexp(1+float64(mant)/float64(uint64(1)<<mantBits), int(exp-bias)))
	}
	if sign != 0 {
		f = -f
	}
	return f
}

// plane pool to limit allocations of large temporary planes.
var planePool sync.Pool

func getPlane(w, h int) *Plane {
	n := w * h
	if v := planePool.Get(); v != nil {
		p := v.(*Plane)
		if cap(p.Pix) >= n {
			p.W, p.H, p.Pix = w, h, p.Pix[:n]
			return p
		}
	}
	return newPlane(w, h)
}

func putPlane(p *Plane) {
	if p != nil {
		planePool.Put(p)
	}
}
