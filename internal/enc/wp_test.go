package enc

import (
	"math/bits"
	"math/rand"
	"testing"
)

// The reference (previous) weighted predictor implementation.
type wpStateRef struct {
	predErr    [4][]uint32
	err        []int32
	prediction [4]int64
	pred       int64
	w          int
}

func newWPRef(w int) *wpStateRef {
	s := &wpStateRef{w: w}
	for i := range s.predErr {
		s.predErr[i] = make([]uint32, (w+2)*2)
	}
	s.err = make([]int32, (w+2)*2)
	return s
}

func errWeightRef(x uint32, maxw uint32) uint32 {
	shift := bits.Len32(x+1) - 1 - 5
	if shift < 0 {
		shift = 0
	}
	return 4 + ((maxw * wpDiv[x>>uint(shift)]) >> uint(shift))
}

func (s *wpStateRef) predict(x, y int, N, W, NE, NW, NN int64) (int64, int64) {
	xs := s.w
	cur, prev := xs+2, 0
	if y&1 != 0 {
		cur, prev = 0, xs+2
	}
	posN := prev + x
	posNE, posNW := posN, posN
	if x < xs-1 {
		posNE = posN + 1
	}
	if x > 0 {
		posNW = posN - 1
	}
	var wts [4]uint32
	for i := 0; i < 4; i++ {
		pe := s.predErr[i]
		wts[i] = errWeightRef(pe[posN]+pe[posNE]+pe[posNW], wpW[i])
	}
	N <<= 3
	W <<= 3
	NE <<= 3
	NW <<= 3
	NN <<= 3
	var teW int64
	if x > 0 {
		teW = int64(s.err[cur+x-1])
	}
	teN := int64(s.err[posN])
	teNW := int64(s.err[posNW])
	sumWN := teN + teW
	teNE := int64(s.err[posNE])
	p := teW
	if abs64(teN) > abs64(p) {
		p = teN
	}
	if abs64(teNW) > abs64(p) {
		p = teNW
	}
	if abs64(teNE) > abs64(p) {
		p = teNE
	}
	s.prediction[0] = W + NE - N
	s.prediction[1] = N - (((sumWN + teNE) * wpP1C) >> 5)
	s.prediction[2] = W - (((sumWN + teNW) * wpP2C) >> 5)
	s.prediction[3] = N - ((teNW*wpP3Ca + teN*wpP3Cb + teNE*wpP3Cc + (NN-N)*wpP3Cd + (NW-W)*wpP3Ce) >> 5)
	wsum := wts[0] + wts[1] + wts[2] + wts[3]
	logw := uint(bits.Len32(wsum) - 1)
	wsum = 0
	for i := range wts {
		wts[i] >>= logw - 4
		wsum += wts[i]
	}
	sum := int64(wsum>>1) - 1
	for i := 0; i < 4; i++ {
		sum += s.prediction[i] * int64(wts[i])
	}
	pred := (sum * int64(wpDiv[wsum-1])) >> 24
	if ((teN ^ teW) | (teN ^ teNW)) <= 0 {
		mx := max(W, NE, N)
		mn := min(W, NE, N)
		pred = max(mn, min(mx, pred))
	}
	s.pred = pred
	return (pred + 3) >> 3, p
}

func (s *wpStateRef) update(val int64, x, y int) {
	xs := s.w
	cur, prev := xs+2, 0
	if y&1 != 0 {
		cur, prev = 0, xs+2
	}
	val <<= 3
	s.err[cur+x] = int32(s.pred - val)
	for i := 0; i < 4; i++ {
		e := uint32((abs64(s.prediction[i]-val) + 3) >> 3)
		s.predErr[i][cur+x] = e
		s.predErr[i][prev+x+1] += e
	}
}

// The optimised weighted predictor must match the reference exactly.
func TestWPMatchesReference(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 40; n++ {
		w, h := 1+r.Intn(70), 1+r.Intn(40)
		span := []int{2, 256, 65536, 1 << 20}[n%4]
		pix := make([]int64, w*h)
		for i := range pix {
			pix[i] = int64(r.Intn(span)) - int64(span/3*(n%2))
			if n%3 == 0 && i > 0 {
				pix[i] = pix[i-1] + int64(r.Intn(5)) - 2
			}
		}
		a, b := newWP(w), newWPRef(w)
		at := func(x, y int) int64 { return pix[y*w+x] }
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var W, N, NW, NE, NN int64
				if x > 0 {
					W = at(x-1, y)
				} else if y > 0 {
					W = at(x, y-1)
				}
				if y > 0 {
					N = at(x, y-1)
				} else {
					N = W
				}
				if x > 0 && y > 0 {
					NW = at(x-1, y-1)
				} else {
					NW = W
				}
				if x+1 < w && y > 0 {
					NE = at(x+1, y-1)
				} else {
					NE = N
				}
				if y > 1 {
					NN = at(x, y-2)
				} else {
					NN = N
				}
				p1, m1 := a.predict(x, y, N, W, NE, NW, NN)
				p2, m2 := b.predict(x, y, N, W, NE, NW, NN)
				if p1 != p2 || m1 != m2 {
					t.Fatalf("case %d at %d,%d: got %d,%d want %d,%d", n, x, y, p1, m1, p2, m2)
				}
				a.update(at(x, y), x, y)
				b.update(at(x, y), x, y)
			}
		}
	}
}
