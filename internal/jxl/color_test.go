package jxl

import (
	"math"
	"testing"
)

func TestSynthesizeICCRoundTrip(t *testing.T) {
	cases := []ColourEncoding{
		{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: PrimP3, TransferFunction: TFSRGB},
		{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: Prim2100, TransferFunction: TFPQ},
		{ColourSpace: CSRGB, WhitePoint: WPDCI, Primaries: PrimP3, TransferFunction: TFDCI},
		{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: PrimSRGB, TransferFunction: TFGamma, Gamma: 1 / 2.2},
		{ColourSpace: CSGrey, WhitePoint: WPD65, TransferFunction: TF709},
	}
	for _, c := range cases {
		icc := SynthesizeICC(&c)
		p := parseICC(icc)
		if p == nil {
			t.Fatalf("%s: profile not parsed", c.describe())
		}
		if p.grey != (c.ColourSpace == CSGrey) {
			t.Fatalf("%s: grey mismatch", c.describe())
		}
		eotf := c.eotf()
		for _, v := range []float64{0.01, 0.2, 0.5, 0.9} {
			if d := math.Abs(p.trc[0](v) - eotf(v)); d > 2e-3 {
				t.Errorf("%s: trc(%v) = %v, want %v", c.describe(), v, p.trc[0](v), eotf(v))
			}
		}
		if c.ColourSpace == CSGrey {
			continue
		}
		// white must map to the D50 PCS white
		w := p.m.apply([3]float64{1, 1, 1})
		for i, want := range []float64{0.9642, 1, 0.8249} {
			if math.Abs(w[i]-want) > 1e-3 {
				t.Errorf("%s: white %v", c.describe(), w)
				break
			}
		}
	}
}

func TestLabToXYZ(t *testing.T) {
	w := labToXYZ(100, 0, 0)
	if math.Abs(w[0]-0.9642) > 1e-6 || math.Abs(w[1]-1) > 1e-6 || math.Abs(w[2]-0.8249) > 1e-6 {
		t.Fatalf("white %v", w)
	}
	if k := labToXYZ(0, 0, 0); k[1] != 0 {
		t.Fatalf("black %v", k)
	}
}

func TestCLUTInterpolationExact(t *testing.T) {
	// 4-input identity-like CLUT: output = (in0+in3)/2, in1, in2
	g := 3
	p := &lutPipe{inCh: 4, outCh: 3, grid: []int{g, g, g, g}, labScaleL: 100, labScaleAB: 255, xyzScale: 1}
	for a := 0; a < g; a++ {
		for b := 0; b < g; b++ {
			for c := 0; c < g; c++ {
				for d := 0; d < g; d++ {
					fa, fb, fc, fd := float64(a)/2, float64(b)/2, float64(c)/2, float64(d)/2
					p.clut = append(p.clut, (fa+fd)/2, fb, fc)
				}
			}
		}
	}
	in := []float64{0.3, 0.8, 0.15, 0.6}
	o := p.eval(in, nil) // XYZ PCS: values pass through
	want := [3]float64{0.45, 0.8, 0.15}
	for i := range o {
		if math.Abs(o[i]-want[i]) > 1e-9 {
			t.Fatalf("got %v want %v", o, want)
		}
	}
}
