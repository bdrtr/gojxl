package webp

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReference decodes the WebP files in $WEBP_REF and compares them with
// the libwebp RGBA decodes next to them (name.rgba: width, height, pixels).
func TestReference(t *testing.T) {
	dir := os.Getenv("WEBP_REF")
	if dir == "" {
		t.Skip("set WEBP_REF")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.webp"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		ref, err := os.ReadFile(strings.TrimSuffix(f, ".webp") + ".rgba")
		if err != nil {
			continue
		}
		w, h := int(binary.LittleEndian.Uint32(ref)), int(binary.LittleEndian.Uint32(ref[4:]))
		im, err := Decode(data)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		img := im.Frames[0].Image
		if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
			t.Errorf("%s: %dx%d, want %dx%d", filepath.Base(f), b.Dx(), b.Dy(), w, h)
			continue
		}
		maxd, nd := 0, 0
		for i, v := range ref[8:] {
			d := int(img.Pix[i]) - int(v)
			if d < 0 {
				d = -d
			}
			if d > 0 {
				nd++
			}
			maxd = max(maxd, d)
		}
		t.Logf("%-16s %4dx%-4d max diff %3d, %d samples differ", filepath.Base(f), w, h, maxd, nd)
		if maxd > 0 && strings.HasPrefix(filepath.Base(f), "ll_") {
			t.Errorf("%s: lossless decode differs", filepath.Base(f))
		}
	}
}

// TestReferenceAnim compares the composited frames of the animations in
// $WEBP_REF with libwebp's (name.frames: width, height, count, RGBA frames).
func TestReferenceAnim(t *testing.T) {
	dir := os.Getenv("WEBP_REF")
	if dir == "" {
		t.Skip("set WEBP_REF")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.webp"))
	for _, f := range files {
		ref, err := os.ReadFile(strings.TrimSuffix(f, ".webp") + ".frames")
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(f)
		w, h, n := int(binary.LittleEndian.Uint32(ref)), int(binary.LittleEndian.Uint32(ref[4:])), int(binary.LittleEndian.Uint32(ref[8:]))
		im, err := Decode(data)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		cs := im.Canvases()
		if len(cs) != n || im.Width != w || im.Height != h {
			t.Errorf("%s: %d frames %dx%d, want %d %dx%d", filepath.Base(f), len(cs), im.Width, im.Height, n, w, h)
			continue
		}
		bad := 0
		for k, c := range cs {
			fr := ref[12+k*w*h*4 : 12+(k+1)*w*h*4]
			maxd := 0
			for i, v := range fr {
				d := int(c.Pix[i]) - int(v)
				maxd = max(maxd, d, -d)
			}
			if maxd > 0 {
				bad++
				t.Logf("%s frame %d: max diff %d", filepath.Base(f), k, maxd)
			}
		}
		t.Logf("%-14s %d frames, loops %d, %d differ", filepath.Base(f), n, im.Loops, bad)
		if bad > 0 {
			t.Errorf("%s: frames differ", filepath.Base(f))
		}
	}
}
