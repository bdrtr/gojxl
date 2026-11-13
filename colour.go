package gojxl

import (
	"encoding/binary"

	"github.com/bdrtr/gojxl/internal/enc"
	"github.com/bdrtr/gojxl/internal/jxl"
	"github.com/bdrtr/gojxl/internal/pngx"
)

// ErrUnsupportedColour is returned for lossy encoding of samples whose
// colour space cannot be converted to XYB: ICC profiles other than
// matrix/TRC RGB or grey ones (such as LUT-based or CMYK profiles), and
// PQ or HLG transfer functions. Lossless encoding stores any profile.
var ErrUnsupportedColour = enc.ErrColourModel

// colourSpec describes the colour space of input samples: an enum
// encoding or an ICC profile (neither: sRGB).
type colourSpec struct {
	enc *jxl.ColourEncoding
	icc []byte
}

// specFromICC describes samples with an ICC profile, as the enum encoding
// it is equivalent to when there is one (as libjxl stores profiles). ok is
// false for malformed profiles, which describe nothing.
func specFromICC(icc []byte) (s colourSpec, ok bool) {
	if len(icc) < 132 || binary.BigEndian.Uint32(icc) != uint32(len(icc)) || string(icc[36:40]) != "acsp" {
		return colourSpec{}, false
	}
	if e := jxl.EncodingFromICC(icc); e != nil {
		if e.IsSRGB() {
			return colourSpec{}, true
		}
		return colourSpec{enc: e}, true
	}
	return colourSpec{icc: icc}, true
}

// specFromPNG describes the samples of a PNG file by its colour chunks,
// in their order of precedence: cICP, iCCP, sRGB, then gAMA and cHRM.
func specFromPNG(ci pngx.Ancillary) colourSpec {
	if c := ci.CICP; c != nil {
		if e := jxl.EncodingFromCICP(c[0], c[1], c[2], c[3]); e != nil {
			return colourSpec{enc: e}
		}
	}
	if ci.ICC != nil {
		if s, ok := specFromICC(ci.ICC); ok {
			return s
		}
		// a malformed profile: the other chunks are fallbacks
	}
	if ci.SRGB || ci.Gamma == 0 && ci.CHRM == nil {
		return colourSpec{}
	}
	e := &jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimSRGB, TransferFunction: jxl.TFSRGB, RenderingIntent: 1}
	switch g := ci.Gamma; {
	case g == 100000:
		e.TransferFunction = jxl.TFLinear
	case g > 0 && g < 100000:
		// gAMA and the header's gamma are both the encoding exponent
		e.TransferFunction, e.Gamma = jxl.TFGamma, float64(g)/1e5
	}
	if c := ci.CHRM; c != nil && c[1] > 0 && c[3] > 0 && c[5] > 0 && c[7] > 0 {
		xy := func(i int) [2]float64 { return [2]float64{float64(c[i]) / 1e5, float64(c[i+1]) / 1e5} }
		e.SetWhite(xy(0))
		e.SetPrimaries([3][2]float64{xy(2), xy(4), xy(6)})
	}
	if e.IsSRGB() {
		return colourSpec{}
	}
	return colourSpec{enc: e}
}

// apply sets the colour description of im. An ICC profile of the other
// colour space (grey or RGB) than the samples, or of another kind (CMYK,
// Lab), is dropped; an enum encoding takes the samples' colour space.
func (s colourSpec) apply(im *enc.Image) {
	switch {
	case s.icc != nil:
		if space := string(s.icc[16:20]); space == "GRAY" && im.Grey || space == "RGB " && !im.Grey {
			im.ICC = s.icc
		}
	case s.enc != nil:
		e := *s.enc
		if im.Grey {
			e.ColourSpace = jxl.CSGrey
		} else if e.ColourSpace == jxl.CSGrey {
			e.ColourSpace, e.Primaries = jxl.CSRGB, jxl.PrimSRGB
		}
		im.Colour = &e
	}
}
