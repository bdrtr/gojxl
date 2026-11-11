package pngx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
)

// DecodeAPNG decodes the frames of an animated PNG, composited onto full
// canvases (*image.NRGBA, or *image.NRGBA64 for 16-bit files), with their
// delays in seconds and the number of plays (0 = forever). ok is false if
// the file is not an animated PNG.
func DecodeAPNG(data []byte) (frames []image.Image, delays []float64, loops int, ok bool, err error) {
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, nil, 0, false, errors.New("png: not a PNG file")
	}
	type chunk struct {
		typ  string
		body []byte
	}
	var chunks []chunk
	for p := 8; p+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		if n < 0 || p+12+n > len(data) {
			return nil, nil, 0, false, errors.New("png: truncated chunk")
		}
		chunks = append(chunks, chunk{string(data[p+4 : p+8]), data[p+8 : p+8+n]})
		p += 12 + n
	}
	if len(chunks) == 0 || chunks[0].typ != "IHDR" || len(chunks[0].body) != 13 {
		return nil, nil, 0, false, errors.New("png: missing IHDR")
	}
	ihdr := chunks[0].body
	W, H := int(binary.BigEndian.Uint32(ihdr)), int(binary.BigEndian.Uint32(ihdr[4:]))
	deep := ihdr[8] == 16
	numFrames := -1
	var shared []chunk // chunks every frame image needs (PLTE, tRNS, ...)
	for _, c := range chunks[1:] {
		switch c.typ {
		case "acTL":
			if len(c.body) != 8 {
				return nil, nil, 0, false, errors.New("png: bad acTL")
			}
			numFrames = int(binary.BigEndian.Uint32(c.body))
			loops = int(binary.BigEndian.Uint32(c.body[4:]))
		case "PLTE", "tRNS", "gAMA", "cHRM", "sRGB", "iCCP", "sBIT":
			shared = append(shared, c)
		}
	}
	if numFrames < 1 || W <= 0 || H <= 0 || W*H > 1<<28 {
		return nil, nil, 0, false, nil
	}
	var canvas draw.Image
	if deep {
		canvas = image.NewNRGBA64(image.Rect(0, 0, W, H))
	} else {
		canvas = image.NewNRGBA(image.Rect(0, 0, W, H))
	}
	snapshot := func() draw.Image {
		switch m := canvas.(type) {
		case *image.NRGBA:
			c := *m
			c.Pix = append([]byte(nil), m.Pix...)
			return &c
		case *image.NRGBA64:
			c := *m
			c.Pix = append([]byte(nil), m.Pix...)
			return &c
		}
		return nil
	}
	type fctl struct {
		w, h, x, y     int
		num, den       uint16
		dispose, blend byte
	}
	var cur *fctl
	var payload [][]byte
	render := func() error {
		if cur == nil {
			return nil
		}
		var buf bytes.Buffer
		buf.WriteString("\x89PNG\r\n\x1a\n")
		h := append([]byte(nil), ihdr...)
		binary.BigEndian.PutUint32(h, uint32(cur.w))
		binary.BigEndian.PutUint32(h[4:], uint32(cur.h))
		writeChunk(&buf, "IHDR", h)
		for _, c := range shared {
			writeChunk(&buf, c.typ, c.body)
		}
		for _, d := range payload {
			writeChunk(&buf, "IDAT", d)
		}
		writeChunk(&buf, "IEND", nil)
		img, err := Decode(buf.Bytes())
		if err != nil {
			return err
		}
		r := image.Rect(cur.x, cur.y, cur.x+cur.w, cur.y+cur.h)
		if !r.In(canvas.Bounds()) {
			return errors.New("png: frame outside the canvas")
		}
		var prev draw.Image
		if cur.dispose == 2 {
			prev = snapshot()
		}
		op := draw.Src
		if cur.blend == 1 {
			op = draw.Over
		}
		draw.Draw(canvas, r, img, image.Point{}, op)
		frames = append(frames, snapshot())
		den := float64(cur.den)
		if den == 0 {
			den = 100
		}
		delays = append(delays, float64(cur.num)/den)
		switch cur.dispose {
		case 1:
			draw.Draw(canvas, r, image.Transparent, image.Point{}, draw.Src)
		case 2:
			canvas = prev
		}
		return nil
	}
	for _, c := range chunks[1:] {
		switch c.typ {
		case "fcTL":
			if err := render(); err != nil {
				return nil, nil, 0, true, err
			}
			if len(c.body) != 26 {
				return nil, nil, 0, true, errors.New("png: bad fcTL")
			}
			b := c.body
			cur = &fctl{
				w: int(binary.BigEndian.Uint32(b[4:])), h: int(binary.BigEndian.Uint32(b[8:])),
				x: int(binary.BigEndian.Uint32(b[12:])), y: int(binary.BigEndian.Uint32(b[16:])),
				num: binary.BigEndian.Uint16(b[20:]), den: binary.BigEndian.Uint16(b[22:]),
				dispose: b[24], blend: b[25],
			}
			if len(frames) == 0 && cur.dispose == 2 {
				cur.dispose = 1 // as specified for the first frame
			}
			payload = nil
		case "IDAT":
			if cur != nil {
				payload = append(payload, c.body)
			}
		case "fdAT":
			if cur != nil && len(c.body) >= 4 {
				payload = append(payload, c.body[4:])
			}
		}
	}
	if err := render(); err != nil {
		return nil, nil, 0, true, err
	}
	if len(frames) == 0 {
		return nil, nil, 0, false, nil
	}
	return frames, delays, loops, true, nil
}
