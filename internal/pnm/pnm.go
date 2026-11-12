// Package pnm reads and writes the Netpbm formats: PGM and PPM (binary P5
// and P6, plain P2 and P3), PAM (P7) and PFM (Pf, PF).
package pnm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
)

// ErrFormat reports malformed input.
var ErrFormat = errors.New("pnm: malformed file")

// Image is a decoded anymap: interleaved samples with Channels per pixel
// (1 grey, 2 grey+alpha, 3 RGB, 4 RGBA).
type Image struct {
	W, H     int
	Channels int
	MaxVal   int      // integer formats
	Samples  []uint16 // integer formats, row-major from the top
	Float    []float32
	// IsFloat is set for PFM; Float then holds the samples (rows from the
	// top: the file's bottom-up order is undone).
	IsFloat bool
}

// Is reports whether data starts like a supported anymap.
func Is(data []byte) bool {
	if len(data) < 3 || data[0] != 'P' {
		return false
	}
	switch data[1] {
	case '2', '3', '5', '6', '7', 'f', 'F':
		return isSpace(data[2])
	}
	return false
}

// BitDepth is the number of bits that hold maxval (1..16).
func (im *Image) BitDepth() int { return bits.Len(uint(im.MaxVal)) }

// MaxPixels bounds the image area Decode accepts.
var MaxPixels = 1 << 28

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

type reader struct {
	b []byte
	i int
}

// skip passes whitespace and comments.
func (r *reader) skip() {
	for r.i < len(r.b) {
		switch c := r.b[r.i]; {
		case c == '#':
			for r.i < len(r.b) && r.b[r.i] != '\n' && r.b[r.i] != '\r' {
				r.i++
			}
		case isSpace(c):
			r.i++
		default:
			return
		}
	}
}

func (r *reader) token() string {
	r.skip()
	s := r.i
	for r.i < len(r.b) && !isSpace(r.b[r.i]) && r.b[r.i] != '#' {
		r.i++
	}
	return string(r.b[s:r.i])
}

func (r *reader) int() (int, error) {
	t := r.token()
	v, err := strconv.Atoi(t)
	if err != nil || v < 0 {
		return 0, ErrFormat
	}
	return v, nil
}

// Decode parses an anymap.
func Decode(data []byte) (*Image, error) {
	if !Is(data) {
		return nil, ErrFormat
	}
	r := &reader{b: data, i: 2}
	im := &Image{}
	kind := data[1]
	var err error
	switch kind {
	case '7':
		if err := r.pamHeader(im); err != nil {
			return nil, err
		}
	case 'f', 'F':
		return r.pfm(im, kind == 'F')
	default:
		if im.W, err = r.int(); err != nil {
			return nil, err
		}
		if im.H, err = r.int(); err != nil {
			return nil, err
		}
		if im.MaxVal, err = r.int(); err != nil {
			return nil, err
		}
		im.Channels = 1
		if kind == '3' || kind == '6' {
			im.Channels = 3
		}
	}
	if im.W <= 0 || im.H <= 0 || im.W > MaxPixels || im.H > MaxPixels || im.W*im.H > MaxPixels || im.MaxVal < 1 || im.MaxVal > 65535 || im.Channels < 1 || im.Channels > 4 {
		return nil, fmt.Errorf("%w: header", ErrFormat)
	}
	n := im.W * im.H * im.Channels
	if kind == '2' || kind == '3' {
		// every sample takes a digit and a separator: refuse before
		// allocating for more samples than the file can hold
		if n > (len(data)-r.i+1)/2 {
			return nil, fmt.Errorf("%w: truncated", ErrFormat)
		}
		im.Samples = make([]uint16, n)
		for i := range im.Samples {
			v, err := r.int()
			if err != nil || v > im.MaxVal {
				return nil, fmt.Errorf("%w: sample", ErrFormat)
			}
			im.Samples[i] = uint16(v)
		}
		return im, nil
	}
	// binary samples follow a single whitespace character (PAM: ENDHDR\n)
	if kind != '7' {
		if r.i >= len(data) || !isSpace(data[r.i]) {
			return nil, ErrFormat
		}
		r.i++
	}
	wide := im.MaxVal > 255
	size := n
	if wide {
		size *= 2
	}
	if len(data)-r.i < size {
		return nil, fmt.Errorf("%w: truncated", ErrFormat)
	}
	p := data[r.i : r.i+size]
	im.Samples = make([]uint16, n)
	if wide {
		for i := range im.Samples {
			im.Samples[i] = binary.BigEndian.Uint16(p[2*i:])
		}
	} else {
		for i, v := range p {
			im.Samples[i] = uint16(v)
		}
	}
	for _, v := range im.Samples {
		if int(v) > im.MaxVal {
			return nil, fmt.Errorf("%w: sample above maxval", ErrFormat)
		}
	}
	return im, nil
}

func (r *reader) pamHeader(im *Image) error {
	tuple := ""
	for {
		t := r.token()
		var err error
		switch t {
		case "":
			return fmt.Errorf("%w: no ENDHDR", ErrFormat)
		case "WIDTH":
			im.W, err = r.int()
		case "HEIGHT":
			im.H, err = r.int()
		case "DEPTH":
			im.Channels, err = r.int()
		case "MAXVAL":
			im.MaxVal, err = r.int()
		case "TUPLTYPE":
			tuple = r.token()
		case "ENDHDR":
			// one newline ends the header
			if r.i < len(r.b) && r.b[r.i] == '\r' {
				r.i++
			}
			if r.i >= len(r.b) || r.b[r.i] != '\n' {
				return fmt.Errorf("%w: ENDHDR", ErrFormat)
			}
			r.i++
			switch tuple {
			case "", "GRAYSCALE", "GRAYSCALE_ALPHA", "RGB", "RGB_ALPHA", "BLACKANDWHITE", "BLACKANDWHITE_ALPHA":
				return nil
			}
			return fmt.Errorf("pnm: unsupported PAM tuple type %q", tuple)
		default:
			return fmt.Errorf("%w: PAM header field %q", ErrFormat, t)
		}
		if err != nil {
			return err
		}
	}
}

func (r *reader) pfm(im *Image, colour bool) (*Image, error) {
	var err error
	if im.W, err = r.int(); err != nil {
		return nil, err
	}
	if im.H, err = r.int(); err != nil {
		return nil, err
	}
	scale, err := strconv.ParseFloat(r.token(), 64)
	if err != nil || scale == 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		return nil, fmt.Errorf("%w: PFM scale", ErrFormat)
	}
	if r.i >= len(r.b) || !isSpace(r.b[r.i]) {
		return nil, ErrFormat
	}
	r.i++
	im.Channels = 1
	if colour {
		im.Channels = 3
	}
	if im.W <= 0 || im.H <= 0 || im.W > MaxPixels || im.H > MaxPixels || im.W*im.H > MaxPixels {
		return nil, fmt.Errorf("%w: header", ErrFormat)
	}
	n := im.W * im.H * im.Channels
	if len(r.b)-r.i < 4*n {
		return nil, fmt.Errorf("%w: truncated", ErrFormat)
	}
	var order binary.ByteOrder = binary.BigEndian
	if scale < 0 {
		order = binary.LittleEndian
	}
	p := r.b[r.i:]
	im.IsFloat = true
	im.Float = make([]float32, n)
	row := im.W * im.Channels
	for y := 0; y < im.H; y++ {
		src := p[4*(im.H-1-y)*row:] // rows are stored bottom-up
		dst := im.Float[y*row : (y+1)*row]
		for i := range dst {
			dst[i] = math.Float32frombits(order.Uint32(src[4*i:]))
		}
	}
	return im, nil
}

// Header writes the header of a binary anymap of w x h pixels with ch
// channels (1-4; 2 and 4 make a PAM) and maxval 255 or 65535.
func Header(w io.Writer, width, height, ch, maxval int) error {
	var b bytes.Buffer
	switch ch {
	case 1:
		fmt.Fprintf(&b, "P5\n%d %d\n%d\n", width, height, maxval)
	case 3:
		fmt.Fprintf(&b, "P6\n%d %d\n%d\n", width, height, maxval)
	default:
		tuple := "GRAYSCALE_ALPHA"
		if ch == 4 {
			tuple = "RGB_ALPHA"
		}
		fmt.Fprintf(&b, "P7\nWIDTH %d\nHEIGHT %d\nDEPTH %d\nMAXVAL %d\nTUPLTYPE %s\nENDHDR\n", width, height, ch, maxval, tuple)
	}
	_, err := w.Write(b.Bytes())
	return err
}

// PFMHeader writes the header of a little-endian PFM (grey: ch 1).
func PFMHeader(w io.Writer, width, height, ch int) error {
	kind := "Pf"
	if ch == 3 {
		kind = "PF"
	}
	_, err := fmt.Fprintf(w, "%s\n%d %d\n-1.0\n", kind, width, height)
	return err
}
