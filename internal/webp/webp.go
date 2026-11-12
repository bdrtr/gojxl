package webp

import (
	"encoding/binary"
	"image"
	"image/color"
)

// Is reports whether data looks like a WebP file.
func Is(data []byte) bool {
	return len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

type chunk struct {
	id   string
	data []byte
}

// chunks splits the RIFF payload into chunks.
func chunks(data []byte) ([]chunk, error) {
	if !Is(data) {
		return nil, errorf("not a WebP file")
	}
	size := int(binary.LittleEndian.Uint32(data[4:8]))
	end := min(len(data), 8+size)
	var cs []chunk
	for p := 12; p+8 <= end; {
		id := string(data[p : p+4])
		n := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
		if n < 0 || p+8+n > end {
			return nil, errorf("chunk %q size", id)
		}
		cs = append(cs, chunk{id, data[p+8 : p+8+n]})
		p += 8 + n + n&1
	}
	if len(cs) == 0 {
		return nil, errorf("no chunks")
	}
	return cs, nil
}

// Frame is an animation frame: an image placed on the canvas.
type Frame struct {
	Image        *image.NRGBA
	X, Y         int  // offset on the canvas
	Duration     int  // milliseconds
	Blend        bool // alpha-blend over the canvas (else replace)
	DisposeToBkg bool // clear the frame's area to the background after display
	HasAlpha     bool // the bitstream carries alpha
}

// Image is a decoded WebP file.
type Image struct {
	Width, Height int // canvas
	// Frames: one for still images (at 0, 0, the canvas size)
	Frames     []Frame
	Animated   bool
	Loops      int         // animations: 0 = forever
	Background color.NRGBA // animations (a hint; players may ignore it)
	HasAlpha   bool
	ICC        []byte
	Exif       []byte
	XMP        []byte
}

// Decode decodes a WebP file (all frames of an animation).
func Decode(data []byte) (*Image, error) {
	cs, err := chunks(data)
	if err != nil {
		return nil, err
	}
	im := &Image{}
	switch cs[0].id {
	case "VP8 ", "VP8L":
		f, alpha, err := decodeFrame(cs[0], nil)
		if err != nil {
			return nil, err
		}
		b := f.Bounds()
		im.Width, im.Height, im.HasAlpha = b.Dx(), b.Dy(), alpha
		im.Frames = []Frame{{Image: f}}
		return im, nil
	case "VP8X":
	default:
		return nil, errorf("first chunk %q", cs[0].id)
	}
	x := cs[0].data
	if len(x) < 10 {
		return nil, errorf("VP8X size")
	}
	flags := x[0]
	im.Width = int(uint32(x[4])|uint32(x[5])<<8|uint32(x[6])<<16) + 1
	im.Height = int(uint32(x[7])|uint32(x[8])<<8|uint32(x[9])<<16) + 1
	im.Animated = flags&2 != 0
	im.HasAlpha = flags&16 != 0
	if im.Width*im.Height > MaxPixels {
		return nil, errorf("canvas too large")
	}
	var alph []byte
	for _, c := range cs[1:] {
		switch c.id {
		case "ICCP":
			im.ICC = c.data
		case "EXIF":
			im.Exif = c.data
		case "XMP ":
			im.XMP = c.data
		case "ANIM":
			if len(c.data) < 6 {
				return nil, errorf("ANIM size")
			}
			bg := c.data[0:4] // BGRA
			im.Background = color.NRGBA{bg[2], bg[1], bg[0], bg[3]}
			im.Loops = int(binary.LittleEndian.Uint16(c.data[4:6]))
		case "ALPH":
			alph = c.data
		case "VP8 ", "VP8L":
			if im.Animated || len(im.Frames) > 0 {
				continue
			}
			f, _, err := decodeFrame(c, alph)
			if err != nil {
				return nil, err
			}
			if b := f.Bounds(); b.Dx() != im.Width || b.Dy() != im.Height {
				return nil, errorf("image size differs from the canvas")
			}
			im.Frames = []Frame{{Image: f}}
		case "ANMF":
			if !im.Animated {
				continue
			}
			fr, err := decodeANMF(c.data)
			if err != nil {
				return nil, err
			}
			b := fr.Image.Bounds()
			if fr.X+b.Dx() > im.Width || fr.Y+b.Dy() > im.Height {
				return nil, errorf("frame outside the canvas")
			}
			im.Frames = append(im.Frames, fr)
		}
	}
	if len(im.Frames) == 0 {
		return nil, errorf("no image data")
	}
	return im, nil
}

func decodeANMF(d []byte) (Frame, error) {
	if len(d) < 16 {
		return Frame{}, errorf("ANMF size")
	}
	u24 := func(b []byte) int { return int(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16) }
	fr := Frame{
		X:            2 * u24(d[0:]),
		Y:            2 * u24(d[3:]),
		Duration:     u24(d[12:]),
		Blend:        d[15]&2 == 0,
		DisposeToBkg: d[15]&1 != 0,
	}
	w, h := u24(d[6:])+1, u24(d[9:])+1
	sub, err := chunksOf(d[16:])
	if err != nil {
		return Frame{}, err
	}
	var alph []byte
	for _, c := range sub {
		switch c.id {
		case "ALPH":
			alph = c.data
		case "VP8 ", "VP8L":
			img, alpha, err := decodeFrame(c, alph)
			if err != nil {
				return Frame{}, err
			}
			if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
				return Frame{}, errorf("frame size")
			}
			fr.Image, fr.HasAlpha = img, alpha
			return fr, nil
		}
	}
	return Frame{}, errorf("frame without image data")
}

// chunksOf splits a sequence of chunks (no RIFF header).
func chunksOf(d []byte) ([]chunk, error) {
	var cs []chunk
	for p := 0; p+8 <= len(d); {
		id := string(d[p : p+4])
		n := int(binary.LittleEndian.Uint32(d[p+4 : p+8]))
		if n < 0 || p+8+n > len(d) {
			return nil, errorf("chunk %q size", id)
		}
		cs = append(cs, chunk{id, d[p+8 : p+8+n]})
		p += 8 + n + n&1
	}
	return cs, nil
}

// decodeFrame decodes a VP8 or VP8L chunk (with an ALPH chunk for VP8).
func decodeFrame(c chunk, alph []byte) (*image.NRGBA, bool, error) {
	if c.id == "VP8L" {
		w, h, pix, alpha, err := DecodeVP8L(c.data)
		if err != nil {
			return nil, false, err
		}
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for i, v := range pix {
			o := img.Pix[4*i : 4*i+4 : 4*i+4]
			o[0], o[1], o[2], o[3] = uint8(v>>16), uint8(v>>8), uint8(v), uint8(v>>24)
		}
		return img, alpha, nil
	}
	return decodeLossy(c.data, alph)
}

// Canvases renders the frames onto the canvas as libwebp's animation
// decoder does: the canvas starts transparent, a frame replaces or (non
// premultiplied) alpha-blends its area, disposal clears the area to
// transparent (the background colour is only a hint). A key frame (one
// the canvas under it does not matter for) is drawn without blending.
func (im *Image) Canvases() []*image.NRGBA {
	w, h := im.Width, im.Height
	full := func(f *Frame) bool {
		b := f.Image.Bounds()
		return b.Dx() == w && b.Dy() == h
	}
	disposed := image.NewNRGBA(image.Rect(0, 0, w, h)) // the canvas before a frame
	var out []*image.NRGBA
	prevKey := false
	for i := range im.Frames {
		f := &im.Frames[i]
		key := i == 0
		if !key && (!f.HasAlpha || !f.Blend) && full(f) {
			key = true
		}
		if !key {
			p := &im.Frames[i-1]
			key = p.DisposeToBkg && (full(p) || prevKey)
		}
		cur := image.NewNRGBA(image.Rect(0, 0, w, h))
		if !key {
			copy(cur.Pix, disposed.Pix)
		}
		b := f.Image.Bounds()
		blend := !key && f.Blend && i > 0
		for y := 0; y < b.Dy(); y++ {
			src := f.Image.Pix[y*f.Image.Stride : y*f.Image.Stride+4*b.Dx()]
			dst := cur.Pix[(f.Y+y)*cur.Stride+4*f.X : (f.Y+y)*cur.Stride+4*(f.X+b.Dx())]
			if !blend {
				copy(dst, src)
				continue
			}
			for x := 0; x < len(src); x += 4 {
				blendPixel(src[x:x+4], dst[x:x+4])
			}
		}
		out = append(out, cur)
		prevKey = key
		copy(disposed.Pix, cur.Pix)
		if f.DisposeToBkg {
			for y := f.Y; y < f.Y+b.Dy(); y++ {
				clear(disposed.Pix[y*disposed.Stride+4*f.X : y*disposed.Stride+4*(f.X+b.Dx())])
			}
		}
	}
	return out
}

// blendPixel draws non-premultiplied src over dst (libwebp's integer
// approximation).
func blendPixel(src, dst []uint8) {
	sa := uint32(src[3])
	switch sa {
	case 0:
		return
	case 255: // libwebp copies opaque pixels
		copy(dst, src)
		return
	}
	da := uint32(dst[3])
	dfa := da * (256 - sa) >> 8
	ba := sa + dfa
	scale := (uint32(1) << 24) / ba
	for c := 0; c < 3; c++ {
		dst[c] = uint8((uint32(src[c])*sa + uint32(dst[c])*dfa) * scale >> 24)
	}
	dst[3] = uint8(ba)
}

// Metadata returns the ICC profile, Exif and XMP chunks of a WebP file
// without decoding it (nil when absent or the file is malformed).
func Metadata(data []byte) (icc, exif, xmp []byte) {
	cs, err := chunks(data)
	if err != nil || cs[0].id != "VP8X" {
		return nil, nil, nil
	}
	for _, c := range cs[1:] {
		switch c.id {
		case "ICCP":
			icc = c.data
		case "EXIF":
			exif = c.data
		case "XMP ":
			xmp = c.data
		}
	}
	return
}

// Lossy reports whether the (first) image of a WebP file is VP8 coded.
func Lossy(data []byte) bool {
	cs, err := chunks(data)
	if err != nil {
		return false
	}
	for _, c := range cs {
		switch c.id {
		case "VP8 ":
			return true
		case "VP8L":
			return false
		case "ANMF":
			sub, err := chunksOf(c.data[min(16, len(c.data)):])
			if err != nil {
				return false
			}
			for _, sc := range sub {
				if sc.id == "VP8 " {
					return true
				}
				if sc.id == "VP8L" {
					return false
				}
			}
		}
	}
	return false
}
