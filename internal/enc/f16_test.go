package enc

import "testing"

func TestF16(t *testing.T) {
	for v, want := range map[float32]uint16{10000: 0x70e2, 1000: 0x63d0, 255: 0x5bf8, 1: 0x3c00} {
		if got := f16(v); got != want {
			t.Errorf("f16(%v) = %#x, want %#x", v, got, want)
		}
	}
}
