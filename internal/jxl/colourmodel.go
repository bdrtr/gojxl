package jxl

import (
	"encoding/binary"
	"math"
)

// Colour descriptions for the encoder: how input samples map to the linear
// sRGB that XYB is defined on, and the enum encoding equivalent to an ICC
// profile.

// ColourModel maps the samples of a colour space to linear sRGB: Curves
// decode each channel (encoded value in [0,1] to linear light) and M, row
// major, converts linear RGB to linear sRGB (D65). A grey model maps the
// sample v to (Y, Y, Y), Y = Curves[0](v), and M is the identity.
type ColourModel struct {
	Grey   bool
	Curves [3]func(float64) float64
	M      [9]float64
	// HLG: the curves give scene light; the display light XYB holds is
	// that times OOTFScale * Y^(OOTFGamma-1), Y its luminance (weights
	// OOTFLum). OOTFGamma 0: no OOTF.
	OOTFGamma, OOTFScale float64
	OOTFLum              [3]float64
}

// ModelFromEncodingHDR returns the model of a PQ or HLG enum encoding for
// XYB, which holds absolute light (1 = 255 nits): PQ samples decode to
// nits / 255, HLG samples to scene light that the OOTF for the intensity
// target turns into display light. nil for other encodings.
func ModelFromEncodingHDR(c *ColourEncoding, intensityTarget float32) *ColourModel {
	if c.WantICC || c.ColourSpace > CSGrey || c.TransferFunction != TFPQ && c.TransferFunction != TFHLG {
		return nil
	}
	lin := *c
	lin.TransferFunction = TFLinear
	m := ModelFromEncoding(&lin)
	if m == nil {
		return nil
	}
	var f func(float64) float64
	if c.TransferFunction == TFPQ {
		f = func(v float64) float64 { return pqEOTF(v) / 255 }
	} else {
		f = hlgInverseOETF
		it := float64(intensityTarget)
		m.OOTFGamma = 1.2 * math.Pow(1.111, math.Log2(it/1000))
		m.OOTFScale = it / 255
		m.OOTFLum = [3]float64{0, 1, 0}
		if !m.Grey {
			w, p := c.whiteXY(), c.primariesXY()
			t := rgbToXYZ(p[0], p[1], p[2], w)
			m.OOTFLum = [3]float64{t[3], t[4], t[5]}
		}
	}
	m.Curves = [3]func(float64) float64{f, f, f}
	return m
}

var (
	identity3 = mat3{1, 0, 0, 0, 1, 0, 0, 0, 1}
	srgbToXYZ = rgbToXYZ(srgbP[0], srgbP[1], srgbP[2], xyD65)
)

// ModelFromICC returns the model of a matrix/TRC RGB or grey ICC profile,
// the inverse of the conversion the decoder renders XYB with; nil for
// other profiles.
func ModelFromICC(icc []byte) *ColourModel {
	p := parseICC(icc)
	if p == nil {
		return nil
	}
	m := &ColourModel{Grey: p.grey, Curves: p.trc, M: identity3}
	if !p.grey {
		toD50 := adapt(xyToXYZ(xyD65), xyToXYZ(xyD50)).mul(srgbToXYZ)
		m.M = toD50.inv().mul(p.m)
	}
	return m
}

// IsSRGB reports whether c is plain sRGB (or grey with the sRGB curve).
func (c *ColourEncoding) IsSRGB() bool { return c.isSRGB() }

// ModelFromEncoding returns the model of an enum colour encoding, the
// inverse of the conversion the decoder renders XYB with; nil for PQ, HLG
// and unknown transfer functions or colour spaces.
func ModelFromEncoding(c *ColourEncoding) *ColourModel {
	if c.WantICC || c.ColourSpace > CSGrey {
		return nil
	}
	switch c.TransferFunction {
	case TFPQ, TFHLG, TFUnknown:
		return nil
	}
	f := c.eotf()
	m := &ColourModel{Grey: c.ColourSpace == CSGrey, Curves: [3]func(float64) float64{f, f, f}, M: identity3}
	if !m.Grey {
		w, p := c.whiteXY(), c.primariesXY()
		toTgt := adapt(xyToXYZ(xyD65), xyToXYZ(w)).mul(srgbToXYZ)
		m.M = toTgt.inv().mul(rgbToXYZ(p[0], p[1], p[2], w))
	}
	return m
}

// EncodingFromICC returns the enum colour encoding a matrix/TRC (RGB or
// grey) ICC profile describes, as libjxl's encoder stores such profiles,
// or nil if the profile has no exact enum equivalent.
func EncodingFromICC(icc []byte) *ColourEncoding {
	p := parseICC(icc)
	if p == nil {
		return nil
	}
	tags := iccTags(icc)
	// the profile's white point and the adaptation of its colourants to
	// the D50 connection space: from 'chad' (v4, most v2), else Bradford
	// from 'wtpt' (v2 media white), else none (a D50 space)
	d50 := xyToXYZ(xyD50)
	white := d50
	toD50 := identity3
	if t := tags["chad"]; len(t) >= 44 && string(t[:4]) == "sf32" {
		for i := range toD50 {
			toD50[i] = rdS15(t[8+4*i:])
		}
		white = toD50.inv().apply(d50)
	} else if t := tags["wtpt"]; len(t) >= 20 && string(t[:4]) == "XYZ " {
		w := [3]float64{rdS15(t[8:]), rdS15(t[12:]), rdS15(t[16:])}
		if w[1] > 0 && (math.Abs(w[0]/w[1]-d50[0]) > 1e-3 || math.Abs(w[2]/w[1]-d50[2]) > 1e-3) {
			white = [3]float64{w[0] / w[1], 1, w[2] / w[1]}
			toD50 = adapt(white, d50)
		}
	}
	if !(white[0] > 0 && white[2] > 0) {
		return nil
	}
	c := &ColourEncoding{ColourSpace: CSRGB, RenderingIntent: 1}
	if p.grey {
		c.ColourSpace = CSGrey
	}
	if ri := binary.BigEndian.Uint32(icc[64:]); ri <= 3 {
		c.RenderingIntent = ri
	}
	c.SetWhite(xyzToXY(white))
	if !p.grey {
		// colourants in the profile's own white
		m := toD50.inv().mul(p.m)
		var xy [3][2]float64
		for i := range xy {
			xy[i] = xyzToXY([3]float64{m[i], m[3+i], m[6+i]})
		}
		c.SetPrimaries(xy)
		// one curve for all channels
		for i := 0; i <= 1024; i++ {
			x, y := float64(i)/1024, srgbOETF(p.trc[0](float64(i)/1024))
			if math.Abs(srgbOETF(p.trc[1](x))-y) > 0.1/255 || math.Abs(srgbOETF(p.trc[2](x))-y) > 0.1/255 {
				return nil
			}
		}
	}
	if !matchTransfer(c, p.trc[0]) {
		return nil
	}
	// the enum must describe the same conversion as the profile, within
	// the rounding of ICC colourant tags: one 8-bit step after sRGB
	// encoding (libjxl accepts about as much)
	a, b := ModelFromEncoding(c), ModelFromICC(icc)
	if a == nil || b == nil {
		return nil
	}
	const n = 16
	for i := 0; i <= n; i++ {
		for j := 0; j <= n; j++ {
			for k := 0; k <= n; k++ {
				if p.grey && (j != i || k != i) {
					continue
				}
				v := [3]float64{float64(i) / n, float64(j) / n, float64(k) / n}
				la, lb := a.linear(v), b.linear(v)
				for ch := range la {
					if math.Abs(srgbOETF(la[ch])-srgbOETF(lb[ch])) > 1.0/255 {
						return nil
					}
				}
			}
		}
	}
	return c
}

// srgbOETF encodes linear light with the sRGB curve (odd extension).
func srgbOETF(v float64) float64 {
	if v < 0 {
		return -srgbOETF(-v)
	}
	if v <= 0.0031308 {
		return 12.92 * v
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

// SetWhite sets the white point: the enum one within the precision of ICC
// tags, else custom.
func (c *ColourEncoding) SetWhite(xy [2]float64) {
	c.WhitePoint, c.White = WPCustom, roundXY(xy)
	for _, k := range []struct {
		code uint32
		xy   [2]float64
	}{{WPD65, xyD65}, {WPDCI, [2]float64{0.314, 0.351}}, {WPE, [2]float64{1.0 / 3, 1.0 / 3}}} {
		if nearXY(xy, k.xy) {
			c.WhitePoint, c.White = k.code, [2]float64{}
		}
	}
}

// SetPrimaries sets the red, green and blue primaries: the enum ones
// within the precision of ICC tags, else custom.
func (c *ColourEncoding) SetPrimaries(xy [3][2]float64) {
	c.Primaries = PrimCustom
	c.Red, c.Green, c.Blue = roundXY(xy[0]), roundXY(xy[1]), roundXY(xy[2])
	for _, k := range []struct {
		code uint32
		xy   [3][2]float64
	}{{PrimSRGB, srgbP}, {PrimP3, primP3}, {Prim2100, prim2100}} {
		if nearXY(xy[0], k.xy[0]) && nearXY(xy[1], k.xy[1]) && nearXY(xy[2], k.xy[2]) {
			c.Primaries = k.code
			c.Red, c.Green, c.Blue = [2]float64{}, [2]float64{}, [2]float64{}
		}
	}
}

// EncodingFromCICP returns the colour encoding of H.273 code points (as in
// PNG cICP chunks) for full-range RGB, or nil if it has no enum equivalent.
func EncodingFromCICP(primaries, transfer, matrix, fullRange byte) *ColourEncoding {
	if matrix != 0 || fullRange != 1 {
		return nil
	}
	c := &ColourEncoding{ColourSpace: CSRGB, WhitePoint: WPD65, RenderingIntent: 1}
	switch primaries {
	case 1:
		c.Primaries = PrimSRGB
	case 9:
		c.Primaries = Prim2100
	case 11:
		c.Primaries, c.WhitePoint = PrimP3, WPDCI
	case 12:
		c.Primaries = PrimP3
	default:
		return nil
	}
	switch transfer {
	case 1, 6, 14, 15:
		c.TransferFunction = TF709
	case 8:
		c.TransferFunction = TFLinear
	case 13:
		c.TransferFunction = TFSRGB
	case 16:
		c.TransferFunction = TFPQ
	case 17:
		c.TransferFunction = TFDCI
	case 18:
		c.TransferFunction = TFHLG
	default:
		return nil
	}
	return c
}

// linear converts an encoded sample triple to linear sRGB.
func (m *ColourModel) linear(v [3]float64) [3]float64 {
	if m.Grey {
		y := m.Curves[0](v[0])
		return [3]float64{y, y, y}
	}
	return mat3(m.M).apply([3]float64{m.Curves[0](v[0]), m.Curves[1](v[1]), m.Curves[2](v[2])})
}

var (
	primP3   = [3][2]float64{{0.680, 0.320}, {0.265, 0.690}, {0.150, 0.060}}
	prim2100 = [3][2]float64{{0.708, 0.292}, {0.170, 0.797}, {0.131, 0.046}}
)

func xyzToXY(v [3]float64) [2]float64 {
	s := v[0] + v[1] + v[2]
	if s == 0 {
		return [2]float64{}
	}
	return [2]float64{v[0] / s, v[1] / s}
}

// nearXY reports whether two chromaticities agree within the precision of
// ICC colourant tags.
func nearXY(a, b [2]float64) bool {
	return math.Abs(a[0]-b[0]) < 5e-4 && math.Abs(a[1]-b[1]) < 5e-4
}

// roundXY rounds to the 1e-6 resolution of the header's custom xy fields.
func roundXY(v [2]float64) [2]float64 {
	return [2]float64{math.Round(v[0]*1e6) / 1e6, math.Round(v[1]*1e6) / 1e6}
}

// matchTransfer sets c's transfer function to the enum one (or gamma) the
// curve f follows, within a tenth of an 8-bit step after sRGB encoding,
// and reports whether there is one.
func matchTransfer(c *ColourEncoding, f func(float64) float64) bool {
	close := func(g func(float64) float64) bool {
		for i := 0; i <= 1024; i++ {
			x := float64(i) / 1024
			if math.Abs(srgbOETF(f(x))-srgbOETF(g(x))) > 0.1/255 {
				return false
			}
		}
		return true
	}
	for _, tf := range []uint32{TFSRGB, TFLinear, TF709, TFDCI} {
		t := ColourEncoding{TransferFunction: tf}
		if close(t.eotf()) {
			c.TransferFunction = tf
			return true
		}
	}
	// a pure power curve: its exponent from the midpoint; the header
	// stores the inverse exponent in units of 1e-7 (at most 1)
	y := f(0.5)
	if !(y > 0 && y < 1) {
		return false
	}
	g := math.Log(y) / math.Log(0.5)
	if g < 1 {
		return false
	}
	inv := math.Round(1e7/g) / 1e7
	t := ColourEncoding{TransferFunction: TFGamma, Gamma: inv}
	if !close(t.eotf()) {
		return false
	}
	c.TransferFunction, c.Gamma = TFGamma, inv
	return true
}
