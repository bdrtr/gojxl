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
