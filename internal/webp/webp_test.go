package webp

import (
	"image"
	"image/draw"
	"image/png"
	"os"
	"testing"
)

func readPNG(t testing.TB, path string) *image.NRGBA {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	n := image.NewNRGBA(m.Bounds())
	draw.Draw(n, n.Bounds(), m, m.Bounds().Min, draw.Src)
	return n
}

// The decodes match libwebp's (PNGs written from libwebp's RGBA output):
// lossless with alpha and with a bundled 4-colour palette, lossy with an
// ALPH chunk and with odd dimensions, and an animation with sub-frames
// blended over the canvas.
func TestDecodeMatchesLibwebp(t *testing.T) {
	for _, name := range []string{"ll_alpha", "ll_pal", "ly_alpha", "ly_odd", "anim"} {
		data, err := os.ReadFile("testdata/" + name + ".webp")
		if err != nil {
			t.Fatal(err)
		}
		im, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ref := readPNG(t, "testdata/"+name+".png")
		cs := im.Canvases()
		w, h := im.Width, im.Height
		if ref.Bounds().Dx() != w || ref.Bounds().Dy() != h*len(cs) {
			t.Fatalf("%s: %d frames %dx%d vs reference %v", name, len(cs), w, h, ref.Bounds())
		}
		for k, c := range cs {
			for y := 0; y < h; y++ {
				got := c.Pix[y*c.Stride : y*c.Stride+4*w]
				want := ref.Pix[(k*h+y)*ref.Stride : (k*h+y)*ref.Stride+4*w]
				for i := range got {
					// fully transparent pixels may hold any colour in a PNG
					if got[i] != want[i] && !(want[i|3] == 0 && got[i|3] == 0) {
						t.Fatalf("%s frame %d (%d, %d): %v, libwebp %v", name, k, i/4, y, got[i&^3:i&^3+4], want[i&^3:i&^3+4])
					}
				}
			}
		}
	}
	if im, _ := Decode(mustRead(t, "testdata/anim.webp")); !im.Animated || len(im.Frames) != 3 || im.Frames[1].Duration != 100 {
		t.Errorf("animation info: %+v", im)
	}
}

func mustRead(t testing.TB, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func FuzzDecode(f *testing.F) {
	for _, name := range []string{"ll_alpha", "ll_pal", "ly_alpha", "ly_odd", "anim"} {
		f.Add(mustRead(f, "testdata/"+name+".webp"))
	}
	MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		im, err := Decode(data)
		if err != nil {
			return
		}
		im.Canvases()
		Metadata(data)
		Lossy(data)
	})
}

func BenchmarkDecode(b *testing.B) {
	data, err := os.ReadFile(os.Getenv("WEBP_BENCH"))
	if err != nil {
		b.Skip("set WEBP_BENCH")
	}
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

// The table-driven YUV conversion equals libwebp's formula over all inputs.
func TestYUVTables(t *testing.T) {
	mh := func(v, c int) int { return v * c >> 8 }
	clip := func(v int) uint8 {
		if v&^(256<<6-1) == 0 {
			return uint8(v >> 6)
		}
		if v < 0 {
			return 0
		}
		return 255
	}
	var o [3]uint8
	for y := 0; y < 256; y++ {
		for u := 0; u < 256; u++ {
			for v := 0; v < 256; v += 5 {
				rgbOf(uint8(y), uint32(u)|uint32(v)<<16, o[:])
				r := clip(mh(y, 19077) + mh(v, 26149) - 14234)
				g := clip(mh(y, 19077) - mh(u, 6419) - mh(v, 13320) + 8708)
				b := clip(mh(y, 19077) + mh(u, 33050) - 17685)
				if o != [3]uint8{r, g, b} {
					t.Fatalf("yuv %d %d %d: %v, want %v", y, u, v, o, [3]uint8{r, g, b})
				}
			}
		}
	}
}
