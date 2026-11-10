package gojxl

import (
	"math/rand"
	"os"
	"testing"
)

// TestCorruptInputs flips random bytes in valid files and checks that the
// decoder returns errors instead of crashing or hanging.
func TestCorruptInputs(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	img := randomImage("smooth", 300, 260, r)
	good, err := EncodeBytes(img, &EncodeOptions{Effort: 3})
	if err != nil {
		t.Fatal(err)
	}
	inputs := [][]byte{good}
	if extra := os.Getenv("GOJXL_FUZZ_FILE"); extra != "" {
		if b, err := os.ReadFile(extra); err == nil {
			inputs = append(inputs, b)
		}
	}
	for _, in := range inputs {
		for i := 0; i < 300; i++ {
			b := append([]byte(nil), in...)
			for k := 0; k < 1+r.Intn(4); k++ {
				p := r.Intn(len(b))
				b[p] ^= byte(1 << r.Intn(8))
			}
			if r.Intn(4) == 0 {
				b = b[:r.Intn(len(b))]
			}
			DecodeBytes(b) // must not panic
		}
	}
}
