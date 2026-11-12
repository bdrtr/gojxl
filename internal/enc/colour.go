package enc

import (
	"errors"
	"math"

	"gojxl/internal/bitio"
	"gojxl/internal/jxl"
)

// greySRGB is the colour encoding written for grey images without one.
var greySRGB = jxl.ColourEncoding{ColourSpace: jxl.CSGrey, WhitePoint: jxl.WPD65, TransferFunction: jxl.TFSRGB, RenderingIntent: 1}

// writeColour writes the colour_encoding bundle of the image header: the
// ICC flag when im.ICC is set (the profile follows the metadata), else
// im.Colour, or sRGB when neither is set. grey selects the grey colour
// space.
func writeColour(w *bitio.Writer, im *Image, grey bool) {
	c := im.Colour
	if im.defaultColour() {
		if !grey {
			w.WriteBool(true) // all default: sRGB
			return
		}
		c = &greySRGB
	}
	w.WriteBool(false)
	w.WriteBool(im.ICC != nil)
	if grey {
		w.WriteEnum(jxl.CSGrey)
	} else {
		w.WriteEnum(jxl.CSRGB)
	}
	if im.ICC != nil {
		return
	}
	w.WriteEnum(c.WhitePoint)
	if c.WhitePoint == jxl.WPCustom {
		writeXY(w, c.White)
	}
	if !grey {
		w.WriteEnum(c.Primaries)
		if c.Primaries == jxl.PrimCustom {
			writeXY(w, c.Red)
			writeXY(w, c.Green)
			writeXY(w, c.Blue)
		}
	}
	if c.TransferFunction == jxl.TFGamma {
		w.WriteBool(true)
		w.Write(24, uint64(math.Round(c.Gamma*1e7)))
	} else {
		w.WriteBool(false)
		w.WriteEnum(c.TransferFunction)
	}
	w.WriteEnum(c.RenderingIntent)
}

// defaultColour reports whether the samples are described as sRGB with
// the default (relative) rendering intent.
func (im *Image) defaultColour() bool {
	c := im.Colour
	return im.ICC == nil && (c == nil || c.IsSRGB() && c.RenderingIntent == 1)
}

// intensityTarget returns the peak luminance to store for HDR samples:
// 10000 nits for PQ and 1000 for HLG (as libjxl sets them), else 0 (the
// default 255).
func (im *Image) intensityTarget() float32 {
	if c := im.Colour; im.ICC == nil && c != nil {
		switch c.TransferFunction {
		case jxl.TFPQ:
			return 10000
		case jxl.TFHLG:
			return 1000
		}
	}
	return 0
}

// writeToneMapping writes the tone_mapping bundle: the default, or the
// intensity target of HDR samples.
func writeToneMapping(w *bitio.Writer, im *Image) {
	it := im.intensityTarget()
	if it == 0 {
		w.WriteBool(true) // default
		return
	}
	w.WriteBool(false)
	w.Write(16, uint64(f16(it)))
	w.Write(16, 0)     // min_nits
	w.WriteBool(false) // not relative to max display
	w.Write(16, 0)     // linear_below
}

// f16 encodes a positive normal value as IEEE half precision (rounded).
func f16(v float32) uint16 {
	b := math.Float32bits(v)
	exp := int(b>>23&0xff) - 127 + 15
	mant := b & 0x7fffff
	h := uint32(exp)<<10 | mant>>13
	if mant&0x1000 != 0 { // round half up
		h++
	}
	return uint16(h)
}

func writeXY(w *bitio.Writer, xy [2]float64) {
	for _, v := range xy {
		w.WriteU32(bitio.PackSigned(int32(math.Round(v*1e6))), bitio.Bits(19), bitsOff(19, 524288), bitsOff(20, 1048576), bitsOff(21, 2097152))
	}
}

// writeICCIfAny writes the image's ICC profile, which follows the image
// metadata.
func writeICCIfAny(w *bitio.Writer, im *Image) {
	if im.ICC != nil {
		writeICC(w, im.ICC)
	}
}

// ErrColourModel reports a colour description that lossy encoding cannot
// convert to XYB: ICC profiles other than matrix/TRC RGB or grey ones,
// and PQ or HLG transfer functions.
var ErrColourModel = errors.New("jxl: lossy encoding needs a matrix/TRC colour profile (not PQ, HLG or LUT based)")

// colourModel returns how the image's samples map to linear sRGB, or nil
// for sRGB samples.
func (im *Image) colourModel() (*jxl.ColourModel, error) {
	var m *jxl.ColourModel
	switch {
	case im.ICC != nil:
		m = jxl.ModelFromICC(im.ICC)
	case im.Colour != nil && !im.Colour.IsSRGB():
		m = jxl.ModelFromEncoding(im.Colour)
	default:
		return nil, nil
	}
	if m == nil || m.Grey != im.Grey {
		return nil, ErrColourModel
	}
	return m, nil
}
