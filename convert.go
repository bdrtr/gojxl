package gojxl

import (
	"bytes"
	"compress/flate"
	"image"
	"io"
	"sync"

	"gojxl/internal/jxl"
	"gojxl/internal/pngx"
)

// ConvertToPNG decodes a JPEG XL image and writes it to w as PNG: the
// first displayed frame, or an APNG of all frames for animations, with the
// colour profile (or cICP code points) of the decoded pixels. The result
// is that of DecodeBytesOptions followed by PNG encoding, but the rows of
// a frame go straight to the PNG writer (which filters and compresses
// strips of rows in parallel): the output image never exists as a whole.
func ConvertToPNG(w io.Writer, data []byte, opt *Options) (err error) {
	defer recoverErr(&err)
	info, ierr := Stat(data)
	if ierr != nil {
		info = &Info{}
	}
	// colour description chunks after IHDR (cICP before iCCP)
	var extra []pngx.Chunk
	if info.CICP != nil {
		extra = append(extra, pngx.CICPChunk(info.CICP[0], info.CICP[1]))
	}
	if info.OutputICC != nil {
		extra = append(extra, pngx.ICCChunk(info.OutputICC))
	}
	if info.Animated {
		anim, err := DecodeAll(data)
		if err != nil {
			return err
		}
		if len(anim.Frames) > 1 {
			frames := make([]image.Image, len(anim.Frames))
			delays := make([]float64, len(anim.Frames))
			for i, fr := range anim.Frames {
				frames[i], delays[i] = fr.Image, fr.Duration
			}
			var buf bytes.Buffer
			if err := pngx.EncodeAPNG(&buf, frames, delays, anim.NumLoops, flate.BestSpeed); err != nil {
				return err
			}
			return writeWithChunks(w, buf.Bytes(), info)
		}
	}
	return writeFramePNG(w, data, opt, info, extra)
}

// writeWithChunks writes a PNG with the colour chunks of info inserted.
func writeWithChunks(w io.Writer, png []byte, info *Info) error {
	if info.OutputICC != nil {
		png = pngx.InsertICC(png, info.OutputICC)
	}
	if info.CICP != nil {
		png = pngx.InsertCICP(png, info.CICP[0], info.CICP[1])
	}
	_, err := w.Write(png)
	return err
}

// writeFramePNG writes the first displayed frame as PNG in the format
// frameToImage and pngx.Encode would give it.
func writeFramePNG(w io.Writer, data []byte, opt *Options, info *Info, extra []pngx.Chunk) error {
	d, err := jxl.NewDecoder(data)
	if err != nil {
		return err
	}
	f, err := d.NextFrame()
	if err != nil {
		return err
	}
	if f == nil {
		return jxl.ErrInvalid
	}
	h := d.Header
	if _, _, tr := orient(int(h.Orientation), f.Width, f.Height); tr != nil {
		var buf bytes.Buffer
		if err := pngx.Encode(&buf, frameToImage(h, f, opt), flate.BestSpeed); err != nil {
			return err
		}
		return writeWithChunks(w, buf.Bytes(), info)
	}
	var alpha *jxl.Plane
	if ai := h.AlphaChannel(); ai >= 0 && ai < len(f.EC) {
		alpha = f.EC[ai]
	}
	deep := h.BitDepth.BitsPerSample > 8 || (opt != nil && opt.Force16)
	W, H := f.Width, f.Height
	depth := 8
	if deep {
		depth = 16
	}
	// float rows of colour converted on the fly (deferred XYB)
	tmps := sync.Pool{New: func() any { return make([]float32, 3*W) }}
	if h.IsGrey() && alpha == nil {
		return pngx.EncodeRows(w, W, H, 0, depth, func(y int, dst []byte) {
			tmp := tmps.Get().([]float32)
			r, _, _ := f.RGBRow(y, tmp)
			if deep {
				for x, v := range r[:W] {
					q := q16(v)
					dst[2*x], dst[2*x+1] = uint8(q>>8), uint8(q)
				}
			} else {
				for x, v := range r[:W] {
					dst[x] = q8(v)
				}
			}
			tmps.Put(tmp)
		}, flate.BestSpeed, extra...)
	}
	// RGB or RGBA (grey with alpha too), RGBA only if any pixel is not
	// fully opaque at the output depth
	ctype, ch := 2, 3
	if alpha != nil && !opaqueAt(alpha, deep) {
		ctype, ch = 6, 4
	}
	if !deep {
		return pngx.EncodeRows(w, W, H, ctype, 8, func(y int, dst []byte) {
			var a []float32
			if ctype == 6 {
				a = alpha.Row(y)
			}
			tmp := tmps.Get().([]float32)
			f.RGB8Row(y, tmp, a, dst, ch)
			tmps.Put(tmp)
		}, flate.BestSpeed, extra...)
	}
	return pngx.EncodeRows(w, W, H, ctype, 16, func(y int, dst []byte) {
		tmp := tmps.Get().([]float32)
		r, g, b := f.RGBRow(y, tmp)
		var a []float32
		if ctype == 6 {
			a = alpha.Row(y)
		}
		for x := 0; x < W; x++ {
			o := dst[2*ch*x : 2*ch*x+2*ch]
			rv, gv, bv := q16(r[x]), q16(g[x]), q16(b[x])
			o[0], o[1], o[2], o[3], o[4], o[5] = uint8(rv>>8), uint8(rv), uint8(gv>>8), uint8(gv), uint8(bv>>8), uint8(bv)
			if a != nil {
				av := q16(a[x])
				o[6], o[7] = uint8(av>>8), uint8(av)
			}
		}
		tmps.Put(tmp)
	}, flate.BestSpeed, extra...)
}

// opaqueAt reports whether every alpha sample quantises to full opacity.
func opaqueAt(a *jxl.Plane, deep bool) bool {
	for _, v := range a.Pix[:a.W*a.H] {
		if deep && q16(v) != 65535 || !deep && q8(v) != 255 {
			return false
		}
	}
	return true
}
