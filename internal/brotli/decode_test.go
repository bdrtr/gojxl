package brotli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata holds streams made by the reference encoder at several
// qualities and window sizes.
func TestDecodeFiles(t *testing.T) {
	dir := "testdata"
	if d := os.Getenv("BROTLI_DIR"); d != "" {
		dir = d
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.br"))
	if len(files) == 0 {
		t.Fatal("no test streams")
	}
	for _, f := range files {
		comp, _ := os.ReadFile(f)
		orig, err := os.ReadFile(f[:strings.Index(f, ".q")])
		if err != nil {
			t.Fatal(err)
		}
		out, err := Decode(comp, 1<<26)
		if err != nil || !bytes.Equal(out, orig) {
			t.Errorf("%s: err=%v len %d vs %d", filepath.Base(f), err, len(out), len(orig))
		}
	}
}

func TestDecodeCorrupt(t *testing.T) {
	comp, _ := os.ReadFile("testdata/t_readme.txt.q11.w22.br")
	for i := 0; i < len(comp); i += 7 {
		d := append([]byte(nil), comp...)
		d[i] ^= 0x55
		Decode(d, 1<<20) // must not panic
		Decode(comp[:i], 1<<20)
	}
}
