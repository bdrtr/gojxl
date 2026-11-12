package webp

// Macroblock reconstruction (prediction plus residual), libwebp's way: a
// work buffer holds each macroblock with its top row and left column of
// unfiltered neighbours (127 above the picture, 129 left of it).

const bps = 32 // work buffer stride

type recon struct {
	d       *vp8
	y, u, v []byte
	ys, uvs int
	// work buffers: luma rows -1..15, cols -1..19; chroma rows -1..7,
	// cols -1..7 (offset of pixel (0, 0): 1 + bps)
	wy, wu, wv [bps * 17]byte
}

const wo = 1 + bps // index of (0, 0) in a work buffer

func (r *recon) init() {}

func clip8(v int32) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func (r *recon) mb(mb *mbInfo, mx, my int) {
	d := r.d
	y0, x0 := my*16, mx*16
	wy, wu, wv := r.wy[:], r.wu[:], r.wv[:]
	// top row (with the top-left and, for luma, 4 top-right samples)
	if my == 0 {
		for i := -1; i < 20; i++ {
			wy[wo-bps+i] = 127
		}
		for i := -1; i < 8; i++ {
			wu[wo-bps+i], wv[wo-bps+i] = 127, 127
		}
	} else {
		above := r.y[(y0-1)*r.ys:]
		copy(wy[wo-bps:wo-bps+16], above[x0:x0+16])
		if mx < d.mbw-1 {
			copy(wy[wo-bps+16:wo-bps+20], above[x0+16:x0+20])
		} else {
			for i := 16; i < 20; i++ {
				wy[wo-bps+i] = above[x0+15]
			}
		}
		ua, va := r.u[(y0/2-1)*r.uvs:], r.v[(y0/2-1)*r.uvs:]
		copy(wu[wo-bps:wo-bps+8], ua[x0/2:x0/2+8])
		copy(wv[wo-bps:wo-bps+8], va[x0/2:x0/2+8])
		if mx == 0 {
			wy[wo-bps-1], wu[wo-bps-1], wv[wo-bps-1] = 129, 129, 129
		} else {
			wy[wo-bps-1] = above[x0-1]
			wu[wo-bps-1], wv[wo-bps-1] = ua[x0/2-1], va[x0/2-1]
		}
	}
	// left column
	for j := 0; j < 16; j++ {
		if mx == 0 {
			wy[wo+j*bps-1] = 129
		} else {
			wy[wo+j*bps-1] = r.y[(y0+j)*r.ys+x0-1]
		}
	}
	for j := 0; j < 8; j++ {
		if mx == 0 {
			wu[wo+j*bps-1], wv[wo+j*bps-1] = 129, 129
		} else {
			wu[wo+j*bps-1] = r.u[(y0/2+j)*r.uvs+x0/2-1]
			wv[wo+j*bps-1] = r.v[(y0/2+j)*r.uvs+x0/2-1]
		}
	}
	co := mb.coeffs[:]
	if mb.i4x4 {
		// the top-right samples serve the right column's blocks too
		for _, row := range [3]int{3, 7, 11} {
			copy(wy[wo+row*bps+16:wo+row*bps+20], wy[wo-bps+16:wo-bps+20])
		}
		for n := 0; n < 16; n++ {
			o := wo + (n/4)*4*bps + (n%4)*4
			pred4(wy, o, mb.imodes[n])
			if mb.nzY>>uint(n)&1 != 0 {
				idct(co[n*16:n*16+16], wy, o)
			}
		}
	} else {
		pred16(wy, checkMode(mx, my, mb.ymode))
		for n := 0; n < 16; n++ {
			if mb.nzY>>uint(n)&1 != 0 {
				idct(co[n*16:n*16+16], wy, wo+(n/4)*4*bps+(n%4)*4)
			}
		}
	}
	cm := checkMode(mx, my, mb.uvmode)
	pred8(wu, cm)
	pred8(wv, cm)
	for n := 0; n < 4; n++ {
		o := wo + (n/2)*4*bps + (n%2)*4
		if mb.nzU>>uint(n)&1 != 0 {
			idct(co[(16+n)*16:(17+n)*16], wu, o)
		}
		if mb.nzV>>uint(n)&1 != 0 {
			idct(co[(20+n)*16:(21+n)*16], wv, o)
		}
	}
	for j := 0; j < 16; j++ {
		copy(r.y[(y0+j)*r.ys+x0:], wy[wo+j*bps:wo+j*bps+16])
	}
	for j := 0; j < 8; j++ {
		copy(r.u[(y0/2+j)*r.uvs+x0/2:], wu[wo+j*bps:wo+j*bps+8])
		copy(r.v[(y0/2+j)*r.uvs+x0/2:], wv[wo+j*bps:wo+j*bps+8])
	}
}

func checkMode(mx, my, m int) int {
	if m != predDC {
		return m
	}
	switch {
	case mx == 0 && my == 0:
		return predDCNoTopLeft
	case mx == 0:
		return predDCNoLeft
	case my == 0:
		return predDCNoTop
	}
	return predDC
}

// idct adds the inverse transform of 16 coefficients to the 4x4 block at
// o (libwebp's TransformOne).
func idct(in []int16, dst []byte, o int) {
	mul1 := func(a int32) int32 { return (a*20091)>>16 + a }
	mul2 := func(a int32) int32 { return a * 35468 >> 16 }
	var tmp [16]int32
	for i := 0; i < 4; i++ {
		i0, i4, i8, i12 := int32(in[i]), int32(in[4+i]), int32(in[8+i]), int32(in[12+i])
		a := i0 + i8
		b := i0 - i8
		c := mul2(i4) - mul1(i12)
		dd := mul1(i4) + mul2(i12)
		tmp[i*4] = a + dd
		tmp[i*4+1] = b + c
		tmp[i*4+2] = b - c
		tmp[i*4+3] = a - dd
	}
	for i := 0; i < 4; i++ {
		dc := tmp[i] + 4
		a := dc + tmp[8+i]
		b := dc - tmp[8+i]
		c := mul2(tmp[4+i]) - mul1(tmp[12+i])
		dd := mul1(tmp[4+i]) + mul2(tmp[12+i])
		row := dst[o+i*bps : o+i*bps+4]
		row[0] = clip8(int32(row[0]) + (a+dd)>>3)
		row[1] = clip8(int32(row[1]) + (b+c)>>3)
		row[2] = clip8(int32(row[2]) + (b-c)>>3)
		row[3] = clip8(int32(row[3]) + (a-dd)>>3)
	}
}

func avg3(a, b, c byte) byte { return byte((int(a) + 2*int(b) + int(c) + 2) >> 2) }
func avg2(a, b byte) byte    { return byte((int(a) + int(b) + 1) >> 1) }

// trueMotion: clip(left + top - top-left) over a size x size block.
func trueMotion(w []byte, o, size int) {
	tl := int32(w[o-bps-1])
	for y := 0; y < size; y++ {
		l := int32(w[o+y*bps-1]) - tl
		for x := 0; x < size; x++ {
			w[o+y*bps+x] = clip8(l + int32(w[o-bps+x]))
		}
	}
}

func fill(w []byte, o, size int, v byte) {
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			w[o+y*bps+x] = v
		}
	}
}

func pred16(w []byte, m int) {
	o := wo
	switch m {
	case predDC, predDCNoTop, predDCNoLeft:
		s, n := 0, 0
		if m != predDCNoTop {
			for i := 0; i < 16; i++ {
				s += int(w[o-bps+i])
			}
			n++
		}
		if m != predDCNoLeft {
			for j := 0; j < 16; j++ {
				s += int(w[o+j*bps-1])
			}
			n++
		}
		if n == 2 {
			fill(w, o, 16, byte((s+16)>>5))
		} else {
			fill(w, o, 16, byte((s+8)>>4))
		}
	case predDCNoTopLeft:
		fill(w, o, 16, 0x80)
	case predTM:
		trueMotion(w, o, 16)
	case predVE:
		for y := 0; y < 16; y++ {
			copy(w[o+y*bps:o+y*bps+16], w[o-bps:o-bps+16])
		}
	case predHE:
		for y := 0; y < 16; y++ {
			v := w[o+y*bps-1]
			for x := 0; x < 16; x++ {
				w[o+y*bps+x] = v
			}
		}
	}
}

func pred8(w []byte, m int) {
	o := wo
	switch m {
	case predDC, predDCNoTop, predDCNoLeft:
		s, n := 0, 0
		if m != predDCNoTop {
			for i := 0; i < 8; i++ {
				s += int(w[o-bps+i])
			}
			n++
		}
		if m != predDCNoLeft {
			for j := 0; j < 8; j++ {
				s += int(w[o+j*bps-1])
			}
			n++
		}
		if n == 2 {
			fill(w, o, 8, byte((s+8)>>4))
		} else {
			fill(w, o, 8, byte((s+4)>>3))
		}
	case predDCNoTopLeft:
		fill(w, o, 8, 0x80)
	case predTM:
		trueMotion(w, o, 8)
	case predVE:
		for y := 0; y < 8; y++ {
			copy(w[o+y*bps:o+y*bps+8], w[o-bps:o-bps+8])
		}
	case predHE:
		for y := 0; y < 8; y++ {
			v := w[o+y*bps-1]
			for x := 0; x < 8; x++ {
				w[o+y*bps+x] = v
			}
		}
	}
}

// pred4 predicts the 4x4 block at o.
func pred4(w []byte, o int, m uint8) {
	set := func(x, y int, v byte) { w[o+y*bps+x] = v }
	top := func(i int) byte { return w[o-bps+i] } // i in -1..7
	left := func(j int) byte { return w[o+j*bps-1] }
	switch m {
	case predDC:
		s := 4
		for i := 0; i < 4; i++ {
			s += int(top(i)) + int(left(i))
		}
		fill(w, o, 4, byte(s>>3))
	case predTM:
		trueMotion(w, o, 4)
	case predVE:
		var v [4]byte
		for i := 0; i < 4; i++ {
			v[i] = avg3(top(i-1), top(i), top(i+1))
		}
		for y := 0; y < 4; y++ {
			copy(w[o+y*bps:o+y*bps+4], v[:])
		}
	case predHE:
		a, b, c, d, e := top(-1), left(0), left(1), left(2), left(3)
		rows := [4]byte{avg3(a, b, c), avg3(b, c, d), avg3(c, d, e), avg3(d, e, e)}
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				set(x, y, rows[y])
			}
		}
	case predRD:
		i, j, k, l := left(0), left(1), left(2), left(3)
		x, a, b, c, d := top(-1), top(0), top(1), top(2), top(3)
		set(0, 3, avg3(j, k, l))
		v := avg3(i, j, k)
		set(1, 3, v)
		set(0, 2, v)
		v = avg3(x, i, j)
		set(2, 3, v)
		set(1, 2, v)
		set(0, 1, v)
		v = avg3(a, x, i)
		set(3, 3, v)
		set(2, 2, v)
		set(1, 1, v)
		set(0, 0, v)
		v = avg3(b, a, x)
		set(3, 2, v)
		set(2, 1, v)
		set(1, 0, v)
		v = avg3(c, b, a)
		set(3, 1, v)
		set(2, 0, v)
		set(3, 0, avg3(d, c, b))
	case predLD:
		a, b, c, d := top(0), top(1), top(2), top(3)
		e, f, g, h := top(4), top(5), top(6), top(7)
		set(0, 0, avg3(a, b, c))
		v := avg3(b, c, d)
		set(1, 0, v)
		set(0, 1, v)
		v = avg3(c, d, e)
		set(2, 0, v)
		set(1, 1, v)
		set(0, 2, v)
		v = avg3(d, e, f)
		set(3, 0, v)
		set(2, 1, v)
		set(1, 2, v)
		set(0, 3, v)
		v = avg3(e, f, g)
		set(3, 1, v)
		set(2, 2, v)
		set(1, 3, v)
		v = avg3(f, g, h)
		set(3, 2, v)
		set(2, 3, v)
		set(3, 3, avg3(g, h, h))
	case predVR:
		i, j, k := left(0), left(1), left(2)
		x, a, b, c, d := top(-1), top(0), top(1), top(2), top(3)
		v := avg2(x, a)
		set(0, 0, v)
		set(1, 2, v)
		v = avg2(a, b)
		set(1, 0, v)
		set(2, 2, v)
		v = avg2(b, c)
		set(2, 0, v)
		set(3, 2, v)
		set(3, 0, avg2(c, d))
		set(0, 3, avg3(k, j, i))
		set(0, 2, avg3(j, i, x))
		v = avg3(i, x, a)
		set(0, 1, v)
		set(1, 3, v)
		v = avg3(x, a, b)
		set(1, 1, v)
		set(2, 3, v)
		v = avg3(a, b, c)
		set(2, 1, v)
		set(3, 3, v)
		set(3, 1, avg3(b, c, d))
	case predVL:
		a, b, c, d := top(0), top(1), top(2), top(3)
		e, f, g, h := top(4), top(5), top(6), top(7)
		set(0, 0, avg2(a, b))
		v := avg2(b, c)
		set(1, 0, v)
		set(0, 2, v)
		v = avg2(c, d)
		set(2, 0, v)
		set(1, 2, v)
		v = avg2(d, e)
		set(3, 0, v)
		set(2, 2, v)
		set(0, 1, avg3(a, b, c))
		v = avg3(b, c, d)
		set(1, 1, v)
		set(0, 3, v)
		v = avg3(c, d, e)
		set(2, 1, v)
		set(1, 3, v)
		v = avg3(d, e, f)
		set(3, 1, v)
		set(2, 3, v)
		set(3, 2, avg3(e, f, g))
		set(3, 3, avg3(f, g, h))
	case predHD:
		i, j, k, l := left(0), left(1), left(2), left(3)
		x, a, b, c := top(-1), top(0), top(1), top(2)
		v := avg2(i, x)
		set(0, 0, v)
		set(2, 1, v)
		v = avg2(j, i)
		set(0, 1, v)
		set(2, 2, v)
		v = avg2(k, j)
		set(0, 2, v)
		set(2, 3, v)
		set(0, 3, avg2(l, k))
		set(3, 0, avg3(a, b, c))
		set(2, 0, avg3(x, a, b))
		v = avg3(i, x, a)
		set(1, 0, v)
		set(3, 1, v)
		v = avg3(j, i, x)
		set(1, 1, v)
		set(3, 2, v)
		v = avg3(k, j, i)
		set(1, 2, v)
		set(3, 3, v)
		set(1, 3, avg3(l, k, j))
	case predHU:
		i, j, k, l := left(0), left(1), left(2), left(3)
		set(0, 0, avg2(i, j))
		v := avg2(j, k)
		set(2, 0, v)
		set(0, 1, v)
		v = avg2(k, l)
		set(2, 1, v)
		set(0, 2, v)
		set(1, 0, avg3(i, j, k))
		v = avg3(j, k, l)
		set(3, 0, v)
		set(1, 1, v)
		v = avg3(k, l, l)
		set(3, 1, v)
		set(1, 2, v)
		for _, p := range [6][2]int{{3, 2}, {2, 2}, {0, 3}, {1, 3}, {2, 3}, {3, 3}} {
			set(p[0], p[1], l)
		}
	}
}
