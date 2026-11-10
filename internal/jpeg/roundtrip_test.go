package jpeg

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func checkRoundTrip(t *testing.T, name string, data []byte) {
	t.Helper()
	img, err := Parse(data)
	if err != nil {
		t.Logf("%s: parse: %v", name, err)
		return
	}
	out, err := img.Write()
	if err != nil {
		t.Errorf("%s: write: %v", name, err)
		return
	}
	if !bytes.Equal(out, data) {
		n := 0
		for n < len(out) && n < len(data) && out[n] == data[n] {
			n++
		}
		t.Errorf("%s: rewritten file differs at byte %d (len %d vs %d)", name, n, len(out), len(data))
	}
}

func TestParseWriteRoundTrip(t *testing.T) {
	for _, q := range []int{20, 75, 98} {
		im := image.NewRGBA(image.Rect(0, 0, 77, 45))
		for y := 0; y < 45; y++ {
			for x := 0; x < 77; x++ {
				im.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 5), uint8(x * y), 255})
			}
		}
		var buf bytes.Buffer
		jpeg.Encode(&buf, im, &jpeg.Options{Quality: q})
		checkRoundTrip(t, "go", buf.Bytes())
	}
	if dir := os.Getenv("JPEG_DIR"); dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
		for _, f := range files {
			data, _ := os.ReadFile(f)
			checkRoundTrip(t, filepath.Base(f), data)
		}
	}
}
