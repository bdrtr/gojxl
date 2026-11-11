package enc

import (
	"bytes"
	"math/rand"
	"os"
	"testing"

	"gojxl/internal/brotli"
)

func TestBrotliCompress(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	rnd := make([]byte, 5000)
	r.Read(rnd)
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 400)
	var inputs [][]byte
	inputs = append(inputs, nil, []byte("a"), []byte("ab"), rnd, text, make([]byte, 100000))
	skew := make([]byte, 70000)
	for i := range skew {
		skew[i] = byte(r.ExpFloat64() * 4)
	}
	inputs = append(inputs, skew)
	for _, f := range []string{"/usr/share/dict/words", "/etc/services", "/usr/share/ghostscript/iccprofiles/default_cmyk.icc", os.Getenv("BROTLI_FILE")} {
		if b, err := os.ReadFile(f); err == nil {
			inputs = append(inputs, b)
		}
	}
	for i, in := range inputs {
		c := BrotliCompress(in)
		d, err := brotli.Decode(c, len(in)+1)
		if err != nil || !bytes.Equal(d, in) {
			t.Fatalf("input %d (%d bytes): roundtrip failed: %v", i, len(in), err)
		}
		t.Logf("input %d: %d -> %d", i, len(in), len(c))
	}
}
