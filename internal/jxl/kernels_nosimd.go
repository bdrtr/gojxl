//go:build !(goexperiment.simd && amd64)

package jxl

// SIMDActive reports whether vectorised kernels are in use.
func SIMDActive() bool { return false }

func idct8x8(c, out []float32, stride int) { idct8x8Scalar(c, out, stride) }
func dequantRow(q []int32, m, o []float32, mul, bias, numer float32) {
	dequantRowScalar(q, m, o, mul, bias, numer)
}
func epfDiffRow(d, a0, b0, a1, b1, a2, b2 []float32, s0, s1, s2 float32) {
	epfDiffRowScalar(d, a0, b0, a1, b1, a2, b2, s0, s1, s2)
}
func plusRow(out, up, mid, dn []float32)              { plusRowScalar(out, up, mid, dn) }
func gabRow(o, t, m, b []float32, w0, w1, w2 float32) { gabRowScalar(o, t, m, b, w0, w1, w2) }
func epfRow4(a *epf4Args)                             { epfRow4Scalar(a, 0) }
func xybRow(c *xybConv, x, y, b, r, g, bo []float32)  { xybRowScalar(c, x, y, b, r, g, bo) }
