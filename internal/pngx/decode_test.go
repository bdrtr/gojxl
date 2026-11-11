package pngx

import (
	"bytes"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func checkSame(t *testing.T, name string, data []byte) {
	t.Helper()
	want, werr := png.Decode(bytes.NewReader(data))
	got, ok, gerr := decodeFast(data)
	if !ok {
		return
	}
	if (werr != nil) != (gerr != nil) {
		t.Fatalf("%s: errors differ: %v vs %v", name, gerr, werr)
	}
	if werr != nil {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: %T differs from image/png's %T", name, got, want)
	}
}

func TestDecodeFast(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for _, sz := range [][2]int{{1, 1}, {7, 3}, {65, 40}, {300, 211}} {
		rect := image.Rect(0, 0, sz[0], sz[1])
		fill := func(p []byte, smooth bool, opaqueStep int) {
			for i := range p {
				if smooth {
					p[i] = byte(i/5 + r.Intn(4))
				} else {
					p[i] = byte(r.Intn(256))
				}
			}
			if opaqueStep > 0 {
				for i := opaqueStep - 1; i < len(p); i += opaqueStep {
					p[i] = 0xff
				}
			}
		}
		for _, smooth := range []bool{true, false} {
			g := image.NewGray(rect)
			fill(g.Pix, smooth, 0)
			g16 := image.NewGray16(rect)
			fill(g16.Pix, smooth, 0)
			rgb := image.NewRGBA(rect)
			fill(rgb.Pix, smooth, 4)
			rgb16 := image.NewRGBA64(rect)
			fill(rgb16.Pix, smooth, 0)
			for i := 6; i < len(rgb16.Pix); i += 8 {
				rgb16.Pix[i], rgb16.Pix[i+1] = 0xff, 0xff
			}
			n := image.NewNRGBA(rect)
			fill(n.Pix, smooth, 0)
			n16 := image.NewNRGBA64(rect)
			fill(n16.Pix, smooth, 0)
			ga := image.NewNRGBA(rect) // written as RGBA; grey+alpha comes from files
			fill(ga.Pix, smooth, 0)
			for _, im := range []image.Image{g, g16, rgb, rgb16, n, n16, ga} {
				for _, lvl := range []png.CompressionLevel{png.NoCompression, png.BestSpeed, png.DefaultCompression, png.BestCompression} {
					var buf bytes.Buffer
					if err := (&png.Encoder{CompressionLevel: lvl}).Encode(&buf, im); err != nil {
						t.Fatal(err)
					}
					checkSame(t, "generated", buf.Bytes())
					// truncations must fail, not panic
					d := buf.Bytes()
					for _, cut := range []int{len(d) / 2, len(d) - 20} {
						if cut > 40 {
							got, ok, err := decodeFast(d[:cut])
							_ = got
							if ok && err == nil {
								// image/png must agree that it decodes
								if _, werr := png.Decode(bytes.NewReader(d[:cut])); werr != nil {
									t.Fatalf("truncated stream decoded")
								}
							}
						}
					}
				}
			}
		}
	}
	// real files, if a directory is given
	if dir := os.Getenv("PNGX_TESTDIR"); dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "*.png"))
		more, _ := filepath.Glob(filepath.Join(dir, "*", "*.png"))
		n := 0
		for _, f := range append(files, more...) {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if _, ok, _ := decodeFast(data); ok {
				n++
			}
			checkSame(t, f, data)
		}
		t.Logf("%d files on the fast path", n)
	}
}

// Corrupted streams must return errors, never panic.
func TestDecodeFastCorrupt(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	m := image.NewNRGBA(image.Rect(0, 0, 50, 40))
	for i := range m.Pix {
		m.Pix[i] = byte(i / 3)
	}
	var buf bytes.Buffer
	png.Encode(&buf, m)
	orig := buf.Bytes()
	for k := 0; k < 3000; k++ {
		d := append([]byte(nil), orig...)
		for j := 0; j < 1+r.Intn(4); j++ {
			p := 33 + r.Intn(len(d)-33)
			d[p] ^= byte(1 << r.Intn(8))
		}
		decodeFast(d)
	}
}
