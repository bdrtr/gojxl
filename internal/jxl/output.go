package jxl

import (
	"math"
	"sync"
)

// Fused output conversion. When a frame is still in XYB (deferred colour
// transform), rows are converted on the fly while writing the output.

type xybConv struct {
	m        [9]float32
	bias     [3]float32
	cbrtBias [3]float32
	lut      *tfLUT
	f        func(float32) float32
	linear   bool
	hlgExp   float32
	lum      [3]float32

	tf8Once sync.Once
	tf8     *tf8 // 8-bit output table (built on first use)
}

func newXYBConv(ih *ImageHeader, t *outputTarget) *xybConv {
	o := &ih.Opsin
	c := &xybConv{}
	scale := float64(255.0) / float64(ih.ToneMapping.IntensityTarget)
	var om mat3
	for i := range om {
		om[i] = float64(o.Matrix[i]) * scale
	}
	if t != nil && !t.srgb {
		om = t.m.mul(om)
	}
	for i := range c.m {
		c.m[i] = float32(om[i])
	}
	for i := 0; i < 3; i++ {
		c.bias[i] = o.OpsinBias[i]
		c.cbrtBias[i] = float32(math.Cbrt(float64(o.OpsinBias[i])))
	}
	if t != nil && !t.srgb {
		c.f = t.enc
		c.hlgExp, c.lum = t.hlgExp, t.lum
	} else {
		tf := xybOutputTF(ih)
		if tf == TFLinear {
			c.linear = true
			return c
		}
		c.f = tfFunc(tf, ih.Colour.Gamma, ih.ToneMapping.IntensityTarget)
	}
	c.lut = buildTFLUT(c.f)
	return c
}

// row converts XYB rows to output RGB rows (in place allowed).
func (c *xybConv) row(x, y, b, r, g, bo []float32) {
	n := len(r)
	x, y, b, g, bo = x[:n], y[:n], b[:n], g[:n], bo[:n]
	xybRow(c, x, y, b, r, g, bo)
	if c.hlgExp != 0 {
		e := float64(c.hlgExp)
		for i := range r {
			l := c.lum[0]*r[i] + c.lum[1]*g[i] + c.lum[2]*bo[i]
			if l <= 0 {
				r[i], g[i], bo[i] = 0, 0, 0
				continue
			}
			s := float32(math.Min(math.Pow(float64(l), e), 1e9))
			r[i] *= s
			g[i] *= s
			bo[i] *= s
		}
	}
	if !c.linear {
		c.lut.apply(r, c.f)
		c.lut.apply(g, c.f)
		c.lut.apply(bo, c.f)
	}
}

// RGBA8Row writes line yy as 8-bit RGBA into dst (4*Width bytes), alpha
// from a (nil: opaque). tmp must hold 3*Width floats. Deferred XYB frames
// go from linear values to 8-bit codes with one table lookup per sample.
func (f *Frame) RGBA8Row(yy int, tmp, a []float32, dst []byte) {
	w := f.Width
	dst = dst[:4*w]
	c := f.conv
	if c != nil && !f.grey && !c.linear && c.hlgExp == 0 {
		c.tf8Once.Do(func() { c.tf8 = buildTF8(c.f) })
	}
	if c != nil && c.tf8 != nil && !f.grey && !c.linear && c.hlgExp == 0 {
		xybTo8(c, f.Color[0].Row(yy)[:w], f.Color[1].Row(yy)[:w], f.Color[2].Row(yy)[:w], dst)
	} else {
		r, g, b := f.RGBRow(yy, tmp)
		r, g, b = r[:w], g[:w], b[:w]
		for x := range r {
			o := dst[4*x : 4*x+3 : 4*x+3]
			o[0], o[1], o[2] = q8(r[x]), q8(g[x]), q8(b[x])
		}
	}
	if a != nil {
		a = a[:w]
		for x, v := range a {
			dst[4*x+3] = q8(v)
		}
	} else {
		for x := 3; x < len(dst); x += 4 {
			dst[x] = 255
		}
	}
}

// q8 quantises a [0, 1] sample to 8 bits.
func q8(v float32) uint8 {
	if !(v > 0) {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(v*255 + 0.5)
}

// tf8 maps linear values to 8-bit output codes q8(f(v)) with one lookup:
// cell k of [k/tf8Size, (k+1)/tf8Size) holds the code at its start and the
// value from which the code is one higher (+Inf if none); cell tf8Size
// holds the code of values >= 1. Values below slowBelow (cells where the
// code steps more than once, near zero for power-law curves) are computed
// directly.
type tf8 struct {
	thr       []float32 // tf8Mask+1 entries (cells past tf8Size unused)
	code      []uint8
	slowBelow float32
	f         func(float32) float32
}

const (
	tf8Size = 4096
	tf8Mask = 2*tf8Size - 1
)

// buildTF8 returns nil when values >= 1 do not all map to 255 (then the
// float path is used).
func buildTF8(f func(float32) float32) *tf8 {
	code := func(v float32) uint8 { return q8(f(v)) }
	if code(1) != 255 {
		return nil
	}
	t := &tf8{thr: make([]float32, tf8Mask+1), code: make([]uint8, tf8Mask+1), f: f}
	inf := float32(math.Inf(1))
	for k := range t.thr {
		t.code[k], t.thr[k] = 255, inf
	}
	for k := 0; k < tf8Size; k++ {
		lo, hi := float32(k)/tf8Size, float32(k+1)/tf8Size
		c0, c1 := code(lo), code(hi)
		t.code[k] = c0
		switch {
		case c1 == c0:
		case c1 == c0+1 && k > 0:
			a, b := lo, hi // code(a) = c0, code(b) = c1
			for {
				m := (a + b) / 2
				if m <= a || m >= b {
					break
				}
				if code(m) > c0 {
					b = m
				} else {
					a = m
				}
			}
			t.thr[k] = b
		default:
			t.slowBelow = hi
		}
	}
	return t
}

// tf8Index is the cell of v >= 0 (tf8Size for v >= 1; also when the
// conversion overflows, which gives a negative int). Cheaper than a float
// min, which must handle NaN and signed zeros.
func tf8Index(v float32) int {
	k := int(v * tf8Size)
	if uint(k) > tf8Size {
		k = tf8Size
	}
	return k & tf8Mask
}

// get returns q8(f(v)).
func (t *tf8) get(v float32) uint8 {
	if v >= t.slowBelow {
		k := tf8Index(v)
		return t.code[k] + uint8(^math.Float32bits(v-t.thr[k])>>31)
	}
	if !(v > 0) {
		return 0
	}
	return q8(t.f(v))
}

// xybTo8 converts XYB rows to 8-bit RGB (dst holds RGBA, alpha untouched).
func xybTo8(c *xybConv, x, y, b []float32, dst []byte) {
	t := c.tf8
	thr, code := t.thr[:tf8Mask+1], t.code[:tf8Mask+1]
	sb := t.slowBelow
	m0, m1, m2, m3, m4, m5, m6, m7, m8 := c.m[0], c.m[1], c.m[2], c.m[3], c.m[4], c.m[5], c.m[6], c.m[7], c.m[8]
	cb0, cb1, cb2 := c.cbrtBias[0], c.cbrtBias[1], c.cbrtBias[2]
	b0, b1, b2 := c.bias[0], c.bias[1], c.bias[2]
	y, b = y[:len(x)], b[:len(x)]
	dst = dst[:4*len(x)]
	for i, X := range x {
		Y, B := y[i], b[i]
		gr := Y + X - cb0
		gg := Y - X - cb1
		gb := B - cb2
		mr := gr*gr*gr + b0
		mg := gg*gg*gg + b1
		mb := gb*gb*gb + b2
		vr := m0*mr + m1*mg + m2*mb
		vg := m3*mr + m4*mg + m5*mb
		vb := m6*mr + m7*mg + m8*mb
		o := dst[4*i : 4*i+3 : 4*i+3]
		if vr >= sb && vg >= sb && vb >= sb {
			// +1 when v >= thr: the sign bit of v-thr (exact for close
			// values, -Inf for thr = +Inf) is clear
			kr, kg, kb := tf8Index(vr), tf8Index(vg), tf8Index(vb)
			o[0] = code[kr] + uint8(^math.Float32bits(vr-thr[kr])>>31)
			o[1] = code[kg] + uint8(^math.Float32bits(vg-thr[kg])>>31)
			o[2] = code[kb] + uint8(^math.Float32bits(vb-thr[kb])>>31)
		} else {
			o[0], o[1], o[2] = t.get(vr), t.get(vg), t.get(vb)
		}
	}
}

// RGBRow returns the output-space colour rows for line yy. tmp must hold
// 3*Width floats; it is used when conversion is needed.
func (f *Frame) RGBRow(yy int, tmp []float32) (r, g, b []float32) {
	w := f.Width
	pr, pg, pb := f.Color[0].Row(yy), f.Color[1].Row(yy), f.Color[2].Row(yy)
	if f.conv == nil {
		return pr, pg, pb
	}
	r, g, b = tmp[:w], tmp[w:2*w], tmp[2*w:3*w]
	f.conv.row(pr, pg, pb, r, g, b)
	if f.grey {
		return g, g, g
	}
	return r, g, b
}

// ParallelRows runs fn over row ranges in parallel.
func ParallelRows(h int, fn func(y0, y1 int)) {
	const band = 16
	n := (h + band - 1) / band
	_ = parallelFor(n, func(i int) error {
		fn(i*band, min(h, (i+1)*band))
		return nil
	})
}

// Materialize converts a deferred-XYB frame to output-space planes.
func (f *Frame) Materialize() {
	if f.conv == nil {
		return
	}
	w := f.Width
	ParallelRows(f.Height, func(y0, y1 int) {
		tmp := make([]float32, 3*w)
		for y := y0; y < y1; y++ {
			r, g, b := f.RGBRow(y, tmp)
			copy(f.Color[0].Row(y), r)
			copy(f.Color[1].Row(y), g)
			copy(f.Color[2].Row(y), b)
		}
	})
	if f.grey {
		f.Color[1], f.Color[2] = f.Color[0], f.Color[0]
	}
	f.conv = nil
}

// xybOutputTF selects the transfer function for XYB images. The inverse
// XYB transform yields linear sRGB primaries: when the image's declared
// encoding shares them (D65, sRGB primaries or grey) its transfer function
// is used, as libjxl does; otherwise output falls back to sRGB so that the
// untagged pixels are not mislabelled (wide gamut is clipped).
func xybOutputTF(ih *ImageHeader) uint32 {
	c := &ih.Colour
	if c.WantICC || c.ColourSpace == CSXYB || c.WhitePoint != WPD65 {
		return TFSRGB
	}
	if c.ColourSpace != CSGrey && c.Primaries != PrimSRGB {
		return TFSRGB
	}
	return c.TransferFunction
}
