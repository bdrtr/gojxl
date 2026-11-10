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
}

func newXYBConv(ih *ImageHeader) *xybConv {
	o := &ih.Opsin
	c := &xybConv{}
	scale := float32(255.0) / ih.ToneMapping.IntensityTarget
	for i := range c.m {
		c.m[i] = o.Matrix[i] * scale
	}
	for i := 0; i < 3; i++ {
		c.bias[i] = o.OpsinBias[i]
		c.cbrtBias[i] = float32(math.Cbrt(float64(o.OpsinBias[i])))
	}
	tf := ih.Colour.TransferFunction
	if ih.Colour.WantICC || ih.Colour.ColourSpace == CSXYB {
		tf = TFSRGB
	}
	if tf == TFLinear {
		c.linear = true
		return c
	}
	c.f = tfFunc(tf, ih.Colour.Gamma, ih.ToneMapping.IntensityTarget)
	c.lut = buildTFLUT(c.f)
	return c
}

// row converts XYB rows to output RGB rows (in place allowed).
func (c *xybConv) row(x, y, b, r, g, bo []float32) {
	n := len(r)
	x, y, b, g, bo = x[:n], y[:n], b[:n], g[:n], bo[:n]
	m := &c.m
	for i := 0; i < n; i++ {
		X, Y, B := x[i], y[i], b[i]
		gr := Y + X - c.cbrtBias[0]
		gg := Y - X - c.cbrtBias[1]
		gb := B - c.cbrtBias[2]
		mr := gr*gr*gr + c.bias[0]
		mg := gg*gg*gg + c.bias[1]
		mb := gb*gb*gb + c.bias[2]
		r[i] = m[0]*mr + m[1]*mg + m[2]*mb
		g[i] = m[3]*mr + m[4]*mg + m[5]*mb
		bo[i] = m[6]*mr + m[7]*mg + m[8]*mb
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
