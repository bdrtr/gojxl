package enc

import "testing"

// A worker without work leaves a nil histogram set, also at index 0.
func TestMergeWorkerHists(t *testing.T) {
	a, b := newHistograms(2, defaultCfg), newHistograms(2, defaultCfg)
	a.add(0, 3)
	b.add(0, 3)
	b.add(1, 40)
	h := mergeWorkerHists([]*histograms{nil, a, nil, b})
	if h == nil {
		t.Fatal("no histograms")
	}
	var n0, n1 uint64
	for _, v := range h.counts[0] {
		n0 += v
	}
	for _, v := range h.counts[1] {
		n1 += v
	}
	if n0 != 2 || n1 != 1 {
		t.Errorf("counts %d %d, want 2 1", n0, n1)
	}
}
