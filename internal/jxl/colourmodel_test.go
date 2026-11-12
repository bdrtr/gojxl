package jxl

import (
	"math"
	"testing"
)

// testEncodings are enum colour encodings the encoder must recognise in
// the ICC profiles synthesised for them.
var testEncodings = []ColourEncoding{
	{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: PrimP3, TransferFunction: TFSRGB, RenderingIntent: 1},
	{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: Prim2100, TransferFunction: TF709, RenderingIntent: 0},
	{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: PrimSRGB, TransferFunction: TFLinear, RenderingIntent: 1},
	{ColourSpace: CSRGB, WhitePoint: WPDCI, Primaries: PrimP3, TransferFunction: TFDCI, RenderingIntent: 1},
	{ColourSpace: CSRGB, WhitePoint: WPD65, Primaries: PrimCustom, Red: [2]float64{0.64, 0.33}, Green: [2]float64{0.21, 0.71},
		Blue: [2]float64{0.15, 0.06}, TransferFunction: TFGamma, Gamma: 0.4547069, RenderingIntent: 1}, // Adobe RGB
	{ColourSpace: CSRGB, WhitePoint: WPCustom, White: [2]float64{0.3457, 0.3585}, Primaries: PrimCustom,
		Red: [2]float64{0.7347, 0.2653}, Green: [2]float64{0.1596, 0.8404}, Blue: [2]float64{0.0366, 0.0001},
		TransferFunction: TFGamma, Gamma: 0.5555556, RenderingIntent: 3}, // ProPhoto
	{ColourSpace: CSGrey, WhitePoint: WPD65, TransferFunction: TFGamma, Gamma: 0.4545455, RenderingIntent: 1},
	{ColourSpace: CSGrey, WhitePoint: WPD65, TransferFunction: TFSRGB, RenderingIntent: 1},
}

func TestEncodingFromSynthesizedICC(t *testing.T) {
	for _, c := range testEncodings {
		icc := SynthesizeICC(&c)
		// the synthesised profiles carry the relative intent
		want := c
		want.RenderingIntent = 0
		got := EncodingFromICC(icc)
		if got == nil {
			t.Errorf("%s: no enum found", c.describe())
			continue
		}
		g := *got
		g.RenderingIntent = 0
		near := func(a, b [2]float64) bool { return math.Abs(a[0]-b[0]) < 1e-4 && math.Abs(a[1]-b[1]) < 1e-4 }
		if g.ColourSpace != want.ColourSpace || g.WhitePoint != want.WhitePoint || g.TransferFunction != want.TransferFunction ||
			want.ColourSpace != CSGrey && g.Primaries != want.Primaries ||
			!near(g.White, want.White) || !near(g.Red, want.Red) || !near(g.Green, want.Green) || !near(g.Blue, want.Blue) ||
			math.Abs(g.Gamma-want.Gamma) > 1e-5 {
			t.Errorf("%s: got %+v, want %+v", c.describe(), g, want)
		}
	}
}

// TestModelInvertsRendering checks that the encoder's model maps samples to
// the linear sRGB from which the decoder renders the same samples again.
func TestModelInvertsRendering(t *testing.T) {
	for _, c := range testEncodings {
		ih := &ImageHeader{Colour: c}
		ih.ToneMapping.IntensityTarget = 255
		for _, viaICC := range []bool{false, true} {
			var m *ColourModel
			var tgt *outputTarget
			if viaICC {
				icc := SynthesizeICC(&c)
				m = ModelFromICC(icc)
				h := *ih
				h.Colour.WantICC = true
				tgt = xybTarget(&h, icc)
			} else {
				m = ModelFromEncoding(&c)
				tgt = xybTarget(ih, nil)
			}
			if m == nil {
				t.Fatalf("%s (icc %v): no model", c.describe(), viaICC)
			}
			for _, v := range [][3]float64{{0.2, 0.5, 0.8}, {1, 1, 1}, {0.05, 0.9, 0.3}, {0.5, 0.5, 0.5}} {
				if m.Grey {
					v[1], v[2] = v[0], v[0]
				}
				lin := m.linear(v)
				conv, enc := mat3(tgt.m), tgt.enc
				if tgt.srgb {
					conv, enc = identity3, func(v float32) float32 { return float32(srgbOETF(float64(v))) }
				}
				dev := conv.apply(lin)
				for ch := 0; ch < 3; ch++ {
					if m.Grey && ch > 0 {
						break
					}
					got := float64(enc(float32(dev[ch])))
					if math.Abs(got-v[ch]) > 2e-3 {
						t.Errorf("%s (icc %v): %v channel %d -> %v", c.describe(), viaICC, v, ch, got)
					}
				}
			}
		}
	}
}

func TestEncodingFromCICP(t *testing.T) {
	if c := EncodingFromCICP(12, 13, 0, 1); c == nil || c.Primaries != PrimP3 || c.TransferFunction != TFSRGB || c.WhitePoint != WPD65 {
		t.Errorf("Display P3: %+v", c)
	}
	if c := EncodingFromCICP(9, 16, 0, 1); c == nil || c.Primaries != Prim2100 || c.TransferFunction != TFPQ {
		t.Errorf("BT.2100 PQ: %+v", c)
	}
	if c := EncodingFromCICP(1, 13, 1, 1); c != nil {
		t.Errorf("YCbCr matrix accepted: %+v", c)
	}
}
