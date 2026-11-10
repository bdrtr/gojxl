package jxl

import "math"

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
