package gojxl

import (
	"bufio"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"math"

	"github.com/bdrtr/gojxl/internal/enc"
	"github.com/bdrtr/gojxl/internal/jxl"
	"github.com/bdrtr/gojxl/internal/pnm"
	"github.com/bdrtr/gojxl/internal/tiff"
)

// errPFMInput reports float anymap input, which the encoder cannot take.
var errPFMInput = errors.New("gojxl: PFM input is not supported")

// pnmPlanar converts an integer anymap to encoder planes at its own bit
// depth (maxval 2^k-1 gives k bits exactly; other maxvals are rescaled to
// the k bits that hold them). Fully opaque alpha is dropped.
func pnmPlanar(p *pnm.Image) *enc.Image {
	depth := p.BitDepth()
	top := int64(1)<<depth - 1
	im := &enc.Image{W: p.W, H: p.H, BitDepth: depth, Grey: p.Channels <= 2, Alpha: p.Channels%2 == 0}
	n := p.W * p.H
	for c := 0; c < p.Channels; c++ {
		ch := make([]int32, n)
		for i := range ch {
			v := int64(p.Samples[i*p.Channels+c])
			if int64(p.MaxVal) != top {
				v = (v*top + int64(p.MaxVal)/2) / int64(p.MaxVal)
			}
			ch[i] = int32(v)
		}
		im.Chans = append(im.Chans, ch)
	}
	if im.Alpha {
		opaque := true
		for _, v := range im.Chans[len(im.Chans)-1] {
			if int64(v) != top {
				opaque = false
				break
			}
		}
		if opaque {
			im.Chans = im.Chans[:len(im.Chans)-1]
			im.Alpha = false
		}
	}
	return im
}

// pnmImage converts an integer anymap to an image (8 or 16 bits).
func pnmImage(p *pnm.Image) image.Image {
	r := image.Rect(0, 0, p.W, p.H)
	n, ch := p.W*p.H, p.Channels
	deep := p.MaxVal > 255
	top := uint32(255)
	if deep {
		top = 65535
	}
	scale := func(v uint16) uint32 { return (uint32(v)*top + uint32(p.MaxVal)/2) / uint32(p.MaxVal) }
	px := func(i int) (r, g, b, a uint32) {
		s := p.Samples[i*ch : i*ch+ch]
		a = top
		switch ch {
		case 1, 2:
			r = scale(s[0])
			g, b = r, r
			if ch == 2 {
				a = scale(s[1])
			}
		case 3:
			r, g, b = scale(s[0]), scale(s[1]), scale(s[2])
		default:
			r, g, b, a = scale(s[0]), scale(s[1]), scale(s[2]), scale(s[3])
		}
		return
	}
	switch {
	case ch == 1 && !deep:
		img := image.NewGray(r)
		for i := 0; i < n; i++ {
			v, _, _, _ := px(i)
			img.Pix[i] = uint8(v)
		}
		return img
	case ch == 1:
		img := image.NewGray16(r)
		for i := 0; i < n; i++ {
			v, _, _, _ := px(i)
			binary.BigEndian.PutUint16(img.Pix[2*i:], uint16(v))
		}
		return img
	case !deep:
		img := image.NewNRGBA(r)
		for i := 0; i < n; i++ {
			r, g, b, a := px(i)
			img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = uint8(r), uint8(g), uint8(b), uint8(a)
		}
		return img
	}
	img := image.NewNRGBA64(r)
	for i := 0; i < n; i++ {
		r, g, b, a := px(i)
		for c, v := range [4]uint32{r, g, b, a} {
			binary.BigEndian.PutUint16(img.Pix[8*i+2*c:], uint16(v))
		}
	}
	return img
}

// ConvertToPNM decodes a JPEG XL image and writes its first displayed
// frame to w as a binary anymap: PGM for grey, PPM for colour, PAM (P7)
// when some pixel is not opaque; 16 bits per sample when the image has
// more than 8 (or with opt.Force16). Anymaps carry no colour profile: the
// samples are those ConvertToPNG writes (opt.SRGB gives sRGB).
func ConvertToPNM(w io.Writer, data []byte, opt *Options) (err error) {
	defer recoverErr(&err)
	img, err := DecodeBytesOptions(data, opt)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 1<<16)
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	switch m := img.(type) {
	case *image.Gray:
		if err := pnm.Header(bw, W, H, 1, 255); err != nil {
			return err
		}
		for y := 0; y < H; y++ {
			bw.Write(m.Pix[y*m.Stride : y*m.Stride+W])
		}
	case *image.Gray16:
		if err := pnm.Header(bw, W, H, 1, 65535); err != nil {
			return err
		}
		for y := 0; y < H; y++ {
			bw.Write(m.Pix[y*m.Stride : y*m.Stride+2*W])
		}
	case *image.NRGBA:
		writeRGBA(bw, m.Pix, m.Stride, W, H, 1)
	case *image.NRGBA64:
		writeRGBA(bw, m.Pix, m.Stride, W, H, 2)
	default:
		return errors.New("gojxl: unexpected image type")
	}
	return bw.Flush()
}

// writeRGBA writes 8-bit (bps 1) or 16-bit (bps 2) RGBA rows as PPM, or as
// a grey or RGB PAM with alpha when some pixel is not opaque (grey when
// every pixel has R = G = B).
func writeRGBA(w *bufio.Writer, pix []byte, stride, W, H, bps int) {
	opaque, grey := true, true
	for y := 0; y < H; y++ {
		row := pix[y*stride : y*stride+4*bps*W]
		for x := 0; x < W; x++ {
			p := row[4*bps*x : 4*bps*(x+1)]
			for k := 0; k < bps; k++ {
				opaque = opaque && p[3*bps+k] == 0xff
				grey = grey && p[k] == p[bps+k] && p[k] == p[2*bps+k]
			}
		}
	}
	maxval := 255
	if bps == 2 {
		maxval = 65535
	}
	// samples kept per pixel: offsets of the bytes of each kept channel
	var keep []int
	switch {
	case opaque:
		keep = []int{0, 1, 2}
	case grey:
		keep = []int{0, 3}
	default:
		keep = []int{0, 1, 2, 3}
	}
	pnm.Header(w, W, H, len(keep), maxval)
	out := make([]byte, len(keep)*bps*W)
	for y := 0; y < H; y++ {
		row := pix[y*stride : y*stride+4*bps*W]
		o := 0
		for x := 0; x < W; x++ {
			p := row[4*bps*x:]
			for _, c := range keep {
				copy(out[o:o+bps], p[c*bps:c*bps+bps])
				o += bps
			}
		}
		w.Write(out)
	}
}

// ConvertToPFM decodes a JPEG XL image and writes the colour samples of
// its first displayed frame to w as a PFM of 32-bit floats (Pf for grey
// images): the values of ConvertToPNG's samples before quantisation, which
// may lie outside [0, 1] (HDR, wide gamut). Alpha is not kept.
func ConvertToPFM(w io.Writer, data []byte, opt *Options) (err error) {
	defer recoverErr(&err)
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
	grey := h.IsGrey()
	ch := 3
	if grey {
		ch = 1
	}
	fw, fh := f.Width, f.Height
	ow, oh, tr := orient(int(h.Orientation), fw, fh)
	// the frame's rows in output space
	rows := make([][3][]float32, fh)
	jxl.ParallelRows(fh, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			tmp := make([]float32, 3*fw)
			r, g, b := f.RGBRow(y, tmp)
			rows[y] = [3][]float32{append([]float32(nil), r[:fw]...), append([]float32(nil), g[:fw]...), append([]float32(nil), b[:fw]...)}
		}
	})
	bw := bufio.NewWriterSize(w, 1<<16)
	if err := pnm.PFMHeader(bw, ow, oh, ch); err != nil {
		return err
	}
	out := make([]byte, 4*ch*ow)
	for oy := oh - 1; oy >= 0; oy-- { // PFM rows go bottom-up
		for ox := 0; ox < ow; ox++ {
			sx, sy := ox, oy
			if tr != nil {
				sx, sy = tr(ox, oy)
			}
			for c := 0; c < ch; c++ {
				binary.LittleEndian.PutUint32(out[4*(ch*ox+c):], math.Float32bits(rows[sy][c][sx]))
			}
		}
		bw.Write(out)
	}
	return bw.Flush()
}

// tiffAsPNM views decoded TIFF samples as an anymap (same layout).
func tiffAsPNM(t *tiff.Image) *pnm.Image {
	return &pnm.Image{W: t.W, H: t.H, Channels: t.Channels, MaxVal: 1<<t.BitDepth - 1, Samples: t.Samples}
}
