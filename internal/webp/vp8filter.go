package webp

import (
	"github.com/bdrtr/gojxl/internal/par"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
)

// Loop filter (applied to the whole frame after reconstruction, macroblock
// by macroblock in raster order: left edge, inner vertical edges, top
// edge, inner horizontal edges) and output conversion, as in libwebp.

type fstrength struct {
	limit, ilevel, hev int
}

func (d *vp8) strengths() (fs [4][2]fstrength) {
	for s := 0; s < 4; s++ {
		base := d.level
		if d.segmentOn {
			base = d.segFilter[s]
			if !d.absDelta {
				base += d.level
			}
		}
		for i4 := 0; i4 < 2; i4++ {
			level := base
			if d.lfDelta {
				level += d.refDelta[0]
				if i4 == 1 {
					level += d.modeDelta[0]
				}
			}
			level = min(max(level, 0), 63)
			if level == 0 {
				continue // limit 0: no filtering
			}
			il := level
			if d.sharpness > 0 {
				if d.sharpness > 4 {
					il >>= 2
				} else {
					il >>= 1
				}
				il = min(il, 9-d.sharpness)
			}
			il = max(il, 1)
			hev := 0
			if level >= 40 {
				hev = 2
			} else if level >= 15 {
				hev = 1
			}
			fs[s][i4] = fstrength{limit: 2*level + il, ilevel: il, hev: hev}
		}
	}
	return
}

// loopFilter filters the frame. Rows of macroblocks run in parallel as a
// wavefront: macroblock (x, y) reads and writes pixels of (x, y-1) that
// (x+1, y-1) filters, so it waits until the row above has done x+1.
func (d *vp8) loopFilter(y, u, v []byte, ys, uvs int, inner []bool, segs []uint8, i4 []bool) {
	fs := d.strengths()
	nw := min(par.Workers(), d.mbh)
	if nw <= 1 || d.mbw*d.mbh < 64 {
		for my := 0; my < d.mbh; my++ {
			for mx := 0; mx < d.mbw; mx++ {
				d.filterMB(y, u, v, ys, uvs, inner, segs, i4, &fs, mx, my)
			}
		}
		return
	}
	done := make([]atomic.Int32, d.mbh) // macroblocks filtered per row
	var next atomic.Int32
	var wg sync.WaitGroup
	// a panic stops the wavefront (rows below would wait for it forever)
	// and is raised again on the caller
	var panicked atomic.Pointer[par.Panic]
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked.CompareAndSwap(nil, &par.Panic{Value: r})
				}
			}()
			for {
				my := int(next.Add(1) - 1)
				if my >= d.mbh || panicked.Load() != nil {
					return
				}
				for mx := 0; mx < d.mbw; mx++ {
					if my > 0 {
						for need := int32(min(mx+2, d.mbw)); done[my-1].Load() < need; {
							if panicked.Load() != nil {
								return
							}
							runtime.Gosched()
						}
					}
					d.filterMB(y, u, v, ys, uvs, inner, segs, i4, &fs, mx, my)
					done[my].Store(int32(mx + 1))
				}
			}
		}()
	}
	wg.Wait()
	if p := panicked.Load(); p != nil {
		panic(p.Value)
	}
}

// filterMB filters the edges of one macroblock (left, inner vertical, top,
// inner horizontal).
func (d *vp8) filterMB(y, u, v []byte, ys, uvs int, inner []bool, segs []uint8, i4 []bool, fs *[4][2]fstrength, mx, my int) {
	i := my*d.mbw + mx
	b := 0
	if i4[i] {
		b = 1
	}
	f := fs[segs[i]][b]
	if f.limit == 0 {
		return
	}
	yo := my*16*ys + mx*16
	uo := my*8*uvs + mx*8
	in := inner[i]
	if d.simple {
		if mx > 0 {
			simpleFilter(y, yo, 1, ys, f.limit+4)
		}
		if in {
			for k := 4; k < 16; k += 4 {
				simpleFilter(y, yo+k, 1, ys, f.limit)
			}
		}
		if my > 0 {
			simpleFilter(y, yo, ys, 1, f.limit+4)
		}
		if in {
			for k := 4; k < 16; k += 4 {
				simpleFilter(y, yo+k*ys, ys, 1, f.limit)
			}
		}
		return
	}
	if mx > 0 {
		filterLoop26(y, yo, 1, ys, 16, f.limit+4, f.ilevel, f.hev)
		filterLoop26(u, uo, 1, uvs, 8, f.limit+4, f.ilevel, f.hev)
		filterLoop26(v, uo, 1, uvs, 8, f.limit+4, f.ilevel, f.hev)
	}
	if in {
		for k := 4; k < 16; k += 4 {
			filterLoop24(y, yo+k, 1, ys, 16, f.limit, f.ilevel, f.hev)
		}
		filterLoop24(u, uo+4, 1, uvs, 8, f.limit, f.ilevel, f.hev)
		filterLoop24(v, uo+4, 1, uvs, 8, f.limit, f.ilevel, f.hev)
	}
	if my > 0 {
		filterLoop26(y, yo, ys, 1, 16, f.limit+4, f.ilevel, f.hev)
		filterLoop26(u, uo, uvs, 1, 8, f.limit+4, f.ilevel, f.hev)
		filterLoop26(v, uo, uvs, 1, 8, f.limit+4, f.ilevel, f.hev)
	}
	if in {
		for k := 4; k < 16; k += 4 {
			filterLoop24(y, yo+k*ys, ys, 1, 16, f.limit, f.ilevel, f.hev)
		}
		filterLoop24(u, uo+4*uvs, uvs, 1, 8, f.limit, f.ilevel, f.hev)
		filterLoop24(v, uo+4*uvs, uvs, 1, 8, f.limit, f.ilevel, f.hev)
	}

}

func iabs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// sclip1 clamps to [-128, 127], sclip2 to [-16, 15], clip1 to [0, 255].
func sclip1(v int) int { return min(max(v, -128), 127) }
func sclip2(v int) int { return min(max(v, -16), 15) }
func clip1(v int) byte { return byte(min(max(v, 0), 255)) }

// simpleFilter filters the 16 pixel positions p + i*vstride across an
// edge between p - hstride and p.
func simpleFilter(px []byte, p, hstride, vstride, thresh int) {
	t2 := 2*thresh + 1
	for i := 0; i < 16; i++ {
		q := p + i*vstride
		p1, p0 := int(px[q-2*hstride]), int(px[q-hstride])
		q0, q1 := int(px[q]), int(px[q+hstride])
		if 4*iabs(p0-q0)+iabs(p1-q1) <= t2 {
			doFilter2(px, q, hstride)
		}
	}
}

func doFilter2(px []byte, q, s int) {
	p1, p0, q0, q1 := int(px[q-2*s]), int(px[q-s]), int(px[q]), int(px[q+s])
	a := 3*(q0-p0) + sclip1(p1-q1)
	a1 := sclip2((a + 4) >> 3)
	a2 := sclip2((a + 3) >> 3)
	px[q-s] = clip1(p0 + a2)
	px[q] = clip1(q0 - a1)
}

// filterLoop26 filters a macroblock edge: size positions p + i*vstride,
// across the edge between p - hstride and p (each position's 8 samples
// loaded once: filter test, high edge variance test and the 2- or 6-tap
// filter).
func filterLoop26(px []byte, p, hstride, vstride, size, thresh, ithresh, hevT int) {
	t2 := 2*thresh + 1
	s := hstride
	for i := 0; i < size; i++ {
		q := p + i*vstride
		w := px[q-4*s : q+3*s+1]
		p3, p2, p1, p0 := int(w[0]), int(w[s]), int(w[2*s]), int(w[3*s])
		q0, q1, q2, q3 := int(w[4*s]), int(w[5*s]), int(w[6*s]), int(w[7*s])
		if 4*iabs(p0-q0)+iabs(p1-q1) > t2 ||
			iabs(p3-p2) > ithresh || iabs(p2-p1) > ithresh || iabs(p1-p0) > ithresh ||
			iabs(q3-q2) > ithresh || iabs(q2-q1) > ithresh || iabs(q1-q0) > ithresh {
			continue
		}
		if iabs(p1-p0) > hevT || iabs(q1-q0) > hevT {
			a := 3*(q0-p0) + sclip1(p1-q1)
			w[3*s] = clip1(p0 + sclip2((a+3)>>3))
			w[4*s] = clip1(q0 - sclip2((a+4)>>3))
			continue
		}
		a := sclip1(3*(q0-p0) + sclip1(p1-q1))
		a1 := (27*a + 63) >> 7
		a2 := (18*a + 63) >> 7
		a3 := (9*a + 63) >> 7
		w[s] = clip1(p2 + a3)
		w[2*s] = clip1(p1 + a2)
		w[3*s] = clip1(p0 + a1)
		w[4*s] = clip1(q0 - a1)
		w[5*s] = clip1(q1 - a2)
		w[6*s] = clip1(q2 - a3)
	}
}

// filterLoop24 filters an inner edge (2- or 4-tap filter).
func filterLoop24(px []byte, p, hstride, vstride, size, thresh, ithresh, hevT int) {
	t2 := 2*thresh + 1
	s := hstride
	for i := 0; i < size; i++ {
		q := p + i*vstride
		w := px[q-4*s : q+3*s+1]
		p3, p2, p1, p0 := int(w[0]), int(w[s]), int(w[2*s]), int(w[3*s])
		q0, q1, q2, q3 := int(w[4*s]), int(w[5*s]), int(w[6*s]), int(w[7*s])
		if 4*iabs(p0-q0)+iabs(p1-q1) > t2 ||
			iabs(p3-p2) > ithresh || iabs(p2-p1) > ithresh || iabs(p1-p0) > ithresh ||
			iabs(q3-q2) > ithresh || iabs(q2-q1) > ithresh || iabs(q1-q0) > ithresh {
			continue
		}
		if iabs(p1-p0) > hevT || iabs(q1-q0) > hevT {
			a := 3*(q0-p0) + sclip1(p1-q1)
			w[3*s] = clip1(p0 + sclip2((a+3)>>3))
			w[4*s] = clip1(q0 - sclip2((a+4)>>3))
			continue
		}
		a := 3 * (q0 - p0)
		a1 := sclip2((a + 4) >> 3)
		a2 := sclip2((a + 3) >> 3)
		a3 := (a1 + 1) >> 1
		w[2*s] = clip1(p1 + a3)
		w[3*s] = clip1(p0 + a2)
		w[4*s] = clip1(q0 - a1)
		w[5*s] = clip1(q1 - a3)
	}
}

// ---------------------------------------------------------------- output

// libwebp's YUV to RGB (14-bit fixed point), as per-sample tables:
// r = clip((yT[y] + vR[v]) >> 6), g = clip((yT[y] - uG[u] - vG[v]) >> 6),
// b = clip((yT[y] + uB[u]) >> 6), clip to [0, 255] via clipTab.
var yT, vR, uG, vG, uB [256]int32

// sums lie in [-17685, 34237], so (sum >> 6) + clipOff in [235, 1046]
const clipOff = 512

var clipTab [1536]uint8

func init() {
	mh := func(v, c int32) int32 { return v * c >> 8 }
	for i := int32(0); i < 256; i++ {
		yT[i] = mh(i, 19077)
		vR[i] = mh(i, 26149) - 14234
		uG[i] = mh(i, 6419)
		vG[i] = mh(i, 13320) - 8708
		uB[i] = mh(i, 33050) - 17685
	}
	for i := range clipTab {
		clipTab[i] = uint8(min(max(i-clipOff, 0), 255))
	}
}

// rgbOf writes the RGB of luma y and packed chroma uv (u | v<<16).
func rgbOf(y byte, uv uint32, o []uint8) {
	u, v := uv&0xff, uv>>16&0xff
	yy := yT[y]
	o[0] = clipTab[(yy+vR[v])>>6+clipOff]
	o[1] = clipTab[(yy-uG[u]-vG[v])>>6+clipOff]
	o[2] = clipTab[(yy+uB[u])>>6+clipOff]
}

// upsampleRows converts one or two luma rows with libwebp's "fancy"
// chroma upsampling (top chroma row tu/tv, current cu/cv).
func upsampleRows(ty, by []byte, tu, tv, cu, cv []byte, td, bd []uint8, w int) {
	last := (w - 1) >> 1
	ty, td = ty[:w], td[:4*w]
	if by != nil {
		by, bd = by[:w], bd[:4*w]
	}
	tl := uint32(tu[0]) | uint32(tv[0])<<16
	l := uint32(cu[0]) | uint32(cv[0])<<16
	rgbOf(ty[0], (3*tl+l+0x00020002)>>2, td[0:])
	if by != nil {
		rgbOf(by[0], (3*l+tl+0x00020002)>>2, bd[0:])
	}
	tu, tv, cu, cv = tu[:last+1], tv[:last+1], cu[:last+1], cv[:last+1]
	for x := 1; x <= last; x++ {
		t := uint32(tu[x]) | uint32(tv[x])<<16
		uv := uint32(cu[x]) | uint32(cv[x])<<16
		avg := tl + t + l + uv + 0x00080008
		d12 := (avg + 2*(t+l)) >> 3
		d03 := (avg + 2*(tl+uv)) >> 3
		rgbOf(ty[2*x-1], (d12+tl)>>1, td[4*(2*x-1):])
		rgbOf(ty[2*x], (d03+t)>>1, td[8*x:])
		if by != nil {
			rgbOf(by[2*x-1], (d03+l)>>1, bd[4*(2*x-1):])
			rgbOf(by[2*x], (d12+uv)>>1, bd[8*x:])
		}
		tl, l = t, uv
	}
	if w&1 == 0 {
		rgbOf(ty[w-1], (3*tl+l+0x00020002)>>2, td[4*(w-1):])
		if by != nil {
			rgbOf(by[w-1], (3*l+tl+0x00020002)>>2, bd[4*(w-1):])
		}
	}
}

// yuvToRGBA fills img (alpha 255) from 4:2:0 planes.
func yuvToRGBA(img *image.NRGBA, yp, up, vp []byte, w, h, ys, uvs int) {
	rowY := func(y int) []byte { return yp[y*ys : y*ys+w] }
	rowU := func(c int) []byte { return up[c*uvs:] }
	rowV := func(c int) []byte { return vp[c*uvs:] }
	dst := func(y int) []uint8 { return img.Pix[y*img.Stride : y*img.Stride+4*w] }
	// row 0 alone, then pairs (2k+1, 2k+2) from chroma rows k and k+1, in
	// parallel bands of pairs; an even height ends with a single row
	pairs := (h - 1) / 2
	band := max(16, pairs/(4*par.Workers())+1)
	par.Do((pairs+band-1)/band, func(_, b int) {
		for k := b * band; k < min((b+1)*band, pairs); k++ {
			y := 2*k + 1
			upsampleRows(rowY(y), rowY(y+1), rowU(k), rowV(k), rowU(k+1), rowV(k+1), dst(y), dst(y+1), w)
			alpha255(dst(y))
			alpha255(dst(y + 1))
		}
	})
	upsampleRows(rowY(0), nil, rowU(0), rowV(0), rowU(0), rowV(0), dst(0), nil, w)
	alpha255(dst(0))
	if y := 2*pairs + 1; y < h {
		upsampleRows(rowY(y), nil, rowU(pairs), rowV(pairs), rowU(pairs), rowV(pairs), dst(y), nil, w)
		alpha255(dst(y))
	}
}

func alpha255(row []uint8) {
	for i := 3; i < len(row); i += 4 {
		row[i] = 255
	}
}

// decodeAlpha decodes an ALPH chunk into img's alpha samples.
func decodeAlpha(d []byte, img *image.NRGBA) error {
	if len(d) < 1 {
		return errorf("ALPH size")
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	method, filter := d[0]&3, d[0]>>2&3
	a := make([]byte, w*h)
	switch method {
	case 0:
		if len(d)-1 < w*h {
			return errorf("ALPH data")
		}
		copy(a, d[1:])
	case 1:
		pix, err := decodeVP8LStream(d[1:], w, h)
		if err != nil {
			return err
		}
		for i, v := range pix {
			a[i] = uint8(v >> 8)
		}
	default:
		return errorf("ALPH compression %d", method)
	}
	for y := 0; y < h; y++ {
		row := a[y*w : (y+1)*w]
		var prev []byte
		if y > 0 {
			prev = a[(y-1)*w : y*w]
		}
		switch {
		case filter == 0:
		case filter == 1 || prev == nil: // horizontal (and every first row)
			pred := byte(0)
			if prev != nil {
				pred = prev[0]
			}
			for i := range row {
				row[i] += pred
				pred = row[i]
			}
		case filter == 2:
			for i := range row {
				row[i] += prev[i]
			}
		default: // gradient
			top := prev[0]
			tl, left := top, top
			for i := range row {
				top = prev[i]
				g := int(left) + int(top) - int(tl)
				left = row[i] + byte(min(max(g, 0), 255))
				tl = top
				row[i] = left
			}
		}
	}
	for i, v := range a {
		img.Pix[4*i+3] = v
	}
	return nil
}
