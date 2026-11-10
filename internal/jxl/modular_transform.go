package jxl

// Inverse modular transforms (RCT, Palette, Squeeze).

func (img *ModularImage) undoTransforms() error {
	for i := len(img.Transforms) - 1; i >= 0; i-- {
		t := &img.Transforms[i]
		var err error
		switch t.ID {
		case TrRCT:
			img.invRCT(t)
		case TrPalette:
			err = img.invPalette(t)
		case TrSqueeze:
			err = img.invSqueeze(t)
		}
		if err != nil {
			return err
		}
	}
	img.Transforms = nil
	return nil
}

func (img *ModularImage) invRCT(t *Transform) {
	b := t.BeginC
	c0, c1, c2 := img.Channels[b], img.Channels[b+1], img.Channels[b+2]
	perm := t.RCTType / 7
	typ := t.RCTType % 7
	n := len(c0.Pix)
	p0, p1, p2 := c0.Pix, c1.Pix[:n], c2.Pix[:n]
	switch typ {
	case 0:
	case 6: // YCgCo
		for i := 0; i < n; i++ {
			Y, Co, Cg := p0[i], p1[i], p2[i]
			tmp := Y - (Cg >> 1)
			G := Cg + tmp
			B := tmp - (Co >> 1)
			R := B + Co
			p0[i], p1[i], p2[i] = R, G, B
		}
	default:
		second := typ >> 1
		third := typ & 1
		for i := 0; i < n; i++ {
			a, bb, c := p0[i], p1[i], p2[i]
			if third != 0 {
				c += a
			}
			if second == 1 {
				bb += a
			} else if second == 2 {
				bb += (a + c) >> 1
			}
			p0[i], p1[i], p2[i] = a, bb, c
		}
	}
	// permutation: output channel order
	if perm != 0 {
		out := [3]*Channel{}
		in := [3]*Channel{c0, c1, c2}
		out[perm%3] = in[0]
		out[(perm+1+perm/3)%3] = in[1]
		out[(perm+2-perm/3)%3] = in[2]
		img.Channels[b], img.Channels[b+1], img.Channels[b+2] = out[0], out[1], out[2]
	}
}

var deltaPalette = [72][3]int32{
	{0, 0, 0}, {4, 4, 4}, {11, 0, 0}, {0, 0, -13}, {0, -12, 0}, {-10, -10, -10},
	{-18, -18, -18}, {-27, -27, -27}, {-18, -18, 0}, {0, 0, -32}, {-32, 0, 0}, {-37, -37, -37},
	{0, -32, -32}, {24, 24, 45}, {50, 50, 50}, {-45, -24, -24}, {-24, -45, -45}, {0, -24, -24},
	{-34, -34, 0}, {-24, 0, -24}, {-45, -45, -24}, {64, 64, 64}, {-32, 0, -32}, {0, -32, 0},
	{-32, 0, 32}, {-24, -45, -24}, {45, 24, 45}, {24, -24, -45}, {-45, -24, 24}, {80, 80, 80},
	{64, 0, 0}, {0, 0, -64}, {0, -64, -64}, {-24, -24, 45}, {96, 96, 96}, {64, 64, 0},
	{45, -24, -24}, {34, -34, 0}, {112, 112, 112}, {24, -45, -45}, {45, 45, -24}, {0, -32, 32},
	{24, -24, 45}, {0, 96, 96}, {45, -24, 24}, {24, -45, -24}, {-24, -45, 24}, {0, -64, 0},
	{96, 0, 0}, {128, 128, 128}, {64, 0, 64}, {144, 144, 144}, {96, 96, 0}, {-36, -36, 36},
	{45, -24, -45}, {45, -45, -24}, {0, 0, -96}, {0, 128, 128}, {0, 96, 0}, {45, 24, -45},
	{-128, 0, 0}, {24, -45, 24}, {-45, 24, -45}, {64, 0, -64}, {64, -64, -64}, {96, 0, 96},
	{45, -45, 24}, {24, 45, -45}, {64, 64, -64}, {128, 128, 0}, {0, 0, -128}, {-24, 45, -45},
}

func paletteValue(pal *Channel, index int64, c int, bitDepth int) int32 {
	palSize := int64(pal.W)
	if index < 0 {
		if c >= 3 {
			return 0
		}
		index = -(index + 1)
		index %= 1 + 2*(72-1)
		r := deltaPalette[(index+1)>>1][c]
		if index&1 == 0 {
			r = -r
		}
		if bitDepth > 8 {
			r *= 1 << (bitDepth - 8)
		}
		return r
	}
	if index >= palSize && index < palSize+64 {
		if c >= 3 {
			return 0
		}
		index -= palSize
		index >>= uint(c * 2)
		return int32((uint64(index%4)*((uint64(1)<<bitDepth)-1))>>2) + int32(1<<max(0, bitDepth-3))
	}
	if index >= palSize+64 {
		if c >= 3 {
			return 0
		}
		index -= palSize + 64
		switch c {
		case 1:
			index /= 5
		case 2:
			index /= 25
		}
		return int32((uint64(index%5) * ((uint64(1) << bitDepth) - 1)) >> 2)
	}
	return pal.Pix[c*pal.W+int(index)]
}

func (img *ModularImage) invPalette(t *Transform) error {
	if img.NbMeta < 1 {
		return invalid("palette without meta channel")
	}
	c0 := t.BeginC + 1
	if c0 >= len(img.Channels) {
		return invalid("palette channel")
	}
	pal := img.Channels[0]
	idxCh := img.Channels[c0]
	w, h := idxCh.W, idxCh.H
	nb := t.NumC
	bitDepth := min(img.BitDepth, 24)
	// create output channels
	outs := make([]*Channel, nb)
	outs[0] = idxCh
	for i := 1; i < nb; i++ {
		outs[i] = newChannel(w, h, idxCh.HShift, idxCh.VShift)
	}
	indices := idxCh.Pix
	if nb > 0 {
		indices = append([]int32(nil), idxCh.Pix...)
		outs[0] = newChannel(w, h, idxCh.HShift, idxCh.VShift)
	}
	if w > 0 {
		for c := 0; c < nb; c++ {
			out := outs[c].Pix
			for i, idx := range indices {
				out[i] = paletteValue(pal, int64(idx), c, bitDepth)
			}
		}
		if t.NbDeltas > 0 && t.DPred != predZero {
			for c := 0; c < nb; c++ {
				ch := outs[c]
				var wp *wpState
				if t.DPred == predWeighted {
					wp = newWPState(&img.WP, w)
				}
				for y := 0; y < h; y++ {
					row := ch.Pix[y*w : (y+1)*w]
					var prev, prev2 []int32
					if y > 0 {
						prev = ch.Pix[(y-1)*w : y*w]
					}
					if y > 1 {
						prev2 = ch.Pix[(y-2)*w : (y-1)*w]
					}
					irow := indices[y*w : (y+1)*w]
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
						if wp != nil {
							wpPred, _ = wp.predict(x, y, N, W, NE, NW, NN)
						}
						if int(irow[x]) < t.NbDeltas {
							row[x] = int32(int64(row[x]) + predictOne(uint8(t.DPred), W, N, NW, NE, NN, WW, NEE, wpPred))
						}
						if wp != nil {
							wp.update(int64(row[x]), x, y)
						}
					}
				}
			}
		}
	}
	// replace channels: remove palette meta channel (index 0), insert outputs
	chs := make([]*Channel, 0, len(img.Channels)+nb)
	chs = append(chs, img.Channels[1:c0]...)
	chs = append(chs, outs...)
	chs = append(chs, img.Channels[c0+1:]...)
	img.Channels = chs
	img.NbMeta--
	return nil
}

func smoothTendency(B, a, n int64) int64 {
	var diff int64
	if B >= a && a >= n {
		diff = (4*B - 3*n - a + 6) / 12
		if diff-(diff&1) > 2*(B-a) {
			diff = 2*(B-a) + 1
		}
		if diff+(diff&1) > 2*(a-n) {
			diff = 2 * (a - n)
		}
	} else if B <= a && a <= n {
		diff = (4*B - 3*n - a - 6) / 12
		if diff+(diff&1) < 2*(B-a) {
			diff = 2*(B-a) - 1
		}
		if diff-(diff&1) < 2*(a-n) {
			diff = 2 * (a - n)
		}
	}
	return diff
}

func (img *ModularImage) invSqueeze(t *Transform) error {
	for i := len(t.Squeezes) - 1; i >= 0; i-- {
		s := t.Squeezes[i]
		begin := s.BeginC
		end := begin + s.NumC - 1
		var offset int
		if s.InPlace {
			offset = end + 1
		} else {
			offset = len(img.Channels) + begin - end - 1
		}
		if begin < img.NbMeta {
			img.NbMeta -= s.NumC
		}
		for c := begin; c <= end; c++ {
			rc := offset + c - begin
			if rc >= len(img.Channels) {
				return invalid("squeeze residual channel")
			}
			ch, res := img.Channels[c], img.Channels[rc]
			if ch.W < res.W || ch.H < res.H {
				return invalid("squeeze residual size")
			}
			var out *Channel
			if s.Horizontal {
				if ch.H != res.H || ch.W != (ch.W+res.W+1)/2 {
					return invalid("squeeze dims")
				}
				out = invHSqueeze(ch, res)
			} else {
				if ch.W != res.W || ch.H != (ch.H+res.H+1)/2 {
					return invalid("squeeze dims")
				}
				out = invVSqueeze(ch, res)
			}
			img.Channels[c] = out
		}
		img.Channels = append(img.Channels[:offset], img.Channels[offset+s.NumC:]...)
	}
	return nil
}

func invHSqueeze(ch, res *Channel) *Channel {
	if res.W == 0 {
		ch.HShift--
		return ch
	}
	ow := ch.W + res.W
	out := newChannel(ow, ch.H, ch.HShift-1, ch.VShift)
	for y := 0; y < ch.H; y++ {
		avg := ch.Pix[y*ch.W : (y+1)*ch.W]
		r := res.Pix[y*res.W : (y+1)*res.W]
		o := out.Pix[y*ow : (y+1)*ow]
		for x := 0; x < res.W; x++ {
			a := int64(avg[x])
			var next int64
			if x+1 < ch.W {
				next = int64(avg[x+1])
			} else {
				next = a
			}
			var left int64
			if x > 0 {
				left = int64(o[2*x-1])
			} else {
				left = a
			}
			diff := int64(r[x]) + smoothTendency(left, a, next)
			A := a + diff/2
			o[2*x] = int32(A)
			o[2*x+1] = int32(A - diff)
		}
		if ow&1 != 0 {
			o[ow-1] = avg[ch.W-1]
		}
	}
	return out
}

func invVSqueeze(ch, res *Channel) *Channel {
	if res.H == 0 {
		ch.VShift--
		return ch
	}
	w := ch.W
	oh := ch.H + res.H
	out := newChannel(w, oh, ch.HShift, ch.VShift-1)
	for y := 0; y < res.H; y++ {
		avg := ch.Pix[y*w : (y+1)*w]
		var navg []int32
		if y+1 < ch.H {
			navg = ch.Pix[(y+1)*w : (y+2)*w]
		} else {
			navg = avg
		}
		r := res.Pix[y*w : (y+1)*w]
		o0 := out.Pix[2*y*w : (2*y+1)*w]
		o1 := out.Pix[(2*y+1)*w : (2*y+2)*w]
		var top []int32
		if y > 0 {
			top = out.Pix[(2*y-1)*w : 2*y*w]
		} else {
			top = avg
		}
		for x := 0; x < w; x++ {
			a := int64(avg[x])
			diff := int64(r[x]) + smoothTendency(int64(top[x]), a, int64(navg[x]))
			A := a + diff/2
			o0[x] = int32(A)
			o1[x] = int32(A - diff)
		}
	}
	if oh&1 != 0 {
		copy(out.Pix[(oh-1)*w:], ch.Pix[(ch.H-1)*w:ch.H*w])
	}
	return out
}
