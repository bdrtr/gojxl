package jxl

import (
	"math"
)

// mirror reflects an out-of-range coordinate (libjxl Mirror).
func mirror(x, size int) int {
	for x < 0 || x >= size {
		if x < 0 {
			x = -x - 1
		} else {
			x = 2*size - 1 - x
		}
	}
	return x
}

func buildUpsampleKernel(weights []float32, shift int) []float32 {
	N := 1 << shift
	H := N / 2
	k := make([]float32, N*N*25)
	for ky := 0; ky < H; ky++ {
		for kx := 0; kx < H; kx++ {
			o0 := (ky*N + kx) * 25
			o1 := (ky*N + (N - 1 - kx)) * 25
			o2 := ((N-1-ky)*N + kx) * 25
			o3 := ((N-1-ky)*N + (N - 1 - kx)) * 25
			for py := 0; py < 5; py++ {
				for px := 0; px < 5; px++ {
					j := 5*ky + py
					i := 5*kx + px
					my, mx := min(i, j), max(i, j)
					w := weights[5*H*my-my*(my-1)/2+mx-my]
					k[o0+py*5+px] = w
					k[o1+py*5+(4-px)] = w
					k[o2+(4-py)*5+px] = w
					k[o3+(4-py)*5+(4-px)] = w
				}
			}
		}
	}
	return k
}

// upsample applies the non-separable 2x/4x/8x upsampler and crops to (w,h).
func upsample(p *Plane, factor int, ih *ImageHeader, w, h int) *Plane {
	var shift int
	var weights []float32
	switch factor {
	case 2:
		shift, weights = 1, ih.Up2Weights[:]
	case 4:
		shift, weights = 2, ih.Up4Weights[:]
	case 8:
		shift, weights = 3, ih.Up8Weights[:]
	default:
		return p
	}
	N := factor
	kern := buildUpsampleKernel(weights, shift)
	ow, oh := p.W*N, p.H*N
	out := newPlane(ow, oh)
	// padded input with mirrored border of 2
	pw := p.W + 4
	pad := make([]float32, pw*(p.H+4))
	for y := -2; y < p.H+2; y++ {
		src := p.Row(mirror(y, p.H))
		dst := pad[(y+2)*pw : (y+3)*pw]
		for x := -2; x < p.W+2; x++ {
			dst[x+2] = src[mirror(x, p.W)]
		}
	}
	_ = parallelFor(p.H, func(y int) error {
		var win [25]float32
		for x := 0; x < p.W; x++ {
			mn, mx := float32(math.Inf(1)), float32(math.Inf(-1))
			for iy := 0; iy < 5; iy++ {
				row := pad[(y+iy)*pw+x : (y+iy)*pw+x+5]
				for ix := 0; ix < 5; ix++ {
					v := row[ix]
					win[iy*5+ix] = v
					if v < mn {
						mn = v
					}
					if v > mx {
						mx = v
					}
				}
			}
			for oy := 0; oy < N; oy++ {
				orow := out.Pix[(y*N+oy)*ow+x*N:]
				for ox := 0; ox < N; ox++ {
					kk := kern[(N*oy+ox)*25 : (N*oy+ox)*25+25]
					var s float32
					for i := 0; i < 25; i++ {
						s += win[i] * kk[i]
					}
					if s < mn {
						s = mn
					}
					if s > mx {
						s = mx
					}
					orow[ox] = s
				}
			}
		}
		return nil
	})
	return cropPlane(out, w, h)
}

// blend composites the frame onto a new canvas.
func (d *Decoder) blend(fd *frameDecoder) error {
	ih, fh := d.Header, fd.fh
	W, H := int(ih.Size.Width), int(ih.Size.Height)
	nec := len(ih.ExtraChannels)

	// background per channel
	getBG := func(src uint32, c int) *Plane {
		r := d.refs[src]
		if r == nil {
			return nil
		}
		if c < 3 {
			return r.color[c]
		}
		if c-3 < len(r.ec) {
			return r.ec[c-3]
		}
		return nil
	}
	canvas := &refFrame{ec: make([]*Plane, nec)}
	planes := make([]*Plane, 3+nec)
	for c := 0; c < 3+nec; c++ {
		src := fh.Blending.Source
		if c >= 3 {
			src = fh.ECBlending[c-3].Source
		}
		bg := getBG(src, c)
		var p *Plane
		if bg != nil && bg.W == W && bg.H == H {
			p = &Plane{W: W, H: H, Pix: append([]float32(nil), bg.Pix...)}
		} else {
			p = newPlane(W, H)
			if bg != nil {
				for y := 0; y < min(H, bg.H); y++ {
					copy(p.Row(y), bg.Row(y)[:min(W, bg.W)])
				}
			}
		}
		planes[c] = p
	}
	// foreground planes
	fg := make([]*Plane, 3+nec)
	for c := 0; c < 3; c++ {
		fg[c] = fd.color[c]
	}
	for i := 0; i < nec; i++ {
		fg[3+i] = fd.ec[i]
	}
	if fg[0] == nil {
		// no colour data (shouldn't happen)
		return invalid("frame without colour")
	}
	fw, fhh := fg[0].W, fg[0].H
	x0, y0 := int(fh.X0), int(fh.Y0)
	xs, xe := max(0, x0), min(W, x0+fw)
	ys, ye := max(0, y0), min(H, y0+fhh)

	// alpha info
	alphaIdx := -1
	for i, e := range ih.ExtraChannels {
		if e.Type == ECAlpha {
			alphaIdx = i
			break
		}
	}
	hasAlpha := alphaIdx >= 0
	clamp01 := func(v float32) float32 {
		if v < 0 {
			return 0
		}
		if v > 1 {
			return 1
		}
		return v
	}
	if xs < xe && ys < ye {
		bgRow := make([][]float32, 3+nec)
		fgRow := make([][]float32, 3+nec)
		tmp := make([][]float32, 3+nec)
		for c := range tmp {
			tmp[c] = make([]float32, xe-xs)
		}
		for y := ys; y < ye; y++ {
			fy := y - y0
			for c := range planes {
				bgRow[c] = planes[c].Row(y)[xs:xe]
				fgRow[c] = fg[c].Row(fy)[xs-x0 : xe-x0]
			}
			n := xe - xs
			// extra channels first
			for i := 0; i < nec; i++ {
				bi := fh.ECBlending[i]
				o := tmp[3+i]
				b, f := bgRow[3+i], fgRow[3+i]
				switch bi.Mode {
				case BlendAdd:
					for x := 0; x < n; x++ {
						o[x] = b[x] + f[x]
					}
				case BlendBlend:
					a := int(bi.AlphaChannel)
					if a >= nec {
						return invalid("blend alpha channel")
					}
					ba, fa := bgRow[3+a], fgRow[3+a]
					if a == i {
						for x := 0; x < n; x++ {
							av := fa[x]
							if bi.Clamp {
								av = clamp01(av)
							}
							o[x] = 1 - (1-av)*(1-ba[x])
						}
					} else if ih.ExtraChannels[a].AlphaAssociated {
						for x := 0; x < n; x++ {
							av := fa[x]
							if bi.Clamp {
								av = clamp01(av)
							}
							o[x] = f[x] + b[x]*(1-av)
						}
					} else {
						for x := 0; x < n; x++ {
							av := fa[x]
							if bi.Clamp {
								av = clamp01(av)
							}
							na := 1 - (1-av)*(1-ba[x])
							var r float32
							if na > 0 {
								r = 1 / na
							}
							o[x] = (f[x]*av + b[x]*ba[x]*(1-av)) * r
						}
					}
				case BlendMulAdd:
					a := int(bi.AlphaChannel)
					if a >= nec {
						return invalid("blend alpha channel")
					}
					fa := fgRow[3+a]
					if a == i {
						copy(o[:n], b)
					} else {
						for x := 0; x < n; x++ {
							av := fa[x]
							if bi.Clamp {
								av = clamp01(av)
							}
							o[x] = b[x] + f[x]*av
						}
					}
				case BlendMul:
					for x := 0; x < n; x++ {
						fv := f[x]
						if bi.Clamp {
							fv = clamp01(fv)
						}
						o[x] = b[x] * fv
					}
				default:
					copy(o[:n], f)
				}
			}
			// colour
			bi := fh.Blending
			switch bi.Mode {
			case BlendAdd:
				for c := 0; c < 3; c++ {
					for x := 0; x < n; x++ {
						tmp[c][x] = bgRow[c][x] + fgRow[c][x]
					}
				}
			case BlendMulAdd:
				if hasAlpha && int(bi.AlphaChannel) < nec {
					fa := fgRow[3+int(bi.AlphaChannel)]
					for c := 0; c < 3; c++ {
						for x := 0; x < n; x++ {
							av := fa[x]
							if bi.Clamp {
								av = clamp01(av)
							}
							tmp[c][x] = bgRow[c][x] + fgRow[c][x]*av
						}
					}
				} else {
					for c := 0; c < 3; c++ {
						for x := 0; x < n; x++ {
							tmp[c][x] = bgRow[c][x] + fgRow[c][x]
						}
					}
				}
			case BlendBlend:
				if hasAlpha && int(bi.AlphaChannel) < nec {
					a := int(bi.AlphaChannel)
					fa, ba := fgRow[3+a], bgRow[3+a]
					oa := tmp[3+a]
					prem := ih.ExtraChannels[a].AlphaAssociated
					for x := 0; x < n; x++ {
						av := fa[x]
						if bi.Clamp {
							av = clamp01(av)
						}
						na := 1 - (1-av)*(1-ba[x])
						if prem {
							for c := 0; c < 3; c++ {
								tmp[c][x] = fgRow[c][x] + bgRow[c][x]*(1-av)
							}
						} else {
							var r float32
							if na > 0 {
								r = 1 / na
							}
							for c := 0; c < 3; c++ {
								tmp[c][x] = (fgRow[c][x]*av + bgRow[c][x]*ba[x]*(1-av)) * r
							}
						}
						oa[x] = na
					}
				} else {
					for c := 0; c < 3; c++ {
						copy(tmp[c][:n], fgRow[c])
					}
				}
			case BlendMul:
				for c := 0; c < 3; c++ {
					for x := 0; x < n; x++ {
						fv := fgRow[c][x]
						if bi.Clamp {
							fv = clamp01(fv)
						}
						tmp[c][x] = bgRow[c][x] * fv
					}
				}
			default:
				for c := 0; c < 3; c++ {
					copy(tmp[c][:n], fgRow[c])
				}
			}
			for c := range planes {
				if c > 0 && c < 3 && fg[c] == fg[0] && planes[c] == planes[0] {
					continue
				}
				copy(bgRow[c], tmp[c][:n])
			}
		}
	}
	canvas.color = [3]*Plane{planes[0], planes[1], planes[2]}
	copy(canvas.ec, planes[3:])
	d.canvas = canvas
	return nil
}

// srgbLUT maps linear [0,1] to sRGB with linear interpolation.
const srgbLUTSize = 1 << 14

var srgbLUT = func() []float32 {
	t := make([]float32, srgbLUTSize+2)
	for i := range t {
		v := float64(i) / srgbLUTSize
		if v <= 0.0031308 {
			t[i] = float32(v * 12.92)
		} else {
			t[i] = float32(1.055*math.Pow(v, 1/2.4) - 0.055)
		}
	}
	return t
}()

func srgbEncodeSlice(px []float32) {
	for i, v := range px {
		if v >= 0 && v < 1 {
			f := v * srgbLUTSize
			k := int(f)
			fr := f - float32(k)
			px[i] = srgbLUT[k] + (srgbLUT[k+1]-srgbLUT[k])*fr
		} else if v < 0 {
			px[i] = -srgbFromLinear(-v)
		} else {
			px[i] = srgbFromLinear(v)
		}
	}
}
