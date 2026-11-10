package jxl

import (
	"gojxl/internal/bitio"
	"gojxl/internal/entropy"
)

func readCodeForTree(br *bitio.Reader, t *MATree) (*entropy.Code, error) {
	return entropy.ReadCode(br, t.numLeaves)
}

// refFrame is a saved reference frame (canvas sized).
type refFrame struct {
	color [3]*Plane
	ec    []*Plane
}

// MaxPixels limits the image area accepted by the decoder (protects against
// corrupt headers requesting huge allocations).
var MaxPixels uint64 = 1 << 28

// Decoder decodes a JPEG XL codestream.
type Decoder struct {
	Container  *Container
	Header     *ImageHeader
	EncodedICC []byte
	ICC        []byte

	cs  []byte
	pos int // byte offset of next frame

	refs       [4]*refFrame
	lf         [4]*refFrame // LF frames by level-1
	canvas     *refFrame
	done       bool
	frameIndex int

	visibleIdx, nonvisibleIdx int

	target     *outputTarget
	targetDone bool
	cmyk       *cmykTransform
	cmykDone   bool
	capture    *JPEGCoeffs
}

// CMYK returns the CMYK -> sRGB transform, or nil for non-CMYK images (or
// CMYK profiles that cannot be evaluated).
func (d *Decoder) CMYK() *cmykTransform {
	if !d.cmykDone {
		d.cmyk = newCMYKTransform(d.Header, d.ICC)
		d.cmykDone = true
	}
	return d.cmyk
}

// OutputTarget returns the colour space XYB images are rendered to.
func (d *Decoder) OutputTarget() *outputTarget {
	if !d.targetDone {
		d.target = xybTarget(d.Header, d.ICC)
		d.targetDone = true
	}
	return d.target
}

// OutputCICP returns H.273 colour primaries / transfer characteristics code
// points for HDR outputs (PQ, HLG), which ICC profiles cannot describe
// faithfully. ok is false for other images.
func (d *Decoder) OutputCICP() (primaries, transfer byte, ok bool) {
	c := &d.Header.Colour
	if c.WantICC || c.ColourSpace == CSXYB || c.ColourSpace == CSUnknown || c.WhitePoint != WPD65 {
		return 0, 0, false
	}
	switch c.TransferFunction {
	case TFPQ:
		transfer = 16
	case TFHLG:
		transfer = 18
	default:
		return 0, 0, false
	}
	switch {
	case c.ColourSpace == CSGrey:
		return 0, 0, false
	case c.Primaries == PrimSRGB:
		primaries = 1
	case c.Primaries == Prim2100:
		primaries = 9
	case c.Primaries == PrimP3:
		primaries = 12
	default:
		return 0, 0, false
	}
	return primaries, transfer, true
}

// OutputICC returns the ICC profile describing decoded pixels, or nil when
// they are plain sRGB.
func (d *Decoder) OutputICC() []byte {
	if d.Header.XYBEncoded {
		return d.OutputTarget().icc
	}
	if d.Header.Colour.WantICC {
		if d.CMYK() != nil {
			return nil // rendered to sRGB
		}
		return d.ICC
	}
	c := &d.Header.Colour
	if c.isSRGB() || c.ColourSpace == CSUnknown || c.ColourSpace == CSXYB {
		return nil
	}
	return SynthesizeICC(c)
}

// NewDecoder parses the container, image header and ICC profile.
func NewDecoder(data []byte) (*Decoder, error) {
	c, err := ParseContainer(data)
	if err != nil {
		return nil, err
	}
	d := &Decoder{Container: c, cs: c.Codestream}
	br := bitio.NewReader(d.cs)
	if d.Header, err = readImageHeader(br); err != nil {
		return nil, err
	}
	if uint64(d.Header.Size.Width)*uint64(d.Header.Size.Height) > MaxPixels {
		return nil, unsupported("image too large")
	}
	if d.Header.Colour.WantICC {
		if d.EncodedICC, err = readEncodedICC(br); err != nil {
			return nil, err
		}
		if d.ICC, err = unpredictICC(d.EncodedICC); err != nil {
			d.ICC = nil // non-fatal: we can still decode pixels
		}
	}
	if err := br.ZeroPadToByte(); err != nil {
		return nil, err
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	d.pos = br.BytePos()
	if d.Header.HavePreview {
		// skip the preview frame
		if _, err := d.skipFrame(true); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// skipFrame parses a frame header + TOC and skips its data.
func (d *Decoder) skipFrame(preview bool) (*FrameHeader, error) {
	ih := d.Header
	hdr := ih
	if preview {
		cp := *ih
		cp.Size = ih.Preview
		hdr = &cp
	}
	br := bitio.NewReader(d.cs[d.pos:])
	fh, err := readFrameHeader(br, hdr)
	if err != nil {
		return nil, err
	}
	toc, err := readTOC(br, fh)
	if err != nil {
		return nil, err
	}
	d.pos += br.BytePos() + toc.Total
	if d.pos > len(d.cs) {
		return nil, bitio.ErrEOF
	}
	return fh, nil
}

// Frame is a fully rendered frame (canvas sized, in the output colour
// space, values nominally in [0,1]).
type Frame struct {
	Width, Height int
	Color         [3]*Plane // R, G, B (or grey replicated)
	EC            []*Plane
	Duration      uint32
	Name          string
	IsLast        bool

	conv *xybConv // non-nil: Color holds XYB, converted on output
	grey bool
}

// Done reports whether all frames have been decoded.
func (d *Decoder) Done() bool { return d.done }

// NextFrame decodes frames until the next displayed frame and returns it.
func (d *Decoder) NextFrame() (*Frame, error) {
	for !d.done {
		f, shown, err := d.decodeOneFrame()
		if err != nil {
			return nil, err
		}
		if shown {
			return f, nil
		}
	}
	return nil, nil
}

func (d *Decoder) decodeOneFrame() (*Frame, bool, error) {
	ih := d.Header
	if d.pos >= len(d.cs) {
		return nil, false, bitio.ErrEOF
	}
	br := bitio.NewReader(d.cs[d.pos:])
	fh, err := readFrameHeader(br, ih)
	if err != nil {
		return nil, false, err
	}
	dbg("frame header: %+v bits=%d", *fh, br.BitsRead())
	toc, err := readTOC(br, fh)
	if err != nil {
		return nil, false, err
	}
	dbg("frame: type=%d enc=%d flags=%x size=%dx%d up=%d gshift=%d passes=%d crop=%v last=%v ycbcr=%v gab=%v epf=%d", fh.Type, fh.Encoding, fh.Flags, fh.XSize, fh.YSize, fh.Upsampling, fh.GroupSizeShift, fh.Passes.NumPasses, fh.HaveCrop, fh.IsLast, fh.DoYCbCr, fh.Filter.Gab, fh.Filter.EPFIters)
	dbg("toc: single=%v sizes=%v", toc.Single, toc.Sizes)
	start := d.pos + br.BytePos()
	end := start + toc.Total
	if end > len(d.cs) {
		return nil, false, bitio.ErrEOF
	}
	if (fh.IsLast || fh.Duration > 0) && (fh.Type == FrameRegular || fh.Type == FrameSkipProgressive) {
		d.visibleIdx++
		d.nonvisibleIdx = 0
	} else {
		d.nonvisibleIdx++
	}
	fd := &frameDecoder{dec: d, ih: ih, fh: fh, toc: toc, data: d.cs[start:end]}
	if toc.Single {
		fd.br = bitio.NewReader(fd.data)
	}
	if err := fd.decode(); err != nil {
		return nil, false, err
	}
	d.pos = end
	if fh.IsLast {
		d.done = true
	}
	shown, f, err := d.render(fd)
	if err != nil {
		return nil, false, err
	}
	d.frameIndex++
	return f, shown, nil
}
