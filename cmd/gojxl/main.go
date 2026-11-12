// Command gojxl decodes and inspects JPEG XL images.
package main

import (
	"bytes"
	"compress/flate"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"gojxl"
	"gojxl/internal/pngx"
)

func usage() {
	fmt.Fprintf(os.Stderr, `gojxl - pure Go JPEG XL tool

Usage:
  gojxl decode [-bench N] input.jxl output.png
  gojxl encode [-e effort] [-d distance] input.png output.jxl  (d=0: lossless)
  gojxl info input.jxl
  gojxl compare original distorted   (SSIMULACRA 2 and PSNR; PNG/JPEG/GIF/JXL)
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "decode", "d":
		cmdDecode(os.Args[2:])
	case "info", "i":
		cmdInfo(os.Args[2:])
	case "compare", "c":
		cmdCompare(os.Args[2:])
	case "encode", "e":
		cmdEncode(os.Args[2:])
	default:
		usage()
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gojxl:", err)
	os.Exit(1)
}

func cmdDecode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	bench := fs.Int("bench", 0, "decode N times and report speed")
	depth := fs.Int("depth", 0, "output bit depth (16 forces 16-bit PNG)")
	cpuprof := fs.String("cpuprofile", "", "write CPU profile to file")
	memprof := fs.String("memprofile", "", "write allocation profile to file")
	quality := fs.Int("q", 90, "JPEG quality when the output is .jpg and no original JPEG is stored")
	fs.Parse(args)
	if fs.NArg() < 1 {
		usage()
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	if *cpuprof != "" {
		pf, err := os.Create(*cpuprof)
		if err != nil {
			fatal(err)
		}
		pprof.StartCPUProfile(pf)
		defer pprof.StopCPUProfile()
	}
	if fs.NArg() >= 2 {
		ext := strings.ToLower(filepath.Ext(fs.Arg(1)))
		if ext == ".jpg" || ext == ".jpeg" {
			// JPEG output: restore the original JPEG when the file is a
			// recompressed JPEG, otherwise encode the pixels.
			t0 := time.Now()
			jpg, err := gojxl.ReconstructJPEG(data)
			if err == nil {
				if err := os.WriteFile(fs.Arg(1), jpg, 0o644); err != nil {
					fatal(err)
				}
				fmt.Fprintf(os.Stderr, "JPEG reconstructed: %d bytes, %v\n", len(jpg), time.Since(t0))
				return
			}
			if !errors.Is(err, gojxl.ErrNoJPEG) {
				fatal(err)
			}
			img, err := gojxl.DecodeBytes(data)
			if err != nil {
				fatal(err)
			}
			f, err := os.Create(fs.Arg(1))
			if err != nil {
				fatal(err)
			}
			if err := jpeg.Encode(f, img, &jpeg.Options{Quality: *quality}); err != nil {
				fatal(err)
			}
			f.Close()
			return
		}
	}
	t0 := time.Now()
	opt := &gojxl.Options{Force16: *depth == 16}
	img, err := gojxl.DecodeBytesOptions(data, opt)
	if err != nil {
		fatal(err)
	}
	el := time.Since(t0)
	if *bench > 0 {
		t0 = time.Now()
		for i := 0; i < *bench; i++ {
			if _, err := gojxl.DecodeBytesOptions(data, opt); err != nil {
				fatal(err)
			}
		}
		el = time.Since(t0) / time.Duration(*bench)
	}
	if *memprof != "" {
		mf, err := os.Create(*memprof)
		if err != nil {
			fatal(err)
		}
		pprof.Lookup("allocs").WriteTo(mf, 0)
		mf.Close()
	}
	b := img.Bounds()
	mp := float64(b.Dx()*b.Dy()) / 1e6
	fmt.Fprintf(os.Stderr, "%dx%d, %.2f MP, %v, %.2f MP/s\n", b.Dx(), b.Dy(), mp, el, mp/el.Seconds())
	if fs.NArg() >= 2 {
		f, err := os.Create(fs.Arg(1))
		if err != nil {
			fatal(err)
		}
		var buf bytes.Buffer
		info, ierr := gojxl.Stat(data)
		var anim *gojxl.Animation
		if ierr == nil && info.Animated {
			if anim, err = gojxl.DecodeAll(data); err != nil {
				fatal(err)
			}
		}
		if anim != nil && len(anim.Frames) > 1 {
			// animation: APNG with all displayed frames
			frames := make([]image.Image, len(anim.Frames))
			delays := make([]float64, len(anim.Frames))
			for i, fr := range anim.Frames {
				frames[i], delays[i] = fr.Image, fr.Duration
			}
			if err := pngx.EncodeAPNG(&buf, frames, delays, anim.NumLoops, flate.BestSpeed); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "APNG: %d frames\n", len(frames))
		} else if err := pngx.Encode(&buf, img, flate.BestSpeed); err != nil {
			fatal(err)
		}
		out := buf.Bytes()
		if ierr == nil {
			if info.OutputICC != nil {
				out = pngx.InsertICC(out, info.OutputICC)
			}
			if info.CICP != nil {
				out = pngx.InsertCICP(out, info.CICP[0], info.CICP[1])
			}
		}
		if _, err := f.Write(out); err != nil {
			fatal(err)
		}
		f.Close()
	}
}

func cmdInfo(args []string) {
	if len(args) < 1 {
		usage()
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fatal(err)
	}
	info, err := gojxl.Stat(data)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("size: %dx%d\n", info.Width, info.Height)
	fmt.Printf("bits per sample: %d (float=%v)\n", info.BitsPerSample, info.FloatSamples)
	fmt.Printf("xyb: %v, grey: %v, alpha: %v, animated: %v\n", info.XYB, info.Grey, info.HasAlpha, info.Animated)
	fmt.Printf("orientation: %d, extra channels: %d, icc: %d bytes\n", info.Orientation, info.NumExtraChannels, len(info.ICC))
}

func cmdEncode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	effort := fs.Int("e", 0, "effort: lossless 1-6 (default 4; 1-3: fixed context model, 4+: learned MA tree), lossy 1-7 (default 7; lower searches fewer transform shapes)")
	dist := fs.Float64("d", 0, "distance for lossy encoding (0 = lossless)")
	cpuprof := fs.String("cpuprofile", "", "write CPU profile to file")
	memprof := fs.String("memprofile", "", "write allocation profile to file")
	pixels := fs.Bool("pixels", false, "for JPEG input: encode decoded pixels instead of recompressing")
	fs.Parse(args)
	if *cpuprof != "" {
		pf, err := os.Create(*cpuprof)
		if err != nil {
			fatal(err)
		}
		pprof.StartCPUProfile(pf)
		defer pprof.StopCPUProfile()
	}
	if *memprof != "" {
		defer func() {
			mf, err := os.Create(*memprof)
			if err != nil {
				fatal(err)
			}
			runtime.GC() // the profile covers allocations up to the last GC
			pprof.Lookup("allocs").WriteTo(mf, 0)
			mf.Close()
		}()
	}
	if fs.NArg() < 2 {
		usage()
	}
	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	if *dist == 0 && !*pixels && len(raw) > 2 && raw[0] == 0xff && raw[1] == 0xd8 {
		// JPEG input: lossless recompression of the DCT coefficients
		t0 := time.Now()
		data, err := gojxl.TranscodeJPEG(raw)
		if err == nil {
			el := time.Since(t0)
			if err := os.WriteFile(fs.Arg(1), data, 0o644); err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "JPEG recompressed: %d -> %d bytes (%.1f%%), %v\n", len(raw), len(data), 100*float64(len(data))/float64(len(raw)), el)
			return
		}
		fmt.Fprintf(os.Stderr, "JPEG recompression not possible (%v); encoding pixels\n", err)
	}
	// animated GIF/APNG input: one JPEG XL animation
	if anim, err := gojxl.DecodeAnimatedInput(raw); err != nil {
		fatal(err)
	} else if anim != nil {
		t0 := time.Now()
		data, err := gojxl.EncodeAnimation(anim, &gojxl.EncodeOptions{Effort: *effort, Distance: float32(*dist)})
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(fs.Arg(1), data, 0o644); err != nil {
			fatal(err)
		}
		b := anim.Frames[0].Image.Bounds()
		fmt.Fprintf(os.Stderr, "%dx%d, %d frames, %d bytes, %v\n", b.Dx(), b.Dy(), len(anim.Frames), len(data), time.Since(t0))
		return
	}
	img, err := decodeInput(raw)
	if err != nil {
		fatal(err)
	}
	b := img.Bounds()
	t0 := time.Now()
	data, err := gojxl.EncodeBytes(img, &gojxl.EncodeOptions{Effort: *effort, Distance: float32(*dist)})
	if err != nil {
		fatal(err)
	}
	el := time.Since(t0)
	if err := os.WriteFile(fs.Arg(1), data, 0o644); err != nil {
		fatal(err)
	}
	mp := float64(b.Dx()*b.Dy()) / 1e6
	fmt.Fprintf(os.Stderr, "%dx%d, %d bytes (%.3f bpp), %v, %.2f MP/s\n", b.Dx(), b.Dy(), len(data), float64(len(data)*8)/float64(b.Dx()*b.Dy()), el, mp/el.Seconds())
}

// loadImage decodes a JPEG XL file or any registered image format.
func loadImage(path string) image.Image {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	if img, err := gojxl.DecodeBytes(data); err == nil {
		return img
	}
	img, err := decodeInput(data)
	if err != nil {
		fatal(fmt.Errorf("%s: %w", path, err))
	}
	return img
}

// decodeInput decodes PNG (with the fast pngx decoder) or any other
// registered image format.
func decodeInput(data []byte) (image.Image, error) {
	if bytes.HasPrefix(data, []byte("\x89PNG")) {
		return pngx.Decode(data)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func cmdCompare(args []string) {
	if len(args) != 2 {
		usage()
	}
	a, b := loadImage(args[0]), loadImage(args[1])
	s, err := gojxl.SSIMULACRA2(a, b)
	if err != nil {
		fatal(err)
	}
	// PSNR over RGB (8-bit scale)
	var se float64
	r := a.Bounds()
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			r1, g1, b1, _ := a.At(r.Min.X+x, r.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(b.Bounds().Min.X+x, b.Bounds().Min.Y+y).RGBA()
			for _, d := range [3]float64{float64(r1) - float64(r2), float64(g1) - float64(g2), float64(b1) - float64(b2)} {
				d /= 257
				se += d * d
			}
		}
	}
	psnr := math.Inf(1)
	if mse := se / float64(3*r.Dx()*r.Dy()); mse > 0 {
		psnr = 10 * math.Log10(255*255/mse)
	}
	fmt.Printf("SSIMULACRA2 %.4f  PSNR %.2f dB\n", s, psnr)
}
