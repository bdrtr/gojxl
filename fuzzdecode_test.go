package gojxl

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand"
	"testing"

	"github.com/bdrtr/gojxl/internal/jxl"
)

// FuzzDecode feeds mutated files of every kind the encoders produce to the
// decoder: it must return (an image or an error) without crashing,
// hanging or exhausting memory. Run with
//
//	go test -run x -fuzz FuzzDecode -fuzztime 5m .
func FuzzDecode(f *testing.F) {
	r := rand.New(rand.NewSource(11))
	add := func(img image.Image, opt *EncodeOptions) {
		b, err := EncodeBytes(img, opt)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	smooth := randomImage("smooth", 72, 56, r)
	add(smooth, &EncodeOptions{Effort: 1})
	add(smooth, &EncodeOptions{Effort: 4})
	add(randomImage("alpha", 40, 30, r), &EncodeOptions{Effort: 4})
	add(randomImage("gray16", 33, 17, r), &EncodeOptions{Effort: 2})
	big := randomImage("smooth", 300, 260, r) // several groups
	add(big, &EncodeOptions{Effort: 4})
	add(big, &EncodeOptions{Distance: 1, Effort: 1})
	add(big, &EncodeOptions{Distance: 2, Effort: 7})
	add(randomImage("smooth", 64, 64, r), &EncodeOptions{Distance: 4, Effort: 5}) // 3 EPF iterations
	add(randomImage("alpha", 48, 40, r), &EncodeOptions{Distance: 1})
	// a recompressed JPEG
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, randomImage("smooth", 80, 64, r), &jpeg.Options{Quality: 85}); err != nil {
		f.Fatal(err)
	}
	if b, err := TranscodeJPEG(jb.Bytes()); err == nil {
		f.Add(b)
	}
	// an animation
	anim := &Animation{Frames: []Frame{{Image: randomImage("smooth", 40, 40, r), Duration: 0.1}, {Image: randomImage("smooth", 40, 40, r), Duration: 0.2}}}
	if b, err := EncodeAnimation(anim, &EncodeOptions{Effort: 2}); err == nil {
		f.Add(b)
	}
	// patches (text at d >= 3: a reference frame and a dictionary)
	add(glyphImage(120, 72), &EncodeOptions{Distance: 3})
	// HDR: lossless and lossy PQ (tone mapped when decoded to sRGB)
	pq := pngWith(f, pqImage(colourTestImage()), chunk("cICP", []byte{9, 16, 0, 1}))
	for _, o := range []*EncodeOptions{{Effort: 1}, {Distance: 1, Effort: 3}} {
		if b, err := ConvertToJXL(pq, o); err == nil {
			f.Add(b)
		}
	}
	// small images only: a few bytes may legitimately declare a huge one
	jxl.MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodeBytes(data)
		DecodeBytesOptions(data, &Options{SRGB: true, Force16: true})
		DecodeAll(data)
	})
}

// glyphImage draws rows of three distinct glyphs on a flat background
// (content the lossy encoder turns into patches).
func glyphImage(w, h int) *image.NRGBA {
	glyphs := [][]string{
		{"..##..", ".#..#.", "#....#", "######", "#....#", "#....#", "#....#"},
		{"#####.", "#....#", "#####.", "#....#", "#....#", "#....#", "#####."},
		{".####.", "#....#", "#.....", "#.....", "#.....", "#....#", ".####."},
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:i+4], []byte{235, 238, 240, 255})
	}
	k := 0
	for y0 := 8; y0+8 < h; y0 += 16 {
		for x0 := 8; x0+7 < w; x0 += 12 {
			for gy, row := range glyphs[k%len(glyphs)] {
				for gx, p := range row {
					if p == '#' {
						copy(img.Pix[img.PixOffset(x0+gx, y0+gy):], []byte{20, 30, 60})
					}
				}
			}
			k++
		}
	}
	return img
}
