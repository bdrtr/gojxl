package gojxl

import (
	"bytes"
	"compress/flate"
	"image"
	stdjpeg "image/jpeg"
	stdpng "image/png"
	"math/rand"
	"os"
	"testing"

	"github.com/bdrtr/gojxl/internal/pngx"
)

// ConvertToPNG must give the same file as decoding to an image and
// encoding that with the colour chunks inserted (what it replaces).
func TestConvertToPNGMatchesImagePath(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	type tc struct {
		kind string
		opt  *EncodeOptions
	}
	for _, c := range []tc{
		{"smooth", &EncodeOptions{Effort: 2}},
		{"smooth", &EncodeOptions{Distance: 1}},
		{"alpha", &EncodeOptions{Effort: 3}},
		{"alpha", &EncodeOptions{Distance: 1.5}},
		{"gray", &EncodeOptions{Effort: 2}},
		{"gray16", &EncodeOptions{Effort: 2}},
		{"rgba64", &EncodeOptions{Effort: 2}},
	} {
		data, err := EncodeBytes(randomImage(c.kind, 300, 270, r), c.opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range []*Options{nil, {Force16: true}} {
			var got bytes.Buffer
			if err := ConvertToPNG(&got, data, o); err != nil {
				t.Fatal(err)
			}
			img, err := DecodeBytesOptions(data, o)
			if err != nil {
				t.Fatal(err)
			}
			var want bytes.Buffer
			if err := pngx.Encode(&want, img, flate.BestSpeed); err != nil {
				t.Fatal(err)
			}
			info, _ := Stat(data)
			w := want.Bytes()
			if info.OutputICC != nil {
				w = pngx.InsertICC(w, info.OutputICC)
			}
			if info.CICP != nil {
				w = pngx.InsertCICP(w, info.CICP[0], info.CICP[1])
			}
			if !bytes.Equal(got.Bytes(), w) {
				t.Errorf("%s %+v force16=%v: PNG differs (%d vs %d bytes)", c.kind, *c.opt, o != nil, got.Len(), len(w))
			}
		}
	}
}

// Grey images with alpha become grey+alpha PNGs: a grey ICC profile may
// not tag an RGB PNG (libpng drops it), so they must not be widened to RGBA.
func TestConvertToPNGGreyAlpha(t *testing.T) {
	data, err := os.ReadFile("testdata/grey_alpha.jxl") // cjxl -d 0 of a 24x16 LA PNG
	if err != nil {
		t.Fatal(err)
	}
	for _, opt := range []*Options{nil, {Force16: true}} {
		var buf bytes.Buffer
		if err := ConvertToPNG(&buf, data, opt); err != nil {
			t.Fatal(err)
		}
		png := buf.Bytes()
		if ct := png[25]; ct != 4 {
			t.Fatalf("Force16 %v: colour type %d, want 4 (grey+alpha)", opt != nil, ct)
		}
		img, err := stdpng.Decode(bytes.NewReader(png))
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 16; y++ {
			for x := 0; x < 24; x++ {
				g, a := uint8((x*10+y*3)%256), uint8((x*7+y*11)%256)
				c := exactNRGBA64(img.At(x, y)) // 8-bit output: v*0x101
				if uint8(c.A>>8) != a || a != 0 && uint8(c.R>>8) != g {
					t.Fatalf("pixel (%d,%d): %v, want grey %d alpha %d", x, y, c, g, a)
				}
			}
		}
	}
}

// Grey images become one-component JPEGs (their grey ICC profile may not
// tag a colour JPEG).
func TestConvertToJPEGGrey(t *testing.T) {
	data, err := os.ReadFile("testdata/grey_alpha.jxl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ConvertToJPEG(&buf, data, 90, nil); err != nil {
		t.Fatal(err)
	}
	img, err := stdjpeg.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.Gray); !ok {
		t.Fatalf("decoded as %T, want a grey JPEG", img)
	}
}
