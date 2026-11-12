package gojxl

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/gif"
	"math"
	"runtime"
	"sync"

	"gojxl/internal/enc"
	"gojxl/internal/pngx"
)

// EncodeAnimation encodes an animation (lossless, or lossy VarDCT when
// opt.Distance > 0). All frames must have the same size; each is shown for
// its Duration (seconds). NumLoops is the number of plays (0 = forever).
func EncodeAnimation(anim *Animation, opt *EncodeOptions) ([]byte, error) {
	if anim == nil || len(anim.Frames) == 0 {
		return nil, errors.New("gojxl: empty animation")
	}
	lossy := opt != nil && opt.Distance > 0
	effort := 4
	if opt != nil && opt.Effort > 0 {
		effort = opt.Effort
	}
	b0 := anim.Frames[0].Image.Bounds()
	ims := make([]*enc.Image, len(anim.Frames))
	alpha, colour, deep := false, false, false
	for i, f := range anim.Frames {
		if f.Image.Bounds().Size() != b0.Size() {
			return nil, errors.New("gojxl: animation frames differ in size")
		}
		ims[i] = toPlanar(f.Image)
		alpha = alpha || ims[i].Alpha
		colour = colour || !ims[i].Grey
		deep = deep || ims[i].BitDepth > 8
	}
	// one image header for all frames: common channels and bit depth
	if lossy {
		deep = false // VarDCT input is 8-bit
	}
	for _, im := range ims {
		if !deep && im.BitDepth > 8 {
			for _, c := range im.Chans {
				for k, v := range c {
					c[k] = (v + 128) / 257
				}
			}
			im.BitDepth = 8
		}
		if deep && im.BitDepth == 8 {
			for _, c := range im.Chans {
				for k, v := range c {
					c[k] = v * 257
				}
			}
			im.BitDepth = 16
		}
		if colour && im.Grey {
			g := im.Chans[0]
			im.Chans = append([][]int32{g, append([]int32(nil), g...), append([]int32(nil), g...)}, im.Chans[1:]...)
			im.Grey = false
		}
		if alpha && !im.Alpha {
			a := make([]int32, im.W*im.H)
			full := int32(1)<<im.BitDepth - 1
			for k := range a {
				a[k] = full
			}
			im.Chans = append(im.Chans, a)
			im.Alpha = true
		}
	}
	// frames identical to their predecessor only extend its duration;
	// others are cropped to the region that changed
	type job struct {
		im       *enc.Image
		x0, y0   int
		crop     bool
		duration float64
	}
	var jobs []job
	for i, im := range ims {
		d := anim.Frames[i].Duration
		if i == 0 {
			jobs = append(jobs, job{im: im, duration: d})
			continue
		}
		r, same := diffRect(ims[i-1], im)
		if same {
			jobs[len(jobs)-1].duration += d
			continue
		}
		j := job{im: im, duration: d}
		if r.Dx() < im.W || r.Dy() < im.H {
			j.im, j.x0, j.y0, j.crop = cropImage(im, r), r.Min.X, r.Min.Y, true
		}
		jobs = append(jobs, j)
	}
	// ticks of 1 ms
	an := &enc.Anim{TpsNum: 1000, TpsDen: 1, Loops: uint32(max(anim.NumLoops, 0))}
	outs := make([][]byte, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			d := math.Round(j.duration * 1000)
			fo := enc.FrameOpts{
				Anim:          an,
				Duration:      uint32(min(max(d, 0), math.MaxUint32)),
				NotLast:       i < len(jobs)-1,
				NoImageHeader: i > 0,
				Crop:          j.crop,
				CropX0:        j.x0,
				CropY0:        j.y0,
			}
			if lossy {
				lo := enc.LossyOptions{Distance: opt.Distance, EPFIters: -1, Sharpness: -1, LibAQ: true, Effort: opt.Effort, Frame: fo}
				outs[i], errs[i] = enc.EncodeVarDCT(j.im, lo)
				return
			}
			// frames are converted copies, compared before encoding starts
			outs[i], errs[i] = enc.EncodeLossless(j.im, enc.Options{Effort: effort, Consume: true, FrameOpts: fo})
		}()
	}
	wg.Wait()
	var out []byte
	for i := range outs {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, outs[i]...)
	}
	return out, nil
}

// diffRect returns the bounding box of the pixels that differ between two
// planar images of the same layout.
func diffRect(a, b *enc.Image) (image.Rectangle, bool) {
	x0, y0, x1, y1 := b.W, b.H, -1, -1
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			k := y*b.W + x
			for c := range b.Chans {
				if a.Chans[c][k] != b.Chans[c][k] {
					x0, y0 = min(x0, x), min(y0, y)
					x1, y1 = max(x1, x), max(y1, y)
					break
				}
			}
		}
	}
	if x1 < 0 {
		return image.Rectangle{}, true
	}
	return image.Rect(x0, y0, x1+1, y1+1), false
}

func cropImage(im *enc.Image, r image.Rectangle) *enc.Image {
	o := *im
	o.W, o.H = r.Dx(), r.Dy()
	o.Chans = make([][]int32, len(im.Chans))
	for c, src := range im.Chans {
		dst := make([]int32, o.W*o.H)
		for y := 0; y < o.H; y++ {
			copy(dst[y*o.W:(y+1)*o.W], src[(r.Min.Y+y)*im.W+r.Min.X:])
		}
		o.Chans[c] = dst
	}
	return &o
}

// DecodeAnimatedInput decodes an animated GIF or APNG into full-canvas
// frames. It returns nil (and no error) for other or single-frame files.
func DecodeAnimatedInput(data []byte) (*Animation, error) {
	switch {
	case bytes.HasPrefix(data, []byte("GIF8")):
		return decodeGIF(data)
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		frames, delays, loops, ok, err := pngx.DecodeAPNG(data)
		if err != nil || !ok || len(frames) < 2 {
			return nil, err
		}
		a := &Animation{NumLoops: loops}
		for i, f := range frames {
			a.Frames = append(a.Frames, Frame{Image: f, Duration: delays[i]})
		}
		return a, nil
	}
	return nil, nil
}

func decodeGIF(data []byte) (*Animation, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if len(g.Image) < 2 {
		return nil, nil
	}
	W, H := g.Config.Width, g.Config.Height
	if W <= 0 || H <= 0 {
		b := g.Image[0].Bounds()
		W, H = b.Max.X, b.Max.Y
	}
	a := &Animation{}
	switch {
	case g.LoopCount == 0:
		a.NumLoops = 0
	case g.LoopCount < 0:
		a.NumLoops = 1
	default:
		a.NumLoops = g.LoopCount + 1
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, W, H))
	for i, fr := range g.Image {
		var disposal byte
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		var prev []byte
		if disposal == gif.DisposalPrevious {
			prev = append([]byte(nil), canvas.Pix...)
		}
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		snap := image.NewNRGBA(canvas.Rect)
		copy(snap.Pix, canvas.Pix)
		delay := 0.0
		if i < len(g.Delay) {
			delay = float64(g.Delay[i]) / 100
		}
		a.Frames = append(a.Frames, Frame{Image: snap, Duration: delay})
		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, fr.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			copy(canvas.Pix, prev)
		}
	}
	return a, nil
}
