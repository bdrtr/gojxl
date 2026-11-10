package jxl

import (
	"bytes"
	"encoding/binary"
	"math"
)

// Colour management: primaries/white point maths, ICC synthesis and
// parsing of matrix/TRC profiles.

type mat3 [9]float64

func (a mat3) mul(b mat3) mat3 {
	var r mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				r[i*3+j] += a[i*3+k] * b[k*3+j]
			}
		}
	}
	return r
}

func (a mat3) inv() mat3 {
	det := a[0]*(a[4]*a[8]-a[5]*a[7]) - a[1]*(a[3]*a[8]-a[5]*a[6]) + a[2]*(a[3]*a[7]-a[4]*a[6])
	id := 1 / det
	return mat3{
		(a[4]*a[8] - a[5]*a[7]) * id, (a[2]*a[7] - a[1]*a[8]) * id, (a[1]*a[5] - a[2]*a[4]) * id,
		(a[5]*a[6] - a[3]*a[8]) * id, (a[0]*a[8] - a[2]*a[6]) * id, (a[2]*a[3] - a[0]*a[5]) * id,
		(a[3]*a[7] - a[4]*a[6]) * id, (a[1]*a[6] - a[0]*a[7]) * id, (a[0]*a[4] - a[1]*a[3]) * id,
	}
}

func (a mat3) apply(v [3]float64) [3]float64 {
	return [3]float64{a[0]*v[0] + a[1]*v[1] + a[2]*v[2], a[3]*v[0] + a[4]*v[1] + a[5]*v[2], a[6]*v[0] + a[7]*v[1] + a[8]*v[2]}
}

func xyToXYZ(xy [2]float64) [3]float64 {
	return [3]float64{xy[0] / xy[1], 1, (1 - xy[0] - xy[1]) / xy[1]}
}

// rgbToXYZ returns the matrix converting linear RGB to XYZ for primaries.
func rgbToXYZ(r, g, b, w [2]float64) mat3 {
	R, G, B := xyToXYZ(r), xyToXYZ(g), xyToXYZ(b)
	m := mat3{R[0], G[0], B[0], R[1], G[1], B[1], R[2], G[2], B[2]}
	s := m.inv().apply(xyToXYZ(w))
	return mat3{R[0] * s[0], G[0] * s[1], B[0] * s[2], R[1] * s[0], G[1] * s[1], B[1] * s[2], R[2] * s[0], G[2] * s[1], B[2] * s[2]}
}

var bradford = mat3{0.8951, 0.2664, -0.1614, -0.7502, 1.7135, 0.0367, 0.0389, -0.0685, 1.0296}

// adapt returns a Bradford chromatic adaptation matrix from white ws to wd.
func adapt(ws, wd [3]float64) mat3 {
	a, b := bradford.apply(ws), bradford.apply(wd)
	d := mat3{b[0] / a[0], 0, 0, 0, b[1] / a[1], 0, 0, 0, b[2] / a[2]}
	return bradford.inv().mul(d).mul(bradford)
}

var (
	xyD65 = [2]float64{0.3127, 0.3290}
	xyD50 = [2]float64{0.3457, 0.3585}
	srgbP = [3][2]float64{{0.64, 0.33}, {0.30, 0.60}, {0.15, 0.06}}
)

func (c *ColourEncoding) whiteXY() [2]float64 {
	switch c.WhitePoint {
	case WPCustom:
		return c.White
	case WPE:
		return [2]float64{1.0 / 3, 1.0 / 3}
	case WPDCI:
		return [2]float64{0.314, 0.351}
	}
	return xyD65
}

func (c *ColourEncoding) primariesXY() [3][2]float64 {
	switch c.Primaries {
	case PrimCustom:
		return [3][2]float64{c.Red, c.Green, c.Blue}
	case Prim2100:
		return [3][2]float64{{0.708, 0.292}, {0.170, 0.797}, {0.131, 0.046}}
	case PrimP3:
		return [3][2]float64{{0.680, 0.320}, {0.265, 0.690}, {0.150, 0.060}}
	}
	return srgbP
}

// isSRGB reports whether an enum encoding is plain sRGB (or sRGB grey).
func (c *ColourEncoding) isSRGB() bool {
	if c.WantICC || c.ColourSpace == CSXYB || c.ColourSpace == CSUnknown {
		return false
	}
	if c.WhitePoint != WPD65 || c.TransferFunction != TFSRGB {
		return false
	}
	return c.ColourSpace == CSGrey || c.Primaries == PrimSRGB
}

// eotf returns the decoding curve (encoded -> linear, relative) of a TF.
func (c *ColourEncoding) eotf() func(float64) float64 {
	switch c.TransferFunction {
	case TFLinear:
		return func(v float64) float64 { return v }
	case TFGamma:
		g := 1 / c.Gamma
		return func(v float64) float64 { return math.Pow(math.Max(v, 0), g) }
	case TF709:
		return func(v float64) float64 {
			if v < 0.081 {
				return v / 4.5
			}
			return math.Pow((v+0.099)/1.099, 1/0.45)
		}
	case TFPQ:
		return func(v float64) float64 {
			p := math.Pow(math.Max(v, 0), 1/78.84375)
			return math.Pow(math.Max(p-0.8359375, 0)/(18.8515625-18.6875*p), 1/0.1593017578125)
		}
	case TFHLG:
		return func(v float64) float64 {
			if v <= 0.5 {
				return v * v / 3
			}
			const a, b, cc = 0.17883277, 0.28466892, 0.55991073
			return (math.Exp((v-cc)/a) + b) / 12
		}
	case TFDCI:
		return func(v float64) float64 { return math.Pow(math.Max(v, 0), 2.6) }
	}
	return func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
}

// ---------------------------------------------------------------- ICC writer

type iccTag struct {
	sig  string
	data []byte
}

func s15(v float64) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(math.Round(v*65536))))
	return b
}

func xyzTag(v [3]float64) []byte {
	b := []byte("XYZ \x00\x00\x00\x00")
	for _, c := range v {
		b = append(b, s15(c)...)
	}
	return b
}

func mlucTag(text string) []byte {
	var b bytes.Buffer
	b.WriteString("mluc\x00\x00\x00\x00")
	binary.Write(&b, binary.BigEndian, uint32(1))
	binary.Write(&b, binary.BigEndian, uint32(12))
	b.WriteString("enUS")
	binary.Write(&b, binary.BigEndian, uint32(len(text)*2))
	binary.Write(&b, binary.BigEndian, uint32(28))
	for _, r := range text {
		binary.Write(&b, binary.BigEndian, uint16(r))
	}
	return b.Bytes()
}

// trcTag encodes a transfer curve as 'para' when possible, else 'curv'.
func trcTag(c *ColourEncoding) []byte {
	para := func(typ uint16, ps ...float64) []byte {
		b := []byte("para\x00\x00\x00\x00")
		b = binary.BigEndian.AppendUint16(b, typ)
		b = append(b, 0, 0)
		for _, p := range ps {
			b = append(b, s15(p)...)
		}
		return b
	}
	switch c.TransferFunction {
	case TFLinear:
		return para(0, 1)
	case TFGamma:
		return para(0, 1/c.Gamma)
	case TFSRGB:
		return para(3, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045)
	case TF709:
		return para(3, 1/0.45, 1/1.099, 0.099/1.099, 1/4.5, 0.081)
	}
	f := c.eotf()
	const n = 4096
	b := []byte("curv\x00\x00\x00\x00")
	b = binary.BigEndian.AppendUint32(b, n)
	for i := 0; i < n; i++ {
		v := f(float64(i) / (n - 1))
		b = binary.BigEndian.AppendUint16(b, uint16(math.Round(math.Min(math.Max(v, 0), 1)*65535)))
	}
	return b
}

func buildICC(colorSpace string, tags []iccTag) []byte {
	hdr := make([]byte, 128)
	copy(hdr[4:], "gjxl")
	hdr[8], hdr[9] = 4, 0x30 // v4.3
	copy(hdr[12:], "mntr")
	copy(hdr[16:], colorSpace)
	copy(hdr[20:], "XYZ ")
	copy(hdr[36:], "acsp")
	binary.BigEndian.PutUint32(hdr[64:], 1) // relative colorimetric
	copy(hdr[68:], s15(0.9642))
	copy(hdr[72:], s15(1.0))
	copy(hdr[76:], s15(0.8249))
	off := 128 + 4 + 12*len(tags)
	table := binary.BigEndian.AppendUint32(nil, uint32(len(tags)))
	var data []byte
	for _, t := range tags {
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
		table = append(table, t.sig...)
		table = binary.BigEndian.AppendUint32(table, uint32(off+len(data)))
		table = binary.BigEndian.AppendUint32(table, uint32(len(t.data)))
		data = append(data, t.data...)
	}
	out := append(append(hdr, table...), data...)
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	return out
}

// SynthesizeICC builds an ICC v4 profile describing an enum encoding.
func SynthesizeICC(c *ColourEncoding) []byte {
	wXYZ := xyToXYZ(c.whiteXY())
	toD50 := adapt(wXYZ, xyToXYZ(xyD50))
	chad := []byte("sf32\x00\x00\x00\x00")
	for _, v := range toD50 {
		chad = append(chad, s15(v)...)
	}
	trc := trcTag(c)
	tags := []iccTag{
		{"desc", mlucTag("gojxl " + c.describe())},
		{"cprt", mlucTag("CC0")},
		{"wtpt", xyzTag([3]float64{0.9642, 1, 0.8249})},
		{"chad", chad},
	}
	if c.ColourSpace == CSGrey {
		tags = append(tags, iccTag{"kTRC", trc})
		return buildICC("GRAY", tags)
	}
	p := c.primariesXY()
	m := toD50.mul(rgbToXYZ(p[0], p[1], p[2], c.whiteXY()))
	tags = append(tags,
		iccTag{"rXYZ", xyzTag([3]float64{m[0], m[3], m[6]})},
		iccTag{"gXYZ", xyzTag([3]float64{m[1], m[4], m[7]})},
		iccTag{"bXYZ", xyzTag([3]float64{m[2], m[5], m[8]})},
		iccTag{"rTRC", trc}, iccTag{"gTRC", trc}, iccTag{"bTRC", trc})
	return buildICC("RGB ", tags)
}

func (c *ColourEncoding) describe() string {
	tf := map[uint32]string{TF709: "709", TFLinear: "linear", TFSRGB: "sRGB", TFPQ: "PQ", TFHLG: "HLG", TFDCI: "DCI", TFGamma: "gamma"}[c.TransferFunction]
	pr := map[uint32]string{PrimSRGB: "sRGB", Prim2100: "Rec2100", PrimP3: "P3", PrimCustom: "custom"}[c.Primaries]
	if c.ColourSpace == CSGrey {
		pr = "grey"
	}
	return pr + " " + tf
}

// ---------------------------------------------------------------- ICC reader

// iccProfile is a parsed matrix/TRC (or grey TRC) profile.
type iccProfile struct {
	grey  bool
	m     mat3 // linear device RGB -> XYZ (D50 PCS)
	trc   [3]func(float64) float64
	space string
}

func iccTags(icc []byte) map[string][]byte {
	if len(icc) < 132 {
		return nil
	}
	n := int(binary.BigEndian.Uint32(icc[128:]))
	tags := map[string][]byte{}
	for i := 0; i < n && 132+12*i+12 <= len(icc); i++ {
		e := icc[132+12*i:]
		off, sz := int(binary.BigEndian.Uint32(e[4:])), int(binary.BigEndian.Uint32(e[8:]))
		if off+sz <= len(icc) && off >= 0 && sz >= 0 {
			tags[string(e[:4])] = icc[off : off+sz]
		}
	}
	return tags
}

func rdS15(b []byte) float64 { return float64(int32(binary.BigEndian.Uint32(b))) / 65536 }

func parseCurve(b []byte) func(float64) float64 {
	if len(b) < 12 {
		return nil
	}
	switch string(b[:4]) {
	case "curv":
		n := int(binary.BigEndian.Uint32(b[8:]))
		if n == 0 {
			return func(v float64) float64 { return v }
		}
		if len(b) < 12+2*n {
			return nil
		}
		if n == 1 {
			g := float64(binary.BigEndian.Uint16(b[12:])) / 256
			return func(v float64) float64 { return math.Pow(math.Max(v, 0), g) }
		}
		t := make([]float64, n)
		for i := range t {
			t[i] = float64(binary.BigEndian.Uint16(b[12+2*i:])) / 65535
		}
		return func(v float64) float64 {
			x := math.Min(math.Max(v, 0), 1) * float64(n-1)
			i := int(x)
			if i >= n-1 {
				return t[n-1]
			}
			f := x - float64(i)
			return t[i]*(1-f) + t[i+1]*f
		}
	case "para":
		typ := binary.BigEndian.Uint16(b[8:])
		cnt := map[uint16]int{0: 1, 1: 3, 2: 4, 3: 5, 4: 7}[typ]
		if cnt == 0 || len(b) < 12+4*cnt {
			return nil
		}
		p := make([]float64, 7)
		for i := 0; i < cnt; i++ {
			p[i] = rdS15(b[12+4*i:])
		}
		g, a, bb, c, d, e, f := p[0], p[1], p[2], p[3], p[4], p[5], p[6]
		pw := func(x float64) float64 { return math.Pow(math.Max(x, 0), g) }
		switch typ {
		case 0:
			return pw
		case 1:
			return func(x float64) float64 {
				if x >= -bb/a {
					return pw(a*x + bb)
				}
				return 0
			}
		case 2:
			return func(x float64) float64 {
				if x >= -bb/a {
					return pw(a*x+bb) + c
				}
				return c
			}
		case 3:
			return func(x float64) float64 {
				if x >= d {
					return pw(a*x + bb)
				}
				return c * x
			}
		default:
			return func(x float64) float64 {
				if x >= d {
					return pw(a*x+bb) + e
				}
				return c*x + f
			}
		}
	}
	return nil
}

// parseICC extracts a matrix/TRC model from an ICC profile, if possible.
func parseICC(icc []byte) *iccProfile {
	if len(icc) < 132 {
		return nil
	}
	space := string(icc[16:20])
	tags := iccTags(icc)
	p := &iccProfile{space: space}
	if space == "GRAY" {
		f := parseCurve(tags["kTRC"])
		if f == nil {
			return nil
		}
		p.grey = true
		p.trc = [3]func(float64) float64{f, f, f}
		return p
	}
	if space != "RGB " {
		return nil
	}
	cols := [3]string{"rXYZ", "gXYZ", "bXYZ"}
	for i, s := range cols {
		t := tags[s]
		if len(t) < 20 {
			return nil
		}
		p.m[i] = rdS15(t[8:])
		p.m[3+i] = rdS15(t[12:])
		p.m[6+i] = rdS15(t[16:])
		p.trc[i] = parseCurve(tags[[3]string{"rTRC", "gTRC", "bTRC"}[i]])
		if p.trc[i] == nil {
			return nil
		}
	}
	return p
}

// invertCurve builds an approximate inverse of a monotone curve on [0,1].
func invertCurve(f func(float64) float64) func(float32) float32 {
	const n = 4096
	fwd := make([]float64, n+1)
	for i := range fwd {
		fwd[i] = f(float64(i) / n)
	}
	return func(v float32) float32 {
		y := float64(v)
		if y <= fwd[0] {
			return 0
		}
		if y >= fwd[n] {
			return 1
		}
		lo, hi := 0, n
		for hi-lo > 1 {
			m := (lo + hi) / 2
			if fwd[m] <= y {
				lo = m
			} else {
				hi = m
			}
		}
		d := fwd[hi] - fwd[lo]
		fr := 0.0
		if d > 0 {
			fr = (y - fwd[lo]) / d
		}
		return float32((float64(lo) + fr) / n)
	}
}

// outputTarget describes the space XYB frames are converted to.
type outputTarget struct {
	srgb bool
	m    mat3 // linear sRGB -> target linear RGB
	grey bool
	enc  func(float32) float32 // linear -> encoded
	icc  []byte                // profile tagging the output (nil = sRGB)
	// HLG inverse OOTF (display -> scene light): rgb *= Y^hlgExp.
	hlgExp float32
	lum    [3]float32
}

// xybTarget chooses the output space for an XYB image: its declared enum
// space, or its ICC profile if that is a matrix/TRC profile; sRGB otherwise.
func xybTarget(ih *ImageHeader, icc []byte) *outputTarget {
	c := &ih.Colour
	srgbToXYZ := rgbToXYZ(srgbP[0], srgbP[1], srgbP[2], xyD65)
	if c.WantICC {
		p := parseICC(icc)
		if p == nil {
			return &outputTarget{srgb: true}
		}
		toD50 := adapt(xyToXYZ(xyD65), xyToXYZ(xyD50)).mul(srgbToXYZ)
		t := &outputTarget{icc: icc, grey: p.grey}
		if p.grey {
			t.m = mat3{toD50[3], toD50[4], toD50[5], toD50[3], toD50[4], toD50[5], toD50[3], toD50[4], toD50[5]}
		} else {
			t.m = p.m.inv().mul(toD50)
		}
		t.enc = invertCurve(p.trc[0])
		return t
	}
	if c.isSRGB() || c.ColourSpace == CSXYB || c.ColourSpace == CSUnknown {
		return &outputTarget{srgb: true}
	}
	t := &outputTarget{icc: SynthesizeICC(c), grey: c.ColourSpace == CSGrey}
	wXYZ := xyToXYZ(c.whiteXY())
	toTgt := adapt(xyToXYZ(xyD65), wXYZ).mul(srgbToXYZ)
	if t.grey {
		t.m = mat3{toTgt[3], toTgt[4], toTgt[5], toTgt[3], toTgt[4], toTgt[5], toTgt[3], toTgt[4], toTgt[5]}
	} else {
		p := c.primariesXY()
		t.m = rgbToXYZ(p[0], p[1], p[2], c.whiteXY()).inv().mul(toTgt)
	}
	t.enc = tfFunc(c.TransferFunction, c.Gamma, ih.ToneMapping.IntensityTarget)
	if c.TransferFunction == TFHLG {
		// libjxl renders HLG display referred: undo the OOTF for the
		// image's peak luminance (BT.2100 system gamma).
		lw := float64(ih.ToneMapping.IntensityTarget)
		gamma := 1.2 * math.Pow(1.111, math.Log2(lw/1000))
		if e := 1/gamma - 1; math.Abs(e) > 0.01 {
			t.hlgExp = float32(e)
			if t.grey {
				t.lum = [3]float32{0, 1, 0}
			} else {
				p := c.primariesXY()
				m := rgbToXYZ(p[0], p[1], p[2], c.whiteXY())
				t.lum = [3]float32{float32(m[3]), float32(m[4]), float32(m[5])}
			}
		}
	}
	return t
}
