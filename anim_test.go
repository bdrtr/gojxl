package gojxl

import (
	"image"
	"image/color"
	"testing"
)

func TestAnimationRoundTrip(t *testing.T) {
	const w, h = 70, 50
	var frames []Frame
	// grey, RGB and RGBA frames (unified to RGBA), a repeated frame (merged
	// into its predecessor's duration) and small changes (cropped)
	g := image.NewGray(image.Rect(0, 0, w, h))
	for i := range g.Pix {
		g.Pix[i] = byte(i % 251)
	}
	frames = append(frames, Frame{Image: g, Duration: 0.1})
	base := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			base.SetNRGBA(x, y, color.NRGBA{byte(x * 3), byte(y * 5), byte(x ^ y), 255})
		}
	}
	frames = append(frames, Frame{Image: base, Duration: 0.05})
	frames = append(frames, Frame{Image: base, Duration: 0.05})
	for k := 0; k < 3; k++ {
		m := image.NewNRGBA(base.Rect)
		copy(m.Pix, base.Pix)
		for y := 10 + k; y < 20+k; y++ {
			for x := 30; x < 40+3*k; x++ {
				m.SetNRGBA(x, y, color.NRGBA{200, byte(k), 7, byte(100 + k)})
			}
		}
		frames = append(frames, Frame{Image: m, Duration: 0.02 * float64(k+1)})
	}
	data, err := EncodeAnimation(&Animation{Frames: frames, NumLoops: 3}, &EncodeOptions{Effort: 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.NumLoops != 3 {
		t.Errorf("loops %d", got.NumLoops)
	}
	// expected: frame 2 merged into frame 1
	want := []Frame{frames[0], {Image: base, Duration: 0.1}, frames[3], frames[4], frames[5]}
	if len(got.Frames) != len(want) {
		t.Fatalf("%d frames, want %d", len(got.Frames), len(want))
	}
	for i, f := range got.Frames {
		if d := f.Duration - want[i].Duration; d > 1e-9 || d < -1e-9 {
			t.Errorf("frame %d duration %v, want %v", i, f.Duration, want[i].Duration)
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				a := color.NRGBAModel.Convert(want[i].Image.At(x, y))
				b := color.NRGBAModel.Convert(f.Image.At(x, y))
				if a != b {
					t.Fatalf("frame %d (%d,%d): %v != %v", i, x, y, b, a)
				}
			}
		}
	}
}
