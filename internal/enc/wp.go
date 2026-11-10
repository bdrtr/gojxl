package enc

import "math/bits"

// Weighted predictor (default parameters), mirroring the decoder exactly.

var wpDiv = func() (t [64]uint32) {
	for i := range t {
		t[i] = (1 << 24) / uint32(i+1)
	}
	return
}()

var wpW = [4]uint32{0xd, 0xc, 0xc, 0xc}

const (
	wpP1C, wpP2C, wpP3Ca, wpP3Cb, wpP3Cc, wpP3Cd, wpP3Ce = 16, 10, 7, 7, 7, 0, 0
)

type wpState struct {
	predErr    [4][]uint32
	err        []int32
	prediction [4]int64
	pred       int64
	w          int
}

func newWP(w int) *wpState {
	s := &wpState{w: w}
	for i := range s.predErr {
		s.predErr[i] = make([]uint32, (w+2)*2)
	}
	s.err = make([]int32, (w+2)*2)
	return s
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func errWeight(x uint32, maxw uint32) uint32 {
	shift := bits.Len32(x+1) - 1 - 5
	if shift < 0 {
		shift = 0
	}
	return 4 + ((maxw * wpDiv[x>>uint(shift)]) >> uint(shift))
}

func (s *wpState) predict(x, y int, N, W, NE, NW, NN int64) (int64, int64) {
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
		wts[i] = errWeight(pe[posN]+pe[posNE]+pe[posNW], wpW[i])
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

func (s *wpState) update(val int64, x, y int) {
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
