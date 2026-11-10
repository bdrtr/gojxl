package jxl

import (
	"math"
)

// render runs the post-decode pipeline for a frame and composites it onto the
// canvas. It returns whether the frame is displayed.
func (d *Decoder) render(fd *frameDecoder) (bool, *Frame, error) {
	ih, fh := d.Header, fd.fh

	// 1. chroma upsampling (YCbCr with subsampling)
	if fh.DoYCbCr && (fh.MaxHShift > 0 || fh.MaxVShift > 0) {
		for c := 0; c < 3; c++ {
			fd.color[c] = upsampleChroma(fd.color[c], fh.HShift[c], fh.VShift[c], fh.XSize, fh.YSize)
		}
	}
	// 2. restoration filters
	if fd.color[0] != nil && (fh.Filter.Gab || fh.Filter.EPFIters > 0) {
		// filters operate on three independent planes
		for c := 1; c < 3; c++ {
			if fd.color[c] == fd.color[0] {
				fd.color[c] = &Plane{W: fd.color[0].W, H: fd.color[0].H, Pix: append([]float32(nil), fd.color[0].Pix...)}
			}
		}
		if fh.Filter.Gab {
			st := startStage()
			fd.applyGaborish()
			st.done("gaborish")
		}
		if fh.Filter.EPFIters > 0 {
			st := startStage()
			fd.applyEPF()
			st.done("epf")
		}
	}
	// 3. patches and splines (at decoded resolution), then upsampling
	if err := fd.applyPatchesSplines(); err != nil {
		return false, nil, err
	}
	if fh.Upsampling > 1 && fd.color[0] != nil {
		for c := 0; c < 3; c++ {
			if c > 0 && fd.color[c] == fd.color[0] {
				continue
			}
			fd.color[c] = upsample(fd.color[c], int(fh.Upsampling), ih, int(fh.Width), int(fh.Height))
		}
		if fd.nbColor == 1 {
			fd.color[1], fd.color[2] = fd.color[0], fd.color[0]
		}
	}
	for i := range fd.ec {
		if up := int(fh.ECUpsampling[i]); up > 1 {
			fd.ec[i] = upsample(fd.ec[i], up, ih, int(fh.Width), int(fh.Height))
		}
	}
	// 4. noise
	if fd.hasNoise && fd.color[0] != nil {
		fd.renderNoise()
	}

	// LF frames are kept (in XYB) for later VarDCT frames.
	if fh.Type == FrameLF {
		d.lf[fh.LFLevel-1] = &refFrame{color: fd.color, ec: fd.ec}
		return false, nil, nil
	}

	canBeRef := !fh.IsLast && (fh.Duration == 0 || fh.SaveAsReference != 0)
	if canBeRef && fh.SaveBeforeCT {
		d.saveReference(fd, fh.SaveAsReference)
	}

	// 5. colour transform (deferred to the output stage when possible)
	deferCT := ih.XYBEncoded && fh.IsLast && !canBeRef && !hasSpot(ih) &&
		(fh.Type == FrameRegular || fh.Type == FrameSkipProgressive) &&
		fd.isFullReplace()
	st := startStage()
	if !deferCT {
		fd.colorTransform()
	}
	st.done("colour")

	// 6. blending onto the canvas
	if err := d.blend(fd); err != nil {
		return false, nil, err
	}
	if canBeRef && !fh.SaveBeforeCT {
		d.refs[fh.SaveAsReference] = d.canvas.clone()
	}

	shown := (fh.Type == FrameRegular || fh.Type == FrameSkipProgressive) && (fh.Duration != 0 || fh.IsLast)
	if !shown {
		return false, nil, nil
	}
	f := &Frame{
		Width: int(ih.Size.Width), Height: int(ih.Size.Height),
		Duration: fh.Duration, Name: fh.Name, IsLast: fh.IsLast,
	}
	cv := d.canvas
	if !fh.IsLast || hasSpot(ih) {
		cv = d.canvas.clone()
	}
	renderSpotColors(cv, ih)
	f.Color = cv.color
	f.EC = cv.ec
	if deferCT {
		f.conv = newXYBConv(ih)
		f.grey = ih.IsGrey()
	}
	return true, f, nil
}

func (r *refFrame) clone() *refFrame {
	n := &refFrame{}
	seen := map[*Plane]*Plane{}
	cp := func(p *Plane) *Plane {
		if p == nil {
			return nil
		}
		if q, ok := seen[p]; ok {
			return q
		}
		q := &Plane{W: p.W, H: p.H, Pix: append([]float32(nil), p.Pix...)}
		seen[p] = q
		return q
	}
	for c := 0; c < 3; c++ {
		n.color[c] = cp(r.color[c])
	}
	n.ec = make([]*Plane, len(r.ec))
	for i, p := range r.ec {
		n.ec[i] = cp(p)
	}
	return n
}

// saveReference stores the frame (before colour transform) as a reference,
// placed on a canvas-sized buffer.
func (d *Decoder) saveReference(fd *frameDecoder, slot uint32) {
	// For save_before_ct, the frame must cover the full canvas (spec) or is
	// a reference-only frame with its own size.
	r := &refFrame{}
	for c := 0; c < 3; c++ {
		if fd.color[c] != nil {
			r.color[c] = &Plane{W: fd.color[c].W, H: fd.color[c].H, Pix: append([]float32(nil), fd.color[c].Pix...)}
		}
	}
	r.ec = make([]*Plane, len(fd.ec))
	for i, p := range fd.ec {
		r.ec[i] = &Plane{W: p.W, H: p.H, Pix: append([]float32(nil), p.Pix...)}
	}
	d.refs[slot] = r
}

// colorTransform converts XYB / YCbCr frames to the output (RGB) space.
func (fd *frameDecoder) colorTransform() {
	ih, fh := fd.ih, fd.fh
	if fd.color[0] == nil {
		return
	}
	if ih.XYBEncoded {
		xybToLinearSRGB(fd.color, &ih.Opsin, ih.ToneMapping.IntensityTarget)
		// output in sRGB transfer (or linear if the image says so)
		tf := ih.Colour.TransferFunction
		if ih.Colour.WantICC || ih.Colour.ColourSpace == CSXYB {
			tf = TFSRGB
		}
		applyTransferFromLinear(fd.color, tf, ih.Colour.Gamma, ih.ToneMapping.IntensityTarget)
		if ih.IsGrey() {
			// keep luminance in channel 0 (Y of a grey image ends up equal in RGB)
			fd.color[1], fd.color[2] = fd.color[0], fd.color[0]
		}
	} else if fh.DoYCbCr {
		ycbcrToRGB(fd.color)
	}
}

// ycbcrToRGB converts (Cb, Y, Cr) planes (JPEG style, centred at 0) to RGB.
func ycbcrToRGB(p [3]*Plane) {
	cb, y, cr := p[0].Pix, p[1].Pix, p[2].Pix
	n := min(len(cb), len(y), len(cr))
	for i := 0; i < n; i++ {
		Y := y[i] + 128.0/255.0
		Cb, Cr := cb[i], cr[i]
		r := Y + 1.402*Cr
		g := Y - 0.344136*Cb - 0.714136*Cr
		b := Y + 1.772*Cb
		cb[i], y[i], cr[i] = r, g, b
	}
}

// xybToLinearSRGB converts XYB planes in place to linear sRGB.
func xybToLinearSRGB(p [3]*Plane, o *OpsinInverse, intensity float32) {
	var cbrtBias [3]float32
	for i := range cbrtBias {
		cbrtBias[i] = float32(math.Cbrt(float64(o.OpsinBias[i])))
	}
	scale := float32(255.0) / intensity
	var m [9]float32
	for i := range m {
		m[i] = o.Matrix[i] * scale
	}
	X, Y, B := p[0].Pix, p[1].Pix, p[2].Pix
	n := len(X)
	parallelRows(n, func(lo, hi int) {
		for i := lo; i < hi; i++ {
			x, y, b := X[i], Y[i], B[i]
			gr := y + x - cbrtBias[0]
			gg := y - x - cbrtBias[1]
			gb := b - cbrtBias[2]
			mr := gr*gr*gr + o.OpsinBias[0]
			mg := gg*gg*gg + o.OpsinBias[1]
			mb := gb*gb*gb + o.OpsinBias[2]
			X[i] = m[0]*mr + m[1]*mg + m[2]*mb
			Y[i] = m[3]*mr + m[4]*mg + m[5]*mb
			B[i] = m[6]*mr + m[7]*mg + m[8]*mb
		}
	})
}

// parallelRows splits [0,n) into chunks processed concurrently.
func parallelRows(n int, fn func(lo, hi int)) {
	const chunk = 1 << 15
	if n <= chunk {
		fn(0, n)
		return
	}
	k := (n + chunk - 1) / chunk
	_ = parallelFor(k, func(i int) error {
		lo := i * chunk
		hi := min(n, lo+chunk)
		fn(lo, hi)
		return nil
	})
}

func srgbFromLinear(v float32) float32 {
	if v <= 0.0031308 {
		return v * 12.92
	}
	return float32(1.055*math.Pow(float64(v), 1/2.4) - 0.055)
}

// tfFunc returns the encoding (from linear) function of a transfer function.
func tfFunc(tf uint32, gamma float64, intensity float32) func(float32) float32 {
	var f func(float32) float32
	switch tf {
	case TFLinear:
		return func(v float32) float32 { return v }
	case TFGamma:
		g := gamma
		f = func(v float32) float32 {
			if v <= 0 {
				return 0
			}
			return float32(math.Pow(float64(v), g))
		}
	case TF709:
		f = func(v float32) float32 {
			if v < 0.018 {
				return 4.5 * v
			}
			return float32(1.099*math.Pow(float64(v), 0.45) - 0.099)
		}
	case TFPQ:
		scale := float64(intensity) / 10000
		f = func(v float32) float32 {
			l := math.Max(0, float64(v)*scale)
			lp := math.Pow(l, 0.1593017578125)
			return float32(math.Pow((0.8359375+18.8515625*lp)/(1+18.6875*lp), 78.84375))
		}
	case TFHLG:
		f = func(v float32) float32 {
			e := math.Max(0, float64(v))
			if e <= 1.0/12 {
				return float32(math.Sqrt(3 * e))
			}
			const a, b, c = 0.17883277, 0.28466892, 0.55991073
			return float32(a*math.Log(12*e-b) + c)
		}
	case TFDCI:
		f = func(v float32) float32 {
			if v <= 0 {
				return 0
			}
			return float32(math.Pow(float64(v), 1/2.6))
		}
	default:
		f = srgbFromLinear
	}
	if tf == TFSRGB || f == nil {
		f = srgbFromLinear
	}
	return f
}

// applyTransferFromLinear encodes linear planes with the transfer function.
func applyTransferFromLinear(p [3]*Plane, tf uint32, gamma float64, intensity float32) {
	if tf == TFLinear {
		return
	}
	f := tfFunc(tf, gamma, intensity)

	lut := buildTFLUT(f)
	done := map[*Plane]bool{}
	for _, pl := range p {
		if pl == nil || done[pl] {
			continue
		}
		done[pl] = true
		px := pl.Pix
		parallelRows(len(px), func(lo, hi int) {
			lut.apply(px[lo:hi], f)
		})
	}
}

// tfLUT approximates a transfer function on [0,1] by linear interpolation.
type tfLUT struct {
	t []float32
}

const tfLUTSize = 16384

func buildTFLUT(f func(float32) float32) *tfLUT {
	t := make([]float32, tfLUTSize+2)
	for i := range t {
		t[i] = f(float32(i) / tfLUTSize)
	}
	return &tfLUT{t: t}
}

func (l *tfLUT) apply(px []float32, f func(float32) float32) {
	t := l.t
	for i, v := range px {
		if v >= 1.0/tfLUTSize && v < 1 {
			fv := v * tfLUTSize
			k := int(fv)
			fr := fv - float32(k)
			a := t[k]
			px[i] = a + (t[k+1]-a)*fr
		} else if v < 0 {
			px[i] = -f(-v)
		} else {
			px[i] = f(v)
		}
	}
}

// upsampleChroma upsamples a subsampled chroma plane by the given shifts
// using the JPEG "fancy" (linear 3:1) filter as libjxl does.
func upsampleChroma(p *Plane, hs, vs int, w, h int) *Plane {
	cur := p
	if hs > 0 {
		ow := cur.W * 2
		out := newPlane(ow, cur.H)
		for y := 0; y < cur.H; y++ {
			in := cur.Row(y)
			o := out.Row(y)
			for x := 0; x < cur.W; x++ {
				c := in[x] * 0.75
				l := in[max(x-1, 0)]
				r := in[min(x+1, cur.W-1)]
				o[2*x] = c + l*0.25
				o[2*x+1] = c + r*0.25
			}
		}
		cur = out
	}
	if vs > 0 {
		oh := cur.H * 2
		out := newPlane(cur.W, oh)
		for y := 0; y < cur.H; y++ {
			in := cur.Row(y)
			up := cur.Row(max(y-1, 0))
			dn := cur.Row(min(y+1, cur.H-1))
			o0 := out.Row(2 * y)
			o1 := out.Row(2*y + 1)
			for x := 0; x < cur.W; x++ {
				c := in[x] * 0.75
				o0[x] = c + up[x]*0.25
				o1[x] = c + dn[x]*0.25
			}
		}
		cur = out
	}
	return cropPlane(cur, w, h)
}

func cropPlane(p *Plane, w, h int) *Plane {
	if p.W == w && p.H == h {
		return p
	}
	out := newPlane(w, h)
	for y := 0; y < h && y < p.H; y++ {
		copy(out.Row(y), p.Row(y)[:min(w, p.W)])
	}
	return out
}

// renderSpotColors mixes spot colour channels into the colour planes.
func renderSpotColors(r *refFrame, ih *ImageHeader) {
	for i, e := range ih.ExtraChannels {
		if e.Type != ECSpotColour || i >= len(r.ec) || r.ec[i] == nil {
			continue
		}
		sp := r.ec[i].Pix
		scale := e.Spot[3]
		for c := 0; c < 3; c++ {
			px := r.color[c].Pix
			col := e.Spot[c]
			for k := range px {
				mix := scale * sp[k]
				px[k] = mix*col + (1-mix)*px[k]
			}
		}
	}
}

func hasSpot(ih *ImageHeader) bool {
	for _, e := range ih.ExtraChannels {
		if e.Type == ECSpotColour {
			return true
		}
	}
	return false
}

// isFullReplace reports whether the frame replaces the whole canvas.
func (fd *frameDecoder) isFullReplace() bool {
	fh, ih := fd.fh, fd.ih
	if fh.X0 != 0 || fh.Y0 != 0 || fh.Width != ih.Size.Width || fh.Height != ih.Size.Height {
		return false
	}
	if fh.Blending.Mode != BlendReplace {
		return false
	}
	for _, b := range fh.ECBlending {
		if b.Mode != BlendReplace {
			return false
		}
	}
	return true
}
