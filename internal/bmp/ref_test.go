package bmp

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReference compares decodes of the bitmaps in $BMP_REF with RGBA
// references (name.rgba: width, height, pixels).
func TestReference(t *testing.T) {
	dir := os.Getenv("BMP_REF")
	if dir == "" {
		t.Skip("set BMP_REF")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.bmp"))
	for _, f := range files {
		ref, err := os.ReadFile(strings.TrimSuffix(f, ".bmp") + ".rgba")
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(f)
		img, err := Decode(data)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		w, h := int(binary.LittleEndian.Uint32(ref)), int(binary.LittleEndian.Uint32(ref[4:]))
		if img.Rect.Dx() != w || img.Rect.Dy() != h {
			t.Errorf("%s: %v, want %dx%d", filepath.Base(f), img.Rect, w, h)
			continue
		}
		maxd := 0
		for i, v := range ref[8:] {
			d := int(img.Pix[i]) - int(v)
			maxd = max(maxd, d, -d)
		}
		t.Logf("%-8s max diff %d", filepath.Base(f), maxd)
		if maxd > 0 {
			t.Errorf("%s differs", filepath.Base(f))
		}
	}
}
