package enc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/jxl"
)

func iccFiles(t *testing.T) [][]byte {
	var out [][]byte
	for _, pat := range []string{"/usr/share/ghostscript/iccprofiles/*.icc", "/usr/share/color/icc/*/*.ic[cm]", "/usr/share/color/icc/*.ic[cm]", os.Getenv("ICC_DIR") + "/*.ic[cm]"} {
		m, _ := filepath.Glob(pat)
		for _, f := range m {
			if b, err := os.ReadFile(f); err == nil {
				out = append(out, b)
			}
		}
	}
	return out
}

func TestPredictICC(t *testing.T) {
	profiles := iccFiles(t)
	// synthetic edge cases
	profiles = append(profiles, []byte("abc"), make([]byte, 128), make([]byte, 131), bytes.Repeat([]byte{0xff, 1, 2}, 300))
	for i, p := range profiles {
		enc := predictICC(p)
		got, err := jxl.UnpredictICC(enc)
		if err != nil || !bytes.Equal(got, p) {
			t.Fatalf("profile %d (%d bytes): roundtrip failed: %v", i, len(p), err)
		}
		bw := &bitio.Writer{}
		writeICC(bw, p)
		dec, err := jxl.ReadICC(bitio.NewReader(bw.Bytes()))
		if err != nil || !bytes.Equal(dec, p) {
			t.Fatalf("profile %d: stream roundtrip failed: %v", i, err)
		}
		simple := &bitio.Writer{}
		writeICCEnc(simple, predictICCSimple(p))
		full := &bitio.Writer{}
		writeICCEnc(full, enc)
		t.Logf("profile %d: %d bytes -> simple %d, predicted %d", i, len(p), len(simple.Bytes()), len(full.Bytes()))
	}
}
