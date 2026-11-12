package gojxl

import (
	"bytes"
	"compress/flate"
	"math/rand"
	"testing"

	"gojxl/internal/pngx"
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
