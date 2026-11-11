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
	"os"
	"path/filepath"
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
	effort := fs.Int("e", 4, "lossless effort 1-6 (1-3: fixed context model, 4+: learned MA tree)")
	dist := fs.Float64("d", 0, "distance for lossy encoding (0 = lossless)")
	cpuprof := fs.String("cpuprofile", "", "write CPU profile to file")
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
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		fatal(err)
	}
	t0 := time.Now()
	data, err := gojxl.EncodeBytes(img, &gojxl.EncodeOptions{Effort: *effort, Distance: float32(*dist)})
	if err != nil {
		fatal(err)
	}
	el := time.Since(t0)
	if err := os.WriteFile(fs.Arg(1), data, 0o644); err != nil {
		fatal(err)
	}
	b := img.Bounds()
	mp := float64(b.Dx()*b.Dy()) / 1e6
	fmt.Fprintf(os.Stderr, "%dx%d, %d bytes (%.3f bpp), %v, %.2f MP/s\n", b.Dx(), b.Dy(), len(data), float64(len(data)*8)/float64(b.Dx()*b.Dy()), el, mp/el.Seconds())
}
