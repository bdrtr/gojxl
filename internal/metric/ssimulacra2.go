// Package metric implements image quality metrics.
package metric

import (
	"errors"
	"image"
	"math"
	"sync"
)

// SSIMULACRA2 returns the SSIMULACRA 2 score of dist against orig (100 =
// identical, 90 ~ visually lossless, 70 ~ high quality, 50 ~ medium). It
// is a port of libjxl's tools/ssimulacra2.cc; inputs are taken as sRGB.
// As the reference tool does, when orig has transparent pixels both
// images are blended onto dark (0.1) and bright (0.9) backgrounds and the
// worse score is returned.
func SSIMULACRA2(orig, dist image.Image) (float64, error) {
	if orig.Bounds().Size() != dist.Bounds().Size() {
		return 0, errors.New("metric: image sizes differ")
	}
	if !opaque(orig) {
		s0, err := ssimulacra2(orig, dist, 0.1)
		if err != nil {
			return 0, err
		}
		s1, err := ssimulacra2(orig, dist, 0.9)
		return min(s0, s1), err
	}
	return ssimulacra2(orig, dist, 0.5)
}

func opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}

func ssimulacra2(orig, dist image.Image, bg float32) (float64, error) {
	w, h := orig.Bounds().Dx(), orig.Bounds().Dy()
	if w < 8 || h < 8 {
		return 0, errors.New("metric: image smaller than 8x8")
	}
	a, b := toLinear(orig, bg), toLinear(dist, bg)
	var subs [6][18]float64 // per scale: ssim[3*2], edge[3*4]
	nscales := 0
	for scale := 0; scale < 6; scale++ {
		// as the reference: the size test comes before downsampling, so
		// the last scale computed may be smaller than 8
		if a.w < 8 || a.h < 8 {
			break
		}
		if scale > 0 {
			a, b = a.downsample(), b.downsample()
		}
		x1, x2 := a.xyb(), b.xyb()
		var ssim [6]float64
		var edge [12]float64
		var wg sync.WaitGroup
		for c := 0; c < 3; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p1, p2 := x1.p[c], x2.p[c]
				n := len(p1)
				mul := make([]float32, n)
				for i := range mul {
					mul[i] = p1[i] * p1[i]
				}
				s11 := blur(mul, a.w, a.h)
				for i := range mul {
					mul[i] = p2[i] * p2[i]
				}
				s22 := blur(mul, a.w, a.h)
				for i := range mul {
					mul[i] = p1[i] * p2[i]
				}
				s12 := blur(mul, a.w, a.h)
				mu1, mu2 := blur(p1, a.w, a.h), blur(p2, a.w, a.h)
				ssim[2*c], ssim[2*c+1] = ssimMap(mu1, mu2, s11, s22, s12)
				edge[4*c], edge[4*c+1], edge[4*c+2], edge[4*c+3] = edgeDiff(p1, mu1, p2, mu2)
			}()
		}
		wg.Wait()
		copy(subs[scale][:6], ssim[:])
		copy(subs[scale][6:], edge[:])
		nscales++
	}
	// weighted sum in the reference's order
	s, i := 0.0, 0
	for c := 0; c < 3; c++ {
		for scale := 0; scale < nscales; scale++ {
			for n := 0; n < 2; n++ {
				s += ssimulacra2Weights[i] * math.Abs(subs[scale][2*c+n])
				s += ssimulacra2Weights[i+1] * math.Abs(subs[scale][6+4*c+n])
				s += ssimulacra2Weights[i+2] * math.Abs(subs[scale][6+4*c+n+2])
				i += 3
			}
		}
	}
	s *= 0.9562382616834844
	s = 2.326765642916932*s - 0.020884521182843837*s*s + 6.248496625763138e-05*s*s*s
	if s > 0 {
		return 100 - 10*math.Pow(s, 0.6276336467831387), nil
	}
	return 100, nil
}

// planes holds three float planes.
type planes struct {
	w, h int
	p    [3][]float32
}

func srgbToLinear(v float32) float32 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return float32(math.Pow((float64(v)+0.055)/1.055, 2.4))
}

// toLinear converts an image to linear sRGB planes (alpha blended onto a
// background of value bg in encoded sRGB).
func toLinear(img image.Image, bg float32) planes {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	p := planes{w: w, h: h}
	for c := range p.p {
		p.p[c] = make([]float32, w*h)
	}
	var lut8 [256]float32
	for i := range lut8 {
		lut8[i] = srgbToLinear(float32(i) / 255)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA() // premultiplied
			i := y*w + x
			if a == 0xffff && r%257 == 0 && g%257 == 0 && bl%257 == 0 {
				p.p[0][i], p.p[1][i], p.p[2][i] = lut8[r>>8], lut8[g>>8], lut8[bl>>8]
				continue
			}
			// premultiplied colour plus (1 - alpha) * background
			af := float32(a) / 0xffff
			for c, v := range [3]uint32{r, g, bl} {
				e := float32(v)/0xffff + (1-af)*bg
				p.p[c][i] = srgbToLinear(e)
			}
		}
	}
	return p
}

// downsample halves the planes by box averaging (edge pixels repeated).
func (p planes) downsample() planes {
	ow, oh := (p.w+1)/2, (p.h+1)/2
	o := planes{w: ow, h: oh}
	for c := range o.p {
		src := p.p[c]
		dst := make([]float32, ow*oh)
		for oy := 0; oy < oh; oy++ {
			for ox := 0; ox < ow; ox++ {
				var s float32
				for iy := 0; iy < 2; iy++ {
					for ix := 0; ix < 2; ix++ {
						x, y := min(2*ox+ix, p.w-1), min(2*oy+iy, p.h-1)
						s += src[y*p.w+x]
					}
				}
				dst[oy*ow+ox] = s * 0.25
			}
		}
		o.p[c] = dst
	}
	return o
}

// xyb converts linear sRGB planes to the rescaled, positive XYB used by
// SSIMULACRA 2.
func (p planes) xyb() planes {
	const bias = 0.0037930732552754493
	cb := float32(math.Cbrt(bias))
	o := planes{w: p.w, h: p.h}
	for c := range o.p {
		o.p[c] = make([]float32, p.w*p.h)
	}
	for i := range p.p[0] {
		r, g, b := p.p[0][i], p.p[1][i], p.p[2][i]
		m0 := 0.30*r + 0.622*g + 0.078*b + bias
		m1 := 0.23*r + 0.692*g + 0.078*b + bias
		m2 := 0.24342268924547819*r + 0.20476744424496821*g + 0.55180986650955360*b + bias
		m0 = float32(math.Cbrt(float64(max(m0, 0)))) - cb
		m1 = float32(math.Cbrt(float64(max(m1, 0)))) - cb
		m2 = float32(math.Cbrt(float64(max(m2, 0)))) - cb
		x, y, bb := 0.5*(m0-m1), 0.5*(m0+m1), m2
		o.p[0][i] = x*14 + 0.42
		o.p[1][i] = y + 0.01
		o.p[2][i] = (bb - y) + 0.55
	}
	return o
}

// recursive Gaussian (sigma 1.5) as in libjxl tools/gauss_blur.cc
var gauss = newRecursiveGaussian(1.5)

type recGauss struct {
	radius int
	n2, d1 [3]float32
}

func newRecursiveGaussian(sigma float64) recGauss {
	radius := math.Round(3.2795*sigma + 0.2546)
	piDiv2r := math.Pi / (2 * radius)
	omega := [3]float64{piDiv2r, 3 * piDiv2r, 5 * piDiv2r}
	p1 := 1 / math.Tan(0.5*omega[0])
	p3 := -1 / math.Tan(0.5*omega[1])
	p5 := 1 / math.Tan(0.5*omega[2])
	r1 := p1 * p1 / math.Sin(omega[0])
	r3 := -p3 * p3 / math.Sin(omega[1])
	r5 := p5 * p5 / math.Sin(omega[2])
	nhs2 := -0.5 * sigma * sigma
	var rho [3]float64
	for i := range rho {
		rho[i] = math.Exp(nhs2*omega[i]*omega[i]) / radius
	}
	d13 := p1*r3 - r1*p3
	d35 := p3*r5 - r3*p5
	d51 := p5*r1 - r5*p1
	z15, z35 := d35/d13, d51/d13
	A := [3][3]float64{{p1, p3, p5}, {r1, r3, r5}, {z15, z35, 1}}
	inv := inv3(A)
	gamma := [3]float64{1, radius*radius - sigma*sigma, z15*rho[0] + z35*rho[1] + rho[2]}
	var beta [3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			beta[i] += inv[i][j] * gamma[j]
		}
	}
	g := recGauss{radius: int(radius)}
	for i := 0; i < 3; i++ {
		g.n2[i] = float32(-beta[i] * math.Cos(omega[i]*(radius+1)))
		g.d1[i] = float32(-2 * math.Cos(omega[i]))
	}
	return g
}

func inv3(m [3][3]float64) [3][3]float64 {
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	var r [3][3]float64
	r[0][0] = (m[1][1]*m[2][2] - m[1][2]*m[2][1]) / det
	r[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) / det
	r[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det
	r[1][0] = (m[1][2]*m[2][0] - m[1][0]*m[2][2]) / det
	r[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) / det
	r[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) / det
	r[2][0] = (m[1][0]*m[2][1] - m[1][1]*m[2][0]) / det
	r[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) / det
	r[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) / det
	return r
}

// blur1D filters n samples read via at(i) (zero outside [0, n)) and
// stores them via set.
func (g *recGauss) blur1D(n int, at func(int) float32, set func(int, float32)) {
	N := g.radius
	var prev, prev2 [3]float32
	get := func(i int) float32 {
		if i < 0 || i >= n {
			return 0
		}
		return at(i)
	}
	for k := -N + 1; k < n; k++ {
		sum := get(k-N-1) + get(k+N-1)
		var out float32
		for i := 0; i < 3; i++ {
			o := g.n2[i]*sum - g.d1[i]*prev[i] - prev2[i]
			prev2[i], prev[i] = prev[i], o
			out += o
		}
		if k >= 0 {
			set(k, out)
		}
	}
}

// blur applies the recursive Gaussian horizontally, then vertically.
func blur(in []float32, w, h int) []float32 {
	tmp := make([]float32, w*h)
	out := make([]float32, w*h)
	for y := 0; y < h; y++ {
		row, trow := in[y*w:(y+1)*w], tmp[y*w:(y+1)*w]
		gauss.blur1D(w, func(i int) float32 { return row[i] }, func(i int, v float32) { trow[i] = v })
	}
	for x := 0; x < w; x++ {
		gauss.blur1D(h, func(i int) float32 { return tmp[i*w+x] }, func(i int, v float32) { out[i*w+x] = v })
	}
	return out
}

func quartic(x float64) float64 {
	x *= x
	return x * x
}

func ssimMap(mu1, mu2, s11, s22, s12 []float32) (float64, float64) {
	const c2 = 0.0009
	var sum1, sum4 float64
	for i := range mu1 {
		m1, m2 := mu1[i], mu2[i]
		numM := 1 - (m1-m2)*(m1-m2)
		numS := 2*(s12[i]-m1*m2) + c2
		denS := (s11[i] - m1*m1) + (s22[i] - m2*m2) + c2
		d := max(1-float64(numM*numS/denS), 0)
		sum1 += d
		sum4 += quartic(d)
	}
	n := float64(len(mu1))
	return sum1 / n, math.Sqrt(math.Sqrt(sum4 / n))
}

func edgeDiff(img1, mu1, img2, mu2 []float32) (float64, float64, float64, float64) {
	var s [4]float64
	for i := range img1 {
		d1 := (1+math.Abs(float64(img2[i]-mu2[i])))/(1+math.Abs(float64(img1[i]-mu1[i]))) - 1
		art := max(d1, 0)
		s[0] += art
		s[1] += quartic(art)
		lost := max(-d1, 0)
		s[2] += lost
		s[3] += quartic(lost)
	}
	n := float64(len(img1))
	return s[0] / n, math.Sqrt(math.Sqrt(s[1] / n)), s[2] / n, math.Sqrt(math.Sqrt(s[3] / n))
}
