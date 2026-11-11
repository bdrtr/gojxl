package enc

import (
	"math/rand"
	"slices"
	"testing"
)

// Channels grouped as one signal must compute the same function, and the
// candidates must reduce to the 15 expected functions.
func TestRCTSignals(t *testing.T) {
	set := rctSignals()
	if len(set.rep) != 15 {
		t.Errorf("%d distinct channel functions, want 15", len(set.rep))
	}
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 200000; n++ {
		var p [3]int32
		for c := range p {
			switch n % 3 {
			case 0:
				p[c] = int32(r.Intn(256))
			case 1:
				p[c] = int32(r.Intn(65536))
			default:
				p[c] = int32(r.Intn(1<<20)) - 1<<19
			}
		}
		var want [16]int32
		var have [16]bool
		for id := 0; id < 42; id++ {
			for c := 0; c < 3; c++ {
				s, v := set.of[id][c], rctComponent(id, c, p)
				if !have[s] {
					want[s], have[s] = v, true
				} else if want[s] != v {
					t.Fatalf("signal %d: id %d channel %d gives %d, expected %d for %v", s, id, c, v, want[s], p)
				}
			}
		}
	}
}

// sortInt32 and bucketize must match the comparison-based versions.
func TestSortAndBucketize(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for n := 0; n < 300; n++ {
		size := r.Intn(3000)
		span := []int32{1, 3, 300, 70000, 1 << 30}[n%5]
		v := make([]int32, size)
		for i := range v {
			v[i] = int32(r.Int63n(int64(span))) - span/3
		}
		a := slices.Clone(v)
		sortInt32(a)
		b := slices.Clone(v)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Fatalf("sortInt32 differs (span %d)", span)
		}
		// thresholds: increasing, from the sorted values
		var th []int32
		for i := 0; i < len(b); i += 1 + r.Intn(200) {
			if len(th) == 0 || b[i] > th[len(th)-1] {
				th = append(th, b[i])
			}
		}
		got := bucketize(v, th)
		for i, x := range v {
			k, _ := slices.BinarySearch(th, x)
			if int(got[i]) != k {
				t.Fatalf("bucketize(%d) = %d, want %d (th %v)", x, got[i], k, th)
			}
		}
	}
}
