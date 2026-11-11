package pngx

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"image"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	fill := func(p []byte, smooth bool) {
		for i := range p {
			if smooth {
				p[i] = byte(i/7 + r.Intn(3))
			} else {
				p[i] = byte(r.Intn(256))
			}
		}
	}
	for _, sz := range [][2]int{{1, 1}, {3, 5}, {257, 300}, {1000, 700}} {
		w, h := sz[0], sz[1]
		rect := image.Rect(0, 0, w, h)
		g := image.NewGray(rect)
		fill(g.Pix, true)
		g16 := image.NewGray16(rect)
		fill(g16.Pix, false)
		n := image.NewNRGBA(rect)
		fill(n.Pix, true)
		no := image.NewNRGBA(rect)
		fill(no.Pix, true)
		for i := 3; i < len(no.Pix); i += 4 {
			no.Pix[i] = 0xff
		}
		n64 := image.NewNRGBA64(rect)
		fill(n64.Pix, false)
		n64o := image.NewNRGBA64(rect)
		fill(n64o.Pix, true)
		for i := 6; i < len(n64o.Pix); i += 8 {
			n64o.Pix[i], n64o.Pix[i+1] = 0xff, 0xff
		}
		for _, im := range []image.Image{g, g16, n, no, n64, n64o} {
			var buf bytes.Buffer
			if err := Encode(&buf, im, flate.BestSpeed); err != nil {
				t.Fatal(err)
			}
			got, err := png.Decode(&buf)
			if err != nil {
				t.Fatalf("%dx%d %T: %v", w, h, im, err)
			}
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					a, b := im.At(x, y), got.At(x, y)
					r1, g1, b1, a1 := a.RGBA()
					r2, g2, b2, a2 := b.RGBA()
					if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
						t.Fatalf("%dx%d %T: pixel (%d,%d) %v != %v", w, h, im, x, y, a, b)
					}
				}
			}
		}
	}
}

func TestEncodeAPNG(t *testing.T) {
	rect := image.Rect(0, 0, 300, 2000) // several parallel strips
	var frames []image.Image
	for i := 0; i < 3; i++ {
		m := image.NewNRGBA(rect)
		for k := range m.Pix {
			m.Pix[k] = byte(k*7 + i)
		}
		frames = append(frames, m)
	}
	var buf bytes.Buffer
	if err := EncodeAPNG(&buf, frames, []float64{0.05, 0.1, 100}, 2, flate.BestSpeed); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// chunk walk: types in order and fcTL/fdAT sequence numbers
	var types []string
	seq := uint32(0)
	for p := 8; p < len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		typ := string(data[p+4 : p+8])
		body := data[p+8 : p+8+n]
		if typ == "fcTL" || typ == "fdAT" {
			if s := binary.BigEndian.Uint32(body); s != seq {
				t.Fatalf("%s sequence %d, want %d", typ, s, seq)
			}
			seq++
		}
		if typ == "fcTL" {
			num, den := binary.BigEndian.Uint16(body[20:]), binary.BigEndian.Uint16(body[22:])
			t.Logf("delay %d/%d", num, den)
		}
		if len(types) == 0 || types[len(types)-1] != typ {
			types = append(types, typ)
		}
		p += 12 + n
	}
	want := "IHDR acTL fcTL IDAT fcTL fdAT fcTL fdAT IEND"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("chunks %q, want %q", got, want)
	}
	// the default image is the first frame
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img.(*image.NRGBA).Pix, frames[0].(*image.NRGBA).Pix) {
		t.Fatal("default image differs from frame 0")
	}
}
