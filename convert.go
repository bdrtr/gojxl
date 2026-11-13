package gojxl

import (
	"bytes"
	"compress/flate"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"sync"

	"gojxl/internal/bmp"
	"gojxl/internal/exif"
	ijpeg "gojxl/internal/jpeg"
	"gojxl/internal/jxl"
	"gojxl/internal/pngx"
	"gojxl/internal/pnm"
	"gojxl/internal/tiff"
	"gojxl/internal/webp"
)

// ConvertToJXL encodes an image file as JPEG XL: PNG or APNG, GIF, JPEG,
// WebP (still or animated), TIFF, BMP, PGM/PPM/PAM, or any other format
// registered with package image. The colour space of
// the input is kept (PNG cICP, iCCP, sRGB and gAMA/cHRM chunks, JPEG ICC
// profiles; opt.ICC overrides them), as are its Exif and XMP metadata
// (the Exif orientation becomes the image's). With Distance 0, JPEG files are
// recompressed losslessly (restorable byte for byte with ReconstructJPEG)
// when possible, unless opt.JPEGPixels is set; animated GIF and APNG files
// become animations.
func ConvertToJXL(data []byte, opt *EncodeOptions) (out []byte, err error) {
	defer recoverErr(&err)
	var o EncodeOptions
	if opt != nil {
		o = *opt
	}
	if o.Distance == 0 && !o.JPEGPixels && len(data) > 2 && data[0] == 0xff && data[1] == 0xd8 {
		if out, err := TranscodeJPEG(data); err == nil {
			return out, nil
		}
	}
	col, meta := readInput(data)
	if o.ICC != nil {
		var ok bool
		if col, ok = specFromICC(o.ICC); !ok {
			return nil, errBadICC
		}
	}
	if o.Exif != nil {
		meta.exif = o.Exif
	}
	if o.XMP != nil {
		meta.xmp = o.XMP
	}
	if pnm.Is(data) {
		// at the anymap's own bit depth
		p, err := pnm.Decode(data)
		if err != nil {
			return nil, err
		}
		if p.IsFloat {
			return nil, errPFMInput
		}
		return encodePlanar(pnmPlanar(p), &o, col, meta)
	}
	if tiff.Is(data) {
		t, err := tiff.Decode(data)
		if err != nil {
			return nil, err
		}
		im := pnmPlanar(tiffAsPNM(t))
		meta.apply(im) // Exif orientation, when any
		if t.Orientation >= 1 && t.Orientation <= 8 {
			im.Orientation = t.Orientation
		}
		return encodePlanarOriented(im, &o, col, meta)
	}
	anim, err := DecodeAnimatedInput(data)
	if err != nil {
		return nil, err
	}
	if anim != nil {
		return encodeAnimation(anim, &o, col, meta)
	}
	img, err := decodeInput(data)
	if err != nil {
		return nil, err
	}
	return encodeImage(img, &o, col, meta)
}

// outputColour returns the ICC profile and cICP code points that describe
// the decoded pixels under opt.
func outputColour(data []byte, opt *Options) (icc, cicp []byte) {
	d, err := newDecoder(data, opt)
	if err != nil {
		return nil, nil
	}
	return d.OutputICC(), outputCICP(d)
}

// DecodeInput decodes an image file of a format ConvertToJXL takes (the
// first frame of an animation): PNG, JPEG, GIF, WebP, TIFF, BMP, PGM, PPM,
// PAM or any other format registered with package image.
func DecodeInput(data []byte) (img image.Image, err error) {
	defer recoverErr(&err)
	return decodeInput(data)
}

// decodeInput decodes PNG (with the fast pngx decoder), JPEG, PGM, PPM,
// PAM or any other registered image format.
func decodeInput(data []byte) (image.Image, error) {
	if bytes.HasPrefix(data, []byte("\x89PNG")) {
		return pngx.Decode(data)
	}
	if pnm.Is(data) {
		p, err := pnm.Decode(data)
		if err != nil {
			return nil, err
		}
		if p.IsFloat {
			return nil, errPFMInput
		}
		return pnmImage(p), nil
	}
	if webp.Is(data) {
		w, err := webp.Decode(data)
		if err != nil {
			return nil, err
		}
		return w.Canvases()[0], nil
	}
	if tiff.Is(data) {
		t, err := tiff.Decode(data)
		if err != nil {
			return nil, err
		}
		return pnmImage(tiffAsPNM(t)), nil
	}
	if bmp.Is(data) {
		return bmp.Decode(data)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

// ConvertToJPEG writes a JPEG XL image to w as JPEG: the original file
// when it is a losslessly recompressed JPEG (see ReconstructJPEG), else
// the first frame's pixels encoded at the given quality (1-100), with the
// colour profile of the decoded pixels and the Exif and XMP metadata.
// opt (may be nil) applies to the pixel path.
func ConvertToJPEG(w io.Writer, data []byte, quality int, opt *Options) (err error) {
	defer recoverErr(&err)
	jpg, err := ReconstructJPEG(data)
	if err == nil {
		_, err = w.Write(jpg)
		return err
	}
	if !errors.Is(err, ErrNoJPEG) {
		return err
	}
	img, err := DecodeBytesOptions(data, opt)
	if err != nil {
		return err
	}
	info, ierr := Stat(data)
	if ierr == nil && info.Grey {
		// grey images (with alpha, or 16-bit) as one-component JPEGs:
		// image/jpeg writes YCbCr for anything but *image.Gray, and a grey
		// ICC profile may not tag a colour JPEG
		if _, ok := img.(*image.Gray); !ok {
			g := image.NewGray(img.Bounds())
			draw.Draw(g, g.Rect, img, img.Bounds().Min, draw.Src)
			img = g
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return err
	}
	out := buf.Bytes()
	if ierr == nil {
		icc, _ := outputColour(data, opt)
		out = ijpeg.InsertMetadata(out, icc, exif.ResetOrientation(info.Exif), info.XMP)
	}
	_, err = w.Write(out)
	return err
}

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
	icc, cicp := outputColour(data, opt)
	// colour description chunks after IHDR (cICP before iCCP), then the
	// metadata (the pixels are oriented: the Exif orientation becomes 1)
	var extra []pngx.Chunk
	if cicp != nil {
		extra = append(extra, pngx.CICPChunk(cicp[0], cicp[1]))
	}
	if icc != nil {
		extra = append(extra, pngx.ICCChunk(icc))
	}
	if info.Exif != nil {
		extra = append(extra, pngx.ExifChunk(exif.ResetOrientation(info.Exif)))
	}
	if info.XMP != nil {
		extra = append(extra, pngx.XMPChunk(info.XMP))
	}
	if info.Animated {
		anim, err := DecodeAllOptions(data, opt)
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
			// grey images stay grey (+alpha): their grey ICC profile may not tag an RGB PNG
			if err := pngx.EncodeAPNGGrey(&buf, frames, delays, anim.NumLoops, flate.BestSpeed, info.Grey); err != nil {
				return err
			}
			return writeWithChunks(w, buf.Bytes(), extra)
		}
	}
	return writeFramePNG(w, data, opt, info, extra)
}

// writeWithChunks writes a PNG with chunks inserted after IHDR.
func writeWithChunks(w io.Writer, png []byte, extra []pngx.Chunk) error {
	_, err := w.Write(pngx.InsertChunks(png, extra...))
	return err
}

// writeFramePNG writes the first displayed frame as PNG in the format
// frameToImage and pngx.Encode would give it.
func writeFramePNG(w io.Writer, data []byte, opt *Options, info *Info, extra []pngx.Chunk) error {
	d, err := newDecoder(data, opt)
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
		if err := pngx.EncodeGrey(&buf, frameToImage(h, f, opt), flate.BestSpeed, h.IsGrey()); err != nil {
			return err
		}
		return writeWithChunks(w, buf.Bytes(), extra)
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
	if h.IsGrey() {
		// grey or grey+alpha (a grey ICC profile may not tag an RGB PNG);
		// alpha only if any pixel is not fully opaque at the output depth
		ga := alpha != nil && !opaqueAt(alpha, deep)
		ctype, ch := 0, 1
		if ga {
			ctype, ch = 4, 2
		}
		return pngx.EncodeRows(w, W, H, ctype, depth, func(y int, dst []byte) {
			tmp := tmps.Get().([]float32)
			r, _, _ := f.RGBRow(y, tmp)
			r = r[:W]
			var a []float32
			if ga {
				a = alpha.Row(y)[:W]
			}
			for x, v := range r {
				if deep {
					q := q16(v)
					dst[2*ch*x], dst[2*ch*x+1] = uint8(q>>8), uint8(q)
					if ga {
						q := q16(a[x])
						dst[2*ch*x+2], dst[2*ch*x+3] = uint8(q>>8), uint8(q)
					}
				} else {
					dst[ch*x] = q8(v)
					if ga {
						dst[ch*x+1] = q8(a[x])
					}
				}
			}
			tmps.Put(tmp)
		}, flate.BestSpeed, extra...)
	}
	// RGB or RGBA, RGBA only if any pixel is not
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
