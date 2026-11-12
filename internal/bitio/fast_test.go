package bitio

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestFast(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	var a, b Writer
	for round := 0; round < 50; round++ {
		// mix plain writes with Fast runs
		for i := r.Intn(20); i > 0; i-- {
			n := uint(r.Intn(33))
			v := r.Uint64() & (1<<n - 1)
			a.Write(n, v)
			b.Write(n, v)
		}
		f := b.Fast()
		for i := r.Intn(200); i > 0; i-- {
			n := uint(r.Intn(33))
			v := r.Uint64() & (1<<n - 1)
			a.Write(n, v)
			f.Put(n, v)
		}
		b.SetFast(f)
		if a.BitsWritten() != b.BitsWritten() {
			t.Fatalf("round %d: %d vs %d bits", round, a.BitsWritten(), b.BitsWritten())
		}
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("output differs")
	}
}
