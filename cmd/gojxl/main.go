// Command gojxl decodes and inspects JPEG XL images.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"gojxl"
	"gojxl/internal/enc"
	"gojxl/internal/pngx"
)

func usage() {
	fmt.Fprintf(os.Stderr, `gojxl - pure Go JPEG XL tool

Usage:
  gojxl decode [-bench N] input.jxl output.png
  gojxl encode [-e effort] [-d distance] input output.jxl  (d=0: lossless)
      input: PNG/APNG, GIF, JPEG (recompressed losslessly at d=0); the
      colour profile of the input is kept
  gojxl info input.jxl
  gojxl compare original distorted   (SSIMULACRA 2 and PSNR; PNG/JPEG/GIF/JXL)
`)
	os.Exit(2)
}

// tuneFromEnv lets GOJXL_* variables override lossy encoder settings
// (for tuning experiments).
func tuneFromEnv(lo *enc.LossyOptions) {
	if v := os.Getenv("GOJXL_EPF"); v != "" {
		lo.EPFIters, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("GOJXL_SHARP"); v != "" {
		lo.Sharpness, _ = strconv.Atoi(v)
	}
	lo.NoGaborish = lo.NoGaborish || os.Getenv("GOJXL_NOGAB") != ""
	lo.NoCfL = lo.NoCfL || os.Getenv("GOJXL_NOCFL") != ""
	lo.LibAQ = lo.LibAQ && os.Getenv("GOJXL_NOLAQ") == ""
	lo.OnlyDCT8 = lo.OnlyDCT8 || os.Getenv("GOJXL_DCT8") != ""
	if v := os.Getenv("GOJXL_MU"); v != "" {
		lo.RDMu, _ = strconv.ParseFloat(v, 64)
	}
	if v := os.Getenv("GOJXL_AQ"); v != "" {
		lo.AQ, _ = strconv.ParseFloat(v, 64)
	}
}

func main() {
	enc.TuneLossy = tuneFromEnv
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
			var buf bytes.Buffer
			if err := gojxl.ConvertToJPEG(&buf, data, *quality); err != nil {
				fatal(err)
			}
			if err := os.WriteFile(fs.Arg(1), buf.Bytes(), 0o644); err != nil {
				fatal(err)
			}
			what := "JPEG encoded"
			if _, err := gojxl.ReconstructJPEG(data); err == nil {
				what = "JPEG reconstructed"
			}
			fmt.Fprintf(os.Stderr, "%s: %d bytes, %v\n", what, buf.Len(), time.Since(t0))
			return
		}
	}
	opt := &gojxl.Options{Force16: *depth == 16}
	if fs.NArg() >= 2 && *bench == 0 && *memprof == "" {
		// decode straight into the PNG writer (no intermediate image)
		t0 := time.Now()
		f, err := os.Create(fs.Arg(1))
		if err != nil {
			fatal(err)
		}
		if err := gojxl.ConvertToPNG(f, data, opt); err != nil {
			f.Close()
			os.Remove(fs.Arg(1))
			fatal(err)
		}
		if err := f.Close(); err != nil {
			fatal(err)
		}
		el := time.Since(t0)
		if info, err := gojxl.Stat(data); err == nil {
			mp := float64(info.Width*info.Height) / 1e6
			fmt.Fprintf(os.Stderr, "%dx%d, %.2f MP, %v (decode and PNG), %.2f MP/s\n", info.Width, info.Height, mp, el, mp/el.Seconds())
		}
		return
	}
	t0 := time.Now()
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
		runtime.GC() // the profile covers allocations up to the last GC
		pprof.Lookup("allocs").WriteTo(mf, 0)
		mf.Close()
	}
	b := img.Bounds()
	mp := float64(b.Dx()*b.Dy()) / 1e6
	fmt.Fprintf(os.Stderr, "%dx%d, %.2f MP, %v, %.2f MP/s\n", b.Dx(), b.Dy(), mp, el, mp/el.Seconds())
	if fs.NArg() >= 2 {
		var buf bytes.Buffer
		if err := gojxl.ConvertToPNG(&buf, data, opt); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(fs.Arg(1), buf.Bytes(), 0o644); err != nil {
			fatal(err)
		}
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
	fmt.Printf("orientation: %d, extra channels: %d\n", info.Orientation, info.NumExtraChannels)
	if info.ICC != nil {
		fmt.Printf("colour: ICC profile, %d bytes\n", len(info.ICC))
	} else {
		fmt.Printf("colour: %s\n", info.Colour)
	}
	if info.Exif != nil || info.XMP != nil {
		fmt.Printf("metadata: Exif %d bytes, XMP %d bytes\n", len(info.Exif), len(info.XMP))
	}
}

func cmdEncode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	effort := fs.Int("e", 0, "effort: lossless 1-6 (default 4; 1-3: fixed context model, 4+: learned MA tree), lossy 1-7 (default 7; lower searches fewer transform shapes, and 1-4 skip adaptive quantisation and Gaborish)")
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
	opt := &gojxl.EncodeOptions{Effort: *effort, Distance: float32(*dist), JPEGPixels: *pixels}
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
		opt.JPEGPixels = true
	}
	// any input: colour profile kept, animations as animations
	t0 := time.Now()
	data, err := gojxl.ConvertToJXL(raw, opt)
	if errors.Is(err, gojxl.ErrUnsupportedColour) {
		fatal(fmt.Errorf("%w; lossless encoding (-d 0) keeps any profile", err))
	}
	if err != nil {
		fatal(err)
	}
	el := time.Since(t0)
	if err := os.WriteFile(fs.Arg(1), data, 0o644); err != nil {
		fatal(err)
	}
	info, err := gojxl.Stat(data)
	if err != nil {
		fatal(err)
	}
	w, h := info.Width, info.Height
	mp := float64(w*h) / 1e6
	kind := ""
	if info.Animated {
		kind = ", animation"
	}
	fmt.Fprintf(os.Stderr, "%dx%d%s, %d bytes (%.3f bpp), %v, %.2f MP/s\n", w, h, kind, len(data), float64(len(data)*8)/float64(w*h), el, mp/el.Seconds())
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
