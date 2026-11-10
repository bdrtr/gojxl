package gojxl

import (
	"errors"
	"math"

	"gojxl/internal/brotli"
	"gojxl/internal/jpeg"
	"gojxl/internal/jxl"
)

// ErrNoJPEG is returned by ReconstructJPEG for files without JPEG
// reconstruction data.
var ErrNoJPEG = errors.New("gojxl: no JPEG reconstruction data (jbrd box)")

// HasJPEG reports whether a JPEG XL file carries JPEG reconstruction data.
func HasJPEG(data []byte) bool {
	c, err := jxl.ParseContainer(data)
	if err != nil {
		return false
	}
	for _, b := range c.Boxes {
		if b.Type == "jbrd" {
			return true
		}
	}
	return false
}

// ReconstructJPEG restores the original JPEG file from a JPEG XL file made
// by lossless JPEG recompression (by gojxl or libjxl), byte for byte.
func ReconstructJPEG(data []byte) (out []byte, err error) {
	defer recoverErr(&err)
	d, err := jxl.NewDecoder(data)
	if err != nil {
		return nil, err
	}
	var jbrd, exif, xmp []byte
	for _, b := range d.Container.Boxes {
		t, payload := b.Type, b.Data
		if t == "brob" && len(payload) >= 4 {
			t = string(payload[:4])
			if payload, err = brotli.Decode(payload[4:], 1<<28); err != nil {
				return nil, err
			}
		}
		switch t {
		case "jbrd":
			jbrd = payload
		case "Exif":
			if len(payload) >= 4 {
				exif = payload[4:]
			}
		case "xml ":
			xmp = payload
		}
	}
	if jbrd == nil {
		return nil, ErrNoJPEG
	}
	dec := func(b []byte) ([]byte, error) { return brotli.Decode(b, 1<<28) }
	img, err := jpeg.ParseJBRD(jbrd, dec, d.ICC, exif, xmp)
	if err != nil {
		return nil, err
	}
	var cp jxl.JPEGCoeffs
	d.CaptureJPEG(&cp)
	if _, err := d.NextFrame(); err != nil {
		return nil, err
	}
	if !cp.OK || cp.DC[0] == nil {
		return nil, errors.New("gojxl: frame is not a recompressed JPEG")
	}
	if math.Abs(float64(cp.RawDen)-1.0/(8*255)) > 1e-8 {
		return nil, errors.New("gojxl: quantisation table is not a JPEG table")
	}
	q := cp.RawQ
	for _, v := range q {
		if v <= 0 || v >= 65536 {
			return nil, errors.New("gojxl: invalid JPEG quantisation table")
		}
	}
	img.W, img.H = int(d.Header.Size.Width), int(d.Header.Size.Height)
	gray := len(img.Comps) == 1
	jc := [3]int{1, 0, 2}
	switch {
	case gray:
		jc = [3]int{-1, 0, -1}
	case !cp.YCbCr:
		jc = [3]int{0, 1, 2}
	}
	is444 := cp.HS == [3]int{} && cp.VS == [3]int{}
	// fixed point ratio of luma to chroma quantisers, natural order
	var sq [3][64]int32
	for c := 0; c < 3; c++ {
		for n := 0; n < 64; n++ {
			i := (n%8)*8 + n/8
			sq[c][n] = (1 << 11) * q[64+i] / q[64*c+i]
		}
	}
	used := 0
	for c := 0; c < 3; c++ {
		if jc[c] < 0 {
			continue
		}
		comp := &img.Comps[jc[c]]
		rh, rv := 0, 0
		switch cp.UpCode[c] {
		case 1:
			rh, rv = 1, 1
		case 2:
			rh = 1
		case 3:
			rv = 1
		}
		comp.H, comp.V = 1<<rh, 1<<rv
		comp.BW, comp.BH = cp.BW>>cp.HS[c], cp.BH>>cp.VS[c]
		comp.Coef = make([]int16, comp.BW*comp.BH*64)
		var dcoff int32
		if !cp.YCbCr {
			dcoff = 1024 / q[64*c]
		}
		for by := 0; by < comp.BH; by++ {
			for bx := 0; bx < comp.BW; bx++ {
				b := by*comp.BW + bx
				ac := cp.AC[c][b*64 : b*64+64]
				dst := comp.Coef[b*64 : b*64+64]
				tile := (by/8)*cp.TW + bx/8
				cfl := is444 && !gray && c != 1 && (cp.YtoX[tile] != 0 || cp.YtoB[tile] != 0)
				var scale int32
				if c == 0 {
					scale = int32(cp.YtoX[tile]) * (1 << 11) / 84
				} else if c == 2 {
					scale = int32(cp.YtoB[tile]) * (1 << 11) / 84
				}
				ys := cp.AC[1][b*64 : b*64+64]
				for n := 0; n < 64; n++ {
					v := ac[n]
					if cfl {
						cs := (scale*sq[c][n] + 1<<10) >> 11
						v += (ys[n]*cs + 1<<10) >> 11
					}
					if n == 0 {
						v = min(max(cp.DC[c][b]-dcoff, -2047), 2047)
					}
					if v < -2048 || v > 2047 {
						return nil, errors.New("gojxl: JPEG coefficient out of range")
					}
					dst[n] = int16(v)
				}
			}
		}
		qc := c
		if gray {
			qc = 1
		}
		if comp.QuantIdx >= len(img.Quants) {
			return nil, errors.New("gojxl: bad quantisation table index")
		}
		qt := &img.Quants[comp.QuantIdx]
		used |= 1 << comp.QuantIdx
		for x := 0; x < 8; x++ {
			for y := 0; y < 8; y++ {
				qt.Values[x*8+y] = uint16(q[qc*64+y*8+x])
			}
		}
	}
	for i := range img.Quants {
		if used&(1<<i) != 0 {
			continue
		}
		if i == 0 {
			return nil, errors.New("gojxl: first quantisation table unused")
		}
		img.Quants[i].Values = img.Quants[i-1].Values
	}
	return img.Write()
}
