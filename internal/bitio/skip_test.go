package bitio

import (
	"math/rand"
	"testing"
)

// SkipBits must leave the reader where reading the bits would.
func TestSkipBits(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 300)
	rng.Read(data)
	for it := 0; it < 2000; it++ {
		a, b := NewReader(data), NewReader(data)
		pre := uint(rng.Intn(40))
		a.Read(pre)
		b.Read(pre)
		n := uint64(rng.Intn(len(data)*8 + 200))
		a.SkipBits(n)
		for m := n; m > 0; {
			k := min(m, 32)
			b.Read(uint(k))
			m -= k
		}
		if a.Overflowed() != b.Overflowed() {
			t.Fatalf("skip %d after %d: overflow %v vs %v", n, pre, a.Overflowed(), b.Overflowed())
		}
		if b.Overflowed() {
			continue
		}
		if a.BitsRead() != b.BitsRead() || a.Read(17) != b.Read(17) {
			t.Fatalf("skip %d after %d: position %d vs %d", n, pre, a.BitsRead(), b.BitsRead())
		}
	}
	r := NewReader(data)
	r.SkipBits(1 << 40)
	if !r.Overflowed() {
		t.Fatal("huge skip should overflow")
	}
}
