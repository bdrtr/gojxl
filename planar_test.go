package gojxl

import (
	"image"
	"image/color"
	"math/rand"
	"reflect"
	"testing"
)

// opaqueWrap hides the concrete type so toPlanar takes the generic path.
type opaqueWrap struct{ image.Image }

func TestToPlanarFastPaths(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	r := image.Rect(3, 2, 40, 31)
	for _, translucent := range []bool{false, true} {
		a8 := image.NewRGBA(r)
		a16 := image.NewRGBA64(r)
		n16 := image.NewNRGBA64(r)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				al := uint8(255)
				if translucent && rnd.Intn(4) == 0 {
					al = uint8(rnd.Intn(256))
				}
				v := func() uint8 { return uint8(rnd.Intn(int(al) + 1)) }
				a8.SetRGBA(x, y, color.RGBA{v(), v(), v(), al})
				w := func() uint16 { return uint16(rnd.Intn(65536)) }
				a16.SetRGBA64(x, y, color.RGBA64{w() / 2, w() / 2, w() / 2, 0xffff})
				n16.SetNRGBA64(x, y, color.NRGBA64{w(), w(), w(), 0xffff})
			}
		}
		for _, im := range []image.Image{a8, a16, n16} {
			got, want := toPlanar(im), toPlanar(opaqueWrap{im})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%T translucent=%v: fast path differs", im, translucent)
			}
		}
	}
}
