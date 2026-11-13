package jxl

import (
	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/entropy"
)

// Frame types.
const (
	FrameRegular         = 0
	FrameLF              = 1
	FrameReferenceOnly   = 2
	FrameSkipProgressive = 3
)

// Encodings.
const (
	EncVarDCT  = 0
	EncModular = 1
)

// Frame flags.
const (
	FlagNoise                   = 1
	FlagPatches                 = 2
	FlagSplines                 = 0x10
	FlagUseLFFrame              = 0x20
	FlagSkipAdaptiveLFSmoothing = 0x80
)

// Blend modes.
const (
	BlendReplace = 0
	BlendAdd     = 1
	BlendBlend   = 2
	BlendMulAdd  = 3
	BlendMul     = 4
)

// BlendingInfo describes how a frame is blended onto the canvas.
type BlendingInfo struct {
	Mode         uint32
	AlphaChannel uint32
	Clamp        bool
	Source       uint32
}

// Passes describes progressive passes.
type Passes struct {
	NumPasses  uint32
	Shift      []uint32
	Downsample []uint32
	LastPass   []uint32
}

// RestorationFilter holds Gaborish and EPF parameters.
type RestorationFilter struct {
	Gab             bool
	GabWeights      [3][2]float32
	EPFIters        uint32
	EPFSharpLUT     [8]float32
	EPFChannelScale [3]float32
	EPFQuantMul     float32
	EPFPass0Sigma   float32
	EPFPass2Sigma   float32
	EPFBorderSadMul float32
	EPFSigmaModular float32
}

func defaultRestorationFilter() RestorationFilter {
	r := RestorationFilter{
		Gab:             true,
		EPFIters:        2,
		EPFChannelScale: [3]float32{40, 5, 3.5},
		EPFQuantMul:     0.46,
		EPFPass0Sigma:   0.9,
		EPFPass2Sigma:   6.5,
		EPFBorderSadMul: 2.0 / 3.0,
		EPFSigmaModular: 1.0,
	}
	for i := range r.GabWeights {
		r.GabWeights[i] = [2]float32{0.115169525, 0.061248592}
	}
	for i := range r.EPFSharpLUT {
		r.EPFSharpLUT[i] = float32(i) / 7
	}
	return r
}

// FrameHeader is a decoded frame header.
type FrameHeader struct {
	Type            uint32
	Encoding        uint32
	Flags           uint64
	DoYCbCr         bool
	JPEGUpsampling  [3]uint32
	Upsampling      uint32
	ECUpsampling    []uint32
	GroupSizeShift  uint32
	XQMScale        uint32
	BQMScale        uint32
	Passes          Passes
	LFLevel         uint32
	HaveCrop        bool
	X0, Y0          int32
	Width, Height   uint32 // frame size in image pixels (before upsampling division)
	Blending        BlendingInfo
	ECBlending      []BlendingInfo
	Duration        uint32
	Timecode        uint32
	IsLast          bool
	SaveAsReference uint32
	SaveBeforeCT    bool
	Name            string
	Filter          RestorationFilter

	// derived
	GroupDim     int
	XSize, YSize int // decoded (pre-upsampling) frame size in pixels
	XGroups      int
	YGroups      int
	XLFGroups    int
	YLFGroups    int
	NumGroups    int
	NumLFGroups  int
	// chroma subsampling shifts per channel (YCbCr), in channel order (Y? see spec)
	HShift               [3]int
	VShift               [3]int
	MaxHShift, MaxVShift int
}

func readBlendingInfo(br *bitio.Reader, hasEC bool, fullFrame bool) BlendingInfo {
	var b BlendingInfo
	b.Mode = br.U32(bitio.Val(0), bitio.Val(1), bitio.Val(2), bitio.BitsOff(2, 3))
	if hasEC && (b.Mode == BlendBlend || b.Mode == BlendMulAdd) {
		b.AlphaChannel = br.U32(bitio.Val(0), bitio.Val(1), bitio.Val(2), bitio.BitsOff(3, 3))
	}
	if (hasEC && (b.Mode == BlendBlend || b.Mode == BlendMulAdd)) || b.Mode == BlendMul {
		b.Clamp = br.Bool()
	}
	return b
}

func readFrameHeader(br *bitio.Reader, ih *ImageHeader) (*FrameHeader, error) {
	f := &FrameHeader{
		Type:           FrameRegular,
		Encoding:       EncVarDCT,
		Upsampling:     1,
		GroupSizeShift: 1,
		XQMScale:       3,
		BQMScale:       2,
		Passes:         Passes{NumPasses: 1},
		IsLast:         true,
		Filter:         defaultRestorationFilter(),
	}
	nec := len(ih.ExtraChannels)
	f.ECUpsampling = make([]uint32, nec)
	for i := range f.ECUpsampling {
		f.ECUpsampling[i] = 1
	}
	f.ECBlending = make([]BlendingInfo, nec)
	f.Width, f.Height = ih.Size.Width, ih.Size.Height
	var err error

	allDefault := br.Bool()
	if !allDefault {
		f.Type = br.Read(2)
		f.Encoding = br.Read(1)
		f.Flags = br.U64()
		if !ih.XYBEncoded {
			f.DoYCbCr = br.Bool()
		}
		if f.Flags&FlagUseLFFrame == 0 {
			if f.DoYCbCr {
				for i := range f.JPEGUpsampling {
					f.JPEGUpsampling[i] = br.Read(2)
				}
			}
			f.Upsampling = br.U32(bitio.Val(1), bitio.Val(2), bitio.Val(4), bitio.Val(8))
			for i := range f.ECUpsampling {
				f.ECUpsampling[i] = br.U32(bitio.Val(1), bitio.Val(2), bitio.Val(4), bitio.Val(8))
			}
		}
		if f.Encoding == EncModular {
			f.GroupSizeShift = br.Read(2)
		}
		if ih.XYBEncoded && f.Encoding == EncVarDCT {
			f.XQMScale = br.Read(3)
			f.BQMScale = br.Read(3)
		} else {
			f.XQMScale, f.BQMScale = 2, 2
		}
		if f.Type != FrameReferenceOnly {
			p := &f.Passes
			p.NumPasses = br.U32(bitio.Val(1), bitio.Val(2), bitio.Val(3), bitio.BitsOff(3, 4))
			if p.NumPasses > 11 {
				return nil, invalid("too many passes")
			}
			if p.NumPasses != 1 {
				nds := br.U32(bitio.Val(0), bitio.Val(1), bitio.Val(2), bitio.BitsOff(1, 3))
				if nds > p.NumPasses-1 {
					return nil, invalid("num_ds")
				}
				p.Shift = make([]uint32, p.NumPasses-1)
				for i := range p.Shift {
					p.Shift[i] = br.Read(2)
				}
				p.Downsample = make([]uint32, nds)
				for i := range p.Downsample {
					p.Downsample[i] = br.U32(bitio.Val(1), bitio.Val(2), bitio.Val(4), bitio.Val(8))
				}
				p.LastPass = make([]uint32, nds)
				for i := range p.LastPass {
					p.LastPass[i] = br.U32(bitio.Val(0), bitio.Val(1), bitio.Val(2), bitio.Bits(3))
					if p.LastPass[i] >= p.NumPasses {
						return nil, invalid("last_pass")
					}
				}
			}
		}
		if f.Type == FrameLF {
			f.LFLevel = br.Read(2) + 1
		}
		if f.Type != FrameLF {
			f.HaveCrop = br.Bool()
		}
		if f.HaveCrop {
			enc := [4]bitio.U32Dist{bitio.Bits(8), bitio.BitsOff(11, 256), bitio.BitsOff(14, 2304), bitio.BitsOff(30, 18688)}
			if f.Type != FrameReferenceOnly {
				f.X0 = bitio.UnpackSigned(br.U32(enc[0], enc[1], enc[2], enc[3]))
				f.Y0 = bitio.UnpackSigned(br.U32(enc[0], enc[1], enc[2], enc[3]))
			}
			f.Width = br.U32(enc[0], enc[1], enc[2], enc[3])
			f.Height = br.U32(enc[0], enc[1], enc[2], enc[3])
		}
		fullFrame := !f.HaveCrop || (f.X0 <= 0 && f.Y0 <= 0 &&
			int64(f.X0)+int64(f.Width) >= int64(ih.Size.Width) &&
			int64(f.Y0)+int64(f.Height) >= int64(ih.Size.Height))
		normal := f.Type == FrameRegular || f.Type == FrameSkipProgressive
		if normal {
			f.Blending = readBlendingInfo(br, nec > 0, fullFrame)
			resets := f.Blending.Mode == BlendReplace && fullFrame
			if !resets {
				f.Blending.Source = br.Read(2)
			}
			for i := range f.ECBlending {
				f.ECBlending[i] = readBlendingInfo(br, nec > 0, fullFrame)
				if !resets {
					f.ECBlending[i].Source = br.Read(2)
				}
			}
			if ih.HaveAnimation {
				f.Duration = br.U32(bitio.Val(0), bitio.Val(1), bitio.Bits(8), bitio.Bits(32))
				if ih.Animation.HaveTimecodes {
					f.Timecode = br.Read(32)
				}
			}
			f.IsLast = br.Bool()
		} else {
			f.IsLast = false
		}
		if f.Type != FrameLF && !f.IsLast {
			f.SaveAsReference = br.Read(2)
		}
		resets := f.Blending.Mode == BlendReplace && fullFrame
		if f.Type == FrameReferenceOnly ||
			(resets && !f.IsLast && (f.Duration == 0 || f.SaveAsReference != 0) && f.Type != FrameLF) {
			f.SaveBeforeCT = br.Bool()
		} else {
			f.SaveBeforeCT = !normal
		}
		f.Name = readName(br)
		if err = readRestorationFilter(br, f); err != nil {
			return nil, err
		}
		if err = skipExtensions(br); err != nil {
			return nil, err
		}
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	f.computeDims(ih)
	// the same area limit as for the image (frames may be cropped to
	// any size, and their group count sizes the table of contents)
	if uint64(f.Width)*uint64(f.Height) > MaxPixels || uint64(f.XSize)*uint64(f.YSize) > MaxPixels {
		return nil, unsupported("frame too large")
	}
	return f, nil
}

func readRestorationFilter(br *bitio.Reader, f *FrameHeader) error {
	r := &f.Filter
	if br.Bool() {
		return nil
	}
	var err error
	r.Gab = br.Bool()
	if r.Gab {
		if br.Bool() {
			for c := 0; c < 3; c++ {
				for k := 0; k < 2; k++ {
					if r.GabWeights[c][k], err = br.F16(); err != nil {
						return err
					}
				}
				s := 1 + 4*(r.GabWeights[c][0]+r.GabWeights[c][1])
				if s < 1e-6 && s > -1e-6 {
					return invalid("gaborish weights")
				}
			}
		}
	}
	r.EPFIters = br.Read(2)
	if r.EPFIters > 0 {
		if f.Encoding == EncVarDCT && br.Bool() {
			for i := range r.EPFSharpLUT {
				if r.EPFSharpLUT[i], err = br.F16(); err != nil {
					return err
				}
			}
		}
		if br.Bool() {
			for i := range r.EPFChannelScale {
				if r.EPFChannelScale[i], err = br.F16(); err != nil {
					return err
				}
			}
			br.Read(32) // pass1/pass2 zeroflush, unused
		}
		if br.Bool() {
			if f.Encoding == EncVarDCT {
				if r.EPFQuantMul, err = br.F16(); err != nil {
					return err
				}
			}
			if r.EPFPass0Sigma, err = br.F16(); err != nil {
				return err
			}
			if r.EPFPass2Sigma, err = br.F16(); err != nil {
				return err
			}
			if r.EPFBorderSadMul, err = br.F16(); err != nil {
				return err
			}
		}
		if f.Encoding == EncModular {
			if r.EPFSigmaModular, err = br.F16(); err != nil {
				return err
			}
		}
	}
	return skipExtensions(br)
}

func divCeil(a, b int) int { return (a + b - 1) / b }

func (f *FrameHeader) computeDims(ih *ImageHeader) {
	w, h := int(f.Width), int(f.Height)
	if f.Type == FrameLF {
		w = divCeil(int(ih.Size.Width), 1<<(3*f.LFLevel))
		h = divCeil(int(ih.Size.Height), 1<<(3*f.LFLevel))
		f.Width, f.Height = uint32(w), uint32(h)
	}
	up := int(f.Upsampling)
	f.XSize = divCeil(w, up)
	f.YSize = divCeil(h, up)
	if f.DoYCbCr {
		// jpeg_upsampling: 0 -> none, 1 -> 2x2, 2 -> 2x1 (h), 3 -> 1x2 (v)
		for c := 0; c < 3; c++ {
			switch f.JPEGUpsampling[c] {
			case 1:
				f.HShift[c], f.VShift[c] = 1, 1
			case 2:
				f.HShift[c] = 1
			case 3:
				f.VShift[c] = 1
			}
			f.MaxHShift = max(f.MaxHShift, f.HShift[c])
			f.MaxVShift = max(f.MaxVShift, f.VShift[c])
		}
		// channel shifts are relative to the max
		for c := 0; c < 3; c++ {
			f.HShift[c] = f.MaxHShift - f.HShift[c]
			f.VShift[c] = f.MaxVShift - f.VShift[c]
		}
	}
	f.GroupDim = 128 << f.GroupSizeShift
	f.XGroups = divCeil(f.XSize, f.GroupDim)
	f.YGroups = divCeil(f.YSize, f.GroupDim)
	f.XLFGroups = divCeil(f.XSize, f.GroupDim*8)
	f.YLFGroups = divCeil(f.YSize, f.GroupDim*8)
	f.NumGroups = f.XGroups * f.YGroups
	f.NumLFGroups = f.XLFGroups * f.YLFGroups
}

// TOC is the table of contents of a frame.
type TOC struct {
	Offsets []int // byte offsets (relative to start of frame data) per section, in logical order
	Sizes   []int
	Single  bool // a single section holds all data
	Total   int
}

func readTOC(br *bitio.Reader, f *FrameHeader) (*TOC, error) {
	n := 1
	single := f.NumGroups == 1 && f.Passes.NumPasses == 1
	if !single {
		n = 1 + f.NumLFGroups + 1 + f.NumGroups*int(f.Passes.NumPasses)
	}
	// every entry takes at least 12 bits: a count the remaining data
	// cannot hold is corrupt (and must not size allocations)
	if rem := len(br.Data())*8 - br.BitsRead(); n > rem/12+1 {
		return nil, bitio.ErrEOF
	}
	var perm []int
	if br.Bool() {
		code, err := entropy.ReadCode(br, 8)
		if err != nil {
			return nil, err
		}
		d := code.NewDecoder(br)
		perm, err = entropy.ReadPermutation(br, d, n, 0)
		if err != nil {
			return nil, err
		}
		if err := d.Finish(); err != nil {
			return nil, err
		}
	}
	if err := br.ZeroPadToByte(); err != nil {
		return nil, err
	}
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = int(br.U32(bitio.Bits(10), bitio.BitsOff(14, 1024), bitio.BitsOff(22, 17408), bitio.BitsOff(30, 4211712)))
	}
	if err := br.ZeroPadToByte(); err != nil {
		return nil, err
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	t := &TOC{Single: single, Sizes: make([]int, n), Offsets: make([]int, n)}
	// sizes are stored in bitstream order; offsets accumulate in that order.
	// Logical section i lives at stream position perm[i].
	offs := make([]int, n)
	off := 0
	for i := 0; i < n; i++ {
		offs[i] = off
		off += sizes[i]
	}
	for i := 0; i < n; i++ {
		j := i
		if perm != nil {
			j = perm[i]
		}
		t.Offsets[i] = offs[j]
		t.Sizes[i] = sizes[j]
	}
	t.Total = off
	return t, nil
}
