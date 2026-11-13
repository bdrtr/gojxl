package jxl

import (
	"math/bits"

	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/entropy"
)

// Predictors.
const (
	predZero     = 0
	predW        = 1
	predN        = 2
	predAvgWN    = 3
	predSelect   = 4
	predGradient = 5
	predWeighted = 6
	predNE       = 7
	predNW       = 8
	predWW       = 9
	predAvgWNW   = 10
	predAvgNNW   = 11
	predAvgNNE   = 12
	predAvgAll   = 13
)

var wpDivLookup = func() (t [64]uint32) {
	for i := range t {
		t[i] = (1 << 24) / uint32(i+1)
	}
	return
}()

// wpState is the weighted predictor's state: per position of two rows
// (current and previous, alternating), the four sub-predictors' errors
// and the prediction error.
type wpState struct {
	pe         [][4]uint32
	err        []int32
	prediction [4]int64
	pred       int64
	w          int
	// header parameters
	wts                                    [4]uint32
	p1c, p2c, p3ca, p3cb, p3cc, p3cd, p3ce int64
}

func newWPState(hdr *WPHeader, w int) *wpState {
	return &wpState{
		pe:  make([][4]uint32, (w+2)*2),
		err: make([]int32, (w+2)*2),
		w:   w,
		wts: hdr.W,
		p1c: hdr.P1C, p2c: hdr.P2C,
		p3ca: hdr.P3Ca, p3cb: hdr.P3Cb, p3cc: hdr.P3Cc, p3cd: hdr.P3Cd, p3ce: hdr.P3Ce,
	}
}

func errorWeight(x uint32, maxw uint32) uint32 {
	shift := max(bits.Len32(x+1)-1-5, 0) // branch-free
	return 4 + ((maxw * wpDivLookup[(x>>uint(shift))&63]) >> uint(shift))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// predict returns the WP prediction and the max-error property.
func (s *wpState) predict(x, y int, N, W, NE, NW, NN int64) (int64, int64) {
	xs := s.w
	curRow, prevRow := xs+2, 0
	if y&1 != 0 {
		curRow, prevRow = 0, xs+2
	}
	posN := prevRow + x
	posNE, posNW := posN, posN
	if x < xs-1 {
		posNE = posN + 1
	}
	if x > 0 {
		posNW = posN - 1
	}
	en, ene, enw := &s.pe[posN], &s.pe[posNE], &s.pe[posNW]
	var wts [4]uint32
	wts[0] = errorWeight(en[0]+ene[0]+enw[0], s.wts[0])
	wts[1] = errorWeight(en[1]+ene[1]+enw[1], s.wts[1])
	wts[2] = errorWeight(en[2]+ene[2]+enw[2], s.wts[2])
	wts[3] = errorWeight(en[3]+ene[3]+enw[3], s.wts[3])
	N <<= 3
	W <<= 3
	NE <<= 3
	NW <<= 3
	NN <<= 3
	errs := s.err
	var teW int64
	if x > 0 {
		teW = int64(errs[curRow+x-1])
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
	p1 := N - (((sumWN + teNE) * s.p1c) >> 5)
	p2 := W - (((sumWN + teNW) * s.p2c) >> 5)
	p3 := N - ((teNW*s.p3ca + teN*s.p3cb + teNE*s.p3cc + (NN-N)*s.p3cd + (NW-W)*s.p3ce) >> 5)
	s.prediction = [4]int64{p0, p1, p2, p3}

	// weighted average
	wsum := wts[0] + wts[1] + wts[2] + wts[3]
	logw := uint(bits.Len32(wsum)-1) - 4
	w0, w1, w2, w3 := wts[0]>>logw, wts[1]>>logw, wts[2]>>logw, wts[3]>>logw
	wsum = w0 + w1 + w2 + w3
	sum := int64(wsum>>1) - 1 + p0*int64(w0) + p1*int64(w1) + p2*int64(w2) + p3*int64(w3)
	pred := (sum * int64(wpDivLookup[(wsum-1)&63])) >> 24

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
	curRow, prevRow := xs+2, 0
	if y&1 != 0 {
		curRow, prevRow = 0, xs+2
	}
	val <<= 3
	s.err[curRow+x] = int32(s.pred - val)
	var e [4]uint32
	for i, pr := range s.prediction {
		e[i] = uint32((abs64(pr-val) + 3) >> 3)
	}
	s.pe[curRow+x] = e
	q := &s.pe[prevRow+x+1]
	q[0] += e[0]
	q[1] += e[1]
	q[2] += e[2]
	q[3] += e[3]
}

func clampedGradient(n, w, l int64) int64 {
	mn, mx := min(n, w), max(n, w)
	grad := n + w - l
	if l < mn {
		return mx
	}
	if l > mx {
		return mn
	}
	return grad
}

func predictOne(p uint8, W, N, NW, NE, NN, WW, NEE, wp int64) int64 {
	switch p {
	case predZero:
		return 0
	case predW:
		return W
	case predN:
		return N
	case predAvgWN:
		return (W + N) / 2
	case predSelect:
		pp := W + N - NW
		if abs64(pp-W) < abs64(pp-N) {
			return W
		}
		return N
	case predGradient:
		return clampedGradient(N, W, NW)
	case predWeighted:
		return wp
	case predNE:
		return NE
	case predNW:
		return NW
	case predWW:
		return WW
	case predAvgWNW:
		return (W + NW) / 2
	case predAvgNNW:
		return (N + NW) / 2
	case predAvgNNE:
		return (N + NE) / 2
	case predAvgAll:
		return (6*N - 2*NN + 7*W + WW + NEE + 3*NE + 8) / 16
	}
	return 0
}

// leaf/inner node of a pruned tree with resolved clusters.
type ptNode struct {
	prop    int32 // -1 for leaf
	split   int32
	left    int32 // child if prop > split ; for leaf: unused
	right   int32
	pred    uint8
	cluster uint8
	offset  int32
	mult    int32
}

// pruneTree specialises the tree for fixed channel index and stream id.
func pruneTree(t *MATree, code *entropy.Code, ch, stream int32) []ptNode {
	out := make([]ptNode, 0, len(t.nodes))
	var rec func(n int32) int32
	rec = func(n int32) int32 {
		for {
			nd := &t.nodes[n]
			if nd.property == 0 || nd.property == 1 {
				v := ch
				if nd.property == 1 {
					v = stream
				}
				if v > nd.splitVal {
					n = nd.lchild
				} else {
					n = nd.rchild
				}
				continue
			}
			break
		}
		nd := &t.nodes[n]
		idx := int32(len(out))
		if nd.property < 0 {
			out = append(out, ptNode{prop: -1, pred: nd.predictor, cluster: code.ContextMap[nd.lchild], offset: nd.offset, mult: int32(nd.mult)})
			return idx
		}
		out = append(out, ptNode{prop: nd.property, split: nd.splitVal})
		l := rec(nd.lchild)
		r := rec(nd.rchild)
		out[idx].left = l
		out[idx].right = r
		return idx
	}
	rec(0)
	return out
}

func decodeChannel(br *bitio.Reader, dec *entropy.Decoder, tree *MATree, wph *WPHeader, img *ModularImage, ci int, stream int) error {
	ch := img.Channels[ci]
	w, h := ch.W, ch.H
	nodes := pruneTree(tree, dec.Code, int32(ci), int32(stream))

	maxProp := -1
	usesWP := false
	for i := range nodes {
		n := &nodes[i]
		if n.prop >= 0 {
			maxProp = max(maxProp, int(n.prop))
			if n.prop == 15 {
				usesWP = true
			}
		} else if n.pred == predWeighted {
			usesWP = true
		}
	}

	// Fast path: single leaf.
	if len(nodes) == 1 {
		leaf := nodes[0]
		if leaf.pred == predZero {
			if tok, ok := dec.SingleToken(leaf.cluster); ok {
				v := int32(int64(bitio.UnpackSigned(tok))*int64(leaf.mult) + int64(leaf.offset))
				for i := range ch.Pix {
					ch.Pix[i] = v
				}
				return nil
			}
			for i := range ch.Pix {
				tok := dec.ReadUintClustered(br, leaf.cluster)
				ch.Pix[i] = int32(int64(bitio.UnpackSigned(tok))*int64(leaf.mult) + int64(leaf.offset))
			}
			return nil
		}
		if !usesWP {
			decodeSingleLeaf(br, dec, &leaf, ch)
			return nil
		}
	}

	var wp *wpState
	if usesWP {
		wp = newWPState(wph, w)
	}

	nprops := 16
	nrefs := 0
	var refIdx []int
	if maxProp >= 16 {
		nrefs = (maxProp-16)/4 + 1
		for j := ci - 1; j >= 0 && len(refIdx) < nrefs; j-- {
			c := img.Channels[j]
			if c.W != w || c.H != h || c.HShift != ch.HShift || c.VShift != ch.VShift {
				continue
			}
			refIdx = append(refIdx, j)
		}
		nprops = 16 + nrefs*4
	}
	props := make([]int32, nprops)
	props[0] = int32(ci)
	props[1] = int32(stream)
	var refRow []int32
	if nrefs > 0 {
		refRow = make([]int32, w*nrefs*4)
	}

	pix := ch.Pix
	for y := 0; y < h; y++ {
		row := pix[y*w : (y+1)*w]
		var prev, prev2 []int32
		if y > 0 {
			prev = pix[(y-1)*w : y*w]
		}
		if y > 1 {
			prev2 = pix[(y-2)*w : (y-1)*w]
		}
		props[2] = int32(y)
		props[9] = 0
		if nrefs > 0 {
			// every row overwrites the entries of the references found;
			// those of missing references stay zero
			for k, j := range refIdx {
				rc := img.Channels[j]
				rr := rc.Pix[y*w : (y+1)*w]
				var rp []int32
				if y > 0 {
					rp = rc.Pix[(y-1)*w : y*w]
				}
				for x := 0; x < w; x++ {
					v := int64(rr[x])
					var vl, vt, vtl int64
					if x > 0 {
						vl = int64(rr[x-1])
					}
					if y > 0 {
						vt = int64(rp[x])
					} else {
						vt = vl
					}
					if x > 0 && y > 0 {
						vtl = int64(rp[x-1])
					} else {
						vtl = vl
					}
					pr := clampedGradient(vt, vl, vtl)
					o := x*nrefs*4 + k*4
					refRow[o] = int32(abs64(v))
					refRow[o+1] = int32(v)
					refRow[o+2] = int32(abs64(v - pr))
					refRow[o+3] = int32(v - pr)
				}
			}
		}
		for x := 0; x < w; x++ {
			var W, N, NW, NE, NN, WW, NEE int64
			if x > 0 {
				W = int64(row[x-1])
			} else if y > 0 {
				W = int64(prev[x])
			}
			if y > 0 {
				N = int64(prev[x])
			} else {
				N = W
			}
			if x > 0 && y > 0 {
				NW = int64(prev[x-1])
			} else {
				NW = W
			}
			if x+1 < w && y > 0 {
				NE = int64(prev[x+1])
			} else {
				NE = N
			}
			if y > 1 {
				NN = int64(prev2[x])
			} else {
				NN = N
			}
			if x > 1 {
				WW = int64(row[x-2])
			} else {
				WW = W
			}
			if x+2 < w && y > 0 {
				NEE = int64(prev[x+2])
			} else {
				NEE = NE
			}
			var wpPred int64
			if usesWP {
				var mx int64
				wpPred, mx = wp.predict(x, y, N, W, NE, NW, NN)
				props[15] = int32(mx)
			}
			if maxProp >= 3 {
				props[3] = int32(x)
				props[4] = int32(abs64(N))
				props[5] = int32(abs64(W))
				props[6] = int32(N)
				props[7] = int32(W)
				props[8] = int32(W - int64(props[9]))
				props[9] = int32(W + N - NW)
				props[10] = int32(W - NW)
				props[11] = int32(NW - N)
				props[12] = int32(N - NE)
				props[13] = int32(N - NN)
				props[14] = int32(W - WW)
				if nrefs == 1 {
					r := refRow[4*x : 4*x+4 : 4*x+4]
					props[16], props[17], props[18], props[19] = r[0], r[1], r[2], r[3]
				} else if nrefs > 0 {
					copy(props[16:], refRow[x*nrefs*4:(x+1)*nrefs*4])
				}
			}
			n := int32(0)
			for nodes[n].prop >= 0 {
				nd := &nodes[n]
				if props[nd.prop] > nd.split {
					n = nd.left
				} else {
					n = nd.right
				}
			}
			leaf := &nodes[n]
			guess := predictOne(leaf.pred, W, N, NW, NE, NN, WW, NEE, wpPred)
			tok := dec.ReadUintClustered(br, leaf.cluster)
			val := int64(int32(int64(bitio.UnpackSigned(tok))*int64(leaf.mult) + int64(leaf.offset) + guess))
			row[x] = int32(val)
			if usesWP {
				wp.update(val, x, y)
			}
		}
	}
	return nil
}

// decodeSingleLeaf handles a tree with one leaf and a non-WP predictor.
func decodeSingleLeaf(br *bitio.Reader, dec *entropy.Decoder, leaf *ptNode, ch *Channel) {
	w, h := ch.W, ch.H
	pix := ch.Pix
	mult := int64(leaf.mult)
	off := int64(leaf.offset)
	cl := leaf.cluster
	for y := 0; y < h; y++ {
		row := pix[y*w : (y+1)*w]
		var prev, prev2 []int32
		if y > 0 {
			prev = pix[(y-1)*w : y*w]
		}
		if y > 1 {
			prev2 = pix[(y-2)*w : (y-1)*w]
		}
		switch {
		case leaf.pred == predGradient && y > 0:
			// x = 0
			{
				N := int64(prev[0])
				tok := dec.ReadUintClustered(br, cl)
				row[0] = int32(int64(bitio.UnpackSigned(tok))*mult + off + N)
			}
			for x := 1; x < w; x++ {
				W := int64(row[x-1])
				N := int64(prev[x])
				NW := int64(prev[x-1])
				g := clampedGradient(N, W, NW)
				tok := dec.ReadUintClustered(br, cl)
				row[x] = int32(int64(bitio.UnpackSigned(tok))*mult + off + g)
			}
			continue
		}
		for x := 0; x < w; x++ {
			var W, N, NW, NE, NN, WW, NEE int64
			if x > 0 {
				W = int64(row[x-1])
			} else if y > 0 {
				W = int64(prev[x])
			}
			if y > 0 {
				N = int64(prev[x])
			} else {
				N = W
			}
			if x > 0 && y > 0 {
				NW = int64(prev[x-1])
			} else {
				NW = W
			}
			if x+1 < w && y > 0 {
				NE = int64(prev[x+1])
			} else {
				NE = N
			}
			if y > 1 {
				NN = int64(prev2[x])
			} else {
				NN = N
			}
			if x > 1 {
				WW = int64(row[x-2])
			} else {
				WW = W
			}
			if x+2 < w && y > 0 {
				NEE = int64(prev[x+2])
			} else {
				NEE = NE
			}
			g := predictOne(leaf.pred, W, N, NW, NE, NN, WW, NEE, 0)
			tok := dec.ReadUintClustered(br, cl)
			row[x] = int32(int64(bitio.UnpackSigned(tok))*mult + off + g)
		}
	}
}
