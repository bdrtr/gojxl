// Package bmp reads Windows bitmaps: 1, 4 and 8-bit palette images (also
// RLE4/RLE8 compressed), 16, 24 and 32-bit images with optional bit
// fields, bottom-up or top-down.
package bmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math/bits"
)

// ErrFormat reports a malformed or unsupported file.
var ErrFormat = errors.New("bmp: invalid or unsupported file")

func errorf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrFormat}, a...)...)
}

// MaxPixels bounds the image area accepted.
var MaxPixels = 1 << 28

// Is reports whether data starts like a bitmap file.
func Is(data []byte) bool {
	return len(data) >= 26 && data[0] == 'B' && data[1] == 'M' && binary.LittleEndian.Uint32(data[14:]) >= 12
}

// Decode decodes a bitmap. The alpha of 32-bit images is used only when
// the file gives an alpha mask or some pixel has non-zero alpha.
func Decode(data []byte) (*image.NRGBA, error) {
	if !Is(data) {
		return nil, errorf("not a bitmap")
	}
	le := binary.LittleEndian
	pixOff := int(le.Uint32(data[10:]))
	hs := int(le.Uint32(data[14:]))
	if 14+hs > len(data) {
		return nil, errorf("header size")
	}
	var w, h, bpp, comp int
	var masks [4]uint32
	hasMasks := false
	if hs == 12 { // OS/2 BITMAPCOREHEADER
		w, h = int(le.Uint16(data[18:])), int(int16(le.Uint16(data[20:])))
		bpp = int(le.Uint16(data[24:]))
	} else {
		if hs < 40 {
			return nil, errorf("header size %d", hs)
		}
		w, h = int(int32(le.Uint32(data[18:]))), int(int32(le.Uint32(data[22:])))
		bpp = int(le.Uint16(data[28:]))
		comp = int(le.Uint32(data[30:]))
		if comp == 3 || comp == 6 { // bit fields (in the header or after it)
			p := 14 + 40
			if hs >= 52 {
				p = 54
			}
			n := 3
			if comp == 6 || hs >= 56 {
				n = 4
			}
			if p+4*n > len(data) {
				return nil, errorf("bit fields")
			}
			for i := 0; i < n; i++ {
				masks[i] = le.Uint32(data[p+4*i:])
			}
			hasMasks = true
		}
	}
	topDown := h < 0
	if topDown {
		h = -h
	}
	if w <= 0 || h <= 0 || w > MaxPixels || h > MaxPixels || w*h > MaxPixels {
		return nil, errorf("size %dx%d", w, h)
	}
	// palette
	var pal [][4]uint8
	if bpp <= 8 {
		entry := 4
		if hs == 12 {
			entry = 3
		}
		n := 1 << bpp
		if hs >= 40 {
			if c := int(le.Uint32(data[46:])); c > 0 && c < n {
				n = c
			}
		}
		p := 14 + hs
		if p+n*entry > len(data) {
			return nil, errorf("palette")
		}
		for i := 0; i < n; i++ {
			e := data[p+i*entry:]
			pal = append(pal, [4]uint8{e[2], e[1], e[0], 255})
		}
	}
	if pixOff < 0 || pixOff > len(data) {
		return nil, errorf("pixel data offset")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	row := func(y int) []uint8 {
		if !topDown {
			y = h - 1 - y
		}
		return img.Pix[y*img.Stride : y*img.Stride+4*w]
	}
	src := data[pixOff:]
	switch {
	case comp == 1 && bpp == 8 || comp == 2 && bpp == 4:
		return img, decodeRLE(src, img, pal, bpp, topDown)
	case comp != 0 && comp != 3 && comp != 6:
		return nil, errorf("compression %d", comp)
	}
	stride := (w*bpp + 31) / 32 * 4
	if len(src) < stride*h {
		return nil, errorf("pixel data short")
	}
	if !hasMasks {
		switch bpp {
		case 16:
			masks = [4]uint32{0x7c00, 0x03e0, 0x001f, 0}
		case 32:
			masks = [4]uint32{0xff0000, 0xff00, 0xff, 0}
		}
	}
	alphaSeen := false
	for y := 0; y < h; y++ {
		s := src[y*stride : (y+1)*stride]
		d := row(y)
		for x := 0; x < w; x++ {
			o := d[4*x : 4*x+4 : 4*x+4]
			switch bpp {
			case 1, 2, 4, 8:
				bit := x * bpp
				i := int(s[bit/8]) >> (8 - bpp - bit%8) & (1<<bpp - 1)
				if i >= len(pal) {
					return nil, errorf("palette index %d", i)
				}
				copy(o, pal[i][:])
			case 24:
				o[0], o[1], o[2], o[3] = s[3*x+2], s[3*x+1], s[3*x], 255
			case 16, 32:
				var v uint32
				if bpp == 16 {
					v = uint32(le.Uint16(s[2*x:]))
				} else {
					v = le.Uint32(s[4*x:])
				}
				for c := 0; c < 4; c++ {
					o[c] = field(v, masks[c])
				}
				if masks[3] == 0 {
					if bpp == 32 && v>>24 != 0 {
						alphaSeen = true
					}
					o[3] = uint8(v >> 24) // provisional (see below)
				}
			default:
				return nil, errorf("%d bits per pixel", bpp)
			}
		}
	}
	if (bpp == 16 || bpp == 32) && masks[3] == 0 {
		// no alpha mask: the fourth byte is alpha only if anything uses it
		// (most 32-bit bitmaps leave it 0)
		for i := 3; i < len(img.Pix); i += 4 {
			if !alphaSeen || bpp == 16 {
				img.Pix[i] = 255
			}
		}
	}
	return img, nil
}

// field extracts a bit field scaled to 8 bits (0 for an empty mask).
func field(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	sh := bits.TrailingZeros32(mask)
	n := bits.OnesCount32(mask)
	f := (v & mask) >> sh
	if n >= 8 {
		return uint8(f >> (n - 8))
	}
	m := uint32(1)<<n - 1
	return uint8(f * 255 / m) // as Pillow scales 5 and 6-bit fields
}

// decodeRLE decodes RLE8 or RLE4 pixel data.
func decodeRLE(src []byte, img *image.NRGBA, pal [][4]uint8, bpp int, topDown bool) error {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	x, y := 0, 0
	put := func(i int) error {
		if i >= len(pal) {
			return errorf("palette index %d", i)
		}
		if x < w && y < h {
			yy := y
			if !topDown {
				yy = h - 1 - y
			}
			copy(img.Pix[yy*img.Stride+4*x:], pal[i][:])
		}
		x++
		return nil
	}
	// pixels not written stay transparent black, as decoders commonly do
	for p := 0; p+1 < len(src); {
		n, c := int(src[p]), int(src[p+1])
		p += 2
		if n > 0 { // run
			for k := 0; k < n; k++ {
				i := c
				if bpp == 4 {
					i = c >> 4
					if k&1 == 1 {
						i = c & 15
					}
				}
				if err := put(i); err != nil {
					return err
				}
			}
			continue
		}
		switch c {
		case 0: // end of line
			x, y = 0, y+1
		case 1: // end of bitmap
			return nil
		case 2: // delta
			if p+2 > len(src) {
				return errorf("RLE delta")
			}
			x += int(src[p])
			y += int(src[p+1])
			p += 2
		default: // absolute run of c pixels
			nbytes := c
			if bpp == 4 {
				nbytes = (c + 1) / 2
			}
			if p+nbytes > len(src) {
				return errorf("RLE absolute run")
			}
			for k := 0; k < c; k++ {
				i := int(src[p+k])
				if bpp == 4 {
					i = int(src[p+k/2]) >> 4
					if k&1 == 1 {
						i = int(src[p+k/2]) & 15
					}
				}
				if err := put(i); err != nil {
					return err
				}
			}
			p += (nbytes + 1) &^ 1 // word aligned
		}
		if y > h {
			break
		}
	}
	return nil
}
