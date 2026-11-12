package jxl

// Scalar reference kernels. SIMD versions (kernels_simd.go) are used when
// built with GOEXPERIMENT=simd on amd64 and the CPU supports AVX2+FMA.

// dequantBias holds a channel's quantisation bias parameters and the
// adjusted values of small quantised coefficients.
type dequantBias struct {
	bias, numer float32
	adj         [2*dqSmall + 1]float32 // value of q at adj[q+dqSmall]
}

const dqSmall = 16

func newDequantBias(bias, numer float32) *dequantBias {
	d := &dequantBias{bias: bias, numer: numer}
	for i := range d.adj {
		switch q := i - dqSmall; q {
		case 0:
		case 1:
			d.adj[i] = bias
		case -1:
			d.adj[i] = -bias
		default:
			f := float32(q)
			d.adj[i] = f - numer/f
		}
	}
	return d
}

// dequantRowScalar dequantises q with the bias adjustment (0 stays 0, +-1
// become +-bias, others q - numer/q); small values come from a table, so
// the loop has one well-predicted branch.
func dequantRowScalar(q []int32, m, o []float32, mul float32, d *dequantBias) {
	m, o = m[:len(q)], o[:len(q)]
	adj := &d.adj
	for k, qv := range q {
		if u := uint32(qv + dqSmall); u <= 2*dqSmall {
			o[k] = adj[u] * m[k] * mul
		} else {
			f := float32(qv)
			o[k] = (f - d.numer/f) * m[k] * mul
		}
	}
}

func epfDiffRowScalar(d, a0, b0, a1, b1, a2, b2 []float32, s0, s1, s2 float32) {
	n := len(d)
	a0, b0, a1, b1, a2, b2 = a0[:n], b0[:n], a1[:n], b1[:n], a2[:n], b2[:n]
	for x := range d {
		d[x] = abs32(a0[x]-b0[x])*s0 + abs32(a1[x]-b1[x])*s1 + abs32(a2[x]-b2[x])*s2
	}
}

// plusRowScalar computes out[x] = mid[x-1]+mid[x]+mid[x+1]+up[x]+dn[x] for
// x in [1, len-1).
func plusRowScalar(out, up, mid, dn []float32) {
	n := len(out)
	up, mid, dn = up[:n], mid[:n], dn[:n]
	for x := 1; x < n-1; x++ {
		out[x] = mid[x] + mid[x-1] + mid[x+1] + up[x] + dn[x]
	}
}

// gabRowScalar applies the 3x3 Gaborish kernel for x in [1, len-1).
func gabRowScalar(o, t, m, b []float32, w0, w1, w2 float32) {
	n := len(o)
	t, m, b = t[:n], m[:n], b[:n]
	for x := 1; x < n-1; x++ {
		sum1 := (m[x-1] + m[x+1]) + (t[x] + b[x])
		sum2 := (t[x-1] + t[x+1]) + (b[x-1] + b[x+1])
		o[x] = sum2*w2 + (sum1*w1 + m[x]*w0)
	}
}

type epf4Args struct {
	o, c, l, r, u, d [3][]float32
	sv, svu, sh, shl []float32
	sig              []float32 // inverse sigma per 8-pixel block, starting at the row start
	rowMul           *[8]float32
	n                int
}

// epfRow4Scalar is the 4-neighbour EPF step (rows start at a multiple of 8).
func epfRow4Scalar(a *epf4Args, from int) {
	for lx := from; lx < a.n; lx++ {
		is := a.sig[lx>>3]
		if is < minSigma {
			for c := 0; c < 3; c++ {
				a.o[c][lx] = a.c[c][lx]
			}
			continue
		}
		inv := is * a.rowMul[lx&7]
		wd := relu32(a.sv[lx]*inv + 1)
		wu := relu32(a.svu[lx]*inv + 1)
		wr := relu32(a.sh[lx]*inv + 1)
		wl := relu32(a.shl[lx]*inv + 1)
		iw := 1 / (1 + wd + wu + wr + wl)
		for c := 0; c < 3; c++ {
			a.o[c][lx] = (a.c[c][lx] + wd*a.d[c][lx] + wu*a.u[c][lx] + wr*a.r[c][lx] + wl*a.l[c][lx]) * iw
		}
	}
}

func xybRowScalar(c *xybConv, x, y, b, r, g, bo []float32) {
	m := &c.m
	for i := range r {
		X, Y, B := x[i], y[i], b[i]
		gr := Y + X - c.cbrtBias[0]
		gg := Y - X - c.cbrtBias[1]
		gb := B - c.cbrtBias[2]
		mr := gr*gr*gr + c.bias[0]
		mg := gg*gg*gg + c.bias[1]
		mb := gb*gb*gb + c.bias[2]
		r[i] = m[0]*mr + m[1]*mg + m[2]*mb
		g[i] = m[3]*mr + m[4]*mg + m[5]*mb
		bo[i] = m[6]*mr + m[7]*mg + m[8]*mb
	}
}
