package enc

import (
	"math"
	"slices"

	"github.com/bdrtr/gojxl/internal/bitio"
)

// Patches for text-like content (libjxl enc_patch_dictionary.cc, used at
// effort 7): small connected components on flat backgrounds that occur
// at least twice are stored once in a reference frame and added back by
// the decoder, so the VarDCT frame only codes the smooth background.

const (
	patchSide    = 4  // flat squares seeding the background search
	maxPatchSize = 32 // largest component side
	patchRefID   = 3  // reference frame slot
)

// patch dequantisation (for deduplication) and colour weights, in XYB
var (
	patchDequant = [3]float32{0.01615, 0.08875, 0.1922}
	patchWeights = [3]float32{30, 3, 1}
)

// textPatch is one distinct component: quantised deltas from its
// background (the identity for deduplication), the float deltas of its
// first occurrence, and where it occurs.
type textPatch struct {
	w, h int
	q    [3][]int8
	f    [3][]float32
	pos  [][2]int
}

func patchSimilar(a, b [3]float32, threshold float32) bool {
	var d float32
	for c := 0; c < 3; c++ {
		d += abs32(a[c]-b[c]) * patchWeights[c]
	}
	return d <= threshold
}

// findTextPatches finds repeated text-like components of the W x H image
// in the XYB planes (row stride pw), before inverse Gaborish.
func findTextPatches(xyb [3][]float32, W, H, pw int) []textPatch {
	px := func(x, y int) [3]float32 {
		o := y*pw + x
		return [3]float32{xyb[0][o], xyb[1][o], xyb[2][o]}
	}
	// screenshots have seeds on 2-70% of the squares, photos on under
	// 0.3%: skip the search (its memory and time) below 1%
	seeds := screenshotSeeds(xyb, W, H, pw)
	if len(seeds) == 0 || len(seeds)*100 < (W/patchSide)*(H/patchSide) {
		return nil
	}
	// per pixel: bit 0 background, bit 1 visited (components)
	flags := make([]uint8, W*H)
	isBG := func(n int) bool { return flags[n]&1 != 0 }
	// a background pixel's colour is that of the seed pixel it grew from
	bgSrc := make([]int32, W*H)
	queue := make([]int32, 0, len(seeds)*patchSide*patchSide)
	for _, sd := range seeds {
		for y := sd[1]; y < sd[1]+patchSide; y++ {
			for x := sd[0]; x < sd[0]+patchSide; x++ {
				p := int32(y*W + x)
				queue = append(queue, p)
				flags[p] = 1
				bgSrc[p] = p
			}
		}
	}

	// grow the background from the seeds over similar pixels
	bg := func(n int) [3]float32 {
		s := int(bgSrc[n])
		return px(s%W, s/W)
	}
	for qi := 0; qi < len(queue); qi++ {
		cur := queue[qi]
		src := bgSrc[cur]
		sx, sy := int(src)%W, int(src)/W
		cx, cy := int(cur)%W, int(cur)/W
		sc := px(sx, sy)
		for dx := -1; dx <= 1; dx++ {
			for dy := -1; dy <= 1; dy++ {
				nx, ny := cx+dx, cy+dy
				if nx < 0 || ny < 0 || nx >= W || ny >= H || isBG(ny*W+nx) {
					continue
				}
				if abs(nx-sx)+abs(ny-sy) > 50 {
					continue
				}
				if patchSimilar(sc, px(nx, ny), 0.8) {
					n := ny*W + nx
					flags[n] = 1
					bgSrc[n] = src
					queue = append(queue, int32(n))
				}
			}
		}
	}
	queue = nil
	// components outside the background
	var stack []int32
	var found []textPatch
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			if flags[y*W+x] != 0 {
				continue
			}
			minX, maxX, minY, maxY := x, x, y, y
			ref := -1
			allSimilar := true
			// marked when pushed: each pixel is on the stack once
			flags[y*W+x] = 2
			stack = append(stack[:0], int32(y*W+x))
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				cx, cy := int(cur)%W, int(cur)/W
				minX, maxX = min(minX, cx), max(maxX, cx)
				minY, maxY = min(minY, cy), max(maxY, cy)
				for dx := -1; dx <= 1; dx++ {
					for dy := -1; dy <= 1; dy++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx, ny := cx+dx, cy+dy
						if nx < 0 || ny < 0 || nx >= W || ny >= H {
							continue
						}
						n := ny*W + nx
						if !isBG(n) {
							if flags[n] == 0 {
								flags[n] = 2
								stack = append(stack, int32(n))
							}
						} else if ref < 0 {
							ref = n
						} else if !patchSimilar(bg(n), bg(ref), 0.03) {
							allSimilar = false
						}
					}
				}
			}
			if ref < 0 || !allSimilar || maxX-minX >= maxPatchSize || maxY-minY >= maxPatchSize {
				continue
			}
			rc := bg(ref)
			hasSimilar := false
			for iy := max(minY-2, 0); iy < min(maxY+3, H) && !hasSimilar; iy++ {
				for ix := max(minX-2, 0); ix < min(maxX+3, W); ix++ {
					if patchSimilar(rc, px(ix, iy), 0.03) {
						hasSimilar = true
						break
					}
				}
			}
			if !hasSimilar {
				continue
			}
			p := textPatch{w: maxX - minX + 1, h: maxY - minY + 1}
			tooBig, tooSmall := false, true
			for c := 0; c < 3; c++ {
				p.q[c] = make([]int8, p.w*p.h)
				p.f[c] = make([]float32, p.w*p.h)
				for iy := 0; iy < p.h; iy++ {
					for ix := 0; ix < p.w; ix++ {
						fv := xyb[c][(minY+iy)*pw+minX+ix] - rc[c]
						p.f[c][iy*p.w+ix] = fv
						v := int(math.Trunc(float64(min(max(fv/patchDequant[c], -32768), 32767))))
						p.q[c][iy*p.w+ix] = int8(v)
						tooBig = tooBig || v != int(int8(v))
						tooSmall = tooSmall && v < 2 && v > -2
					}
				}
			}
			if tooSmall || tooBig {
				continue
			}
			p.pos = [][2]int{{minX, minY}}
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return nil
	}
	// identical quantised patches merge; keep those occurring twice
	cmp := func(a, b textPatch) int {
		if a.w != b.w {
			return a.w - b.w
		}
		if a.h != b.h {
			return a.h - b.h
		}
		for c := 0; c < 3; c++ {
			if r := slices.Compare(a.q[c], b.q[c]); r != 0 {
				return r
			}
		}
		return 0
	}
	// equal patches by position, as libjxl sorts (patch, positions) pairs:
	// the first occurrence is the one stored
	slices.SortStableFunc(found, func(a, b textPatch) int {
		if r := cmp(a, b); r != 0 {
			return r
		}
		if a.pos[0][0] != b.pos[0][0] {
			return a.pos[0][0] - b.pos[0][0]
		}
		return a.pos[0][1] - b.pos[0][1]
	})
	var out []textPatch
	for i := 0; i < len(found); {
		j := i + 1
		for j < len(found) && cmp(found[i], found[j]) == 0 {
			found[i].pos = append(found[i].pos, found[j].pos...)
			j++
		}
		if len(found[i].pos) >= 2 {
			out = append(out, found[i])
		}
		i = j
	}
	maxArea := 0
	for _, p := range out {
		maxArea = max(maxArea, p.w*p.h)
	}
	if maxArea < 20 {
		return nil
	}
	return out
}

// screenshotSeeds returns the top left corners of flat 4x4 squares with
// at least 8 of the 9 squares around starting with the same colour (the
// seeds of the background search).
func screenshotSeeds(xyb [3][]float32, W, H, pw int) [][2]int {
	px := func(x, y int) [3]float32 {
		o := y*pw + x
		return [3]float32{xyb[0][o], xyb[1][o], xyb[2][o]}
	}
	same := func(x, y int, c [3]float32) bool {
		o := y*pw + x
		for i := 0; i < 3; i++ {
			if math.Abs(float64(c[i]-xyb[i][o])) > 1e-4 {
				return false
			}
		}
		return true
	}
	sw, sh := W/patchSide, H/patchSide
	if sw < 3 || sh < 3 {
		return nil
	}
	var seeds [][2]int
	for py := 1; py < sh-2; py++ { // as libjxl (its last seed row stays unset)
		for qx := 1; qx < sw-1; qx++ {
			ox, oy := qx*patchSide, py*patchSide
			base := px(ox, oy)
			flat := true
			for iy := 0; iy < patchSide && flat; iy++ {
				for ix := 0; ix < patchSide; ix++ {
					if !same(ox+ix, oy+iy, base) {
						flat = false
						break
					}
				}
			}
			if !flat {
				continue
			}
			n := 0
			for y := (py - 1) * patchSide; y <= (py+1)*patchSide; y += patchSide {
				for x := (qx - 1) * patchSide; x <= (qx+1)*patchSide; x += patchSide {
					if same(x, y, base) {
						n++
					}
				}
			}
			if n >= 8 {
				seeds = append(seeds, [2]int{ox, oy})
			}
		}
	}
	return seeds
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// patchLayout places the patches in a reference image (libjxl's greedy
// bin packing, largest first) and returns its size and the positions.
func patchLayout(ps []textPatch) (rw, rh int, at [][2]int) {
	slices.SortStableFunc(ps, func(a, b textPatch) int { return b.w*b.h - a.w*a.h })
	maxW, maxH, total := 0, 0, 0
	for _, p := range ps {
		maxW, maxH, total = max(maxW, p.w), max(maxH, p.h), total+p.w*p.h
	}
	side := float32(math.Sqrt(float64(total)))
	rw, rh = int(max(float32(maxW), side)), int(max(float32(maxH), side))
	at = make([][2]int, len(ps))
	for {
		rw, rh = int(float32(rw)*1.05+1), int(float32(rh)*1.05+1)
		occ := make([]bool, rw*rh)
		ok := true
		maxY := 0
		for i, p := range ps {
			placed := false
			for y0 := 0; y0+p.h <= rh && !placed; y0++ {
				for x0 := 0; x0+p.w <= rw; {
					free, next := true, x0+1
					for y := y0; y < y0+p.h && free; y++ {
						for x := x0; x < x0+p.w; x++ {
							if occ[y*rw+x] {
								free, next = false, x+1
								break
							}
						}
					}
					if free {
						at[i] = [2]int{x0, y0}
						placed = true
						break
					}
					x0 = next
				}
			}
			if !placed {
				ok = false
				break
			}
			x0, y0 := at[i][0], at[i][1]
			for y := y0; y < y0+p.h; y++ {
				for x := x0; x < x0+p.w; x++ {
					occ[y*rw+x] = true
				}
			}
			maxY = max(maxY, y0+p.h)
		}
		if ok {
			return rw, maxY, at
		}
	}
}

// f16Float decodes IEEE half precision bits.
func f16Float(h uint16) float32 {
	exp := int(h >> 10 & 0x1f)
	mant := uint32(h & 0x3ff)
	switch exp {
	case 0:
		return float32(math.Ldexp(float64(mant), -24))
	case 31:
		return float32(math.Inf(1))
	}
	return math.Float32frombits(uint32(h>>15)<<31 | uint32(exp-15+127)<<23 | mant<<13)
}

// patchPlan is what the VarDCT frame needs: the reference frame (a
// complete frame, to precede it) and the dictionary to write in LfGlobal.
type patchPlan struct {
	frame []byte
	toks  [][2]uint32 // (context, value)
}

// planPatches finds patches in the padded XYB planes of a W x H image,
// writes their reference frame and subtracts what the decoder will add.
// nil when there are none.
// nec extra channels (alpha) are left alone by the patches (kNone).
func planPatches(xyb [3][]float32, W, H, pw int, d float32, nec int) *patchPlan {
	ps := findTextPatches(xyb, W, H, pw)
	if len(ps) == 0 {
		return nil
	}
	rw, rh, at := patchLayout(ps)
	if rw > 1024 || rh > 1024 {
		return nil // one modular group (group_size_shift 3)
	}
	// libjxl's lossy modular XYB quantisation for the distance
	inv := [3]float64{65536 / (1 + 23*float64(d)), 4096 / (1 + 14*float64(d)), 4096 / (1 + 14*float64(d))}
	var fbits [3]uint16
	var step [3]float32
	for c := 0; c < 3; c++ {
		fbits[c] = f16(float32(128 / inv[c]))
		step[c] = f16Float(fbits[c]) / 128
	}
	n := rw * rh
	q := [3][]int32{make([]int32, n), make([]int32, n), make([]int32, n)} // X, Y, B
	for i, p := range ps {
		x0, y0 := at[i][0], at[i][1]
		for c := 0; c < 3; c++ {
			for iy := 0; iy < p.h; iy++ {
				for ix := 0; ix < p.w; ix++ {
					q[c][(y0+iy)*rw+x0+ix] = int32(math.Round(float64(p.f[c][iy*p.w+ix] / step[c])))
				}
			}
		}
	}
	// what the decoder reconstructs: X = x*fX, Y = y*fY, B = (b-y + y)*fB
	dec := [3][]float32{make([]float32, n), make([]float32, n), make([]float32, n)}
	chY, chX, chB := make([]int32, n), make([]int32, n), make([]int32, n)
	for i := 0; i < n; i++ {
		chY[i], chX[i], chB[i] = q[1][i], q[0][i], q[2][i]-q[1][i]
		dec[0][i] = float32(chX[i]) * step[0]
		dec[1][i] = float32(chY[i]) * step[1]
		dec[2][i] = float32(chB[i]+chY[i]) * step[2]
	}
	chs := []channel{{w: rw, h: rh, pix: chY}, {w: rw, h: rh, pix: chX}, {w: rw, h: rh, pix: chB}}
	for range nec { // every frame has the image's extra channels; unused
		chs = append(chs, channel{w: rw, h: rh, pix: make([]int32, n)})
	}
	frame := writePatchFrame(chs, rw, rh, fbits, nec)
	// subtract the decoded patches
	for i, p := range ps {
		x0, y0 := at[i][0], at[i][1]
		for _, pos := range p.pos {
			for c := 0; c < 3; c++ {
				for iy := 0; iy < p.h; iy++ {
					row := xyb[c][(pos[1]+iy)*pw+pos[0]:]
					ref := dec[c][(y0+iy)*rw+x0:]
					for ix := 0; ix < p.w; ix++ {
						row[ix] -= ref[ix]
					}
				}
			}
		}
	}
	// the dictionary
	pl := &patchPlan{frame: frame}
	add := func(ctx int, v int) { pl.toks = append(pl.toks, [2]uint32{uint32(ctx), uint32(v)}) }
	add(0, len(ps))
	for i, p := range ps {
		add(1, patchRefID)
		add(3, at[i][0])
		add(3, at[i][1])
		add(2, p.w-1)
		add(2, p.h-1)
		add(7, len(p.pos)-1)
		for j, pos := range p.pos {
			if j == 0 {
				add(4, pos[0])
				add(4, pos[1])
			} else {
				add(6, int(bitio.PackSigned(int32(pos[0]-p.pos[j-1][0]))))
				add(6, int(bitio.PackSigned(int32(pos[1]-p.pos[j-1][1]))))
			}
			add(5, 2) // kAdd
			for range nec {
				add(5, 0) // kNone: alpha is coded in the frame itself
			}
		}
	}
	return pl
}

// writeDict writes the patch dictionary (start of LfGlobal).
func (pl *patchPlan) writeDict(w *bitio.Writer) {
	h := newHistograms(10, defaultCfg)
	for _, t := range pl.toks {
		h.add(int(t[0]), t[1])
	}
	c := writeCode(w, h)
	for _, t := range pl.toks {
		c.write(w, int(t[0]), t[1])
	}
}

// writePatchFrame writes a modular reference-only frame (saved before the
// colour transform in slot patchRefID) with the channels Y, X, B-Y and
// the LF dequantisation factors fbits (half floats of 128 x step).
func writePatchFrame(chs []channel, rw, rh int, fbits [3]uint16, nec int) []byte {
	w := &bitio.Writer{}
	w.WriteBool(false)                            // not all default
	w.Write(2, 2)                                 // reference only
	w.Write(1, 1)                                 // modular
	w.WriteU64(0)                                 // flags
	w.WriteU32(1, val(1), val(2), val(4), val(8)) // upsampling
	for range nec {
		w.WriteU32(1, val(1), val(2), val(4), val(8)) // extra channel upsampling
	}
	w.Write(2, 3)     // group size 1024: one group
	w.WriteBool(true) // crop (size only for reference frames)
	cd := [4]bitio.U32Dist{bitio.Bits(8), bitsOff(11, 256), bitsOff(14, 2304), bitsOff(30, 18688)}
	w.WriteU32(uint32(rw), cd[0], cd[1], cd[2], cd[3])
	w.WriteU32(uint32(rh), cd[0], cd[1], cd[2], cd[3])
	w.Write(2, patchRefID)                                                // save as reference
	w.WriteBool(true)                                                     // save before colour transform
	w.WriteU32(0, val(0), bitio.Bits(4), bitsOff(5, 16), bitsOff(10, 48)) // name
	w.WriteBool(false)                                                    // restoration filter not default
	w.WriteBool(false)                                                    // no Gaborish
	w.Write(2, 0)                                                         // no EPF
	w.WriteU64(0)                                                         // filter extensions
	w.WriteU64(0)                                                         // frame extensions
	// LfGlobal: LF dequantisation, then the modular image with its own tree
	sec := &bitio.Writer{}
	sec.WriteBool(false)
	for _, b := range fbits {
		sec.Write(16, uint64(b))
	}
	sec.WriteBool(false) // no global tree
	writeModularLocalEffort(sec, chs, 4)
	data := sec.Bytes()
	w.WriteBool(false) // no permutation
	w.ZeroPadToByte()
	w.WriteU32(uint32(len(data)), bitio.Bits(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712))
	w.ZeroPadToByte()
	w.AppendBytes(data)
	return w.Bytes()
}

// countSeeds counts the background seed squares (for tests).
func countSeeds(xyb [3][]float32, W, H, pw int) int {
	return len(screenshotSeeds(xyb, W, H, pw))
}
