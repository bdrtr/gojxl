package tiff

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReference decodes the TIFFs in $TIFF_REF and compares them with
// 16-bit RGBA references next to them (name.rgba16: width, height, LE
// samples), with grey replicated, 8-bit scaled by 257 and missing alpha
// opaque.
func TestReference(t *testing.T) {
	dir := os.Getenv("TIFF_REF")
	if dir == "" {
		t.Skip("set TIFF_REF")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tif"))
	for _, f := range files {
		ref, err := os.ReadFile(strings.TrimSuffix(f, ".tif") + ".rgba16")
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(f)
		name := filepath.Base(f)
		im, err := Decode(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		w, h := int(binary.LittleEndian.Uint32(ref)), int(binary.LittleEndian.Uint32(ref[4:]))
		if im.W != w || im.H != h {
			t.Errorf("%s: %dx%d, want %dx%d", name, im.W, im.H, w, h)
			continue
		}
		mul := uint32(1)
		if im.BitDepth == 8 {
			mul = 257
		}
		maxd := 0
		for i := 0; i < w*h; i++ {
			s := im.Samples[i*im.Channels : (i+1)*im.Channels]
			var px [4]uint32
			switch im.Channels {
			case 1:
				px = [4]uint32{uint32(s[0]), uint32(s[0]), uint32(s[0]), 0xffff / mul}
			case 2:
				px = [4]uint32{uint32(s[0]), uint32(s[0]), uint32(s[0]), uint32(s[1])}
			case 3:
				px = [4]uint32{uint32(s[0]), uint32(s[1]), uint32(s[2]), 0xffff / mul}
			default:
				px = [4]uint32{uint32(s[0]), uint32(s[1]), uint32(s[2]), uint32(s[3])}
			}
			for c := 0; c < 4; c++ {
				want := int(binary.LittleEndian.Uint16(ref[8+8*i+2*c:]))
				d := int(px[c]*mul) - want
				maxd = max(maxd, d, -d)
			}
		}
		t.Logf("%-8s %dx%d %d ch %2d-bit: max diff %d", name, w, h, im.Channels, im.BitDepth, maxd)
		if maxd > 0 {
			t.Errorf("%s differs", name)
		}
	}
}
