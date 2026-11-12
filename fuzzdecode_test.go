package gojxl

import (
	"bytes"
	"image"
	"image/jpeg"
	"math/rand"
	"testing"

	"gojxl/internal/jxl"
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
	// small images only: a few bytes may legitimately declare a huge one
	jxl.MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodeBytes(data)
		DecodeAll(data)
	})
}
