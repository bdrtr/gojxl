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
	// per channel curves (ICC profiles whose channels differ), else nil
	f3     [3]func(float32) float32
	lut3   [3]*tfLUT
	post   *hdrToSDR // tone mapping of linear sRGB to SDR sRGB
	linear bool
	hlgExp float32
	lum    [3]float32

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
	} else if ih.IsGrey() {
		// grey sRGB output is the luminance, as libjxl (srgb_to_luma rows
		// of kSRGBLuminances); the green channel alone differs by up to
		// ~3e-4 where lossy coding left X or B-Y non-zero
		const lr, lg, lb = 0.2126, 0.7152, 0.0722
		om = mat3{lr, lg, lb, lr, lg, lb, lr, lg, lb}.mul(om)
	}
	for i := range c.m {
		c.m[i] = float32(om[i])
	}
	for i := 0; i < 3; i++ {
		c.bias[i] = o.OpsinBias[i]
		c.cbrtBias[i] = float32(math.Cbrt(float64(o.OpsinBias[i])))
	}
	if t != nil && t.hdr != nil {
		c.post = t.hdr
		return c
	}
	zeroNeg := false
	if t != nil && !t.srgb {
		c.f, zeroNeg = t.enc, t.zeroNeg
		c.hlgExp, c.lum = t.hlgExp, t.lum
		if t.enc3[0] != nil {
			c.f3 = t.enc3
			for i, f := range t.enc3 {
				c.lut3[i] = buildTFLUT(f)
			}
		}
	} else {
		tf := xybOutputTF(ih)
		if tf == TFLinear {
			c.linear = true
			return c
		}
		c.f = tfFunc(tf, ih.Colour.Gamma, ih.ToneMapping.IntensityTarget)
		zeroNeg = tfZeroesNegatives(tf)
	}
	c.lut = buildTFLUT(c.f)
	if zeroNeg {
		c.lut.powerLaw()
	}
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
	switch {
	case c.post != nil:
		c.post.rows(r, g, bo)
	case c.linear:
	case c.lut3[0] != nil:
		c.lut3[0].apply(r, c.f3[0])
		c.lut3[1].apply(g, c.f3[1])
		c.lut3[2].apply(bo, c.f3[2])
	default:
		c.lut.apply(r, c.f)
		c.lut.apply(g, c.f)
		c.lut.apply(bo, c.f)
	}
}

// RGBA8Row writes line yy as 8-bit RGBA into dst (4*Width bytes), alpha
// from a (nil: opaque). tmp must hold 3*Width floats. Deferred XYB frames
// go from linear values to 8-bit codes with one table lookup per sample.
func (f *Frame) RGBA8Row(yy int, tmp, a []float32, dst []byte) {
	f.RGB8Row(yy, tmp, a, dst, 4)
}

// RGB8Row is RGBA8Row with stride bytes per pixel: 4 (RGBA, alpha from a
// or opaque) or 3 (RGB, a unused). tmp is only needed for frames whose
// colour is converted without the 8-bit table (it may be nil otherwise).
func (f *Frame) RGB8Row(yy int, tmp, a []float32, dst []byte, stride int) {
	w := f.Width
	dst = dst[:stride*w]
	c := f.conv
	if c != nil && !f.grey && !c.linear && c.hlgExp == 0 && c.lut3[0] == nil && c.post == nil {
		c.tf8Once.Do(func() { c.tf8 = buildTF8(c.f) })
	}
	if c != nil && c.tf8 != nil && !f.grey && !c.linear && c.hlgExp == 0 {
		xybTo8(c, f.Color[0].Row(yy)[:w], f.Color[1].Row(yy)[:w], f.Color[2].Row(yy)[:w], dst, stride)
	} else {
		if tmp == nil && c != nil {
			tmp = make([]float32, 3*w)
		}
		r, g, b := f.RGBRow(yy, tmp)
		r, g, b = r[:w], g[:w], b[:w]
		for x := range r {
			o := dst[stride*x : stride*x+3 : stride*x+3]
			o[0], o[1], o[2] = q8(r[x]), q8(g[x]), q8(b[x])
		}
	}
	if stride != 4 {
		return
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

// xybTo8 converts XYB rows to 8-bit RGB, stride bytes per pixel in dst
// (the fourth, alpha, byte of RGBA is left untouched). Chunks go through
// linear float rows first: one loop holding the matrix and one holding
// the table use fewer registers than a fused loop, which spills.
func xybTo8(c *xybConv, x, y, b []float32, dst []byte, stride int) {
	const chunk = 256
	var buf [3][chunk]float32
	y, b = y[:len(x)], b[:len(x)]
	dst = dst[:stride*len(x)]
	for i0 := 0; i0 < len(x); i0 += chunk {
		n := min(chunk, len(x)-i0)
		r, g, bo := buf[0][:n], buf[1][:n], buf[2][:n]
		xybRow(c, x[i0:i0+n], y[i0:i0+n], b[i0:i0+n], r, g, bo)
		o := dst[stride*i0:]
		tf8Row(c.tf8, r, o, stride)
		tf8Row(c.tf8, g, o[1:], stride)
		tf8Row(c.tf8, bo, o[2:], stride)
	}
}

// tf8Row writes t.get(v) for each v to dst[stride*i].
func tf8Row(t *tf8, v []float32, dst []byte, stride int) {
	thr, code := t.thr[:tf8Mask+1], t.code[:tf8Mask+1]
	sb := t.slowBelow
	if len(v) == 0 {
		return
	}
	dst = dst[:stride*(len(v)-1)+1]
	for i, v := range v {
		var q uint8
		if v >= sb {
			// +1 when v >= thr: the sign bit of v-thr (exact for close
			// values, -Inf for thr = +Inf) is clear
			k := tf8Index(v)
			q = code[k] + uint8(^math.Float32bits(v-thr[k])>>31)
		} else {
			q = t.get(v)
		}
		dst[stride*i] = q
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
