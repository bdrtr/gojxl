// Command gojxl decodes and inspects JPEG XL images.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"gojxl"
	"gojxl/internal/enc"
	"gojxl/internal/pngx"
)

func usage() {
	fmt.Fprintf(os.Stderr, `gojxl - pure Go JPEG XL tool

Usage:
  gojxl decode [-depth 16] input.jxl output.png|.jpg
  gojxl decode [-format png|jpg] -o outdir input.jxl...
  gojxl encode [-e effort] [-d distance | -q quality] input output.jxl
  gojxl encode [-e effort] [-d distance | -q quality] -o outdir input...
      input: PNG/APNG, GIF, JPEG (recompressed losslessly at d=0); the
      colour profile and Exif/XMP metadata of the input are kept;
      d=0 (q=100) is lossless; "-" reads stdin / writes stdout
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
	lo.NoSmall = lo.NoSmall || os.Getenv("GOJXL_NOSMALL") != ""
	if v := os.Getenv("GOJXL_SMALLMASK"); v != "" {
		lo.SmallMask, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("GOJXL_SMALLMUL"); v != "" {
		for i, f := range strings.Split(v, ",") {
			if i < 4 {
				lo.SmallMul[i], _ = strconv.ParseFloat(f, 64)
			}
		}
	}
	if v := os.Getenv("GOJXL_SPLITMUL"); v != "" {
		lo.SplitMul, _ = strconv.ParseFloat(v, 64)
	}
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

// readFile reads a file, or standard input for "-".
func readFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// writeFile writes a file, or standard output for "-".
func writeFile(path string, data []byte) error {
	if path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// batch runs convert(input, output) for every input, writing outputs into
// dir with the extension ext, jobs at a time; it reports each result and
// exits with status 1 if any failed.
func batch(inputs []string, dir, ext string, jobs int, convert func(in, out string) (string, error)) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(err)
	}
	t0 := time.Now()
	var mu sync.Mutex
	failed := 0
	sem := make(chan struct{}, max(jobs, 1))
	var wg sync.WaitGroup
	for _, in := range inputs {
		out := filepath.Join(dir, strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))+ext)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			msg, err := convert(in, out)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "%s: error: %v\n", in, err)
				return
			}
			fmt.Fprintf(os.Stderr, "%s -> %s: %s\n", in, out, msg)
		}()
	}
	wg.Wait()
	el := time.Since(t0)
	fmt.Fprintf(os.Stderr, "%d files in %v (%.1f files/s), %d failed\n", len(inputs), el.Round(time.Millisecond), float64(len(inputs))/el.Seconds(), failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// defaultJobs is the number of files converted at once in batch mode (each
// conversion is parallel too).
func defaultJobs() int { return min(4, runtime.NumCPU()) }

func cmdDecode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	bench := fs.Int("bench", 0, "decode N times and report speed")
	depth := fs.Int("depth", 0, "output bit depth (16 forces 16-bit PNG)")
	cpuprof := fs.String("cpuprofile", "", "write CPU profile to file")
	memprof := fs.String("memprofile", "", "write allocation profile to file")
	quality := fs.Int("q", 90, "JPEG quality when the output is JPEG and no original JPEG is stored")
	outDir := fs.String("o", "", "batch mode: decode every input into this directory")
	format := fs.String("format", "png", "output format when it does not follow from the name: png or jpg")
	jobs := fs.Int("j", defaultJobs(), "batch mode: files decoded at once")
	fs.Parse(args)
	if fs.NArg() < 1 {
		usage()
	}
	if *format != "png" && *format != "jpg" {
		fatal(fmt.Errorf("unknown format %q", *format))
	}
	if *cpuprof != "" {
		pf, err := os.Create(*cpuprof)
		if err != nil {
			fatal(err)
		}
		pprof.StartCPUProfile(pf)
		defer pprof.StopCPUProfile()
	}
	opt := &gojxl.Options{Force16: *depth == 16}
	convert := func(in, out string) (string, error) {
		f := *format
		if ext := strings.ToLower(filepath.Ext(out)); ext == ".jpg" || ext == ".jpeg" {
			f = "jpg"
		} else if ext == ".png" || ext == ".apng" {
			f = "png"
		}
		return decodeOne(in, out, f, opt, *quality)
	}
	if *outDir != "" {
		batch(fs.Args(), *outDir, "."+*format, *jobs, convert)
		return
	}
	if fs.NArg() >= 2 && *bench == 0 && *memprof == "" {
		msg, err := convert(fs.Arg(0), fs.Arg(1))
		if err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	// decode to an image (speed measurement, profiles), then write
	data, err := readFile(fs.Arg(0))
	if err != nil {
		fatal(err)
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
		if err := writeFile(fs.Arg(1), buf.Bytes()); err != nil {
			fatal(err)
		}
	}
}

// decodeOne converts the JPEG XL file in to out (format png or jpg) and
// returns a summary.
func decodeOne(in, out, format string, opt *gojxl.Options, quality int) (string, error) {
	data, err := readFile(in)
	if err != nil {
		return "", err
	}
	t0 := time.Now()
	if format == "jpg" {
		// the original JPEG for a recompressed JPEG, else the pixels
		var buf bytes.Buffer
		if err := gojxl.ConvertToJPEG(&buf, data, quality); err != nil {
			return "", err
		}
		if err := writeFile(out, buf.Bytes()); err != nil {
			return "", err
		}
		what := "JPEG encoded"
		if _, err := gojxl.ReconstructJPEG(data); err == nil {
			what = "JPEG reconstructed"
		}
		return fmt.Sprintf("%s: %d bytes, %v", what, buf.Len(), time.Since(t0)), nil
	}
	// PNG: rows go straight into the file (no intermediate image)
	var w io.Writer = os.Stdout
	var f *os.File
	if out != "-" {
		if f, err = os.Create(out); err != nil {
			return "", err
		}
		w = f
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	err = gojxl.ConvertToPNG(bw, data, opt)
	if err == nil {
		err = bw.Flush()
	}
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out)
		}
	}
	if err != nil {
		return "", err
	}
	el := time.Since(t0)
	info, err := gojxl.Stat(data)
	if err != nil {
		return "", err
	}
	mp := float64(info.Width*info.Height) / 1e6
	return fmt.Sprintf("%dx%d, %.2f MP, %v (decode and PNG), %.2f MP/s", info.Width, info.Height, mp, el, mp/el.Seconds()), nil
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
	quality := fs.Float64("q", -1, "quality 0-100 instead of -d, as in cjxl (100 = lossless, 90 = d 1)")
	cpuprof := fs.String("cpuprofile", "", "write CPU profile to file")
	memprof := fs.String("memprofile", "", "write allocation profile to file")
	pixels := fs.Bool("pixels", false, "for JPEG input: encode decoded pixels instead of recompressing")
	outDir := fs.String("o", "", "batch mode: encode every input into this directory")
	jobs := fs.Int("j", defaultJobs(), "batch mode: files encoded at once")
	fs.Parse(args)
	if *quality >= 0 {
		*dist = float64(gojxl.DistanceFromQuality(float32(*quality)))
	}
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
	convert := func(in, out string) (string, error) {
		return encodeOne(in, out, &gojxl.EncodeOptions{Effort: *effort, Distance: float32(*dist), JPEGPixels: *pixels})
	}
	if *outDir != "" {
		if fs.NArg() < 1 {
			usage()
		}
		batch(fs.Args(), *outDir, ".jxl", *jobs, convert)
		return
	}
	if fs.NArg() < 2 {
		usage()
	}
	msg, err := convert(fs.Arg(0), fs.Arg(1))
	if err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, msg)
}

// encodeOne converts the image file in to JPEG XL in out and returns a
// summary.
func encodeOne(in, out string, opt *gojxl.EncodeOptions) (string, error) {
	raw, err := readFile(in)
	if err != nil {
		return "", err
	}
	note := ""
	if opt.Distance == 0 && !opt.JPEGPixels && len(raw) > 2 && raw[0] == 0xff && raw[1] == 0xd8 {
		// JPEG input: lossless recompression of the DCT coefficients
		t0 := time.Now()
		data, err := gojxl.TranscodeJPEG(raw)
		if err == nil {
			el := time.Since(t0)
			if err := writeFile(out, data); err != nil {
				return "", err
			}
			return fmt.Sprintf("JPEG recompressed: %d -> %d bytes (%.1f%%), %v", len(raw), len(data), 100*float64(len(data))/float64(len(raw)), el), nil
		}
		note = fmt.Sprintf(" (JPEG recompression not possible: %v; pixels encoded)", err)
		opt.JPEGPixels = true
	}
	// any input: colour profile and metadata kept, animations as animations
	t0 := time.Now()
	data, err := gojxl.ConvertToJXL(raw, opt)
	if errors.Is(err, gojxl.ErrUnsupportedColour) {
		return "", fmt.Errorf("%w; lossless encoding (-d 0) keeps any profile", err)
	}
	if err != nil {
		return "", err
	}
	el := time.Since(t0)
	if err := writeFile(out, data); err != nil {
		return "", err
	}
	info, err := gojxl.Stat(data)
	if err != nil {
		return "", err
	}
	w, h := info.Width, info.Height
	mp := float64(w*h) / 1e6
	kind := ""
	if info.Animated {
		kind = ", animation"
	}
	return fmt.Sprintf("%dx%d%s, %d bytes (%.3f bpp), %v, %.2f MP/s%s", w, h, kind, len(data), float64(len(data)*8)/float64(w*h), el, mp/el.Seconds(), note), nil
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
