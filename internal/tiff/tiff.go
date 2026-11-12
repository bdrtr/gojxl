// Package tiff reads baseline TIFF images (the first image of a file):
// 1-16 bit grey, palette and RGB samples with optional alpha, stored in
// strips or tiles, chunky or planar, uncompressed or compressed with LZW,
// Deflate or PackBits (with the horizontal differencing predictor).
package tiff

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"gojxl/internal/pngx"
)

// ErrFormat reports a malformed or unsupported file.
var ErrFormat = errors.New("tiff: invalid or unsupported file")

func errorf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrFormat}, a...)...)
}

// MaxPixels bounds the image area accepted.
var MaxPixels = 1 << 28

// Image is a decoded image: interleaved samples, Channels per pixel (1
// grey, 2 grey+alpha, 3 RGB, 4 RGBA), BitDepth bits each (8 or 16; other
// depths are scaled up to 8).
type Image struct {
	W, H        int
	Channels    int
	BitDepth    int
	Samples     []uint16
	Alpha       bool
	ICC         []byte
	XMP         []byte
	Orientation int // 1-8 (0: not given)
}

// Is reports whether data starts with a TIFF header.
func Is(data []byte) bool {
	return len(data) >= 8 && (string(data[:4]) == "II*\x00" || string(data[:4]) == "MM\x00*")
}

type field struct {
	typ   uint16
	count uint32
	data  []byte // raw value bytes
}

type reader struct {
	data []byte
	bo   binary.ByteOrder
}

func (r *reader) u16(b []byte) uint32 { return uint32(r.bo.Uint16(b)) }
func (r *reader) u32(b []byte) uint32 { return r.bo.Uint32(b) }

// values returns the integer values of a SHORT or LONG (or BYTE) field.
func (r *reader) values(f *field) []uint32 {
	var out []uint32
	if f == nil {
		return nil
	}
	switch f.typ {
	case 1, 7: // BYTE, UNDEFINED
		for _, b := range f.data {
			out = append(out, uint32(b))
		}
	case 3: // SHORT
		for i := 0; i+2 <= len(f.data); i += 2 {
			out = append(out, r.u16(f.data[i:]))
		}
	case 4, 13: // LONG, IFD
		for i := 0; i+4 <= len(f.data); i += 4 {
			out = append(out, r.u32(f.data[i:]))
		}
	}
	return out
}

func typeSize(t uint16) int {
	switch t {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11, 13:
		return 4
	case 5, 10, 12, 16, 17, 18:
		return 8
	}
	return 0
}

// Tags used.
const (
	tWidth        = 256
	tHeight       = 257
	tBits         = 258
	tCompression  = 259
	tPhotometric  = 262
	tStripOffsets = 273
	tOrientation  = 274
	tSamples      = 277
	tRowsPerStrip = 278
	tStripCounts  = 279
	tPlanar       = 284
	tPredictor    = 317
	tColorMap     = 320
	tTileWidth    = 322
	tTileLength   = 323
	tTileOffsets  = 324
	tTileCounts   = 325
	tExtraSamples = 338
	tSampleFormat = 339
	tXMP          = 700
	tICC          = 34675
)

// Decode decodes the first image of a TIFF file.
func Decode(data []byte) (*Image, error) {
	if !Is(data) {
		return nil, errorf("not a TIFF file")
	}
	r := &reader{data: data, bo: binary.LittleEndian}
	if data[0] == 'M' {
		r.bo = binary.BigEndian
	}
	off := int(r.u32(data[4:]))
	if off < 8 || off+2 > len(data) {
		return nil, errorf("IFD offset")
	}
	n := int(r.u16(data[off:]))
	if off+2+12*n > len(data) {
		return nil, errorf("IFD size")
	}
	fs := map[uint16]*field{}
	for i := 0; i < n; i++ {
		e := data[off+2+12*i:]
		tag, typ, count := uint16(r.u16(e)), uint16(r.u16(e[2:])), r.u32(e[4:])
		sz := typeSize(typ)
		if sz == 0 {
			continue
		}
		total := uint64(sz) * uint64(count)
		var v []byte
		if total <= 4 {
			v = e[8 : 8+total]
		} else {
			p := uint64(r.u32(e[8:]))
			if p+total > uint64(len(data)) {
				return nil, errorf("tag %d data", tag)
			}
			v = data[p : p+total]
		}
		fs[tag] = &field{typ, count, v}
	}
	get := func(tag uint16, def uint32) uint32 {
		if f := fs[tag]; f != nil {
			if v := r.values(f); len(v) > 0 {
				return v[0]
			}
		}
		return def
	}
	im := &Image{
		W: int(get(tWidth, 0)), H: int(get(tHeight, 0)),
		Orientation: int(get(tOrientation, 0)),
	}
	if f := fs[tICC]; f != nil {
		im.ICC = f.data
	}
	if f := fs[tXMP]; f != nil {
		im.XMP = f.data
	}
	spp := int(get(tSamples, 1))
	bits := int(get(tBits, 1))
	if f := fs[tBits]; f != nil {
		for _, b := range r.values(f) {
			if int(b) != bits {
				return nil, errorf("mixed bits per sample")
			}
		}
	}
	if get(tSampleFormat, 1) != 1 {
		return nil, errorf("only unsigned integer samples are supported")
	}
	if im.W <= 0 || im.H <= 0 || im.W > MaxPixels || im.H > MaxPixels || im.W*im.H > MaxPixels {
		return nil, errorf("image size")
	}
	if spp < 1 || spp > 8 || (bits != 1 && bits != 2 && bits != 4 && bits != 8 && bits != 16) {
		return nil, errorf("%d samples of %d bits", spp, bits)
	}
	photo := get(tPhotometric, 1)
	comp := get(tCompression, 1)
	pred := get(tPredictor, 1)
	planar := get(tPlanar, 1)
	if pred != 1 && pred != 2 {
		return nil, errorf("predictor %d", pred)
	}

	// raw samples, one plane per sample when planar
	planes := 1
	cps := spp // samples per stored pixel
	if planar == 2 {
		planes, cps = spp, 1
	}
	rowBytes := (im.W*cps*bits + 7) / 8
	raw := make([][]byte, planes)
	for p := range raw {
		raw[p] = make([]byte, rowBytes*im.H)
	}
	if fs[tTileOffsets] != nil {
		tw, tl := int(get(tTileWidth, 0)), int(get(tTileLength, 0))
		if tw <= 0 || tl <= 0 || tw%16 != 0 || tl%16 != 0 {
			return nil, errorf("tile size")
		}
		offs, cnts := r.values(fs[tTileOffsets]), r.values(fs[tTileCounts])
		across, down := (im.W+tw-1)/tw, (im.H+tl-1)/tl
		if len(offs) < across*down*planes || len(cnts) < len(offs) {
			return nil, errorf("tile offsets")
		}
		tileRow := (tw*cps*bits + 7) / 8
		buf := make([]byte, tileRow*tl)
		for i := 0; i < across*down*planes; i++ {
			p, t := i/(across*down), i%(across*down)
			if err := decompress(data, offs[i], cnts[i], comp, buf); err != nil {
				return nil, err
			}
			if pred == 2 {
				unpredict(buf, tileRow, tl, cps, bits, r.bo)
			}
			tx, ty := (t%across)*tw, (t/across)*tl
			// copy the part inside the image (whole bytes: tile widths are
			// multiples of 16, so tile starts fall on byte boundaries)
			x0 := tx * cps * bits / 8
			nb := min(tileRow, rowBytes-x0)
			for y := 0; y < tl && ty+y < im.H; y++ {
				copy(raw[p][(ty+y)*rowBytes+x0:(ty+y)*rowBytes+x0+nb], buf[y*tileRow:])
			}
		}
	} else {
		rps := int(get(tRowsPerStrip, uint32(im.H)))
		if rps <= 0 || rps > im.H {
			rps = im.H
		}
		offs, cnts := r.values(fs[tStripOffsets]), r.values(fs[tStripCounts])
		per := (im.H + rps - 1) / rps
		if len(offs) < per*planes {
			return nil, errorf("strip offsets")
		}
		if len(cnts) < len(offs) {
			// missing counts: the strips run to the next offset or the end
			cnts = make([]uint32, len(offs))
			for i := range offs {
				cnts[i] = uint32(len(data)) - offs[i]
			}
		}
		for i := 0; i < per*planes; i++ {
			p, s := i/per, i%per
			rows := min(rps, im.H-s*rps)
			dst := raw[p][s*rps*rowBytes : (s*rps+rows)*rowBytes]
			if err := decompress(data, offs[i], cnts[i], comp, dst); err != nil {
				return nil, err
			}
			if pred == 2 {
				unpredict(dst, rowBytes, rows, cps, bits, r.bo)
			}
		}
	}
	return im, im.convert(raw, rowBytes, spp, bits, planar == 2, photo, r, fs)
}

// convert turns the raw sample planes into interleaved 8 or 16-bit samples.
func (im *Image) convert(raw [][]byte, rowBytes, spp, bits int, planar bool, photo uint32, r *reader, fs map[uint16]*field) error {
	// sample k of pixel x in row y, as stored (bits wide)
	sample := func(x, y, k int) uint32 {
		p, idx := 0, x*spp+k
		if planar {
			p, idx = k, x
		}
		row := raw[p][y*rowBytes:]
		switch bits {
		case 8:
			return uint32(row[idx])
		case 16:
			return uint32(r.u16(row[2*idx:]))
		}
		bit := idx * bits
		return uint32(row[bit/8]) >> (8 - bits - bit%8) & (1<<bits - 1)
	}
	extra := 0
	alpha := false
	if f := fs[tExtraSamples]; f != nil {
		v := r.values(f)
		extra = len(v)
		alpha = len(v) > 0 && (v[0] == 1 || v[0] == 2)
		if len(v) > 0 && v[0] == 1 {
			return errorf("associated (premultiplied) alpha is not supported")
		}
	}
	var colours int
	switch photo {
	case 0, 1: // grey (0: white is zero)
		colours = 1
	case 2:
		colours = 3
	case 3:
		colours = 1
	default:
		return errorf("photometric interpretation %d", photo)
	}
	if spp < colours || spp > colours+extra && extra > 0 || extra == 0 && spp != colours {
		return errorf("%d samples for photometric %d", spp, photo)
	}
	im.Alpha = alpha
	im.Channels = colours
	if photo == 3 {
		im.Channels = 3
	}
	if alpha {
		im.Channels++
	}
	maxIn := uint32(1)<<bits - 1
	im.BitDepth = 8
	if bits == 16 {
		im.BitDepth = 16
	}
	var cmap []uint32
	if photo == 3 {
		f := fs[tColorMap]
		if f == nil {
			return errorf("palette without a colour map")
		}
		cmap = r.values(f)
		if len(cmap) < 3<<bits {
			return errorf("colour map size")
		}
		// 16-bit entries: 8-bit output if every entry is a multiple of 257
		im.BitDepth = 8
		for _, v := range cmap[:3<<bits] {
			if v%257 != 0 {
				im.BitDepth = 16
				break
			}
		}
	}
	top := uint32(1)<<im.BitDepth - 1
	scale := func(v uint32) uint16 {
		if maxIn == top {
			return uint16(v)
		}
		return uint16((v*top + maxIn/2) / maxIn)
	}
	im.Samples = make([]uint16, im.W*im.H*im.Channels)
	o := 0
	n := 1 << bits
	for y := 0; y < im.H; y++ {
		for x := 0; x < im.W; x++ {
			switch photo {
			case 3:
				i := sample(x, y, 0)
				for c := 0; c < 3; c++ {
					v := cmap[c*n+int(i)]
					if im.BitDepth == 8 {
						v /= 257
					}
					im.Samples[o] = uint16(v)
					o++
				}
			case 0:
				im.Samples[o] = scale(maxIn - sample(x, y, 0))
				o++
			default:
				for c := 0; c < colours; c++ {
					im.Samples[o] = scale(sample(x, y, c))
					o++
				}
			}
			if alpha {
				im.Samples[o] = scale(sample(x, y, colours))
				o++
			}
		}
	}
	return nil
}

// decompress fills dst with the strip or tile at off.
func decompress(data []byte, off, count, comp uint32, dst []byte) error {
	if uint64(off)+uint64(count) > uint64(len(data)) {
		return errorf("strip outside the file")
	}
	src := data[off : off+count]
	switch comp {
	case 1:
		if len(src) < len(dst) {
			return errorf("short strip")
		}
		copy(dst, src)
	case 5:
		return lzwDecode(src, dst)
	case 8, 32946:
		if pngx.InflateZlib(src, dst) == nil {
			return nil
		}
		// tolerate streams that hold more (or a few bytes less) data
		zr, err := zlib.NewReader(bytes.NewReader(src))
		if err != nil {
			return errorf("deflate: %v", err)
		}
		if _, err := io.ReadFull(zr, dst); err != nil {
			return errorf("deflate: %v", err)
		}
	case 32773:
		return packBits(src, dst)
	default:
		return errorf("compression %d", comp)
	}
	return nil
}

func packBits(src, dst []byte) error {
	o := 0
	for i := 0; i < len(src) && o < len(dst); {
		n := int(int8(src[i]))
		i++
		switch {
		case n >= 0:
			if i+n+1 > len(src) {
				return errorf("PackBits literal")
			}
			o += copy(dst[o:], src[i:i+n+1])
			i += n + 1
		case n != -128:
			if i >= len(src) {
				return errorf("PackBits run")
			}
			for k := 0; k < 1-n && o < len(dst); k++ {
				dst[o] = src[i]
				o++
			}
			i++
		}
	}
	if o < len(dst) {
		return errorf("PackBits data short")
	}
	return nil
}

// lzwDecode decodes TIFF LZW (MSB-first codes, code width grows one code
// early).
func lzwDecode(src, dst []byte) error {
	const clear, eoi = 256, 257
	var prefix [4096]uint16
	var suffix [4096]byte
	var length [4096]uint16
	for i := 0; i < 256; i++ {
		suffix[i], length[i] = byte(i), 1
	}
	var buf uint64
	nbits := uint(0)
	pos, o := 0, 0
	width := uint(9)
	next := 258
	prev := -1
	var stack [4096]byte
	for o < len(dst) {
		for nbits < width {
			var b byte
			if pos < len(src) {
				b = src[pos]
			} else if pos > len(src)+4 {
				return errorf("LZW data short")
			}
			pos++
			buf = buf<<8 | uint64(b)
			nbits += 8
		}
		code := int(buf >> (nbits - width) & (1<<width - 1))
		nbits -= width
		switch {
		case code == clear:
			width, next, prev = 9, 258, -1
			continue
		case code == eoi:
			if o < len(dst) {
				return errorf("LZW data short")
			}
			return nil
		}
		var first byte
		if code < next && (code < 256 || code >= 258) {
			// emit the string for code
			n := int(length[code])
			k := n
			for c := code; k > 0; k-- {
				stack[k-1] = suffix[c]
				c = int(prefix[c])
			}
			first = stack[0]
			o += copy(dst[o:], stack[:n])
		} else if code == next && prev >= 0 {
			// the KwKwK case: prev's string plus its first byte
			n := int(length[prev])
			k := n
			for c := prev; k > 0; k-- {
				stack[k-1] = suffix[c]
				c = int(prefix[c])
			}
			first = stack[0]
			stack[n] = first
			o += copy(dst[o:], stack[:n+1])
		} else {
			return errorf("LZW code %d", code)
		}
		if prev >= 0 && next < 4096 {
			prefix[next] = uint16(prev)
			suffix[next] = first
			length[next] = length[prev] + 1
			next++
		}
		prev = code
		if next+1 >= 1<<width && width < 12 {
			width++
		}
	}
	return nil
}

// unpredict undoes horizontal differencing (8 or 16-bit samples).
func unpredict(b []byte, rowBytes, rows, cps, bits int, bo binary.ByteOrder) {
	for y := 0; y < rows; y++ {
		row := b[y*rowBytes : (y+1)*rowBytes]
		switch bits {
		case 8:
			for i := cps; i < len(row); i++ {
				row[i] += row[i-cps]
			}
		case 16:
			for i := 2 * cps; i+2 <= len(row); i += 2 {
				bo.PutUint16(row[i:], bo.Uint16(row[i:])+bo.Uint16(row[i-2*cps:]))
			}
		}
	}
}

// Metadata returns the ICC profile and XMP packet of the first image
// without decoding its samples (nil when absent or the file is malformed).
func Metadata(data []byte) (icc, xmp []byte) {
	if !Is(data) {
		return nil, nil
	}
	r := &reader{data: data, bo: binary.LittleEndian}
	if data[0] == 'M' {
		r.bo = binary.BigEndian
	}
	off := int(r.u32(data[4:]))
	if off < 8 || off+2 > len(data) {
		return nil, nil
	}
	n := int(r.u16(data[off:]))
	for i := 0; i < n && off+2+12*i+12 <= len(data); i++ {
		e := data[off+2+12*i:]
		tag, count := uint16(r.u16(e)), uint64(r.u32(e[4:]))
		if tag != tICC && tag != tXMP || count <= 4 {
			continue
		}
		p := uint64(r.u32(e[8:]))
		if p+count > uint64(len(data)) {
			continue
		}
		if tag == tICC {
			icc = data[p : p+count]
		} else {
			xmp = data[p : p+count]
		}
	}
	return
}
