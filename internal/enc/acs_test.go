package enc

import (
	"math"
	"testing"

	"gojxl/internal/jxl"
)

// Every varblock transform type, tiled over the whole image, must decode
// to a close approximation of the input.
func TestVarblockTypes(t *testing.T) {
	const w, h = 136, 120 // not multiples of the large transforms
	im := &Image{W: w, H: h, BitDepth: 8}
	for c := 0; c < 3; c++ {
		ch := make([]int32, w*h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				v := 128 + 60*math.Sin(float64(x)*0.11+float64(c)) + 50*math.Cos(float64(y)*0.07*float64(c+1))
				if (x/9+y/13)%5 == 0 {
					v += 30
				}
				ch[y*w+x] = int32(min(max(v, 0), 255))
			}
		}
		im.Chans = append(im.Chans, ch)
	}
	for ft := 0; ft < len(vtypes); ft++ {
		opt := LossyOptions{Distance: 1, EPFIters: -1, Sharpness: -1, forceType: ft}
		if ft == 0 {
			opt.OnlyDCT8 = true
		}
		data, err := EncodeVarDCT(im, opt)
		if err != nil {
			t.Fatal(err)
		}
		d, err := jxl.NewDecoder(data)
		if err != nil {
			t.Fatal(err)
		}
		f, err := d.NextFrame()
		if err != nil {
			t.Fatalf("type %d: %v", ft, err)
		}
		var se float64
		tmp := make([]float32, 3*w)
		for y := 0; y < h; y++ {
			r, g, b := f.RGBRow(y, tmp)
			for x := 0; x < w; x++ {
				for c, v := range [3]float32{r[x], g[x], b[x]} {
					e := float64(v)*255 - float64(im.Chans[c][y*w+x])
					se += e * e
				}
			}
		}
		psnr := 10 * math.Log10(255*255/(se/float64(3*w*h)))
		t.Logf("type %d (acs %d): %d bytes, PSNR %.2f", ft, vtypes[ft].acs, len(data), psnr)
		if psnr < 34 {
			t.Errorf("type %d (acs %d): PSNR %.2f", ft, vtypes[ft].acs, psnr)
		}
	}
}
