package gojxl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	ijpeg "gojxl/internal/jpeg"
	"gojxl/internal/jxl"
	"gojxl/internal/pngx"
)

func colourTestImage() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / 95), uint8(y * 4), uint8(128 + 64*math.Sin(float64(x+y)/9)), 255})
		}
	}
	return img
}

var (
	encP3     = jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimP3, TransferFunction: jxl.TFSRGB, RenderingIntent: 1}
	encAdobe  = jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimCustom, Red: [2]float64{0.64, 0.33}, Green: [2]float64{0.21, 0.71}, Blue: [2]float64{0.15, 0.06}, TransferFunction: jxl.TFGamma, Gamma: 0.4547069, RenderingIntent: 1}
	encLinear = jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimSRGB, TransferFunction: jxl.TFLinear, RenderingIntent: 1}
)

// lutOnlyICC is a valid RGB profile without matrix/TRC tags (as for a
// LUT-based profile): the tag signatures are renamed.
func lutOnlyICC() []byte {
	icc := jxl.SynthesizeICC(&encP3)
	n := int(binary.BigEndian.Uint32(icc[128:]))
	for i := 0; i < n; i++ {
		e := icc[132+12*i:]
		if e[1] == 'X' || e[1] == 'T' { // rXYZ.. rTRC..
			e[0] = 'x'
		}
	}
	return icc
}

func headerOf(t *testing.T, data []byte) *jxl.Decoder {
	t.Helper()
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func sameEncoding(a, b jxl.ColourEncoding) bool {
	near := func(x, y [2]float64) bool { return math.Abs(x[0]-y[0]) < 1e-4 && math.Abs(x[1]-y[1]) < 1e-4 }
	return a.ColourSpace == b.ColourSpace && a.WhitePoint == b.WhitePoint && a.Primaries == b.Primaries &&
		a.TransferFunction == b.TransferFunction && math.Abs(a.Gamma-b.Gamma) < 1e-6 &&
		near(a.White, b.White) && near(a.Red, b.Red) && near(a.Green, b.Green) && near(a.Blue, b.Blue)
}

func psnr8(a, b image.Image) float64 {
	var se float64
	r := a.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c1 := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA)
			c2 := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA)
			for _, d := range [3]float64{float64(c1.R) - float64(c2.R), float64(c1.G) - float64(c2.G), float64(c1.B) - float64(c2.B)} {
				se += d * d
			}
		}
	}
	mse := se / float64(3*r.Dx()*r.Dy())
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

// Samples tagged with a profile come back unchanged (lossless) or close
// (lossy, rendered into the same space), with the profile as the enum
// encoding it describes, or as itself when it has none.
func TestEncodeWithICC(t *testing.T) {
	src := colourTestImage()
	for _, c := range []struct {
		name string
		icc  []byte
		enc  *jxl.ColourEncoding
	}{
		{"P3", jxl.SynthesizeICC(&encP3), &encP3},
		{"Adobe", jxl.SynthesizeICC(&encAdobe), &encAdobe},
		{"linear", jxl.SynthesizeICC(&encLinear), &encLinear},
		{"lut", lutOnlyICC(), nil},
	} {
		for _, dist := range []float32{0, 1} {
			data, err := EncodeBytes(src, &EncodeOptions{ICC: c.icc, Distance: dist, Effort: 3})
			if c.enc == nil && dist > 0 {
				if !errors.Is(err, ErrUnsupportedColour) {
					t.Errorf("%s lossy: err %v, want ErrUnsupportedColour", c.name, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s d%v: %v", c.name, dist, err)
			}
			d := headerOf(t, data)
			col := d.Header.Colour
			switch {
			case c.enc == nil:
				if !col.WantICC || !bytes.Equal(d.ICC, c.icc) {
					t.Errorf("%s d%v: profile not stored as is", c.name, dist)
				}
			case col.WantICC || !sameEncoding(col, *c.enc):
				t.Errorf("%s d%v: colour %+v, want %+v", c.name, dist, col, *c.enc)
			}
			img, err := DecodeBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			p := psnr8(src, img)
			if dist == 0 && !math.IsInf(p, 1) || dist > 0 && p < 34 {
				t.Errorf("%s d%v: PSNR %.2f", c.name, dist, p)
			}
		}
	}
	if _, err := EncodeBytes(src, &EncodeOptions{ICC: []byte("not a profile")}); err == nil {
		t.Error("malformed ICC accepted")
	}
}

// pngWith returns a PNG of img with extra chunks after IHDR.
func pngWith(t *testing.T, img image.Image, chunks ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	p := b.Bytes()
	out := append([]byte(nil), p[:33]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[33:]...)
}

func chunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b[4:]))
}

func u32s(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

func TestConvertToJXLPNGColour(t *testing.T) {
	src := colourTestImage()
	iccP3 := pngx.ICCChunk(jxl.SynthesizeICC(&encP3))
	iccSRGB := pngx.ICCChunk(jxl.SynthesizeICC(&jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimSRGB, TransferFunction: jxl.TFSRGB, RenderingIntent: 1}))
	gamma22 := jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimSRGB, TransferFunction: jxl.TFGamma, Gamma: 0.45455, RenderingIntent: 1}
	linP3 := encP3
	linP3.TransferFunction = jxl.TFLinear
	srgb := jxl.ColourEncoding{ColourSpace: jxl.CSRGB, WhitePoint: jxl.WPD65, Primaries: jxl.PrimSRGB, TransferFunction: jxl.TFSRGB}
	for _, c := range []struct {
		name   string
		chunks [][]byte
		want   jxl.ColourEncoding
	}{
		{"none", nil, srgb},
		{"gAMA", [][]byte{chunk("gAMA", u32s(45455))}, gamma22},
		{"gAMA+cHRM", [][]byte{chunk("gAMA", u32s(100000)), chunk("cHRM", u32s(31270, 32900, 68000, 32000, 26500, 69000, 15000, 6000))}, linP3},
		{"sRGB wins", [][]byte{chunk("sRGB", []byte{0}), chunk("gAMA", u32s(45455))}, srgb},
		{"iCCP", [][]byte{chunk(iccP3.Type, iccP3.Data)}, encP3},
		{"sRGB iCCP wins", [][]byte{chunk(iccSRGB.Type, iccSRGB.Data), chunk("gAMA", u32s(45455))}, srgb},
		{"cICP wins", [][]byte{chunk("cICP", []byte{12, 13, 0, 1}), chunk(iccSRGB.Type, iccSRGB.Data)}, encP3},
	} {
		file := pngWith(t, src, c.chunks...)
		for _, dist := range []float32{0, 1.5} {
			data, err := ConvertToJXL(file, &EncodeOptions{Distance: dist, Effort: 2})
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if col := headerOf(t, data).Header.Colour; col.WantICC || !sameEncoding(col, c.want) {
				t.Errorf("%s d%v: colour %+v, want %+v", c.name, dist, col, c.want)
			}
		}
	}
}

func TestJPEGColour(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, colourTestImage(), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	file := ijpeg.InsertMetadata(b.Bytes(), jxl.SynthesizeICC(&encP3), nil, nil)
	if !bytes.Equal(ijpeg.ICCProfile(file), jxl.SynthesizeICC(&encP3)) {
		t.Fatal("ICC not found in JPEG")
	}
	// pixels: the profile as its enum
	data, err := ConvertToJXL(file, &EncodeOptions{JPEGPixels: true, Distance: 1})
	if err != nil {
		t.Fatal(err)
	}
	if col := headerOf(t, data).Header.Colour; !sameEncoding(col, encP3) {
		t.Errorf("JPEG pixels: colour %+v", col)
	}
	// back to JPEG: the decoded pixels' profile goes into the file
	var out bytes.Buffer
	if err := ConvertToJPEG(&out, data, 90); err != nil {
		t.Fatal(err)
	}
	if e := jxl.EncodingFromICC(ijpeg.ICCProfile(out.Bytes())); e == nil || !sameEncoding(*e, encP3) {
		t.Errorf("JPEG output profile: %+v", e)
	}
	// recompressed: the original file comes back
	data, err = ConvertToJXL(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ConvertToJPEG(&out, data, 90); err != nil || !bytes.Equal(out.Bytes(), file) {
		t.Errorf("JPEG not restored (%v)", err)
	}
}

func TestAnimationColour(t *testing.T) {
	a, b := colourTestImage(), colourTestImage()
	b.Set(5, 5, color.NRGBA{1, 2, 3, 255})
	anim := &Animation{Frames: []Frame{{Image: a, Duration: 0.1}, {Image: b, Duration: 0.1}}}
	for _, dist := range []float32{0, 1} {
		data, err := EncodeAnimation(anim, &EncodeOptions{ICC: jxl.SynthesizeICC(&encAdobe), Distance: dist})
		if err != nil {
			t.Fatal(err)
		}
		if col := headerOf(t, data).Header.Colour; !sameEncoding(col, encAdobe) {
			t.Errorf("d%v: colour %+v", dist, col)
		}
		got, err := DecodeAll(data)
		if err != nil {
			t.Fatal(err)
		}
		if p := psnr8(b, got.Frames[1].Image); dist == 0 && !math.IsInf(p, 1) || p < 34 {
			t.Errorf("d%v: frame 2 PSNR %.2f", dist, p)
		}
	}
}
