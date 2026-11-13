package jxl

import (
	"math"
	"sort"

	"github.com/bdrtr/gojxl/internal/bitio"
	"github.com/bdrtr/gojxl/internal/entropy"
)

// ---------------------------------------------------------------- patches

type patchRef struct {
	ref            int
	x0, y0, xs, ys int
}

type patchPos struct {
	x, y   int
	refIdx int
	blend  []patchBlend // 1 + num extra channels
}

type patchDict struct {
	refs []patchRef
	pos  []patchPos
}

func (fd *frameDecoder) readPatches(br *bitio.Reader) error {
	code, err := entropy.ReadCode(br, 10)
	if err != nil {
		return err
	}
	d := code.NewDecoder(br)
	rd := func(ctx int) int { return int(d.ReadUint(br, ctx)) }
	nec := len(fd.ih.ExtraChannels)
	xsize, ysize := fd.fh.XSize, fd.fh.YSize
	numRef := rd(0)
	maxRef := 1024 + xsize*ysize/4
	if numRef > maxRef {
		return invalid("too many patches")
	}
	pd := &patchDict{}
	total := 0
	for id := 0; id < numRef; id++ {
		var r patchRef
		r.ref = rd(1)
		if r.ref >= 4 || fd.dec.refs[r.ref] == nil || fd.dec.refs[r.ref].color[0] == nil {
			return invalid("patch reference frame")
		}
		rf := fd.dec.refs[r.ref]
		r.x0 = rd(3)
		r.y0 = rd(3)
		r.xs = rd(2) + 1
		r.ys = rd(2) + 1
		if r.x0+r.xs > rf.color[0].W || r.y0+r.ys > rf.color[0].H {
			return invalid("patch reference position")
		}
		count := rd(7) + 1
		total += count
		if total > maxRef*4 {
			return invalid("too many patches")
		}
		chooseAlpha := nec > 1
		for i := 0; i < count; i++ {
			p := patchPos{refIdx: len(pd.refs)}
			if i == 0 {
				p.x = rd(4)
				p.y = rd(4)
			} else {
				last := pd.pos[len(pd.pos)-1]
				dx := int(bitio.UnpackSigned(uint32(rd(6))))
				dy := int(bitio.UnpackSigned(uint32(rd(6))))
				p.x = last.x + dx
				p.y = last.y + dy
				if p.x < 0 || p.y < 0 {
					return invalid("negative patch position")
				}
			}
			if p.x+r.xs > xsize || p.y+r.ys > ysize {
				return invalid("patch out of frame")
			}
			p.blend = make([]patchBlend, nec+1)
			for j := range p.blend {
				m := rd(5)
				if m >= 8 {
					return invalid("patch blend mode")
				}
				b := patchBlend{mode: uint8(m)}
				usesAlpha := m == pbBlendAbove || m == pbBlendBelow || m == pbAWAddAbove || m == pbAWAddBelow
				if usesAlpha && chooseAlpha {
					b.alpha = rd(8)
					if b.alpha >= nec {
						return invalid("patch alpha channel")
					}
				}
				if usesAlpha || m == pbMul {
					b.clamp = rd(9) != 0
				}
				p.blend[j] = b
			}
			pd.pos = append(pd.pos, p)
		}
		pd.refs = append(pd.refs, r)
	}
	if err := d.Finish(); err != nil {
		return err
	}
	fd.patches = pd
	return nil
}

func (fd *frameDecoder) applyPatches() {
	pd := fd.patches
	if pd == nil || len(pd.pos) == 0 || fd.color[0] == nil {
		return
	}
	nec := len(fd.ih.ExtraChannels)
	planes := make([]*Plane, 3+nec)
	copy(planes, fd.color[:])
	copy(planes[3:], fd.ec)
	// apply patches in order; parallelise over rows
	H := planes[0].H
	byRow := make([][]int, H)
	for i, p := range pd.pos {
		r := pd.refs[p.refIdx]
		for y := p.y; y < p.y+r.ys && y < H; y++ {
			byRow[y] = append(byRow[y], i)
		}
	}
	_ = parallelFor(H, func(y int) error {
		idx := byRow[y]
		if len(idx) == 0 {
			return nil
		}
		bgRow := make([][]float32, 3+nec)
		fgRow := make([][]float32, 3+nec)
		tmp := make([][]float32, 3+nec)
		for _, i := range idx {
			p := pd.pos[i]
			r := pd.refs[p.refIdx]
			rf := fd.dec.refs[r.ref]
			iy := y - p.y
			for c := 0; c < 3+nec; c++ {
				var src *Plane
				if c < 3 {
					src = rf.color[c]
				} else {
					src = rf.ec[c-3]
				}
				bgRow[c] = planes[c].Row(y)[p.x : p.x+r.xs]
				fgRow[c] = src.Row(r.y0 + iy)[r.x0 : r.x0+r.xs]
				if len(tmp[c]) < r.xs {
					tmp[c] = make([]float32, r.xs)
				}
			}
			performBlending(bgRow, fgRow, bgRow, r.xs, p.blend[0], p.blend[1:], fd.ih.ExtraChannels, tmp)
		}
		return nil
	})
}

// ---------------------------------------------------------------- splines

type splinePoint struct{ x, y float32 }

type quantSpline struct {
	cps   [][2]int64
	color [3][32]int32
	sigma [32]int32
}

type splineSegment struct {
	cx, cy      float32
	color       [3]float32
	invSigma    float32
	sigma4Inten float32
	maxDist     float32
	y0, y1      int
}

type splineSet struct {
	start   []splinePoint
	splines []quantSpline
	qadj    int32
	segs    []splineSegment
	byRow   [][]int32
}

func (fd *frameDecoder) readSplines(br *bitio.Reader) error {
	code, err := entropy.ReadCode(br, 6)
	if err != nil {
		return err
	}
	d := code.NewDecoder(br)
	rd := func(ctx int) uint32 { return d.ReadUint(br, ctx) }
	numPixels := fd.fh.XSize * fd.fh.YSize
	maxCP := min(1<<20, numPixels/2)
	ns := int(rd(2))
	if ns > maxCP || ns+1 > maxCP {
		return invalid("too many splines")
	}
	ns++
	s := &splineSet{}
	var lx, ly int64
	for i := 0; i < ns; i++ {
		dx, dy := rd(1), rd(1)
		var x, y int64
		if i != 0 {
			x = int64(bitio.UnpackSigned(dx)) + lx
			y = int64(bitio.UnpackSigned(dy)) + ly
		} else {
			x, y = int64(dx), int64(dy)
		}
		if x >= 1<<23 || x <= -(1<<23) || y >= 1<<23 || y <= -(1<<23) {
			return invalid("spline position")
		}
		s.start = append(s.start, splinePoint{float32(x), float32(y)})
		lx, ly = x, y
	}
	s.qadj = bitio.UnpackSigned(rd(0))
	totalCP := ns
	for i := 0; i < ns; i++ {
		var q quantSpline
		n := int(rd(3))
		totalCP += n
		if n > maxCP || totalCP > maxCP {
			return invalid("too many control points")
		}
		q.cps = make([][2]int64, n)
		for k := range q.cps {
			a := int64(bitio.UnpackSigned(rd(4)))
			b := int64(bitio.UnpackSigned(rd(4)))
			if a >= 1<<30 || a <= -(1<<30) || b >= 1<<30 || b <= -(1<<30) {
				return invalid("spline delta")
			}
			q.cps[k] = [2]int64{a, b}
		}
		for c := 0; c < 3; c++ {
			for k := 0; k < 32; k++ {
				q.color[c][k] = bitio.UnpackSigned(rd(5))
			}
		}
		for k := 0; k < 32; k++ {
			q.sigma[k] = bitio.UnpackSigned(rd(5))
		}
		s.splines = append(s.splines, q)
	}
	if err := d.Finish(); err != nil {
		return err
	}
	fd.splines = s
	return nil
}

var splineChannelWeight = [4]float32{0.0042, 0.075, 0.07, .3333}

func invAdjustedQuant(a int32) float32 {
	if a >= 0 {
		return 1 / (1 + .125*float32(a))
	}
	return 1 - .125*float32(a)
}

func fastCosf(x float32) float32 {
	pi2 := float32(math.Pi * 2)
	npi2 := float32(math.Floor(float64(x*(0.5/float32(math.Pi))))) * pi2
	xm := x - npi2
	xpi := min(xm, pi2-xm)
	above := xpi >= float32(math.Pi/2)
	xph := xpi
	if above {
		xph = float32(math.Pi) - xpi
	}
	xs := xph * 0.25
	x2 := xs * xs
	x4 := x2 * x2
	c := x4*0.06960438 + (x2*-0.84087373 + 1.68179268)
	c1 := c*c - 1.414213562
	c2 := c1*c1 - 1
	if above {
		return -c2
	}
	return c2
}

func fastErff(x float32) float32 {
	ax := x
	if ax < 0 {
		ax = -ax
	}
	d1 := ax*7.77394369e-02 + 2.05260015e-04
	d2 := d1*ax + 2.32120216e-01
	d3 := d2*ax + 2.77820801e-01
	d4 := d3*ax + 1
	d5 := d4 * d4
	inv := 1 / d5
	r := 1 - inv*inv
	if x <= 0 {
		return -r
	}
	return r
}

func continuousIDCT(dct *[32]float32, t float32) float32 {
	var res float32
	th := t + 0.5
	for i := 0; i < 32; i++ {
		res += sqrt2 * (dct[i] * fastCosf(float32(math.Pi)/32*float32(i)*th))
	}
	return res
}

func catmullRom(points []splinePoint) []splinePoint {
	if len(points) == 0 {
		return nil
	}
	if len(points) == 1 {
		return []splinePoint{points[0]}
	}
	const kNum = 16
	p0 := points[0]
	p1 := points[1]
	ext := make([]splinePoint, 0, len(points)+2)
	ext = append(ext, splinePoint{p0.x + (p0.x - p1.x), p0.y + (p0.y - p1.y)})
	ext = append(ext, points...)
	l := points[len(points)-1]
	l2 := points[len(points)-2]
	ext = append(ext, splinePoint{l.x + (l.x - l2.x), l.y + (l.y - l2.y)})
	res := make([]splinePoint, 0, (len(points)-1)*kNum+1)
	for start := 0; start < len(ext)-3; start++ {
		p := ext[start : start+4]
		res = append(res, p[1])
		var d [3]float32
		var t [4]float32
		for k := 0; k < 3; k++ {
			d[k] = float32(math.Sqrt(math.Hypot(float64(p[k+1].x-p[k].x), float64(p[k+1].y-p[k].y))))
			t[k+1] = t[k] + d[k]
		}
		for i := 1; i < kNum; i++ {
			tt := d[0] + (float32(i)/kNum)*d[1]
			var a [3]splinePoint
			for k := 0; k < 3; k++ {
				f := (tt - t[k]) / d[k]
				a[k] = splinePoint{p[k].x + f*(p[k+1].x-p[k].x), p[k].y + f*(p[k+1].y-p[k].y)}
			}
			var b [2]splinePoint
			for k := 0; k < 2; k++ {
				f := (tt - t[k]) / (d[k] + d[k+1])
				b[k] = splinePoint{a[k].x + f*(a[k+1].x-a[k].x), a[k].y + f*(a[k+1].y-a[k].y)}
			}
			f := (tt - t[1]) / d[1]
			res = append(res, splinePoint{b[0].x + f*(b[1].x-b[0].x), b[0].y + f*(b[1].y-b[0].y)})
		}
	}
	res = append(res, ext[len(ext)-2])
	return res
}

type drawPoint struct {
	p   splinePoint
	mul float32
}

func equallySpaced(points []splinePoint) []drawPoint {
	var out []drawPoint
	cur := points[0]
	out = append(out, drawPoint{cur, 1})
	next := 0
	for next < len(points) {
		prev := cur
		arcPrev := float32(0)
		for {
			if next == len(points) {
				out = append(out, drawPoint{prev, arcPrev})
				return out
			}
			dx, dy := points[next].x-prev.x, points[next].y-prev.y
			arcNext := float32(math.Sqrt(float64(dx*dx + dy*dy)))
			if arcPrev+arcNext >= 1 {
				f := (1 - arcPrev) / arcNext
				cur = splinePoint{prev.x + f*dx, prev.y + f*dy}
				out = append(out, drawPoint{cur, 1})
				break
			}
			arcPrev += arcNext
			prev = points[next]
			next++
		}
	}
	return out
}

func (fd *frameDecoder) prepareSplines() error {
	s := fd.splines
	W, H := fd.color[0].W, fd.color[0].H
	ytox, ytob := float32(0), float32(1)
	if fd.vd != nil {
		ytox, ytob = fd.vd.baseX, fd.vd.baseB
	}
	invQ := invAdjustedQuant(s.qadj)
	imgSize := uint64(W) * uint64(H)
	areaLimit := min(1024*imgSize+(1<<32), 1<<42)
	var totalArea uint64
	for si := range s.splines {
		q := &s.splines[si]
		px := float32(math.Round(float64(s.start[si].x)))
		py := float32(math.Round(float64(s.start[si].y)))
		cx, cy := int64(px), int64(py)
		cps := []splinePoint{{float32(cx), float32(cy)}}
		var ddx, ddy int64
		var manhattan uint64
		for _, d := range q.cps {
			ddx += d[0]
			ddy += d[1]
			manhattan += uint64(abs64(ddx) + abs64(ddy))
			if manhattan > areaLimit {
				return invalid("spline too long")
			}
			cx += ddx
			cy += ddy
			if cx >= 1<<23 || cx <= -(1<<23) || cy >= 1<<23 || cy <= -(1<<23) {
				return invalid("spline position")
			}
			cps = append(cps, splinePoint{float32(cx), float32(cy)})
		}
		var color [3][32]float32
		var sigma [32]float32
		for c := 0; c < 3; c++ {
			for i := 0; i < 32; i++ {
				f := float32(1)
				if i == 0 {
					f = float32(math.Sqrt(0.5))
				}
				color[c][i] = float32(q.color[c][i]) * f * splineChannelWeight[c] * invQ
			}
		}
		for i := 0; i < 32; i++ {
			color[0][i] += ytox * color[1][i]
			color[2][i] += ytob * color[1][i]
		}
		for i := 0; i < 32; i++ {
			f := float32(1)
			if i == 0 {
				f = float32(math.Sqrt(0.5))
			}
			sigma[i] = float32(q.sigma[i]) * f * splineChannelWeight[3] * invQ
		}
		totalArea += manhattan // rough bound
		for k := 1; k < len(cps); k++ {
			if cps[k] == cps[k-1] {
				return invalid("identical spline control points")
			}
		}
		inter := catmullRom(cps)
		pts := equallySpaced(inter)
		arc := float32(len(pts)-2) + pts[len(pts)-1].mul
		if arc <= 0 {
			continue
		}
		invArc := 1 / arc
		for k, dp := range pts {
			prog := min(1, float32(k)*invArc)
			var col [3]float32
			for c := 0; c < 3; c++ {
				col[c] = continuousIDCT(&color[c], 31*prog)
			}
			sg := continuousIDCT(&sigma, 31*prog)
			inten := dp.mul
			if math.IsInf(float64(sg), 0) || math.IsNaN(float64(sg)) || sg == 0 ||
				math.IsInf(float64(1/sg), 0) || math.IsInf(float64(inten), 0) || math.IsNaN(float64(inten)) {
				continue
			}
			maxColor := float32(0.01)
			for c := 0; c < 3; c++ {
				v := col[c] * inten
				if v < 0 {
					v = -v
				}
				maxColor = max(maxColor, v)
			}
			md := float32(math.Sqrt(float64(-2 * sg * sg * (float32(math.Log(0.1))*3 - float32(math.Log(float64(maxColor)))))))
			y0 := int(math.Round(float64(dp.p.y - md)))
			y0 = max(y0, 0)
			y1 := int(math.Round(float64(dp.p.y+md))) + 1
			y1 = min(y1, H)
			if y1 <= y0 {
				continue
			}
			s.segs = append(s.segs, splineSegment{
				cx: dp.p.x, cy: dp.p.y, color: col, invSigma: 1 / sg,
				sigma4Inten: .25 * sg * inten, maxDist: md, y0: y0, y1: y1,
			})
		}
	}
	_ = totalArea
	s.byRow = make([][]int32, H)
	for i, sg := range s.segs {
		for y := sg.y0; y < sg.y1; y++ {
			s.byRow[y] = append(s.byRow[y], int32(i))
		}
	}
	return nil
}

func (fd *frameDecoder) applySplines() error {
	if fd.splines == nil || fd.color[0] == nil {
		return nil
	}
	if err := fd.prepareSplines(); err != nil {
		return err
	}
	s := fd.splines
	W := fd.color[0].W
	X, Y, B := fd.color[0], fd.color[1], fd.color[2]
	_ = parallelFor(len(s.byRow), func(y int) error {
		rows := [3][]float32{X.Row(y), Y.Row(y), B.Row(y)}
		for _, si := range s.byRow[y] {
			sg := &s.segs[si]
			start := int(math.Round(float64(sg.cx - sg.maxDist)))
			end := int(math.Round(float64(sg.cx + sg.maxDist)))
			if end < 0 || start >= W {
				continue
			}
			x0 := max(0, start)
			x1 := min(W, end+1)
			dy := float32(y) - sg.cy
			for x := x0; x < x1; x++ {
				dx := float32(x) - sg.cx
				dist := float32(math.Sqrt(float64(dx*dx + dy*dy)))
				f := fastErff((dist*0.5+0.353553391)*sg.invSigma) - fastErff((dist*0.5-0.353553391)*sg.invSigma)
				li := sg.sigma4Inten * (f * f)
				for c := 0; c < 3; c++ {
					rows[c][x] += sg.color[c] * li
				}
			}
		}
		return nil
	})
	return nil
}

func (fd *frameDecoder) applyPatchesSplines() error {
	if fd.patches != nil {
		fd.applyPatches()
	}
	return fd.applySplines()
}

var _ = sort.Ints
