package gojxl

import (
	"image"
	"image/color"
	"io"
	"os"
	"strconv"

	"gojxl/internal/enc"
	"gojxl/internal/jpeg"
)

// EncodeOptions controls lossless encoding.
type EncodeOptions struct {
	// Effort trades speed for size; 0 selects the default.
	//
	// Lossless (1-6, default 4): 1-3 use fixed context models (fast); 4+
	// learn an MA tree from the image, choose colour transforms and
	// per-group channel palettes, and tune the entropy coder (slower,
	// typically 5-40% smaller).
	//
	// Lossy (1-7, default 7): fewer transform shapes are searched at lower
	// efforts: 5-6 skip 32x16/16x32, 3-4 all rectangular shapes, 1-2 use
	// only 8x8 DCTs (about 40% less CPU, 2-40% larger at equal quality).
	Effort int
	// Distance > 0 selects lossy (VarDCT) encoding with the given
	// perceptual distance (1.0 ~ visually lossless). 0 means lossless.
	Distance float32
}

// Encode writes img as a lossless JPEG XL codestream.
func Encode(w io.Writer, img image.Image, opt *EncodeOptions) error {
	data, err := EncodeBytes(img, opt)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// EncodeBytes encodes img losslessly and returns the JPEG XL bytes.
func EncodeBytes(img image.Image, opt *EncodeOptions) ([]byte, error) {
	o := enc.Options{Effort: 4, Consume: true} // the planar copy is ours
	if opt != nil && opt.Effort > 0 {
		o.Effort = opt.Effort
	}
	im := toPlanar(img)
	if opt != nil && opt.Distance > 0 {
		to8Bit(im)
		lo := enc.LossyOptions{Distance: opt.Distance, EPFIters: -1, Sharpness: -1, Effort: opt.Effort}
		if v := os.Getenv("GOJXL_EPF"); v != "" {
			lo.EPFIters, _ = strconv.Atoi(v)
		}
		if v := os.Getenv("GOJXL_SHARP"); v != "" {
			lo.Sharpness, _ = strconv.Atoi(v)
		}
		lo.NoGaborish = os.Getenv("GOJXL_NOGAB") != ""
		lo.NoCfL = os.Getenv("GOJXL_NOCFL") != ""
		lo.LibAQ = os.Getenv("GOJXL_NOLAQ") == ""
		lo.OnlyDCT8 = os.Getenv("GOJXL_DCT8") != ""
		if v := os.Getenv("GOJXL_MU"); v != "" {
			lo.RDMu, _ = strconv.ParseFloat(v, 64)
		}
		lo.AQ = 0
		if v := os.Getenv("GOJXL_AQ"); v != "" {
			lo.AQ, _ = strconv.ParseFloat(v, 64)
		}
		return enc.EncodeVarDCT(im, lo)
	}
	return enc.EncodeLossless(im, o)
}

func toPlanar(img image.Image) *enc.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	im := &enc.Image{W: w, H: h, BitDepth: 8}
	n := w * h
	switch src := img.(type) {
	case *image.Gray:
		im.Grey = true
		c := make([]int32, n)
		for y := 0; y < h; y++ {
			row := src.Pix[y*src.Stride : y*src.Stride+w]
			for x, v := range row {
				c[y*w+x] = int32(v)
			}
		}
		im.Chans = [][]int32{c}
		return im
	case *image.Gray16:
		im.Grey, im.BitDepth = true, 16
		c := make([]int32, n)
		for y := 0; y < h; y++ {
			row := src.Pix[y*src.Stride:]
			for x := 0; x < w; x++ {
				c[y*w+x] = int32(row[2*x])<<8 | int32(row[2*x+1])
			}
		}
		im.Chans = [][]int32{c}
		return im
	case *image.NRGBA:
		r, g, bl, a := make([]int32, n), make([]int32, n), make([]int32, n), make([]int32, n)
		opaque := true
		for y := 0; y < h; y++ {
			row := src.Pix[y*src.Stride : y*src.Stride+4*w]
			for x := 0; x < w; x++ {
				i := y*w + x
				r[i], g[i], bl[i], a[i] = int32(row[4*x]), int32(row[4*x+1]), int32(row[4*x+2]), int32(row[4*x+3])
				opaque = opaque && row[4*x+3] == 255
			}
		}
		im.Chans = [][]int32{r, g, bl}
		if !opaque {
			im.Alpha = true
			im.Chans = append(im.Chans, a)
		}
		return im
	case *image.RGBA:
		// premultiplied: exact only when opaque (else the generic path)
		if r, g, bl, ok := splitOpaque(src.Pix, src.Stride, w, h, 1); ok {
			im.Chans = [][]int32{r, g, bl}
			return im
		}
	case *image.RGBA64:
		if r, g, bl, ok := splitOpaque(src.Pix, src.Stride, w, h, 2); ok {
			im.BitDepth = 16
			im.Chans = [][]int32{r, g, bl}
			return im
		}
	case *image.NRGBA64:
		if r, g, bl, ok := splitOpaque(src.Pix, src.Stride, w, h, 2); ok {
			im.BitDepth = 16
			im.Chans = [][]int32{r, g, bl}
			return im
		}
	}
	// generic path (16-bit precision when needed)
	deep := false
	switch img.ColorModel() {
	case color.RGBA64Model, color.NRGBA64Model, color.Gray16Model:
		deep = true
	}
	shift := uint32(8)
	if deep {
		im.BitDepth = 16
		shift = 0
	}
	r, g, bl, a := make([]int32, n), make([]int32, n), make([]int32, n), make([]int32, n)
	opaque := true
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBA64Model.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA64)
			i := y*w + x
			r[i], g[i], bl[i], a[i] = int32(uint32(c.R)>>shift), int32(uint32(c.G)>>shift), int32(uint32(c.B)>>shift), int32(uint32(c.A)>>shift)
			opaque = opaque && c.A == 0xffff
		}
	}
	im.Chans = [][]int32{r, g, bl}
	if !opaque {
		im.Alpha = true
		im.Chans = append(im.Chans, a)
	}
	return im
}

// splitOpaque splits interleaved RGBA samples of bs bytes (big-endian)
// into planes; ok is false if any pixel is not fully opaque.
func splitOpaque(pix []byte, stride, w, h, bs int) (r, g, b []int32, ok bool) {
	n := w * h
	r, g, b = make([]int32, n), make([]int32, n), make([]int32, n)
	for y := 0; y < h; y++ {
		row := pix[y*stride : y*stride+4*bs*w]
		o := y * w
		if bs == 1 {
			for x := 0; x < w; x++ {
				p := row[4*x : 4*x+4 : 4*x+4]
				if p[3] != 0xff {
					return nil, nil, nil, false
				}
				r[o+x], g[o+x], b[o+x] = int32(p[0]), int32(p[1]), int32(p[2])
			}
			continue
		}
		for x := 0; x < w; x++ {
			p := row[8*x : 8*x+8 : 8*x+8]
			if p[6] != 0xff || p[7] != 0xff {
				return nil, nil, nil, false
			}
			r[o+x] = int32(p[0])<<8 | int32(p[1])
			g[o+x] = int32(p[2])<<8 | int32(p[3])
			b[o+x] = int32(p[4])<<8 | int32(p[5])
		}
	}
	return r, g, b, true
}

// TranscodeJPEG losslessly recompresses a JPEG file into JPEG XL: the DCT
// coefficients are kept, so decoding gives the same image as decoding the
// JPEG (typically ~20% smaller), and a 'jbrd' box allows decoders to
// restore the original JPEG file byte for byte.
func TranscodeJPEG(data []byte) (out []byte, err error) {
	defer recoverErr(&err)
	j, err := jpeg.Parse(data)
	if err != nil {
		return nil, err
	}
	cs, err := enc.EncodeJPEG(j)
	if err != nil {
		return nil, err
	}
	// reconstruction data so the original file can be restored exactly
	jbrd, exif, xmp, err := enc.EncodeJBRD(j)
	if err != nil {
		return cs, nil
	}
	return enc.WrapContainer(cs, jbrd, exif, xmp), nil
}

// to8Bit converts a deeper image to 8 bits per sample (VarDCT input).
func to8Bit(im *enc.Image) {
	if im.BitDepth == 8 {
		return
	}
	maxv := int32(1)<<im.BitDepth - 1
	for _, c := range im.Chans {
		for i, v := range c {
			c[i] = (v*255 + maxv/2) / maxv
		}
	}
	im.BitDepth = 8
}
