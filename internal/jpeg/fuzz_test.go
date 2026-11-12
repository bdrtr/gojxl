package jpeg

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// FuzzParse: Parse must return a result or an error on any input.
//
//	go test -run x -fuzz FuzzParse -fuzztime 2m ./internal/jpeg
func FuzzParse(f *testing.F) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 9), uint8(x ^ y), 255})
		}
	}
	for _, q := range []int{50, 95} {
		var b bytes.Buffer
		jpeg.Encode(&b, img, &jpeg.Options{Quality: q})
		f.Add(b.Bytes())
	}
	gray := image.NewGray(image.Rect(0, 0, 16, 16))
	var b bytes.Buffer
	jpeg.Encode(&b, gray, nil)
	f.Add(b.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		Parse(data)
	})
}
