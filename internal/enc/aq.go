package enc

import "math"

// Adaptive quantisation map: a port of libjxl's InitialQuantField
// (enc_adaptive_quantization.cc, as used at efforts 5-7). From the XYB
// image before inverse Gaborish it computes, per 8x8 block, a quantisation
// multiplier q (larger = finer): visual masking from the local Laplacian of
// Y, modulated by gamma, high-frequency content and blue content.

const (
	aqSGmul     = 226.77216153508914
	aqSGmul2    = 1.0 / 73.377132366608819
	aqInvLog2e  = 0.6931471805599453
	aqSGRetMul  = aqSGmul2 * 18.6580932135 * aqInvLog2e
	aqSGVOffset = 7.7825991679894591
	aqAcQuant   = 0.765
)

// ratioOfDerivatives is RatioOfDerivativesOfCubicRootToSimpleGamma.
func ratioOfDerivatives(v float32, invert bool) float32 {
	const eps = 1e-2
	v = max(v, 0)
	num := float32(aqSGRetMul*3*aqSGmul)*v*v + eps
	den := float32(aqInvLog2e*aqSGmul)*v*v*v + float32(aqSGVOffset*aqInvLog2e+eps)
	if invert {
		return num / den
	}
	return den / num
}

func maskingSqrt(v float32) float32 {
	const logOffset, mul = 27.505837037000106, 211.66567973503678
	return 0.25 * float32(math.Sqrt(float64(v)*math.Sqrt(mul*1e8)+logOffset))
}

func computeMask(v float32) float32 {
	const (
		base    = -0.7647
		mul4    = 9.4708735624378946
		mul2    = 17.35036561631863
		offset2 = 302.59587815579727
		mul3    = 6.7943250517376494
		offset3 = 3.7179635626140772
		offset4 = 0.25 * offset3
		mul0    = 0.80061762862741759
	)
	v1 := max(v*mul0, 1e-3)
	v2 := 1 / (v1 + offset2)
	v3 := 1 / (v1*v1 + offset3)
	v4 := 1 / (v1*v1 + offset4)
	return base + mul4*v4 + mul2*v2 + mul3*v3
}

// adaptiveQuantMap returns the per-block quantisation multipliers for a
// padded XYB image (pw x ph, multiples of 8) at distance d.
func adaptiveQuantMap(xyb [3][]float32, pw, ph int, d float32) []float32 {
	bw, bh := pw/8, ph/8
	Y := xyb[1]
	const matchGammaOffset = 0.019
	const limit = 0.2
	// local differences, summed over 4x4 cells
	qw, qh := pw/4, ph/4
	pre := make([]float32, qw*qh)
	parallel(qh, func(qy int) {
		acc := make([]float32, pw)
		for y := qy * 4; y < qy*4+4; y++ {
			y1, y2 := max(y-1, 0), min(y+1, ph-1)
			row, r1, r2 := Y[y*pw:], Y[y1*pw:], Y[y2*pw:]
			for x := 0; x < pw; x++ {
				x1, x2 := max(x-1, 0), min(x+1, pw-1)
				base := 0.25 * (r2[x] + r1[x] + row[x1] + row[x2])
				g := ratioOfDerivatives(row[x]+matchGammaOffset, false)
				diff := g * (row[x] - base)
				diff = min(diff*diff, limit)
				diff = maskingSqrt(diff)
				if y%4 != 0 {
					acc[x] += diff
				} else {
					acc[x] = diff
				}
			}
		}
		for qx := 0; qx < qw; qx++ {
			pre[qy*qw+qx] = (acc[4*qx] + acc[4*qx+1] + acc[4*qx+2] + acc[4*qx+3]) * 0.25
		}
	})
	// fuzzy erosion: weighted four smallest of each 3x3 neighbourhood,
	// summed per block (2x2 cells)
	kMulBase := [4]float32{0.125, 0.1, 0.09, 0.06}
	kMulAdd := [4]float32{0, -0.1, -0.09, -0.06}
	var em float32
	if d < 2 {
		em = (2 - d) / 2
	}
	var kMul [4]float32
	var norm float32
	for i := range kMul {
		kMul[i] = kMulBase[i] + em*kMulAdd[i]
		norm += kMul[i]
	}
	for i := range kMul {
		kMul[i] *= 0.29959705784054957 / norm
	}
	aq := make([]float32, bw*bh)
	for fy := 0; fy < qh; fy++ {
		ym1, yp1 := max(fy-1, 0), min(fy+1, qh-1)
		rowt, row, rowb := pre[ym1*qw:], pre[fy*qw:], pre[yp1*qw:]
		for fx := 0; fx < qw; fx++ {
			xm1, xp1 := max(fx-1, 0), min(fx+1, qw-1)
			m := [4]float32{row[fx], row[xm1], row[xp1], rowt[xm1]}
			if m[0] > m[1] {
				m[0], m[1] = m[1], m[0]
			}
			if m[0] > m[2] {
				m[0], m[2] = m[2], m[0]
			}
			if m[0] > m[3] {
				m[0], m[3] = m[3], m[0]
			}
			if m[1] > m[2] {
				m[1], m[2] = m[2], m[1]
			}
			if m[1] > m[3] {
				m[1], m[3] = m[3], m[1]
			}
			if m[2] > m[3] {
				m[2], m[3] = m[3], m[2]
			}
			for _, v := range [5]float32{rowt[fx], rowt[xp1], rowb[xm1], rowb[fx], rowb[xp1]} {
				storeMin4(v, &m)
			}
			v := kMul[0]*m[0] + kMul[1]*m[1] + kMul[2]*m[2] + kMul[3]*m[3]
			b := (fy/2)*bw + fx/2
			if fx%2 == 0 && fy%2 == 0 {
				aq[b] = v
			} else {
				aq[b] += v
			}
		}
	}
	// per-block modulations
	scale := float32(aqAcQuant) / d
	dampen := float32(1)
	if d >= 2 {
		dampen = max(1-(d-2)/(14-2), 0)
	}
	mul, add := scale*dampen, (1-dampen)*0.48*scale
	X, B := xyb[0], xyb[2]
	parallel(bh, func(by int) {
		for bx := 0; bx < bw; bx++ {
			x0, y0 := bx*8, by*8
			out := computeMask(aq[by*bw+bx])
			// gamma
			var ratio float32
			for dy := 0; dy < 8; dy++ {
				o := (y0+dy)*pw + x0
				for dx := 0; dx < 8; dx++ {
					iny := Y[o+dx] + 0.16
					inx := X[o+dx]
					ratio += ratioOfDerivatives(iny-inx, true) + ratioOfDerivatives(iny+inx, true)
				}
			}
			ratio *= 0.5 / 64
			out += 0.1005613337192697 * float32(math.Log2(float64(ratio)))
			maskVal := out
			// high frequencies of Y
			var sumY float32
			const valmin = 0.0206
			for dy := 0; dy < 8; dy++ {
				o := (y0+dy)*pw + x0
				on := o
				if dy < 7 {
					on += pw
				}
				for dx := 0; dx < 8; dx++ {
					p := Y[o+dx]
					if dx < 7 {
						sumY += min(valmin, abs32(p-Y[o+dx+1]))
					}
					sumY += min(valmin, abs32(p-Y[on+dx]))
				}
			}
			hf := out + sumY*-0.38 + 0.42
			// blue
			const limitB, offsetB = 0.010474084867598155, 0.0031994768654636393
			var sumB float32
			for dy := 0; dy < 8; dy++ {
				o := (y0+dy)*pw + x0
				for dx := 0; dx < 8; dx++ {
					yEff := Y[o+dx] + offsetB + abs32(X[o+dx])
					if b := B[o+dx]; b > yEff {
						sumB += min(b-yEff, limitB)
					}
				}
			}
			if sumB >= 32*limitB {
				sumB = 64*limitB - sumB
			}
			sumB = min(sumB, 15.463398341612438*limitB)
			blue := maskVal + sumB*0.90590804735610064
			out = min(hf, blue)
			aq[by*bw+bx] = float32(math.Exp(float64(out)))*mul + add
		}
	})
	return aq
}

func storeMin4(v float32, m *[4]float32) {
	if v >= m[3] {
		return
	}
	switch {
	case v < m[0]:
		m[3], m[2], m[1], m[0] = m[2], m[1], m[0], v
	case v < m[1]:
		m[3], m[2], m[1] = m[2], m[1], v
	case v < m[2]:
		m[3], m[2] = m[2], v
	default:
		m[3] = v
	}
}
