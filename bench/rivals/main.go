// rivals: decode/encode timing and quality of Go JPEG XL libraries.
//
//	rivals decode lib file.jxl N       -> min wall ms, writes out_<lib>.png
//	rivals encode lib src.png q N      -> min wall ms, size, writes enc_<lib>.jxl
//	rivals lossless lib src.png N      -> min wall ms, size
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"strconv"
	"time"

	"github.com/gen2brain/jpegxl"
	g2 "github.com/gen2brain/jxl"
	kp "github.com/kpfaulkner/jxl-go/core"

	"github.com/bdrtr/gojxl"
)

func decode(lib string, data []byte) (image.Image, error) {
	switch lib {
	case "gojxl":
		return gojxl.DecodeBytes(data)
	case "gen2brain-jxl":
		return g2.Decode(bytes.NewReader(data))
	case "jpegxl-wasm":
		return jpegxl.Decode(bytes.NewReader(data))
	case "jxl-go":
		d := kp.NewJXLDecoder(bytes.NewReader(data), nil)
		im, err := d.Decode()
		if err != nil {
			return nil, err
		}
		return im.ToImage()
	}
	return nil, fmt.Errorf("unknown lib %s", lib)
}

func encode(lib string, img image.Image, q int, lossless bool) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	switch lib {
	case "gojxl":
		o := &gojxl.EncodeOptions{}
		if !lossless {
			o.Distance = gojxl.DistanceFromQuality(float32(q))
		}
		var b []byte
		b, err = gojxl.EncodeBytes(img, o)
		return b, err
	case "gen2brain-jxl":
		err = g2.Encode(&buf, img, g2.EncodeOptions{Quality: q, Lossless: lossless})
	case "jpegxl-wasm":
		err = jpegxl.Encode(&buf, img, jpegxl.Options{Quality: q, Lossless: lossless})
	default:
		return nil, fmt.Errorf("lib %s cannot encode", lib)
	}
	return buf.Bytes(), err
}

// timeIt returns the best of n runs of f, after running it untimed for at
// least half a second: short runs from a cold start otherwise catch a
// laptop CPU before it has raised its clock.
func timeIt(n int, f func() error) (time.Duration, error) {
	for t := time.Now(); time.Since(t) < 500*time.Millisecond; {
		if err := f(); err != nil {
			return 0, err
		}
	}
	best := time.Duration(1 << 62)
	for i := 0; i < n; i++ {
		t := time.Now()
		if err := f(); err != nil {
			return 0, err
		}
		best = min(best, time.Since(t))
	}
	return best, nil
}

func main() {
	mode, lib := os.Args[1], os.Args[2]
	data, err := os.ReadFile(os.Args[3])
	if err != nil {
		panic(err)
	}
	if lib == "jpegxl-wasm" {
		jpegxl.InitDecoder() // compile the WASM module outside the timing
		jpegxl.InitEncoder()
	}
	switch mode {
	case "decode":
		n, _ := strconv.Atoi(os.Args[4])
		var img image.Image
		d, err := timeIt(n, func() (e error) { img, e = decode(lib, data); return })
		if err != nil {
			fmt.Printf("%s ERROR %v\n", lib, err)
			os.Exit(1)
		}
		f, _ := os.Create("out_" + lib + ".png")
		png.Encode(f, img)
		f.Close()
		fmt.Printf("%s decode %.1f ms\n", lib, float64(d.Microseconds())/1000)
	case "encode", "lossless":
		src, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			panic(err)
		}
		q, n := 0, 0
		if mode == "encode" {
			q, _ = strconv.Atoi(os.Args[4])
			n, _ = strconv.Atoi(os.Args[5])
		} else {
			n, _ = strconv.Atoi(os.Args[4])
		}
		var out []byte
		d, err := timeIt(n, func() (e error) { out, e = encode(lib, src, q, mode == "lossless"); return })
		if err != nil {
			fmt.Printf("%s ERROR %v\n", lib, err)
			os.Exit(1)
		}
		os.WriteFile("enc_"+lib+".jxl", out, 0o644)
		fmt.Printf("%s %s %.1f ms %d bytes\n", lib, mode, float64(d.Microseconds())/1000, len(out))
	}
}
