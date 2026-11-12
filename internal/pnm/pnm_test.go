package pnm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestDecodeFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		w, h int
		ch   int
		max  int
		want []uint16
	}{
		{"P5", append([]byte("P5\n# comment\n3 1\n255\n"), 1, 2, 3), 3, 1, 1, 255, []uint16{1, 2, 3}},
		{"P5 16", append([]byte("P5 2 1 1023\n"), 0x03, 0xff, 0x01, 0x00), 2, 1, 1, 1023, []uint16{1023, 256}},
		{"P6", append([]byte("P6\n1 1\n255\n"), 10, 20, 30), 1, 1, 3, 255, []uint16{10, 20, 30}},
		{"P2", []byte("P2\n2 1\n15\n7 15\n"), 2, 1, 1, 15, []uint16{7, 15}},
		{"P3", []byte("P3 1 1 255 1 2 3"), 1, 1, 3, 255, []uint16{1, 2, 3}},
		{"P7", append([]byte("P7\nWIDTH 1\nHEIGHT 1\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n"), 1, 2, 3, 4), 1, 1, 4, 255, []uint16{1, 2, 3, 4}},
	}
	for _, c := range cases {
		im, err := Decode(c.data)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if im.W != c.w || im.H != c.h || im.Channels != c.ch || im.MaxVal != c.max {
			t.Fatalf("%s: got %dx%d ch %d max %d", c.name, im.W, im.H, im.Channels, im.MaxVal)
		}
		for i, v := range c.want {
			if im.Samples[i] != v {
				t.Fatalf("%s: sample %d = %d, want %d", c.name, i, im.Samples[i], v)
			}
		}
	}
	if im, _ := Decode([]byte("P5 1 1 1023\n\x03\xff")); im.BitDepth() != 10 {
		t.Errorf("bit depth %d", im.BitDepth())
	}
}

func TestPFM(t *testing.T) {
	var b bytes.Buffer
	if err := PFMHeader(&b, 2, 2, 1); err != nil {
		t.Fatal(err)
	}
	// rows bottom-up: the file holds row 1 then row 0
	for _, v := range []float32{3, 4, 1, 2} {
		binary.Write(&b, binary.LittleEndian, math.Float32bits(v))
	}
	im, err := Decode(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !im.IsFloat || im.Channels != 1 {
		t.Fatal("not a grey float image")
	}
	for i, v := range []float32{1, 2, 3, 4} {
		if im.Float[i] != v {
			t.Fatalf("sample %d = %v", i, im.Float[i])
		}
	}
	// big endian (positive scale)
	be := append([]byte("PF\n1 1\n1.0\n"), 0x3f, 0x80, 0, 0, 0x40, 0, 0, 0, 0xbf, 0x80, 0, 0)
	if im, err := Decode(be); err != nil || im.Float[0] != 1 || im.Float[1] != 2 || im.Float[2] != -1 {
		t.Fatalf("big endian PFM: %v %v", err, im)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, d := range []string{
		"P6\n2 2\n255\n\x01\x02",   // truncated
		"P5\n1 1\n100\n\x80",       // above maxval
		"P5\n0 1\n255\n",           // empty
		"P5\n1 1\n70000\n\x00\x00", // maxval
		"P7\nWIDTH 1\nHEIGHT 1\nDEPTH 5\nMAXVAL 255\nENDHDR\n\x00\x00\x00\x00\x00",
		"P7\nWIDTH 1\nHEIGHT 1\nDEPTH 1\nMAXVAL 255\nTUPLTYPE CMYK\nENDHDR\n\x00",
		"P5\n99999999999 99999999999\n255\n", // area overflow
		"Pf\n1 1\n0\n\x00\x00\x00\x00",       // scale 0
	} {
		if _, err := Decode([]byte(d)); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	if _, err := Decode([]byte("P5\n1 1\n255\n")); !errors.Is(err, ErrFormat) {
		t.Errorf("truncated: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte("P6\n2 1\n255\n\x01\x02\x03\x04\x05\x06"))
	f.Add([]byte("P7\nWIDTH 1\nHEIGHT 1\nDEPTH 2\nMAXVAL 65535\nTUPLTYPE GRAYSCALE_ALPHA\nENDHDR\n\x00\x01\x00\x02"))
	f.Add([]byte("PF\n1 1\n-1.0\n\x00\x00\x80\x3f\x00\x00\x80\x3f\x00\x00\x80\x3f"))
	f.Add([]byte("P3 1 1 255 1 2 3"))
	MaxPixels = 1 << 20
	f.Fuzz(func(t *testing.T, data []byte) {
		im, err := Decode(data)
		if err != nil {
			return
		}
		n := im.W * im.H * im.Channels
		if (im.IsFloat && len(im.Float) != n) || (!im.IsFloat && len(im.Samples) != n) {
			t.Fatalf("%d samples for %dx%dx%d", len(im.Samples), im.W, im.H, im.Channels)
		}
	})
}
