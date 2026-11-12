package pngx

import (
	"bytes"
	"compress/flate"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"testing"
)

// FuzzDecode: Decode must return an image or an error on any input.
//
//	go test -run x -fuzz FuzzDecode -fuzztime 2m ./internal/pngx
func FuzzDecode(f *testing.F) {
	r := rand.New(rand.NewSource(3))
	smooth := func(img interface {
		Set(x, y int, c color.Color)
		Bounds() image.Rectangle
	}, alpha bool) {
		b := img.Bounds()
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				a := uint8(255)
				if alpha {
					a = uint8(x * 7)
				}
				img.Set(x, y, color.NRGBA{uint8(x * 3), uint8(y * 5), uint8(r.Intn(4)), a})
			}
		}
	}
	add := func(img image.Image) {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			f.Fatal(err)
		}
		f.Add(b.Bytes())
		b.Reset()
		if err := Encode(&b, img, flate.BestSpeed); err == nil {
			f.Add(b.Bytes())
		}
	}
	rgba := image.NewNRGBA(image.Rect(0, 0, 37, 21))
	smooth(rgba, true)
	add(rgba)
	rgb := image.NewRGBA(image.Rect(0, 0, 40, 30))
	smooth(rgb, false)
	add(rgb)
	g := image.NewGray16(image.Rect(0, 0, 17, 9))
	for i := range g.Pix {
		g.Pix[i] = uint8(i * 13)
	}
	add(g)
	f.Fuzz(func(t *testing.T, data []byte) {
		Decode(data)
	})
}
