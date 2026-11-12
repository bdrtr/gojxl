package gojxl

import (
	"image"

	"gojxl/internal/jxl"
)

func q8(v float32) uint8 {
	if !(v > 0) {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(v*255 + 0.5)
}

func q16(v float32) uint16 {
	if !(v > 0) {
		return 0
	}
	if v >= 1 {
		return 65535
	}
	return uint16(v*65535 + 0.5)
}

// orient maps output (ox,oy) -> source (sx,sy).
func orient(o int, w, h int) (ow, oh int, f func(ox, oy int) (int, int)) {
	switch o {
	case 2:
		return w, h, func(x, y int) (int, int) { return w - 1 - x, y }
	case 3:
		return w, h, func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4:
		return w, h, func(x, y int) (int, int) { return x, h - 1 - y }
	case 5:
		return h, w, func(x, y int) (int, int) { return y, x }
	case 6:
		return h, w, func(x, y int) (int, int) { return y, h - 1 - x }
	case 7:
		return h, w, func(x, y int) (int, int) { return w - 1 - y, h - 1 - x }
	case 8:
		return h, w, func(x, y int) (int, int) { return w - 1 - y, x }
	}
	return w, h, nil
}

func frameToImage(h *jxl.ImageHeader, f *jxl.Frame, opt *Options) image.Image {
	w, ht := f.Width, f.Height
	ai := h.AlphaChannel()
	var alpha *jxl.Plane
	if ai >= 0 && ai < len(f.EC) {
		alpha = f.EC[ai]
	}
	deep := h.BitDepth.BitsPerSample > 8 || (opt != nil && opt.Force16)
	grey := h.IsGrey()
	ow, oh, tr := orient(int(h.Orientation), w, ht)

	if tr != nil {
		return orientedImage(f, alpha, deep, grey, ow, oh, tr)
	}
	switch {
	case grey && alpha == nil && !deep:
		img := image.NewGray(image.Rect(0, 0, w, ht))
		jxl.ParallelRows(ht, func(y0, y1 int) {
			tmp := make([]float32, 3*w)
			for y := y0; y < y1; y++ {
				r, _, _ := f.RGBRow(y, tmp)
				row := img.Pix[y*img.Stride : y*img.Stride+w]
				for x := range row {
					row[x] = q8(r[x])
				}
			}
		})
		return img
	case grey && alpha == nil:
		img := image.NewGray16(image.Rect(0, 0, w, ht))
		jxl.ParallelRows(ht, func(y0, y1 int) {
			tmp := make([]float32, 3*w)
			for y := y0; y < y1; y++ {
				r, _, _ := f.RGBRow(y, tmp)
				row := img.Pix[y*img.Stride:]
				for x := 0; x < w; x++ {
					v := q16(r[x])
					row[2*x], row[2*x+1] = uint8(v>>8), uint8(v)
				}
			}
		})
		return img
	case !deep:
		img := image.NewNRGBA(image.Rect(0, 0, w, ht))
		jxl.ParallelRows(ht, func(y0, y1 int) {
			tmp := make([]float32, 3*w)
			for y := y0; y < y1; y++ {
				var a []float32
				if alpha != nil {
					a = alpha.Row(y)
				}
				f.RGBA8Row(y, tmp, a, img.Pix[y*img.Stride:y*img.Stride+4*w])
			}
		})
		return img
	}
	img := image.NewNRGBA64(image.Rect(0, 0, w, ht))
	jxl.ParallelRows(ht, func(y0, y1 int) {
		tmp := make([]float32, 3*w)
		for y := y0; y < y1; y++ {
			r, g, b := f.RGBRow(y, tmp)
			row := img.Pix[y*img.Stride:]
			var a []float32
			if alpha != nil {
				a = alpha.Row(y)
			}
			for x := 0; x < w; x++ {
				o := row[8*x : 8*x+8 : 8*x+8]
				rv, gv, bv := q16(r[x]), q16(g[x]), q16(b[x])
				av := uint16(65535)
				if a != nil {
					av = q16(a[x])
				}
				o[0], o[1], o[2], o[3] = uint8(rv>>8), uint8(rv), uint8(gv>>8), uint8(gv)
				o[4], o[5], o[6], o[7] = uint8(bv>>8), uint8(bv), uint8(av>>8), uint8(av)
			}
		}
	})
	return img
}

func orientedImage(f *jxl.Frame, alpha *jxl.Plane, deep, grey bool, ow, oh int, tr func(int, int) (int, int)) image.Image {
	f.Materialize()
	w := f.Width
	R, G, B := f.Color[0].Pix, f.Color[1].Pix, f.Color[2].Pix
	var A []float32
	if alpha != nil {
		A = alpha.Pix
	}
	idx := func(x, y int) int {
		sx, sy := tr(x, y)
		return sy*w + sx
	}
	switch {
	case grey && A == nil && !deep:
		img := image.NewGray(image.Rect(0, 0, ow, oh))
		for y := 0; y < oh; y++ {
			for x := 0; x < ow; x++ {
				img.Pix[y*img.Stride+x] = q8(R[idx(x, y)])
			}
		}
		return img
	case !deep:
		img := image.NewNRGBA(image.Rect(0, 0, ow, oh))
		for y := 0; y < oh; y++ {
			row := img.Pix[y*img.Stride:]
			for x := 0; x < ow; x++ {
				i := idx(x, y)
				a := uint8(255)
				if A != nil {
					a = q8(A[i])
				}
				row[4*x], row[4*x+1], row[4*x+2], row[4*x+3] = q8(R[i]), q8(G[i]), q8(B[i]), a
			}
		}
		return img
	}
	img := image.NewNRGBA64(image.Rect(0, 0, ow, oh))
	for y := 0; y < oh; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < ow; x++ {
			i := idx(x, y)
			av := uint16(65535)
			if A != nil {
				av = q16(A[i])
			}
			rv, gv, bv := q16(R[i]), q16(G[i]), q16(B[i])
			if grey {
				gv, bv = rv, rv
			}
			o := row[8*x : 8*x+8 : 8*x+8]
			o[0], o[1], o[2], o[3] = uint8(rv>>8), uint8(rv), uint8(gv>>8), uint8(gv)
			o[4], o[5], o[6], o[7] = uint8(bv>>8), uint8(bv), uint8(av>>8), uint8(av)
		}
	}
	return img
}
