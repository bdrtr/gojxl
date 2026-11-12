package enc

import (
	"math"
	"math/rand"
	"testing"
)

func TestFastLog2(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	worst := 0.0
	for i := 0; i < 1000000; i++ {
		x := float64(r.Uint64()>>uint(r.Intn(48))) + 65536
		d := math.Abs(fastLog2(x) - math.Log2(x))
		worst = max(worst, d)
	}
	if worst > 1e-9 {
		t.Fatalf("worst error %g", worst)
	}
}
