package gojxl

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"gojxl/internal/jxl"
	"gojxl/internal/pnm"
)

// A 10-bit PPM is stored at 10 bits and comes back exactly (as 16-bit
// PPM samples v*65535/1023, within 1: the expansion may replicate bits).
func TestPNMRoundTrip(t *testing.T) {
	const w, h = 37, 23
	var b bytes.Buffer
	fmt.Fprintf(&b, "P6\n%d %d\n1023\n", w, h)
	src := make([]uint16, 3*w*h)
	for i := range src {
		src[i] = uint16((i*37 + i/3*11) % 1024)
		binary.Write(&b, binary.BigEndian, src[i])
	}
	data, err := ConvertToJXL(b.Bytes(), &EncodeOptions{Effort: 2})
	if err != nil {
		t.Fatal(err)
	}
	d, err := jxl.NewDecoder(data)
	if err != nil {
		t.Fatal(err)
	}
	if bps := d.Header.BitDepth.BitsPerSample; bps != 10 {
		t.Fatalf("stored at %d bits", bps)
	}
	var out bytes.Buffer
	if err := ConvertToPNM(&out, data, nil); err != nil {
		t.Fatal(err)
	}
	p, err := pnm.Decode(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if p.W != w || p.H != h || p.Channels != 3 || p.MaxVal != 65535 {
		t.Fatalf("output %dx%d ch %d max %d", p.W, p.H, p.Channels, p.MaxVal)
	}
	for i, v := range src {
		if want := math.Round(float64(v) * 65535 / 1023); math.Abs(float64(p.Samples[i])-want) > 1 {
			t.Fatalf("sample %d: %d, want %v", i, p.Samples[i], want)
		}
	}
	// PFM: the same samples as floats
	out.Reset()
	if err := ConvertToPFM(&out, data, nil); err != nil {
		t.Fatal(err)
	}
	f, err := pnm.Decode(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range src {
		if d := math.Abs(float64(f.Float[i]) - float64(v)/1023); d > 1e-6 {
			t.Fatalf("PFM sample %d: %v, want %v", i, f.Float[i], float64(v)/1023)
		}
	}
}

// Grey and alpha anymaps keep their layout: PGM stays PGM, a grey PAM
// with alpha stays a grey PAM with alpha; opaque alpha is dropped.
func TestPNMLayouts(t *testing.T) {
	grey := append([]byte("P5\n4 1\n255\n"), 0, 85, 170, 255)
	ga := append([]byte("P7\nWIDTH 2\nHEIGHT 1\nDEPTH 2\nMAXVAL 255\nTUPLTYPE GRAYSCALE_ALPHA\nENDHDR\n"), 10, 128, 200, 255)
	opaque := append([]byte("P7\nWIDTH 1\nHEIGHT 1\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n"), 1, 2, 3, 255)
	for _, c := range []struct {
		name string
		in   []byte
		ch   int
		want []uint16
	}{
		{"grey", grey, 1, []uint16{0, 85, 170, 255}},
		{"grey+alpha", ga, 2, []uint16{10, 128, 200, 255}},
		{"opaque RGBA", opaque, 3, []uint16{1, 2, 3}},
	} {
		data, err := ConvertToJXL(c.in, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var out bytes.Buffer
		if err := ConvertToPNM(&out, data, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		p, err := pnm.Decode(out.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Channels != c.ch {
			t.Fatalf("%s: %d channels, want %d", c.name, p.Channels, c.ch)
		}
		for i, v := range c.want {
			if p.Samples[i] != v {
				t.Fatalf("%s: sample %d = %d, want %d", c.name, i, p.Samples[i], v)
			}
		}
	}
	if _, err := ConvertToJXL([]byte("PF\n1 1\n-1.0\n\x00\x00\x80\x3f\x00\x00\x80\x3f\x00\x00\x80\x3f"), nil); err == nil {
		t.Error("PFM input accepted")
	}
}
