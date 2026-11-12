package jxl

import "math"

// Conversion of decoded samples to sRGB (Decoder.ForceSRGB): XYB images
// are rendered to sRGB directly; other images are converted after decoding
// through their colour model (matrix/TRC ICC profiles and enum encodings)
// or the A2B0 table of a LUT-based RGB profile. PQ and HLG images (which
// need tone mapping) and unknown spaces are left as they are.

// srgbConv converts samples of the image's colour space to sRGB.
type srgbConv struct {
	grey  bool
	dec   [3]*tfLUT // encoded -> linear per channel
	decF  [3]func(float32) float32
	m     [9]float32 // linear device RGB -> linear sRGB
	pipe  *lutPipe   // LUT profiles: device -> PCS XYZ (D50)
	toRGB mat3       // XYZ D50 -> linear sRGB
	enc   *tfLUT     // linear -> sRGB
	encF  func(float32) float32
	// PQ / HLG samples: decoded to light relative to the intensity
	// target, converted with m, then tone mapped
	hdr      *hdrToSDR
	hdrTF    uint32
	it       float64
	hlgGamma float64    // HLG system gamma for the intensity target
	lumSrc   [3]float64 // luminances of the source primaries
}

// SRGBConverter returns the conversion of the image's (non-XYB) samples to
// sRGB, or nil when there is none to do (sRGB images, XYB and CMYK images,
// which render to sRGB anyway) or none possible (PQ, HLG, unknown).
func (d *Decoder) SRGBConverter() *srgbConv {
	if !d.srgbDone {
		d.srgb = newSRGBConv(d)
		d.srgbDone = true
	}
	return d.srgb
}

func newSRGBConv(d *Decoder) *srgbConv {
	ih := d.Header
	c := &ih.Colour
	if ih.XYBEncoded || d.CMYK() != nil {
		return nil
	}
	encF := tfFunc(TFSRGB, 0, ih.ToneMapping.IntensityTarget)
	sc := &srgbConv{encF: encF, enc: buildTFLUT(encF)}
	var model *ColourModel
	switch {
	case c.WantICC:
		if model = ModelFromICC(d.ICC); model == nil {
			// a LUT-based RGB profile
			if len(d.ICC) < 132 || string(d.ICC[16:20]) != "RGB " {
				return nil
			}
			pipe := parseLUT(iccTags(d.ICC)["A2B0"], string(d.ICC[20:24]) == "Lab ")
			if pipe == nil || pipe.inCh != 3 {
				return nil
			}
			sc.pipe = pipe
			sc.toRGB = adapt(xyToXYZ(xyD65), xyToXYZ(xyD50)).mul(srgbToXYZ).inv()
			return sc
		}
	case c.isSRGB():
		return nil
	case c.ColourSpace <= CSGrey && (c.TransferFunction == TFPQ || c.TransferFunction == TFHLG):
		lin := *c
		lin.TransferFunction = TFLinear
		lm := ModelFromEncoding(&lin)
		if lm == nil {
			return nil
		}
		it := ih.ToneMapping.IntensityTarget
		sc.hdrTF, sc.it, sc.grey = c.TransferFunction, float64(it), lm.Grey
		for i, v := range lm.M {
			sc.m[i] = float32(v)
		}
		sc.hlgGamma = 1.2 * math.Pow(1.111, math.Log2(float64(it)/1000))
		// libjxl linearises and tone maps in the image's primaries (then
		// converts), weighing luminance as in the output space (sRGB)
		for i := range sc.lumSrc {
			sc.lumSrc[i] = srgbToXYZ[3+i]
		}
		if lm.Grey {
			sc.lumSrc = [3]float64{0, 1, 0}
		}
		sc.hdr = newHDRToSDR(it, c.TransferFunction == TFHLG, &sc.lumSrc)
		return sc
	default:
		if model = ModelFromEncoding(c); model == nil {
			return nil
		}
	}
	sc.grey = model.Grey
	for i, f := range model.Curves {
		f := f
		sc.decF[i] = func(v float32) float32 { return float32(f(float64(v))) }
		sc.dec[i] = buildTFLUT(sc.decF[i])
	}
	for i, v := range model.M {
		sc.m[i] = float32(v)
	}
	return sc
}

// hdrRow converts rows of PQ or HLG samples to SDR sRGB in place (g and b
// may alias r for grey).
func (sc *srgbConv) hdrRow(r, g, b []float32) {
	grey := &g[0] == &r[0]
	for x := range r {
		v := [3]float64{float64(r[x]), float64(g[x]), float64(b[x])}
		for i := range v {
			if sc.hdrTF == TFPQ {
				v[i] = pqEOTF(v[i]) / sc.it
			} else {
				v[i] = hlgInverseOETF(v[i])
			}
		}
		if sc.hdrTF == TFHLG {
			// OOTF: scene light to display light (peak 1 = intensity target)
			y := sc.lumSrc[0]*v[0] + sc.lumSrc[1]*v[1] + sc.lumSrc[2]*v[2]
			if grey {
				y = v[0]
			}
			k := 0.0
			if y > 0 {
				k = math.Pow(y, sc.hlgGamma-1)
			}
			for i := range v {
				v[i] *= k
			}
		}
		if grey {
			v[1], v[2] = v[0], v[0]
		}
		sc.hdr.mapLight(&v)
		if !grey {
			m := &sc.m
			v = [3]float64{
				float64(m[0])*v[0] + float64(m[1])*v[1] + float64(m[2])*v[2],
				float64(m[3])*v[0] + float64(m[4])*v[1] + float64(m[5])*v[2],
				float64(m[6])*v[0] + float64(m[7])*v[1] + float64(m[8])*v[2],
			}
		}
		r[x], g[x], b[x] = float32(v[0]), float32(v[1]), float32(v[2])
	}
	if grey {
		sc.hdr.encodeRows(r, r[:0], r[:0])
		return
	}
	sc.hdr.encodeRows(r, g, b)
}

// hlgInverseOETF maps the HLG signal to scene light in [0, 1].
func hlgInverseOETF(v float64) float64 {
	if v <= 0.5 {
		return v * v / 3
	}
	const a, b, c = 0.17883277, 0.28466892, 0.55991073
	return (math.Exp((v-c)/a) + b) / 12
}

// apply converts colour planes (one for grey images) in place.
func (sc *srgbConv) apply(col [3]*Plane) {
	p0 := col[0]
	w, h := p0.W, p0.H
	if sc.hdr != nil {
		ParallelRows(h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				if sc.grey || col[1] == nil {
					r := p0.Row(y)[:w]
					sc.hdrRow(r, r, r)
				} else {
					sc.hdrRow(col[0].Row(y)[:w], col[1].Row(y)[:w], col[2].Row(y)[:w])
				}
			}
		})
		return
	}
	if sc.grey || col[1] == nil {
		ParallelRows(h, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				r := p0.Row(y)[:w]
				sc.dec[0].apply(r, sc.decF[0])
				sc.enc.apply(r, sc.encF)
			}
		})
		return
	}
	ParallelRows(h, func(y0, y1 int) {
		var in [3]float64
		for y := y0; y < y1; y++ {
			r, g, b := col[0].Row(y)[:w], col[1].Row(y)[:w], col[2].Row(y)[:w]
			if sc.pipe != nil {
				for x := range r {
					in[0], in[1], in[2] = float64(r[x]), float64(g[x]), float64(b[x])
					lin := sc.toRGB.apply(sc.pipe.eval(in[:], nil))
					r[x], g[x], b[x] = float32(lin[0]), float32(lin[1]), float32(lin[2])
				}
			} else {
				sc.dec[0].apply(r, sc.decF[0])
				sc.dec[1].apply(g, sc.decF[1])
				sc.dec[2].apply(b, sc.decF[2])
				m := &sc.m
				for x := range r {
					lr, lg, lb := r[x], g[x], b[x]
					r[x] = m[0]*lr + m[1]*lg + m[2]*lb
					g[x] = m[3]*lr + m[4]*lg + m[5]*lb
					b[x] = m[6]*lr + m[7]*lg + m[8]*lb
				}
			}
			// out of gamut: clipped
			for _, row := range [3][]float32{r, g, b} {
				for x, v := range row {
					row[x] = min(max(v, 0), 1)
				}
				sc.enc.apply(row, sc.encF)
			}
		}
	})
}
