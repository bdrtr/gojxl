package tiff

import (
	"encoding/binary"
	"os"
	"testing"
)

// The decodes match libtiff's (ImageMagick 16-bit RGBA dumps, name.raw):
// 16-bit LZW with the predictor, big-endian 16x16 tiles, planar PackBits,
// 1-bit bilevel and RGBA with unassociated alpha.
func TestDecodeMatchesLibtiff(t *testing.T) {
	for _, name := range []string{"rgb16_lzw", "tiles_mm", "planar", "bilevel", "rgba"} {
		data, err := os.ReadFile("testdata/" + name + ".tif")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := os.ReadFile("testdata/" + name + ".raw")
		if err != nil {
			t.Fatal(err)
		}
		im, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if im.W != 37 || im.H != 29 {
			t.Fatalf("%s: %dx%d", name, im.W, im.H)
		}
		mul := uint32(1)
		if im.BitDepth == 8 {
			mul = 257
		}
		for i := 0; i < im.W*im.H; i++ {
			s := im.Samples[i*im.Channels : (i+1)*im.Channels]
			px := [4]uint32{0, 0, 0, 0xffff / mul}
			switch im.Channels {
			case 1, 2:
				px[0], px[1], px[2] = uint32(s[0]), uint32(s[0]), uint32(s[0])
			default:
				px[0], px[1], px[2] = uint32(s[0]), uint32(s[1]), uint32(s[2])
			}
			if im.Alpha {
				px[3] = uint32(s[im.Channels-1])
			}
			for c := 0; c < 4; c++ {
				if want := uint32(binary.LittleEndian.Uint16(ref[8*i+2*c:])); px[c]*mul != want {
					t.Fatalf("%s pixel %d channel %d: %d, libtiff %d", name, i, c, px[c]*mul, want)
				}
			}
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, name := range []string{"rgb16_lzw", "tiles_mm", "planar", "bilevel", "rgba"} {
		b, err := os.ReadFile("testdata/" + name + ".tif")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		im, err := Decode(data)
		if err == nil && len(im.Samples) != im.W*im.H*im.Channels {
			t.Fatalf("%d samples for %dx%dx%d", len(im.Samples), im.W, im.H, im.Channels)
		}
		Metadata(data)
	})
}
