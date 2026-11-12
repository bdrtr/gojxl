// Package gojxl is a pure Go JPEG XL (ISO/IEC 18181) decoder.
package gojxl

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"

	"gojxl/internal/jxl"
)

func init() {
	image.RegisterFormat("jxl", "\xff\x0a", Decode, DecodeConfig)
	image.RegisterFormat("jxl", "\x00\x00\x00\x0cJXL \x0d\x0a\x87\x0a", Decode, DecodeConfig)
}

// Info describes a JPEG XL image.
type Info struct {
	Width, Height    int // displayed size (after orientation)
	BitsPerSample    int
	FloatSamples     bool
	HasAlpha         bool
	Grey             bool
	XYB              bool
	Animated         bool
	Orientation      int
	NumExtraChannels int
	ICC              []byte
	// OutputICC is the profile that describes the decoded pixels, or nil if
	// they are sRGB.
	OutputICC []byte
	// CICP holds H.273 (primaries, transfer) code points for HDR (PQ/HLG)
	// output, or nil.
	CICP []byte
	// Exif (TIFF format) and XMP metadata from the container's boxes, or
	// nil. The Exif orientation tag is as stored; Orientation is the one
	// decoders apply.
	Exif, XMP []byte
	// Colour describes the colour encoding of the image header.
	Colour string
}

func readAll(r io.Reader) ([]byte, error) {
	if b, ok := r.(*bytes.Reader); ok {
		buf := make([]byte, b.Len())
		_, err := io.ReadFull(b, buf)
		return buf, err
	}
	return io.ReadAll(r)
}

// DecodeConfig returns the colour model and dimensions of a JPEG XL image.
func DecodeConfig(r io.Reader) (image.Config, error) {
	data, err := readAll(r)
	if err != nil {
		return image.Config{}, err
	}
	d, err := jxl.NewDecoder(data)
	if err != nil {
		return image.Config{}, err
	}
	w, h := d.Header.OrientedSize()
	return image.Config{ColorModel: colorModel(d.Header), Width: w, Height: h}, nil
}

// Stat returns information about the image without decoding pixels.
func Stat(data []byte) (*Info, error) {
	d, err := jxl.NewDecoder(data)
	if err != nil {
		return nil, err
	}
	h := d.Header
	w, ht := h.OrientedSize()
	_, meta, _ := boxMetadata(d.Container.Boxes, false)
	return &Info{
		Width: w, Height: ht,
		Exif:             meta.exif,
		XMP:              meta.xmp,
		Colour:           h.Colour.String(),
		BitsPerSample:    int(h.BitDepth.BitsPerSample),
		FloatSamples:     h.BitDepth.Float,
		HasAlpha:         h.AlphaChannel() >= 0,
		Grey:             h.IsGrey(),
		XYB:              h.XYBEncoded,
		Animated:         h.HaveAnimation,
		Orientation:      int(h.Orientation),
		NumExtraChannels: len(h.ExtraChannels),
		ICC:              d.ICC,
		OutputICC:        outputICC(d),
		CICP:             outputCICP(d),
	}, nil
}

func colorModel(h *jxl.ImageHeader) color.Model {
	deep := h.BitDepth.BitsPerSample > 8
	alpha := h.AlphaChannel() >= 0
	switch {
	case h.IsGrey() && !alpha && deep:
		return color.Gray16Model
	case h.IsGrey() && !alpha:
		return color.GrayModel
	case deep:
		return color.NRGBA64Model
	}
	return color.NRGBAModel
}

// Decode decodes the first (or only) displayed frame of a JPEG XL image.
func Decode(r io.Reader) (image.Image, error) {
	data, err := readAll(r)
	if err != nil {
		return nil, err
	}
	return DecodeBytes(data)
}

// Options controls decoding.
type Options struct {
	// Force16 always produces 16-bit images.
	Force16 bool
	// SRGB converts the output to sRGB (gamut clipped): wide-gamut and
	// ICC-tagged images come out untagged, for software without colour
	// management. PQ and HLG images, which need tone mapping, keep their
	// space.
	SRGB bool
}

// newDecoder parses the headers and applies the options that affect
// rendering.
func newDecoder(data []byte, opt *Options) (*jxl.Decoder, error) {
	d, err := jxl.NewDecoder(data)
	if err == nil && opt != nil {
		d.ForceSRGB = opt.SRGB
	}
	return d, err
}

// DecodeBytes decodes a JPEG XL image held in memory.
func DecodeBytes(data []byte) (image.Image, error) {
	return DecodeBytesOptions(data, nil)
}

// DecodeBytesOptions decodes with options.
func DecodeBytesOptions(data []byte, opt *Options) (img image.Image, err error) {
	defer recoverErr(&err)
	d, err := newDecoder(data, opt)
	if err != nil {
		return nil, err
	}
	f, err := d.NextFrame()
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, jxl.ErrInvalid
	}
	return frameToImage(d.Header, f, opt), nil
}

// Frame is one frame of an animation.
type Frame struct {
	Image    image.Image
	Duration float64 // seconds
}

// Animation holds all displayed frames.
type Animation struct {
	Frames   []Frame
	NumLoops int
}

// DecodeAll decodes all displayed frames.
func DecodeAll(data []byte) (anim *Animation, err error) {
	return DecodeAllOptions(data, nil)
}

// DecodeAllOptions decodes all displayed frames with options.
func DecodeAllOptions(data []byte, opt *Options) (anim *Animation, err error) {
	defer recoverErr(&err)
	d, err := newDecoder(data, opt)
	if err != nil {
		return nil, err
	}
	a := &Animation{}
	if d.Header.HaveAnimation {
		a.NumLoops = int(d.Header.Animation.NumLoops)
	}
	for !d.Done() {
		f, err := d.NextFrame()
		if err != nil {
			return nil, err
		}
		if f == nil {
			break
		}
		fr := Frame{Image: frameToImage(d.Header, f, opt)}
		if an := d.Header.Animation; d.Header.HaveAnimation && an.TpsNumerator > 0 {
			fr.Duration = float64(f.Duration) * float64(an.TpsDenominator) / float64(an.TpsNumerator)
		}
		a.Frames = append(a.Frames, fr)
	}
	return a, nil
}

// outputICC returns the ICC profile describing decoded pixels: the embedded
// profile for non-XYB images (pixels are stored in that space), nil when the
// decoder converts to sRGB.
func outputCICP(d *jxl.Decoder) []byte {
	if p, t, ok := d.OutputCICP(); ok {
		return []byte{p, t}
	}
	return nil
}

func outputICC(d *jxl.Decoder) []byte {
	return d.OutputICC()
}

// recoverErr converts a panic caused by malformed input into an error.
func recoverErr(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w: malformed input (%v)", jxl.ErrInvalid, r)
	}
}
