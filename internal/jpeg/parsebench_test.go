package jpeg

import (
	"os"
	"testing"
)

func BenchmarkParseBig(b *testing.B) {
	data, err := os.ReadFile(os.Getenv("JPEG_BENCH"))
	if err != nil {
		b.Skip()
	}
	for i := 0; i < b.N; i++ {
		if _, err := Parse(data); err != nil {
			b.Fatal(err)
		}
	}
}
