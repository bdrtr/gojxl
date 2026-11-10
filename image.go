package gojxl

import (
	"image"
	"math"

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

// orientTransform maps output (ox,oy) -> source (sx,sy).
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
	var alpha []float32
	if ai >= 0 && ai < len(f.EC) {
		alpha = f.EC[ai].Pix
	}
	deep := h.BitDepth.BitsPerSample > 8 || (opt != nil && opt.Force16)
	grey := h.IsGrey()
	ow, oh, tr := orient(int(h.Orientation), w, ht)
	R, G, B := f.Color[0].Pix, f.Color[1].Pix, f.Color[2].Pix
	idx := func(x, y int) int {
		if tr != nil {
			sx, sy := tr(x, y)
			return sy*w + sx
		}
		return y*w + x
	}
	_ = math.Abs
	switch {
	case grey && alpha == nil && !deep:
		img := image.NewGray(image.Rect(0, 0, ow, oh))
		for y := 0; y < oh; y++ {
			row := img.Pix[y*img.Stride:]
			for x := 0; x < ow; x++ {
				row[x] = q8(R[idx(x, y)])
			}
		}
		return img
	case grey && alpha == nil:
		img := image.NewGray16(image.Rect(0, 0, ow, oh))
		for y := 0; y < oh; y++ {
			row := img.Pix[y*img.Stride:]
			for x := 0; x < ow; x++ {
				v := q16(R[idx(x, y)])
				row[2*x], row[2*x+1] = uint8(v>>8), uint8(v)
			}
		}
		return img
	case !deep:
		img := image.NewNRGBA(image.Rect(0, 0, ow, oh))
		for y := 0; y < oh; y++ {
			row := img.Pix[y*img.Stride:]
			for x := 0; x < ow; x++ {
				i := idx(x, y)
				o := row[4*x : 4*x+4 : 4*x+4]
				o[0], o[1], o[2] = q8(R[i]), q8(G[i]), q8(B[i])
				if alpha != nil {
					o[3] = q8(alpha[i])
				} else {
					o[3] = 255
				}
			}
		}
		return img
	}
	img := image.NewNRGBA64(image.Rect(0, 0, ow, oh))
	for y := 0; y < oh; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < ow; x++ {
			i := idx(x, y)
			o := row[8*x : 8*x+8 : 8*x+8]
			r, g, b := q16(R[i]), q16(G[i]), q16(B[i])
			a := uint16(65535)
			if alpha != nil {
				a = q16(alpha[i])
			}
			o[0], o[1], o[2], o[3] = uint8(r>>8), uint8(r), uint8(g>>8), uint8(g)
			o[4], o[5], o[6], o[7] = uint8(b>>8), uint8(b), uint8(a>>8), uint8(a)
		}
	}
	return img
}
