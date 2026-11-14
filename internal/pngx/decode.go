package pngx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/bdrtr/gojxl/internal/par"
	"image"
	"image/png"
)

// Decode decodes a PNG file. Non-interlaced 8- and 16-bit grey, grey+alpha,
// RGB and RGBA images without tRNS use the fast in-memory path and return
// the same image types as image/png (*image.Gray, *image.Gray16,
// *image.RGBA, *image.RGBA64, *image.NRGBA, *image.NRGBA64); other files
// are passed to image/png. Chunk CRCs and the zlib checksum are not
// verified on the fast path.
func Decode(data []byte) (image.Image, error) {
	if img, ok, err := decodeFast(data); ok {
		return img, err
	}
	return png.Decode(bytes.NewReader(data))
}

func decodeFast(data []byte) (image.Image, bool, error) {
	if len(data) < 8+25 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, false, nil
	}
	var ihdr []byte
	var idat [][]byte
	total := 0
	for p := 8; p+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		if n < 0 || p+12+n > len(data) {
			return nil, false, nil
		}
		typ, body := string(data[p+4:p+8]), data[p+8:p+8+n]
		switch typ {
		case "IHDR":
			ihdr = body
		case "IDAT":
			idat = append(idat, body)
			total += n
		case "tRNS", "PLTE", "acTL":
			return nil, false, nil // colour keys, palettes, animation
		case "IEND":
			p = len(data)
			continue
		}
		p += 12 + n
	}
	if len(ihdr) != 13 || len(idat) == 0 {
		return nil, false, nil
	}
	w, h := int(binary.BigEndian.Uint32(ihdr)), int(binary.BigEndian.Uint32(ihdr[4:]))
	depth, ct, interlace := int(ihdr[8]), ihdr[9], ihdr[12]
	var nc int
	switch ct {
	case 0:
		nc = 1
	case 2:
		nc = 3
	case 4:
		nc = 2
	case 6:
		nc = 4
	default:
		return nil, false, nil
	}
	if (depth != 8 && depth != 16) || interlace != 0 || ihdr[10] != 0 || ihdr[11] != 0 ||
		w <= 0 || h <= 0 || w > 1<<24 || h > 1<<24 || int64(w)*int64(h) > 1<<28 {
		return nil, false, nil
	}
	bpp := nc * depth / 8
	stride := w * bpp
	// deflate expands at most ~1032:1: a header promising more than the
	// data can hold is corrupt (and must not size the buffers)
	if uint64(h)*uint64(stride+1) > 1040*uint64(total)+1<<16 {
		return nil, true, errInflate
	}
	src := idat[0]
	if len(idat) > 1 {
		src = make([]byte, 0, total)
		for _, d := range idat {
			src = append(src, d...)
		}
	}
	rect := image.Rect(0, 0, w, h)
	var img image.Image
	var pix []byte
	var pstride int
	switch {
	case ct == 0 && depth == 8:
		m := image.NewGray(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	case ct == 0:
		m := image.NewGray16(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	case ct == 2 && depth == 8:
		m := image.NewRGBA(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	case ct == 2:
		m := image.NewRGBA64(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	case depth == 8: // RGBA, grey+alpha
		m := image.NewNRGBA(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	default:
		m := image.NewNRGBA64(rect)
		img, pix, pstride = m, m.Pix, m.Stride
	}
	// convert one unfiltered row into the image
	conv := func(y int, s []byte) {
		d := pix[y*pstride : y*pstride+pstride]
		switch {
		case ct == 0 || ct == 6:
			copy(d, s)
		case ct == 2 && depth == 8:
			for x := 0; x < w; x++ {
				p := d[4*x : 4*x+4 : 4*x+4]
				p[0], p[1], p[2], p[3] = s[3*x], s[3*x+1], s[3*x+2], 0xff
			}
		case ct == 2:
			for x := 0; x < w; x++ {
				copy(d[8*x:8*x+6], s[6*x:6*x+6])
				d[8*x+6], d[8*x+7] = 0xff, 0xff
			}
		case depth == 8: // grey+alpha
			for x := 0; x < w; x++ {
				g := s[2*x]
				d[4*x], d[4*x+1], d[4*x+2], d[4*x+3] = g, g, g, s[2*x+1]
			}
		default:
			for x := 0; x < w; x++ {
				g0, g1 := s[4*x], s[4*x+1]
				d[8*x], d[8*x+1], d[8*x+2], d[8*x+3], d[8*x+4], d[8*x+5] = g0, g1, g0, g1, g0, g1
				d[8*x+6], d[8*x+7] = s[4*x+2], s[4*x+3]
			}
		}
	}
	raw := make([]byte, h*(stride+1))
	rs := stride + 1
	// rows are unfiltered out of place (back-references of the deflate
	// stream still read the filtered bytes): straight into the image when
	// its layout matches the file's, else via two row buffers
	direct := ct == 0 || ct == 6
	var bufs [2][]byte
	if !direct {
		bufs[0], bufs[1] = make([]byte, stride), make([]byte, stride)
	}
	up := make([]byte, stride) // zero row above the first
	row := func(y int) error {
		cur := bufs[y&1]
		if direct {
			cur = pix[y*pstride : y*pstride+stride]
		}
		copy(cur, raw[y*rs+1:(y+1)*rs])
		if err := unfilterRow(raw[y*rs], cur, up, bpp); err != nil {
			return err
		}
		if !direct {
			conv(y, cur)
		}
		up = cur
		return nil
	}
	if len(raw) < 1<<20 || par.Workers() == 1 {
		if err := inflateZlib(src, raw, nil); err != nil {
			return nil, true, err
		}
		for y := 0; y < h; y++ {
			if err := row(y); err != nil {
				return nil, true, err
			}
		}
		return img, true, nil
	}
	// large images: unfilter rows while the rest is being inflated
	progress := make(chan int, len(raw)>>progressShift+2)
	errc := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("png: inflate: %v", r)
			}
			close(progress)
			errc <- err
		}()
		err = inflateZlib(src, raw, progress)
	}()
	y := 0
	for done := range progress {
		for ; y < h && (y+1)*rs <= done; y++ {
			if err := row(y); err != nil {
				for range progress {
				}
				<-errc
				return nil, true, err
			}
		}
	}
	if err := <-errc; err != nil {
		return nil, true, err
	}
	for ; y < h; y++ {
		if err := row(y); err != nil {
			return nil, true, err
		}
	}
	return img, true, nil
}

// unfilterRow reverses the PNG filter f of row cur given the previous
// (already unfiltered) row up.
func unfilterRow(f byte, cur, up []byte, bpp int) error {
	up = up[:len(cur)]
	switch f {
	case 0:
	case 1: // sub
		for i := bpp; i < len(cur); i++ {
			cur[i] += cur[i-bpp]
		}
	case 2: // up
		for i, u := range up {
			cur[i] += u
		}
	case 3: // average
		for i := 0; i < bpp; i++ {
			cur[i] += up[i] / 2
		}
		for i := bpp; i < len(cur); i++ {
			cur[i] += byte((int(cur[i-bpp]) + int(up[i])) / 2)
		}
	case 4: // paeth
		for i := 0; i < bpp; i++ {
			cur[i] += up[i]
		}
		paeth(cur, up, bpp)
	default:
		return errors.New("png: bad filter type")
	}
	return nil
}

func paeth(cur, up []byte, bpp int) {
	switch bpp {
	case 3:
		paethN(cur, up, 3)
	case 4:
		paethN(cur, up, 4)
	default:
		paethN(cur, up, bpp)
	}
}

// paethN keeps the left (a) and upper-left (c) samples of each channel in
// registers; selection is branch-free (min/max compile to CMOV).
func paethN(cur, up []byte, bpp int) {
	n := len(cur)
	up = up[:n]
	for k := 0; k < bpp; k++ {
		a, c := int32(cur[k]), int32(up[k])
		for i := k + bpp; i < n; i += bpp {
			b := int32(up[i])
			pa, pb, pc := b-c, a-c, a+b-2*c
			pa, pb, pc = max(pa, -pa), max(pb, -pb), max(pc, -pc)
			bc := b
			if pc < pb {
				bc = c
			}
			p := a
			if pa > min(pb, pc) {
				p = bc
			}
			a = int32(byte(int32(cur[i]) + p))
			cur[i] = byte(a)
			c = b
		}
	}
}
