package gojxl

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func threadTestImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 128 + 60*math.Sin(float64(x)*0.05) + 50*math.Cos(float64(y)*0.07)
			img.SetNRGBA(x, y, color.NRGBA{uint8(v), uint8(255 - v), uint8((x ^ y) & 255), 255})
		}
	}
	return img
}

// maxGoroutines runs fn and samples the goroutine count meanwhile.
func maxGoroutines(fn func()) int {
	var stop atomic.Bool
	var best atomic.Int32
	done := make(chan struct{})
	go func() {
		for !stop.Load() {
			if n := int32(runtime.NumGoroutine()); n > best.Load() {
				best.Store(n)
			}
			time.Sleep(20 * time.Microsecond)
		}
		close(done)
	}()
	fn()
	stop.Store(true)
	<-done
	return int(best.Load())
}

// With SetMaxThreads(1) conversions start no goroutines, and the cap does
// not change any result.
func TestSetMaxThreads(t *testing.T) {
	src := threadTestImage(600, 500)
	var pngBuf, jpgBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpgBuf, src, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	jobs := map[string]func() ([]byte, error){
		"png lossless":   func() ([]byte, error) { return ConvertToJXL(pngBuf.Bytes(), nil) },
		"jpeg transcode": func() ([]byte, error) { return TranscodeJPEG(jpgBuf.Bytes()) },
		"lossy":          func() ([]byte, error) { return EncodeBytes(src, &EncodeOptions{Distance: 1.5}) },
	}
	for name, job := range jobs {
		want, err := job()
		if err != nil {
			t.Fatal(name, err)
		}
		var got, back []byte
		var decoded image.Image
		old := SetMaxThreads(1)
		base := runtime.NumGoroutine()
		peak := maxGoroutines(func() {
			got, err = job()
			if err == nil {
				decoded, err = DecodeBytes(got)
			}
			if err == nil && name == "jpeg transcode" {
				back, err = ReconstructJPEG(got)
			}
		})
		SetMaxThreads(old)
		if err != nil {
			t.Fatal(name, err)
		}
		// the sampler is the one extra goroutine
		if peak > base+1 {
			t.Errorf("%s: %d goroutines with one thread (%d before)", name, peak, base)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: output differs with one thread", name)
		}
		if decoded.Bounds() != src.Bounds() {
			t.Errorf("%s: decoded %v", name, decoded.Bounds())
		}
		if name == "jpeg transcode" && !bytes.Equal(back, jpgBuf.Bytes()) {
			t.Errorf("JPEG not restored with one thread")
		}
	}
}
