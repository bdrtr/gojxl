package jxl

import (
	"encoding/binary"
	"math"
)

// LUT based ICC transforms (lut8/lut16/lutAtoB), used to render CMYK images
// (colour channels C, M, Y plus a Black extra channel) to sRGB.

type lutPipe struct {
	inCh, outCh int
	aCurves     []func(float64) float64 // per input channel (may be nil = identity)
	grid        []int                   // CLUT grid points per input dimension
	clut        []float64               // CLUT values normalised to [0,1]
	mCurves     []func(float64) float64
	matrix      *[12]float64
	bCurves     []func(float64) float64
	pcsLab      bool
	labScaleL   float64 // L = v*labScaleL
	labScaleAB  float64 // a = v*labScaleAB - 128
	xyzScale    float64
}

func tableCurve(t []float64) func(float64) float64 {
	n := len(t)
	if n < 2 {
		return nil
	}
	return func(v float64) float64 {
		x := math.Min(math.Max(v, 0), 1) * float64(n-1)
		i := int(x)
		if i >= n-1 {
			return t[n-1]
		}
		f := x - float64(i)
		return t[i]*(1-f) + t[i+1]*f
	}
}

func readU16Table(b []byte, n int) []float64 {
	t := make([]float64, n)
	for i := range t {
		t[i] = float64(binary.BigEndian.Uint16(b[2*i:])) / 65535
	}
	return t
}

// parseLUT parses an mft1, mft2 or mAB tag. pcsLab selects the PCS
// encoding of the output.
func parseLUT(b []byte, pcsLab bool) *lutPipe {
	if len(b) < 48 {
		return nil
	}
	p := &lutPipe{pcsLab: pcsLab}
	typ := string(b[:4])
	switch typ {
	case "mft1", "mft2":
		inCh, outCh, g := int(b[8]), int(b[9]), int(b[10])
		if inCh < 1 || inCh > 8 || outCh != 3 || g < 2 {
			return nil
		}
		p.inCh, p.outCh = inCh, outCh
		nodes := 1
		for i := 0; i < inCh; i++ {
			p.grid = append(p.grid, g)
			nodes *= g
		}
		var inN, outN, es int
		off := 48
		if typ == "mft2" {
			if len(b) < 52 {
				return nil
			}
			inN, outN, es = int(binary.BigEndian.Uint16(b[48:])), int(binary.BigEndian.Uint16(b[50:])), 2
			off = 52
		} else {
			inN, outN, es = 256, 256, 1
		}
		need := off + es*(inCh*inN+nodes*outCh+outCh*outN)
		if inN < 2 || outN < 2 || len(b) < need {
			return nil
		}
		rd := func(n int) []float64 {
			t := make([]float64, n)
			for i := range t {
				if es == 2 {
					t[i] = float64(binary.BigEndian.Uint16(b[off+2*i:])) / 65535
				} else {
					t[i] = float64(b[off+i]) / 255
				}
			}
			off += es * n
			return t
		}
		for i := 0; i < inCh; i++ {
			p.aCurves = append(p.aCurves, tableCurve(rd(inN)))
		}
		p.clut = rd(nodes * outCh)
		for i := 0; i < outCh; i++ {
			p.bCurves = append(p.bCurves, tableCurve(rd(outN)))
		}
		if typ == "mft2" {
			p.labScaleL, p.labScaleAB, p.xyzScale = 65535.0/652.80, 65535.0/256, 65535.0/32768
		} else {
			p.labScaleL, p.labScaleAB, p.xyzScale = 100, 255, 1
		}
	case "mAB ":
		inCh, outCh := int(b[8]), int(b[9])
		if inCh < 1 || inCh > 8 || outCh != 3 {
			return nil
		}
		p.inCh, p.outCh = inCh, outCh
		p.labScaleL, p.labScaleAB, p.xyzScale = 100, 255, 65535.0/32768
		offB := int(binary.BigEndian.Uint32(b[12:]))
		offMat := int(binary.BigEndian.Uint32(b[16:]))
		offM := int(binary.BigEndian.Uint32(b[20:]))
		offC := int(binary.BigEndian.Uint32(b[24:]))
		offA := int(binary.BigEndian.Uint32(b[28:]))
		curves := func(off, n int) []func(float64) float64 {
			if off == 0 {
				return nil
			}
			var cs []func(float64) float64
			for i := 0; i < n; i++ {
				if off+12 > len(b) {
					return nil
				}
				c := b[off:]
				cs = append(cs, parseCurve(c))
				off += curveSize(c)
				off = (off + 3) &^ 3
			}
			return cs
		}
		p.bCurves = curves(offB, outCh)
		if offB != 0 && p.bCurves == nil {
			return nil
		}
		if offC != 0 {
			if offC+20 > len(b) {
				return nil
			}
			nodes := 1
			for i := 0; i < inCh; i++ {
				g := int(b[offC+i])
				if g < 2 {
					return nil
				}
				p.grid = append(p.grid, g)
				nodes *= g
			}
			prec := int(b[offC+16])
			d := b[offC+20:]
			if (prec != 1 && prec != 2) || len(d) < prec*nodes*outCh {
				return nil
			}
			p.clut = make([]float64, nodes*outCh)
			for i := range p.clut {
				if prec == 2 {
					p.clut[i] = float64(binary.BigEndian.Uint16(d[2*i:])) / 65535
				} else {
					p.clut[i] = float64(d[i]) / 255
				}
			}
			p.aCurves = curves(offA, inCh)
			p.mCurves = curves(offM, outCh)
			if offMat != 0 && offMat+48 <= len(b) {
				var m [12]float64
				for i := range m {
					m[i] = rdS15(b[offMat+4*i:])
				}
				p.matrix = &m
			}
		} else {
			return nil // matrix-only AtoB with 4 inputs is not meaningful
		}
	default:
		return nil
	}
	return p
}

// curveSize returns the byte size of a curv/para element.
func curveSize(c []byte) int {
	switch string(c[:4]) {
	case "curv":
		return 12 + 2*int(binary.BigEndian.Uint32(c[8:]))
	case "para":
		return 12 + 4*map[uint16]int{0: 1, 1: 3, 2: 4, 3: 5, 4: 7}[binary.BigEndian.Uint16(c[8:])]
	}
	return len(c)
}

func applyCurve(f func(float64) float64, v float64) float64 {
	if f == nil {
		return v
	}
	return f(v)
}

// eval maps device values in [0,1] to PCS XYZ (D50, relative).
func (p *lutPipe) eval(in []float64, scratch []float64) [3]float64 {
	var x [8]float64
	for i := 0; i < p.inCh; i++ {
		v := math.Min(math.Max(in[i], 0), 1)
		if i < len(p.aCurves) {
			v = applyCurve(p.aCurves[i], v)
		}
		x[i] = v
	}
	// CLUT interpolation as in lcms: tetrahedral over the last three
	// inputs, linear over any leading ones.
	var o [3]float64
	n := p.inCh
	var base [8]int
	var frac [8]float64
	var stride [8]int
	s := p.outCh
	for i := n - 1; i >= 0; i-- {
		stride[i] = s
		s *= p.grid[i]
		f := x[i] * float64(p.grid[i]-1)
		k := int(f)
		if k >= p.grid[i]-1 {
			k = p.grid[i] - 2
		}
		base[i] = k
		frac[i] = f - float64(k)
	}
	if n < 3 {
		// multilinear for 1-2 inputs
		for corner := 0; corner < 1<<n; corner++ {
			w, idx := 1.0, 0
			for i := 0; i < n; i++ {
				if corner>>i&1 != 0 {
					w *= frac[i]
					idx += (base[i] + 1) * stride[i]
				} else {
					w *= 1 - frac[i]
					idx += base[i] * stride[i]
				}
			}
			for c := 0; c < 3; c++ {
				o[c] += w * p.clut[idx+c]
			}
		}
	} else {
		lead := n - 3
		for corner := 0; corner < 1<<lead; corner++ {
			w, off := 1.0, 0
			for i := 0; i < lead; i++ {
				if corner>>i&1 != 0 {
					w *= frac[i]
					off += (base[i] + 1) * stride[i]
				} else {
					w *= 1 - frac[i]
					off += base[i] * stride[i]
				}
			}
			if w == 0 {
				continue
			}
			t := p.tetra(off, base[lead:n], frac[lead:n], stride[lead:n])
			for c := 0; c < 3; c++ {
				o[c] += w * t[c]
			}
		}
	}
	for c := 0; c < 3; c++ {
		if c < len(p.mCurves) {
			o[c] = applyCurve(p.mCurves[c], o[c])
		}
	}
	if p.matrix != nil {
		m := p.matrix
		o = [3]float64{
			m[0]*o[0] + m[1]*o[1] + m[2]*o[2] + m[9],
			m[3]*o[0] + m[4]*o[1] + m[5]*o[2] + m[10],
			m[6]*o[0] + m[7]*o[1] + m[8]*o[2] + m[11],
		}
	}
	for c := 0; c < 3; c++ {
		if c < len(p.bCurves) {
			o[c] = applyCurve(p.bCurves[c], math.Min(math.Max(o[c], 0), 1))
		}
	}
	if !p.pcsLab {
		return [3]float64{o[0] * p.xyzScale, o[1] * p.xyzScale, o[2] * p.xyzScale}
	}
	L := o[0] * p.labScaleL
	a := o[1]*p.labScaleAB - 128
	bb := o[2]*p.labScaleAB - 128
	return labToXYZ(L, a, bb)
}

func labToXYZ(L, a, b float64) [3]float64 {
	fy := (L + 16) / 116
	fx := fy + a/500
	fz := fy - b/200
	inv := func(t float64) float64 {
		if t > 6.0/29 {
			return t * t * t
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (t - 4.0/29)
	}
	return [3]float64{0.9642 * inv(fx), inv(fy), 0.8249 * inv(fz)}
}

// cmykTransform renders CMYK (JPEG XL convention: 0 = full ink) to sRGB.
type cmykTransform struct {
	pipe  *lutPipe
	toRGB mat3 // XYZ D50 -> linear sRGB
	black int  // index of the Black extra channel
}

func newCMYKTransform(ih *ImageHeader, icc []byte) *cmykTransform {
	if len(icc) < 132 || string(icc[16:20]) != "CMYK" || ih.XYBEncoded {
		return nil
	}
	black := -1
	for i, ec := range ih.ExtraChannels {
		if ec.Type == ECBlack {
			black = i
			break
		}
	}
	if black < 0 {
		return nil
	}
	// The perceptual table is used regardless of the header's intent, as
	// libjxl's CMS does (it also matches relative colorimetric with black
	// point compensation closely).
	pipe := parseLUT(iccTags(icc)["A2B0"], string(icc[20:24]) == "Lab ")
	if pipe == nil || pipe.inCh != 4 {
		return nil
	}
	toXYZ := adapt(xyToXYZ(xyD65), xyToXYZ(xyD50)).mul(rgbToXYZ(srgbP[0], srgbP[1], srgbP[2], xyD65))
	return &cmykTransform{pipe: pipe, toRGB: toXYZ.inv(), black: black}
}

// apply converts planes in place: c, m, y become sRGB R, G, B.
func (t *cmykTransform) apply(c, m, y, k *Plane) {
	w, h := c.W, c.H
	ParallelRows(h, func(y0, y1 int) {
		var in [4]float64
		for yy := y0; yy < y1; yy++ {
			rc, rm, ry, rk := c.Row(yy)[:w], m.Row(yy)[:w], y.Row(yy)[:w], k.Row(yy)[:w]
			for x := 0; x < w; x++ {
				in[0], in[1], in[2], in[3] = 1-float64(rc[x]), 1-float64(rm[x]), 1-float64(ry[x]), 1-float64(rk[x])
				xyz := t.pipe.eval(in[:], nil)
				lin := t.toRGB.apply(xyz)
				rc[x] = srgbFromLinear(float32(math.Min(math.Max(lin[0], 0), 1)))
				rm[x] = srgbFromLinear(float32(math.Min(math.Max(lin[1], 0), 1)))
				ry[x] = srgbFromLinear(float32(math.Min(math.Max(lin[2], 0), 1)))
			}
		}
	})
}

// tetra performs tetrahedral interpolation in a 3-D sub-cube of the CLUT.
func (p *lutPipe) tetra(off int, base []int, frac []float64, stride []int) [3]float64 {
	rx, ry, rz := frac[0], frac[1], frac[2]
	x0 := off + base[0]*stride[0] + base[1]*stride[1] + base[2]*stride[2]
	X, Y, Z := stride[0], stride[1], stride[2]
	t := p.clut
	var o [3]float64
	for c := 0; c < 3; c++ {
		P := func(dx, dy, dz int) float64 { return t[x0+dx*X+dy*Y+dz*Z+c] }
		c0 := P(0, 0, 0)
		var c1, c2, c3 float64
		switch {
		case rx >= ry && ry >= rz:
			c1, c2, c3 = P(1, 0, 0)-c0, P(1, 1, 0)-P(1, 0, 0), P(1, 1, 1)-P(1, 1, 0)
		case rx >= rz && rz >= ry:
			c1, c2, c3 = P(1, 0, 0)-c0, P(1, 1, 1)-P(1, 0, 1), P(1, 0, 1)-P(1, 0, 0)
		case rz >= rx && rx >= ry:
			c1, c2, c3 = P(1, 0, 1)-P(0, 0, 1), P(1, 1, 1)-P(1, 0, 1), P(0, 0, 1)-c0
		case ry >= rx && rx >= rz:
			c1, c2, c3 = P(1, 1, 0)-P(0, 1, 0), P(0, 1, 0)-c0, P(1, 1, 1)-P(1, 1, 0)
		case ry >= rz && rz >= rx:
			c1, c2, c3 = P(1, 1, 1)-P(0, 1, 1), P(0, 1, 0)-c0, P(0, 1, 1)-P(0, 1, 0)
		default:
			c1, c2, c3 = P(1, 1, 1)-P(0, 1, 1), P(0, 1, 1)-P(0, 0, 1), P(0, 0, 1)-c0
		}
		o[c] = c0 + c1*rx + c2*ry + c3*rz
	}
	return o
}
