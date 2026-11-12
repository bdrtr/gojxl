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
	if err := ConvertToJPEG(&out, data, 90, nil); err != nil {
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
	if err := ConvertToJPEG(&out, data, 90, nil); err != nil || !bytes.Equal(out.Bytes(), file) {
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

// iccWithChannelCurves returns a P3 profile whose green and blue tone
// curves are pure gammas (1.8, 2.6) unlike the red one (sRGB).
func iccWithChannelCurves() []byte {
	icc := append([]byte(nil), jxl.SynthesizeICC(&encP3)...)
	n := int(binary.BigEndian.Uint32(icc[128:]))
	for i := 0; i < n; i++ {
		var g float64
		switch string(icc[132+12*i : 136+12*i]) {
		case "gTRC":
			g = 1.8
		case "bTRC":
			g = 2.6
		default:
			continue
		}
		for len(icc)%4 != 0 {
			icc = append(icc, 0)
		}
		off := len(icc)
		icc = append(icc, "para\x00\x00\x00\x00\x00\x00\x00\x00"...)
		icc = binary.BigEndian.AppendUint32(icc, uint32(int32(math.Round(g*65536))))
		binary.BigEndian.PutUint32(icc[136+12*i:], uint32(off))
		binary.BigEndian.PutUint32(icc[140+12*i:], 16)
	}
	binary.BigEndian.PutUint32(icc, uint32(len(icc)))
	return icc
}

// Lossy images tagged with a profile whose channel curves differ decode
// back into that space channel by channel.
func TestChannelCurvesProfile(t *testing.T) {
	src := colourTestImage()
	icc := iccWithChannelCurves()
	data, err := EncodeBytes(src, &EncodeOptions{ICC: icc, Distance: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d := headerOf(t, data); !d.Header.Colour.WantICC {
		t.Fatal("profile not stored as ICC")
	}
	img, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if p := psnr8(src, img); p < 34 {
		t.Errorf("PSNR %.2f", p)
	}
}

// toSRGB8 converts 8-bit samples of a colour space to sRGB.
func toSRGB8(src *image.NRGBA, c *jxl.ColourEncoding) *image.NRGBA {
	m := jxl.ModelFromEncoding(c)
	oetf := func(v float64) uint8 {
		v = math.Min(math.Max(v, 0), 1)
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return uint8(math.Round(v * 255))
	}
	out := image.NewNRGBA(src.Rect)
	for i := 0; i < len(src.Pix); i += 4 {
		var l [3]float64
		for k := 0; k < 3; k++ {
			l[k] = m.Curves[k](float64(src.Pix[i+k]) / 255)
		}
		for k := 0; k < 3; k++ {
			out.Pix[i+k] = oetf(m.M[3*k]*l[0] + m.M[3*k+1]*l[1] + m.M[3*k+2]*l[2])
		}
		out.Pix[i+3] = src.Pix[i+3]
	}
	return out
}

// Options.SRGB renders both XYB and stored-space images to sRGB, untagged.
func TestDecodeToSRGB(t *testing.T) {
	src := colourTestImage()
	want := toSRGB8(src, &encP3)
	for _, dist := range []float32{0, 1} {
		data, err := EncodeBytes(src, &EncodeOptions{ICC: jxl.SynthesizeICC(&encP3), Distance: dist})
		if err != nil {
			t.Fatal(err)
		}
		img, err := DecodeBytesOptions(data, &Options{SRGB: true})
		if err != nil {
			t.Fatal(err)
		}
		if p := psnr8(want, img); dist == 0 && p < 50 || p < 34 {
			t.Errorf("d%v: PSNR %.2f against the sRGB conversion", dist, p)
		}
		var out bytes.Buffer
		if err := ConvertToPNG(&out, data, &Options{SRGB: true}); err != nil {
			t.Fatal(err)
		}
		if a := pngx.ReadAncillary(out.Bytes()); a.ICC != nil || a.CICP != nil {
			t.Errorf("d%v: PNG still tagged", dist)
		}
	}
}

// pqImage returns src as BT.2100 PQ samples (16-bit; reference white at
// 203 nits), the way HDR PNGs carry them.
func pqImage(src *image.NRGBA) *image.NRGBA64 {
	m := [9]float64{0.6274, 0.3293, 0.0433, 0.0691, 0.9195, 0.0114, 0.0164, 0.0880, 0.8956}
	pq := func(nits float64) uint16 {
		const m1, m2 = 0.1593017578125, 78.84375
		const c1, c2, c3 = 0.8359375, 18.8515625, 18.6875
		y := math.Pow(math.Max(nits, 0)/10000, m1)
		return uint16(math.Round(math.Pow((c1+c2*y)/(1+c3*y), m2) * 65535))
	}
	out := image.NewNRGBA64(src.Rect)
	for i := 0; i < len(src.Pix); i += 4 {
		var l [3]float64
		for k := range l {
			v := float64(src.Pix[i+k]) / 255
			if v <= 0.04045 {
				l[k] = v / 12.92
			} else {
				l[k] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		for k := 0; k < 3; k++ {
			p := pq(203 * (m[3*k]*l[0] + m[3*k+1]*l[1] + m[3*k+2]*l[2]))
			out.Pix[2*i+2*k], out.Pix[2*i+2*k+1] = uint8(p>>8), uint8(p)
		}
		out.Pix[2*i+6], out.Pix[2*i+7] = 255, 255
	}
	return out
}

// PQ input keeps its peak luminance (10000 nits, as libjxl stores it), and
// decoding to sRGB tone maps it to an SDR image of similar brightness.
func TestPQToSDR(t *testing.T) {
	src := colourTestImage()
	file := pngWith(t, pqImage(src), chunk("cICP", []byte{9, 16, 0, 1}))
	data, err := ConvertToJXL(file, &EncodeOptions{Effort: 1})
	if err != nil {
		t.Fatal(err)
	}
	if d := headerOf(t, data); d.Header.ToneMapping.IntensityTarget != 10000 || d.Header.Colour.TransferFunction != jxl.TFPQ {
		t.Fatalf("intensity target %v, transfer %d", d.Header.ToneMapping.IntensityTarget, d.Header.Colour.TransferFunction)
	}
	img, err := DecodeBytesOptions(data, &Options{SRGB: true})
	if err != nil {
		t.Fatal(err)
	}
	var ms, mo float64
	r := src.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := src.NRGBAAt(x, y)
			o := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			ms += float64(c.R) + float64(c.G) + float64(c.B)
			mo += float64(o.R) + float64(o.G) + float64(o.B)
		}
	}
	// reference white (203 nits) lands at 203/255 of the SDR peak
	if ratio := mo / ms; ratio < 0.75 || ratio > 1.0 {
		t.Errorf("SDR brightness %.2f of the source's", ratio)
	}
	if icc, cicp := outputColour(data, &Options{SRGB: true}); icc != nil || cicp != nil {
		t.Error("SDR output still tagged")
	}
}

// Lossy PQ and HLG encoding keeps the samples' space: decoding gives the
// signal back (XYB holds the light, 1 = 255 nits).
func TestLossyHDR(t *testing.T) {
	src := colourTestImage()
	pqImg := pqImage(src)
	hlgImg := image.NewNRGBA64(pqImg.Rect)
	for i := 0; i < len(src.Pix); i += 4 {
		for k := 0; k < 3; k++ {
			// HLG signal of a scene light ramp (a plain test pattern)
			s := float64(src.Pix[i+k]) / 255 * 0.8
			var e float64
			if s <= 1.0/12 {
				e = math.Sqrt(3 * s)
			} else {
				e = 0.17883277*math.Log(12*s-0.28466892) + 0.55991073
			}
			v := uint16(math.Round(e * 65535))
			hlgImg.Pix[2*i+2*k], hlgImg.Pix[2*i+2*k+1] = uint8(v>>8), uint8(v)
		}
		hlgImg.Pix[2*i+6], hlgImg.Pix[2*i+7] = 255, 255
	}
	for _, c := range []struct {
		name string
		img  image.Image
		tf   byte
	}{{"PQ", pqImg, 16}, {"HLG", hlgImg, 18}} {
		file := pngWith(t, c.img, chunk("cICP", []byte{9, c.tf, 0, 1}))
		data, err := ConvertToJXL(file, &EncodeOptions{Distance: 1})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		d := headerOf(t, data)
		if !d.Header.XYBEncoded || d.Header.Colour.TransferFunction != uint32(c.tf) {
			t.Fatalf("%s: xyb %v, transfer %d", c.name, d.Header.XYBEncoded, d.Header.Colour.TransferFunction)
		}
		img, err := DecodeBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		var se float64
		b := c.img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r1, g1, b1, _ := c.img.At(x, y).RGBA()
				r2, g2, b2, _ := img.At(x, y).RGBA()
				for _, dd := range [3]float64{float64(r1) - float64(r2), float64(g1) - float64(g2), float64(b1) - float64(b2)} {
					se += dd * dd
				}
			}
		}
		if p := 10 * math.Log10(65535*65535/(se/float64(3*b.Dx()*b.Dy()))); p < 32 {
			t.Errorf("%s: PSNR %.2f", c.name, p)
		}
	}
}
