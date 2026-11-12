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

// wpState holds, per position of two alternating rows, the four
// sub-predictors' errors and the prediction error.
type wpState struct {
	pe         [][4]uint32
	err        []int32
	prediction [4]int64
	pred       int64
	w          int
}

func newWP(w int) *wpState {
	return &wpState{pe: make([][4]uint32, (w+2)*2), err: make([]int32, (w+2)*2), w: w}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func errWeight(x uint32, maxw uint32) uint32 {
	shift := max(bits.Len32(x+1)-1-5, 0) // branch-free
	return 4 + ((maxw * wpDiv[(x>>uint(shift))&63]) >> uint(shift))
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
	en, ene, enw := &s.pe[posN], &s.pe[posNE], &s.pe[posNW]
	var wts [4]uint32
	wts[0] = errWeight(en[0]+ene[0]+enw[0], wpW[0])
	wts[1] = errWeight(en[1]+ene[1]+enw[1], wpW[1])
	wts[2] = errWeight(en[2]+ene[2]+enw[2], wpW[2])
	wts[3] = errWeight(en[3]+ene[3]+enw[3], wpW[3])
	N <<= 3
	W <<= 3
	NE <<= 3
	NW <<= 3
	NN <<= 3
	errs := s.err
	var teW int64
	if x > 0 {
		teW = int64(errs[cur+x-1])
	}
	teN := int64(errs[posN])
	teNW := int64(errs[posNW])
	sumWN := teN + teW
	teNE := int64(errs[posNE])
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
	p0 := W + NE - N
	p1 := N - (((sumWN + teNE) * wpP1C) >> 5)
	p2 := W - (((sumWN + teNW) * wpP2C) >> 5)
	p3 := N - ((teNW*wpP3Ca + teN*wpP3Cb + teNE*wpP3Cc + (NN-N)*wpP3Cd + (NW-W)*wpP3Ce) >> 5)
	s.prediction = [4]int64{p0, p1, p2, p3}
	wsum := wts[0] + wts[1] + wts[2] + wts[3]
	logw := uint(bits.Len32(wsum)-1) - 4
	w0, w1, w2, w3 := wts[0]>>logw, wts[1]>>logw, wts[2]>>logw, wts[3]>>logw
	wsum = w0 + w1 + w2 + w3
	sum := int64(wsum>>1) - 1 + p0*int64(w0) + p1*int64(w1) + p2*int64(w2) + p3*int64(w3)
	pred := (sum * int64(wpDiv[(wsum-1)&63])) >> 24
	// clamp when the errors agree in sign (selected without a branch: the
	// condition is data-dependent)
	clamped := max(min(W, NE, N), min(max(W, NE, N), pred))
	if ((teN ^ teW) | (teN ^ teNW)) <= 0 {
		pred = clamped
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
	var e [4]uint32
	for i, pr := range s.prediction {
		e[i] = uint32((abs64(pr-val) + 3) >> 3)
	}
	s.pe[cur+x] = e
	q := &s.pe[prev+x+1]
	q[0] += e[0]
	q[1] += e[1]
	q[2] += e[2]
	q[3] += e[3]
}
