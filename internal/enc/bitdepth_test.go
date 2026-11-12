package enc

import "testing"

// Only depths dividing the original one are exact for every decoder: 241
// is the 7-bit 120 expanded by rounding, but libjxl gives 240 back.
func TestReducedBitDepth(t *testing.T) {
	for _, tc := range []struct {
		vals  []int32
		depth int
		want  int
	}{
		{[]int32{0, 255}, 8, 1},
		{[]int32{0, 85, 170, 255}, 8, 2},
		{[]int32{0, 17, 34, 255}, 8, 4},
		{[]int32{0, 241, 255}, 8, 8}, // 7-bit looking
		{[]int32{0, 2, 4, 254}, 8, 8},
		{[]int32{0, 0x0101, 0xffff}, 16, 8},
		{[]int32{0, 0x1111, 0xffff}, 16, 4},
		{[]int32{0, 1023}, 10, 1},
		{[]int32{0, 33, 1023}, 10, 5},
		{[]int32{3, 0}, 2, 1},
		{[]int32{2, 0}, 2, 2},
	} {
		if got := reducedBitDepth([][]int32{tc.vals}, tc.depth); got != tc.want {
			t.Errorf("%v at %d bits: got %d, want %d", tc.vals, tc.depth, got, tc.want)
		}
	}
}
