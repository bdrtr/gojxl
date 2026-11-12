package jxl

import "math"

// HDR to SDR for sRGB output (Decoder.ForceSRGB), as libjxl renders with a
// display peak (djxl --display_nits): PQ through its Rec. 2408 tone mapper
// (cms/tone_mapping.h) from the image's intensity target down to sdrPeak
// nits, HLG through its OOTF for that peak, both followed by its gamut
// mapping (desaturating towards grey of the same luminance). Input is
// linear display light, 1 = intensity target, in the space whose
// luminances are lum; output is relative to the SDR peak.

// sdrPeak is the peak luminance of the SDR output (libjxl's default
// intensity target).
const sdrPeak = 255

type hdrToSDR struct {
	lum              [3]float64 // luminances of the primaries
	hlg              bool
	hlgExp           float64 // HLG: OOTF exponent (gamma - 1)
	srcPeak, dstPeak float64
	pqMin, pqRange   float64
	invPQRange       float64
	minLum, maxLum   float64
	ks, invOneMinKs  float64
	normalizer       float64
	invTargetPeak    float64
	enc              *tfLUT
	encF             func(float32) float32
}

// pqOETF maps absolute luminance (nits) to the PQ signal; pqEOTF inverts.
func pqOETF(nits float64) float64 {
	const m1, m2 = 0.1593017578125, 78.84375
	const c1, c2, c3 = 0.8359375, 18.8515625, 18.6875
	y := math.Pow(math.Max(nits, 0)/10000, m1)
	return math.Pow((c1+c2*y)/(1+c3*y), m2)
}

func pqEOTF(e float64) float64 {
	const m1, m2 = 0.1593017578125, 78.84375
	const c1, c2, c3 = 0.8359375, 18.8515625, 18.6875
	p := math.Pow(math.Max(e, 0), 1/m2)
	return 10000 * math.Pow(math.Max(p-c1, 0)/(c2-c3*p), 1/m1)
}

// newHDRToSDR maps PQ (hlg false) or HLG light of primaries with the
// luminances lum (nil: the sRGB primaries).
func newHDRToSDR(intensityTarget float32, hlg bool, lum *[3]float64) *hdrToSDR {
	h := &hdrToSDR{srcPeak: float64(intensityTarget), dstPeak: sdrPeak, hlg: hlg}
	for i := range h.lum {
		h.lum[i] = srgbToXYZ[3+i]
	}
	if lum != nil {
		h.lum = *lum
	}
	h.hlgExp = math.Pow(1.111, math.Log2(h.dstPeak/h.srcPeak)) - 1
	h.pqMin = pqOETF(0)
	h.pqRange = pqOETF(h.srcPeak) - h.pqMin
	h.invPQRange = 1 / h.pqRange
	h.minLum = (pqOETF(0) - h.pqMin) * h.invPQRange
	h.maxLum = (pqOETF(h.dstPeak) - h.pqMin) * h.invPQRange
	h.ks = 1.5*h.maxLum - 0.5
	h.invOneMinKs = 1 / math.Max(1e-6, 1-h.ks)
	h.normalizer = h.srcPeak / h.dstPeak
	h.invTargetPeak = 1 / h.dstPeak
	h.encF = tfFunc(TFSRGB, 0, sdrPeak)
	h.enc = buildTFLUT(h.encF)
	return h
}

// toneMap is Rec2408ToneMapperBase::ToneMap.
func (h *hdrToSDR) toneMap(rgb *[3]float64) {
	lum := h.srcPeak * (h.lum[0]*rgb[0] + h.lum[1]*rgb[1] + h.lum[2]*rgb[2])
	npq := math.Min(1, (pqOETF(lum)-h.pqMin)*h.invPQRange)
	e2 := npq
	if npq >= h.ks {
		t := (npq - h.ks) * h.invOneMinKs
		t2 := t * t
		t3 := t2 * t
		e2 = (2*t3-3*t2+1)*h.ks + (t3-2*t2+t)*(1-h.ks) + (-2*t3+3*t2)*h.maxLum
	}
	om := 1 - e2
	om2 := om * om
	e3 := h.minLum*om2*om2 + e2
	e4 := e3*h.pqRange + h.pqMin
	newLum := min(max(pqEOTF(e4), 0), h.dstPeak)
	if lum <= 1e-6 {
		c := newLum * h.invTargetPeak
		rgb[0], rgb[1], rgb[2] = c, c, c
		return
	}
	m := newLum / lum * h.normalizer
	for i := range rgb {
		rgb[i] *= m
	}
}

// gamutMap is libjxl's GamutMapScalar (saturation preservation 0.1).
func (h *hdrToSDR) gamutMap(rgb *[3]float64) {
	const preserve = 0.1
	lum := h.lum[0]*rgb[0] + h.lum[1]*rgb[1] + h.lum[2]*rgb[2]
	var mixSat, mixLum float64
	for _, v := range rgb {
		d := v - lum
		inv := 1.0
		if d != 0 {
			inv = 1 / d
		}
		vod := v * inv
		if d < 0 {
			mixSat = math.Max(mixSat, vod)
		}
		if d <= 0 {
			mixLum = math.Max(mixLum, mixSat)
		} else {
			mixLum = math.Max(mixLum, vod-inv)
		}
	}
	mix := min(max(preserve*(mixSat-mixLum)+mixLum, 0), 1)
	for i, v := range rgb {
		rgb[i] = v + mix*(lum-v)
	}
	mx := math.Max(1, math.Max(rgb[0], math.Max(rgb[1], rgb[2])))
	for i := range rgb {
		rgb[i] /= mx
	}
}

// mapLight maps one pixel of display light to the SDR range.
func (h *hdrToSDR) mapLight(c *[3]float64) {
	if h.hlg {
		if math.Abs(h.hlgExp) <= 0.01 {
			return
		}
		y := h.lum[0]*c[0] + h.lum[1]*c[1] + h.lum[2]*c[2]
		k := math.Min(math.Pow(math.Max(y, 0), h.hlgExp), 1e9)
		for i := range c {
			c[i] *= k
		}
		if h.hlgExp < 0 {
			h.gamutMap(c)
		}
		return
	}
	if h.srcPeak > h.dstPeak {
		h.toneMap(c)
		h.gamutMap(c)
	}
}

// rows maps rows of linear sRGB light to sRGB in place.
func (h *hdrToSDR) rows(r, g, b []float32) {
	var c [3]float64
	for x := range r {
		c[0], c[1], c[2] = float64(r[x]), float64(g[x]), float64(b[x])
		h.mapLight(&c)
		r[x], g[x], b[x] = float32(c[0]), float32(c[1]), float32(c[2])
	}
	h.encodeRows(r, g, b)
}

// encodeRows clips linear sRGB rows to [0, 1] and applies the sRGB curve
// (rows may be empty).
func (h *hdrToSDR) encodeRows(r, g, b []float32) {
	for _, row := range [3][]float32{r, g, b} {
		for x, v := range row {
			row[x] = min(max(v, 0), 1)
		}
		h.enc.apply(row, h.encF)
	}
}
