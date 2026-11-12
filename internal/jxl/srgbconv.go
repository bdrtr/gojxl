package jxl

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

// apply converts colour planes (one for grey images) in place.
func (sc *srgbConv) apply(col [3]*Plane) {
	p0 := col[0]
	w, h := p0.W, p0.H
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
