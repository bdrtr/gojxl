package jxl

// Patch blend modes (also used for frame blending).
const (
	pbNone       = 0
	pbReplace    = 1
	pbAdd        = 2
	pbMul        = 3
	pbBlendAbove = 4
	pbBlendBelow = 5
	pbAWAddAbove = 6
	pbAWAddBelow = 7
)

type patchBlend struct {
	mode  uint8
	alpha int
	clamp bool
}

func frameBlendToPatch(b BlendingInfo) patchBlend {
	m := uint8(pbReplace)
	switch b.Mode {
	case BlendAdd:
		m = pbAdd
	case BlendBlend:
		m = pbBlendAbove
	case BlendMulAdd:
		m = pbAWAddAbove
	case BlendMul:
		m = pbMul
	}
	return patchBlend{mode: m, alpha: int(b.AlphaChannel), clamp: b.Clamp}
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func alphaBlendSingle(bg, bga, fg, fga, out []float32, n int, prem, clamp bool, sameAlpha bool) {
	if sameAlpha {
		for x := 0; x < n; x++ {
			fa := fga[x]
			if clamp {
				fa = clamp01(fa)
			}
			out[x] = 1 - (1-fa)*(1-bga[x])
		}
		return
	}
	if prem {
		for x := 0; x < n; x++ {
			fa := fga[x]
			if clamp {
				fa = clamp01(fa)
			}
			out[x] = fg[x] + bg[x]*(1-fa)
		}
		return
	}
	for x := 0; x < n; x++ {
		fa := fga[x]
		if clamp {
			fa = clamp01(fa)
		}
		na := 1 - (1-fa)*(1-bga[x])
		var r float32
		if na > 0 {
			r = 1 / na
		}
		out[x] = (fg[x]*fa + bg[x]*bga[x]*(1-fa)) * r
	}
}

func alphaWeightedAdd(bg, fg, fga, out []float32, n int, clamp, same bool) {
	if same {
		copy(out[:n], bg[:n])
		return
	}
	for x := 0; x < n; x++ {
		a := fga[x]
		if clamp {
			a = clamp01(a)
		}
		out[x] = bg[x] + fg[x]*a
	}
}

func mulBlend(bg, fg, out []float32, n int, clamp bool) {
	for x := 0; x < n; x++ {
		f := fg[x]
		if clamp {
			f = clamp01(f)
		}
		out[x] = bg[x] * f
	}
}

// performBlending blends fg over bg (rows already offset to the segment)
// and writes the result to out. out may alias bg. tmp must hold 3+nec
// rows of at least n floats.
func performBlending(bg, fg, out [][]float32, n int, color patchBlend, ec []patchBlend, info []ExtraChannelInfo, tmp [][]float32) {
	nec := len(info)
	hasAlpha := false
	for _, e := range info {
		if e.Type == ECAlpha {
			hasAlpha = true
			break
		}
	}
	for i := 0; i < nec; i++ {
		b := ec[i]
		o := tmp[3+i][:n]
		switch b.mode {
		case pbAdd:
			for x := 0; x < n; x++ {
				o[x] = bg[3+i][x] + fg[3+i][x]
			}
		case pbBlendAbove:
			a := b.alpha
			alphaBlendSingle(bg[3+i], bg[3+a], fg[3+i], fg[3+a], o, n, info[a].AlphaAssociated, b.clamp, a == i)
		case pbBlendBelow:
			a := b.alpha
			alphaBlendSingle(fg[3+i], fg[3+a], bg[3+i], bg[3+a], o, n, info[a].AlphaAssociated, b.clamp, a == i)
		case pbAWAddAbove:
			a := b.alpha
			alphaWeightedAdd(bg[3+i], fg[3+i], fg[3+a], o, n, b.clamp, a == i)
		case pbAWAddBelow:
			a := b.alpha
			alphaWeightedAdd(fg[3+i], bg[3+i], bg[3+a], o, n, b.clamp, a == i)
		case pbMul:
			mulBlend(bg[3+i], fg[3+i], o, n, b.clamp)
		case pbReplace:
			copy(o, fg[3+i][:n])
		default:
			copy(o, bg[3+i][:n])
		}
	}
	a := color.alpha
	add := func() {
		for c := 0; c < 3; c++ {
			for x := 0; x < n; x++ {
				tmp[c][x] = bg[c][x] + fg[c][x]
			}
		}
	}
	blendWeighted := func(bottom, top [][]float32) {
		prem := info[a].AlphaAssociated
		ta, ba := top[3+a], bottom[3+a]
		for x := 0; x < n; x++ {
			fa := ta[x]
			if color.clamp {
				fa = clamp01(fa)
			}
			na := 1 - (1-fa)*(1-ba[x])
			if prem {
				for c := 0; c < 3; c++ {
					tmp[c][x] = top[c][x] + bottom[c][x]*(1-fa)
				}
			} else {
				var r float32
				if na > 0 {
					r = 1 / na
				}
				for c := 0; c < 3; c++ {
					tmp[c][x] = (top[c][x]*fa + bottom[c][x]*ba[x]*(1-fa)) * r
				}
			}
			tmp[3+a][x] = na
		}
	}
	addWeighted := func(bottom, top [][]float32) {
		for c := 0; c < 3; c++ {
			alphaWeightedAdd(bottom[c], top[c], top[3+a], tmp[c], n, color.clamp, false)
		}
	}
	cp := func(src [][]float32) {
		for c := 0; c < 3; c++ {
			copy(tmp[c][:n], src[c][:n])
		}
	}
	switch color.mode {
	case pbAdd:
		add()
	case pbAWAddAbove:
		if hasAlpha {
			addWeighted(bg, fg)
		} else {
			add()
		}
	case pbAWAddBelow:
		if hasAlpha {
			addWeighted(fg, bg)
		} else {
			add()
		}
	case pbBlendAbove:
		if hasAlpha {
			blendWeighted(bg, fg)
		} else {
			cp(fg)
		}
	case pbBlendBelow:
		if hasAlpha {
			blendWeighted(fg, bg)
		} else {
			cp(fg)
		}
	case pbMul:
		for c := 0; c < 3; c++ {
			mulBlend(bg[c], fg[c], tmp[c], n, color.clamp)
		}
	case pbReplace:
		cp(fg)
	default:
		cp(bg)
	}
	for i := 0; i < 3+nec; i++ {
		copy(out[i][:n], tmp[i][:n])
	}
}

// blend composites the frame onto a new canvas.
func (d *Decoder) blend(fd *frameDecoder) error {
	ih, fh := d.Header, fd.fh
	W, H := int(ih.Size.Width), int(ih.Size.Height)
	nec := len(ih.ExtraChannels)
	if fd.color[0] == nil {
		return invalid("frame without colour")
	}
	// Fast path: a full-canvas frame replacing everything needs no background.
	full := fh.X0 == 0 && fh.Y0 == 0 && fd.color[0].W == W && fd.color[0].H == H
	replaceAll := fh.Blending.Mode == BlendReplace
	for i := range fh.ECBlending {
		replaceAll = replaceAll && fh.ECBlending[i].Mode == BlendReplace
	}
	if full && replaceAll && len(fd.ec) == nec {
		ok := true
		for _, p := range fd.ec {
			ok = ok && p.W == W && p.H == H
		}
		if ok {
			canvas := &refFrame{color: fd.color, ec: append([]*Plane(nil), fd.ec...)}
			d.canvas = canvas
			return nil
		}
	}
	// background planes (copies of the source reference frames)
	planes := make([]*Plane, 3+nec)
	for c := 0; c < 3+nec; c++ {
		src := fh.Blending.Source
		if c >= 3 {
			src = fh.ECBlending[c-3].Source
		}
		var bg *Plane
		if r := d.refs[src]; r != nil {
			if c < 3 {
				bg = r.color[c]
			} else if c-3 < len(r.ec) {
				bg = r.ec[c-3]
			}
		}
		p := newPlane(W, H)
		if bg != nil {
			if bg.W == W && bg.H == H {
				copy(p.Pix, bg.Pix)
			} else {
				for y := 0; y < min(H, bg.H); y++ {
					copy(p.Row(y), bg.Row(y)[:min(W, bg.W)])
				}
			}
		}
		planes[c] = p
	}
	fg := make([]*Plane, 3+nec)
	copy(fg, fd.color[:])
	copy(fg[3:], fd.ec)
	fw, fhh := fg[0].W, fg[0].H
	x0, y0 := int(fh.X0), int(fh.Y0)
	xs, xe := max(0, x0), min(W, x0+fw)
	ys, ye := max(0, y0), min(H, y0+fhh)
	cb := frameBlendToPatch(fh.Blending)
	ecb := make([]patchBlend, nec)
	for i := range ecb {
		ecb[i] = frameBlendToPatch(fh.ECBlending[i])
		if int(fh.ECBlending[i].AlphaChannel) >= nec && (ecb[i].mode == pbBlendAbove || ecb[i].mode == pbAWAddAbove) {
			return invalid("blend alpha channel")
		}
	}
	if (cb.mode == pbBlendAbove || cb.mode == pbAWAddAbove) && nec > 0 && cb.alpha >= nec {
		return invalid("blend alpha channel")
	}
	if xs < xe && ys < ye {
		n := xe - xs
		_ = parallelFor(ye-ys, func(i int) error {
			y := ys + i
			fy := y - y0
			bgRow := make([][]float32, 3+nec)
			fgRow := make([][]float32, 3+nec)
			tmp := make([][]float32, 3+nec)
			for c := range planes {
				bgRow[c] = planes[c].Row(y)[xs:xe]
				fgRow[c] = fg[c].Row(fy)[xs-x0 : xe-x0]
				tmp[c] = make([]float32, n)
			}
			performBlending(bgRow, fgRow, bgRow, n, cb, ecb, ih.ExtraChannels, tmp)
			return nil
		})
	}
	canvas := &refFrame{ec: make([]*Plane, nec)}
	canvas.color = [3]*Plane{planes[0], planes[1], planes[2]}
	copy(canvas.ec, planes[3:])
	d.canvas = canvas
	return nil
}
