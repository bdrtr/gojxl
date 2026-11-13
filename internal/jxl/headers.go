package jxl

import (
	"errors"
	"fmt"

	"github.com/bdrtr/gojxl/internal/bitio"
)

// Errors returned by the decoder.
var (
	ErrNotJXL      = errors.New("jxl: not a JPEG XL file")
	ErrInvalid     = errors.New("jxl: invalid codestream")
	ErrUnsupported = errors.New("jxl: unsupported feature")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// SizeHeader encodes image dimensions.
type SizeHeader struct {
	Width, Height uint32
}

func ratioWidth(ratio, height uint32) uint32 {
	switch ratio {
	case 1:
		return height
	case 2:
		return uint32(uint64(height) * 12 / 10)
	case 3:
		return uint32(uint64(height) * 4 / 3)
	case 4:
		return uint32(uint64(height) * 3 / 2)
	case 5:
		return uint32(uint64(height) * 16 / 9)
	case 6:
		return uint32(uint64(height) * 5 / 4)
	case 7:
		return height * 2
	}
	return 0
}

func readSizeHeader(br *bitio.Reader) SizeHeader {
	var s SizeHeader
	small := br.Bool()
	if small {
		s.Height = (br.Read(5) + 1) * 8
	} else {
		s.Height = br.U32(bitio.BitsOff(9, 1), bitio.BitsOff(13, 1), bitio.BitsOff(18, 1), bitio.BitsOff(30, 1))
	}
	ratio := br.Read(3)
	if ratio == 0 {
		if small {
			s.Width = (br.Read(5) + 1) * 8
		} else {
			s.Width = br.U32(bitio.BitsOff(9, 1), bitio.BitsOff(13, 1), bitio.BitsOff(18, 1), bitio.BitsOff(30, 1))
		}
	} else {
		s.Width = ratioWidth(ratio, s.Height)
	}
	return s
}

func readPreviewHeader(br *bitio.Reader) SizeHeader {
	var s SizeHeader
	div8 := br.Bool()
	if div8 {
		s.Height = 8 * br.U32(bitio.Val(16), bitio.Val(32), bitio.BitsOff(5, 1), bitio.BitsOff(9, 33))
	} else {
		s.Height = br.U32(bitio.BitsOff(6, 1), bitio.BitsOff(8, 65), bitio.BitsOff(10, 321), bitio.BitsOff(12, 1345))
	}
	ratio := br.Read(3)
	if ratio == 0 {
		if div8 {
			s.Width = 8 * br.U32(bitio.Val(16), bitio.Val(32), bitio.BitsOff(5, 1), bitio.BitsOff(9, 33))
		} else {
			s.Width = br.U32(bitio.BitsOff(6, 1), bitio.BitsOff(8, 65), bitio.BitsOff(10, 321), bitio.BitsOff(12, 1345))
		}
	} else {
		s.Width = ratioWidth(ratio, s.Height)
	}
	return s
}

// BitDepth describes sample representation.
type BitDepth struct {
	Float         bool
	BitsPerSample uint32
	ExpBits       uint32
}

func readBitDepth(br *bitio.Reader) (BitDepth, error) {
	var b BitDepth
	b.Float = br.Bool()
	if !b.Float {
		b.BitsPerSample = br.U32(bitio.Val(8), bitio.Val(10), bitio.Val(12), bitio.BitsOff(6, 1))
		if b.BitsPerSample > 31 {
			return b, invalid("bits per sample %d", b.BitsPerSample)
		}
	} else {
		b.BitsPerSample = br.U32(bitio.Val(32), bitio.Val(16), bitio.Val(24), bitio.BitsOff(6, 1))
		b.ExpBits = br.Read(4) + 1
		if b.ExpBits < 2 || b.ExpBits > 8 {
			return b, invalid("exp bits")
		}
		if b.BitsPerSample > 32 || b.BitsPerSample < b.ExpBits+2 {
			return b, invalid("float bits per sample")
		}
	}
	return b, nil
}

// Extra channel types.
const (
	ECAlpha         = 0
	ECDepth         = 1
	ECSpotColour    = 2
	ECSelectionMask = 3
	ECBlack         = 4
	ECCFA           = 5
	ECThermal       = 6
	ECNonOptional   = 15
	ECOptional      = 16
)

// ExtraChannelInfo describes an extra channel.
type ExtraChannelInfo struct {
	Type            uint32
	BitDepth        BitDepth
	DimShift        uint32
	Name            string
	AlphaAssociated bool
	Spot            [4]float32
	CFAChannel      uint32
}

func readName(br *bitio.Reader) string {
	n := br.U32(bitio.Val(0), bitio.Bits(4), bitio.BitsOff(5, 16), bitio.BitsOff(10, 48))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(br.Read(8))
	}
	return string(b)
}

func readExtraChannelInfo(br *bitio.Reader) (ExtraChannelInfo, error) {
	e := ExtraChannelInfo{BitDepth: BitDepth{BitsPerSample: 8}}
	if br.Bool() {
		return e, nil
	}
	t, err := br.Enum()
	if err != nil {
		return e, err
	}
	e.Type = t
	if e.BitDepth, err = readBitDepth(br); err != nil {
		return e, err
	}
	e.DimShift = br.U32(bitio.Val(0), bitio.Val(3), bitio.Val(4), bitio.BitsOff(3, 1))
	e.Name = readName(br)
	switch t {
	case ECAlpha:
		e.AlphaAssociated = br.Bool()
	case ECSpotColour:
		for i := range e.Spot {
			if e.Spot[i], err = br.F16(); err != nil {
				return e, err
			}
		}
	case ECCFA:
		e.CFAChannel = br.U32(bitio.Val(1), bitio.Bits(2), bitio.BitsOff(4, 3), bitio.BitsOff(8, 19))
	case ECDepth, ECSelectionMask, ECBlack, ECThermal, ECNonOptional, ECOptional:
	default:
		return e, invalid("extra channel type %d", t)
	}
	return e, nil
}

// Colour space values.
const (
	CSRGB     = 0
	CSGrey    = 1
	CSXYB     = 2
	CSUnknown = 3
)

// White points.
const (
	WPD65    = 1
	WPCustom = 2
	WPE      = 10
	WPDCI    = 11
)

// Primaries.
const (
	PrimSRGB   = 1
	PrimCustom = 2
	Prim2100   = 9
	PrimP3     = 11
)

// Transfer functions.
const (
	TF709     = 1
	TFUnknown = 2
	TFLinear  = 8
	TFSRGB    = 13
	TFPQ      = 16
	TFDCI     = 17
	TFHLG     = 18
	TFGamma   = 255 // internal marker: Gamma field valid
)

// ColourEncoding describes the colour space of the image.
type ColourEncoding struct {
	WantICC          bool
	ColourSpace      uint32
	WhitePoint       uint32
	White            [2]float64
	Primaries        uint32
	Red, Green, Blue [2]float64
	TransferFunction uint32
	Gamma            float64 // if TransferFunction == TFGamma; value is 1/gamma exponent as encoded (gamma/1e7)
	RenderingIntent  uint32
}

func defaultColourEncoding() ColourEncoding {
	return ColourEncoding{
		ColourSpace:      CSRGB,
		WhitePoint:       WPD65,
		Primaries:        PrimSRGB,
		TransferFunction: TFSRGB,
		RenderingIntent:  1,
	}
}

func readCustomXY(br *bitio.Reader) [2]float64 {
	var r [2]float64
	for i := range r {
		v := br.U32(bitio.Bits(19), bitio.BitsOff(19, 524288), bitio.BitsOff(20, 1048576), bitio.BitsOff(21, 2097152))
		r[i] = float64(bitio.UnpackSigned(v)) / 1e6
	}
	return r
}

func readColourEncoding(br *bitio.Reader) (ColourEncoding, error) {
	c := defaultColourEncoding()
	if br.Bool() {
		return c, nil
	}
	var err error
	c.WantICC = br.Bool()
	if c.ColourSpace, err = br.Enum(); err != nil {
		return c, err
	}
	if c.ColourSpace > 3 {
		return c, invalid("colour space")
	}
	if c.WantICC {
		return c, nil
	}
	if c.ColourSpace != CSXYB {
		if c.WhitePoint, err = br.Enum(); err != nil {
			return c, err
		}
		switch c.WhitePoint {
		case WPD65, WPE, WPDCI:
		case WPCustom:
			c.White = readCustomXY(br)
		default:
			return c, invalid("white point")
		}
		if c.ColourSpace != CSGrey {
			if c.Primaries, err = br.Enum(); err != nil {
				return c, err
			}
			switch c.Primaries {
			case PrimSRGB, Prim2100, PrimP3:
			case PrimCustom:
				c.Red = readCustomXY(br)
				c.Green = readCustomXY(br)
				c.Blue = readCustomXY(br)
			default:
				return c, invalid("primaries")
			}
		}
	}
	if br.Bool() {
		c.TransferFunction = TFGamma
		c.Gamma = float64(br.Read(24)) / 1e7
		if c.Gamma <= 0 || c.Gamma > 1 {
			return c, invalid("gamma")
		}
	} else {
		if c.TransferFunction, err = br.Enum(); err != nil {
			return c, err
		}
		switch c.TransferFunction {
		case TF709, TFUnknown, TFLinear, TFSRGB, TFPQ, TFDCI, TFHLG:
		default:
			return c, invalid("transfer function")
		}
	}
	if c.RenderingIntent, err = br.Enum(); err != nil {
		return c, err
	}
	if c.RenderingIntent > 3 {
		return c, invalid("rendering intent")
	}
	return c, nil
}

// ToneMapping metadata.
type ToneMapping struct {
	IntensityTarget      float32
	MinNits              float32
	RelativeToMaxDisplay bool
	LinearBelow          float32
}

func readToneMapping(br *bitio.Reader) (ToneMapping, error) {
	t := ToneMapping{IntensityTarget: 255}
	if br.Bool() {
		return t, nil
	}
	var err error
	if t.IntensityTarget, err = br.F16(); err != nil {
		return t, err
	}
	if t.MinNits, err = br.F16(); err != nil {
		return t, err
	}
	t.RelativeToMaxDisplay = br.Bool()
	if t.LinearBelow, err = br.F16(); err != nil {
		return t, err
	}
	if t.IntensityTarget <= 0 || t.MinNits < 0 || t.MinNits > t.IntensityTarget {
		return t, invalid("tone mapping")
	}
	return t, nil
}

func skipExtensions(br *bitio.Reader) error {
	ext := br.U64()
	var total uint64
	for i := 0; i < 64; i++ {
		if ext&(1<<i) != 0 {
			sz := br.U64()
			total += sz
			if total > 1<<40 {
				return invalid("extensions too large")
			}
		}
	}
	br.SkipBits(total)
	return nil
}

// AnimationHeader describes animation timing.
type AnimationHeader struct {
	TpsNumerator   uint32
	TpsDenominator uint32
	NumLoops       uint32
	HaveTimecodes  bool
}

// OpsinInverse holds the XYB -> linear RGB parameters.
type OpsinInverse struct {
	Matrix             [9]float32
	OpsinBias          [3]float32
	QuantBias          [3]float32
	QuantBiasNumerator float32
}

func defaultOpsinInverse() OpsinInverse {
	return OpsinInverse{
		Matrix: [9]float32{
			11.031566901960783, -9.866943921568629, -0.16462299647058826,
			-3.254147380392157, 4.418770392156863, -0.16462299647058826,
			-3.6588512862745097, 2.7129230470588235, 1.9459282392156863,
		},
		OpsinBias: [3]float32{-0.0037930732552754493, -0.0037930732552754493, -0.0037930732552754493},
		QuantBias: [3]float32{
			1.0 - 0.05465007330715401,
			1.0 - 0.07005449891748593,
			1.0 - 0.049935103337343655,
		},
		QuantBiasNumerator: 0.145,
	}
}

// ImageHeader is the full image header (SizeHeader + ImageMetadata +
// CustomTransformData).
type ImageHeader struct {
	Size                SizeHeader
	Orientation         uint32
	HaveIntrinsicSize   bool
	IntrinsicSize       SizeHeader
	HavePreview         bool
	Preview             SizeHeader
	HaveAnimation       bool
	Animation           AnimationHeader
	BitDepth            BitDepth
	Modular16BitBuffers bool
	ExtraChannels       []ExtraChannelInfo
	XYBEncoded          bool
	Colour              ColourEncoding
	ToneMapping         ToneMapping
	Opsin               OpsinInverse
	Up2Weights          [15]float32
	Up4Weights          [55]float32
	Up8Weights          [210]float32
}

// AlphaChannel returns the index of the first alpha extra channel, or -1.
func (h *ImageHeader) AlphaChannel() int {
	for i, e := range h.ExtraChannels {
		if e.Type == ECAlpha {
			return i
		}
	}
	return -1
}

// IsGrey reports whether the image is greyscale.
func (h *ImageHeader) IsGrey() bool { return h.Colour.ColourSpace == CSGrey }

func readImageHeader(br *bitio.Reader) (*ImageHeader, error) {
	if br.Read(16) != 0x0aff {
		return nil, ErrNotJXL
	}
	h := &ImageHeader{
		Size:                readSizeHeader(br),
		Orientation:         1,
		BitDepth:            BitDepth{BitsPerSample: 8},
		Modular16BitBuffers: true,
		XYBEncoded:          true,
		Colour:              defaultColourEncoding(),
		ToneMapping:         ToneMapping{IntensityTarget: 255},
		Opsin:               defaultOpsinInverse(),
		Up2Weights:          defaultUp2,
		Up4Weights:          defaultUp4,
		Up8Weights:          defaultUp8,
	}
	var err error
	allDefault := br.Bool()
	if !allDefault {
		extra := br.Bool()
		if extra {
			h.Orientation = br.Read(3) + 1
			if h.HaveIntrinsicSize = br.Bool(); h.HaveIntrinsicSize {
				h.IntrinsicSize = readSizeHeader(br)
			}
			if h.HavePreview = br.Bool(); h.HavePreview {
				h.Preview = readPreviewHeader(br)
			}
			if h.HaveAnimation = br.Bool(); h.HaveAnimation {
				a := &h.Animation
				a.TpsNumerator = br.U32(bitio.Val(100), bitio.Val(1000), bitio.BitsOff(10, 1), bitio.BitsOff(30, 1))
				a.TpsDenominator = br.U32(bitio.Val(1), bitio.Val(1001), bitio.BitsOff(8, 1), bitio.BitsOff(10, 1))
				a.NumLoops = br.U32(bitio.Val(0), bitio.Bits(3), bitio.Bits(16), bitio.Bits(32))
				a.HaveTimecodes = br.Bool()
			}
		}
		if h.BitDepth, err = readBitDepth(br); err != nil {
			return nil, err
		}
		h.Modular16BitBuffers = br.Bool()
		nec := br.U32(bitio.Val(0), bitio.Val(1), bitio.BitsOff(4, 2), bitio.BitsOff(12, 1))
		if nec > 256 {
			return nil, invalid("too many extra channels")
		}
		h.ExtraChannels = make([]ExtraChannelInfo, nec)
		for i := range h.ExtraChannels {
			if h.ExtraChannels[i], err = readExtraChannelInfo(br); err != nil {
				return nil, err
			}
		}
		h.XYBEncoded = br.Bool()
		if h.Colour, err = readColourEncoding(br); err != nil {
			return nil, err
		}
		if extra {
			if h.ToneMapping, err = readToneMapping(br); err != nil {
				return nil, err
			}
		}
		if err = skipExtensions(br); err != nil {
			return nil, err
		}
	}
	defaultM := br.Bool()
	if !defaultM {
		if h.XYBEncoded {
			if !br.Bool() { // opsin all_default
				o := &h.Opsin
				for i := range o.Matrix {
					if o.Matrix[i], err = br.F16(); err != nil {
						return nil, err
					}
				}
				for i := range o.OpsinBias {
					if o.OpsinBias[i], err = br.F16(); err != nil {
						return nil, err
					}
				}
				for i := range o.QuantBias {
					if o.QuantBias[i], err = br.F16(); err != nil {
						return nil, err
					}
				}
				if o.QuantBiasNumerator, err = br.F16(); err != nil {
					return nil, err
				}
			}
		}
		cw := br.Read(3)
		if cw&1 != 0 {
			for i := range h.Up2Weights {
				if h.Up2Weights[i], err = br.F16(); err != nil {
					return nil, err
				}
			}
		}
		if cw&2 != 0 {
			for i := range h.Up4Weights {
				if h.Up4Weights[i], err = br.F16(); err != nil {
					return nil, err
				}
			}
		}
		if cw&4 != 0 {
			for i := range h.Up8Weights {
				if h.Up8Weights[i], err = br.F16(); err != nil {
					return nil, err
				}
			}
		}
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	if h.Size.Width == 0 || h.Size.Height == 0 {
		return nil, invalid("zero image size")
	}
	return h, nil
}

// OrientedSize returns the displayed size after applying orientation.
func (h *ImageHeader) OrientedSize() (int, int) {
	if h.Orientation > 4 {
		return int(h.Size.Height), int(h.Size.Width)
	}
	return int(h.Size.Width), int(h.Size.Height)
}
