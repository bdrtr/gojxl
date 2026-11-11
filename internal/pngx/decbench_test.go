package pngx

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

func benchFile(b *testing.B) []byte {
	data, err := os.ReadFile(os.Getenv("PNGX_BENCH"))
	if err != nil {
		b.Skip("set PNGX_BENCH")
	}
	b.SetBytes(int64(len(data)))
	return data
}

func BenchmarkDecodeFast(b *testing.B) {
	data := benchFile(b)
	for i := 0; i < b.N; i++ {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeStd(b *testing.B) {
	data := benchFile(b)
	for i := 0; i < b.N; i++ {
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
