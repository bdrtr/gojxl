// Command gojxl decodes and inspects JPEG XL images.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"image/png"
	"os"
	"time"

	"gojxl"
)

func usage() {
	fmt.Fprintf(os.Stderr, `gojxl - pure Go JPEG XL tool

Usage:
  gojxl decode [-bench N] input.jxl output.png
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
	fs.Parse(args)
	if fs.NArg() < 1 {
		usage()
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	t0 := time.Now()
	img, err := gojxl.DecodeBytes(data)
	if err != nil {
		fatal(err)
	}
	el := time.Since(t0)
	if *bench > 0 {
		t0 = time.Now()
		for i := 0; i < *bench; i++ {
			if _, err := gojxl.DecodeBytes(data); err != nil {
				fatal(err)
			}
		}
		el = time.Since(t0) / time.Duration(*bench)
	}
	b := img.Bounds()
	mp := float64(b.Dx()*b.Dy()) / 1e6
	fmt.Fprintf(os.Stderr, "%dx%d, %.2f MP, %v, %.2f MP/s\n", b.Dx(), b.Dy(), mp, el, mp/el.Seconds())
	if fs.NArg() >= 2 {
		f, err := os.Create(fs.Arg(1))
		if err != nil {
			fatal(err)
		}
		w := bufio.NewWriter(f)
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(w, img); err != nil {
			fatal(err)
		}
		w.Flush()
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
