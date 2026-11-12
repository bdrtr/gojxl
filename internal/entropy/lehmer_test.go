package entropy

import (
	"math/rand"
	"slices"
	"testing"
)

func lehmerNaive(lehmer []uint32) []int {
	temp := make([]int, len(lehmer))
	for i := range temp {
		temp[i] = i
	}
	perm := make([]int, len(lehmer))
	for i, l := range lehmer {
		perm[i] = temp[l]
		temp = slices.Delete(temp, int(l), int(l)+1)
	}
	return perm
}

func TestDecodeLehmer(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for it := 0; it < 500; it++ {
		n := 1 + rng.Intn(300)
		m := rng.Intn(n + 1)
		lehmer := make([]uint32, n)
		for i := 0; i < m; i++ {
			if rng.Intn(3) > 0 {
				lehmer[i] = uint32(rng.Intn(n - i))
			}
		}
		if got, want := decodeLehmer(lehmer, m), lehmerNaive(lehmer); !slices.Equal(got, want) {
			t.Fatalf("n=%d m=%d: %v vs %v", n, m, got, want)
		}
	}
}
