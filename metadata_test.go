package gojxl

import (
	"bytes"
	"compress/zlib"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/bdrtr/gojxl/internal/exif"
	ijpeg "github.com/bdrtr/gojxl/internal/jpeg"
	"github.com/bdrtr/gojxl/internal/pngx"
)

// testExif is big-endian TIFF data with orientation 6 and a Make tag.
var testExif = []byte("MM\x00*\x00\x00\x00\x08\x00\x02\x01\x0f\x00\x02\x00\x00\x00\x04ABC\x00\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")

// testXMP is long enough to be stored Brotli-compressed.
var testXMP = []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
	strings.Repeat(`<rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>gojxl</dc:title></rdf:Description>`, 4) +
	`</rdf:RDF></x:xmpmeta>`)

func checkMeta(t *testing.T, what string, data []byte, orientation int) *Info {
	t.Helper()
	info, err := Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Orientation != orientation || !bytes.Equal(info.Exif, testExif) || !bytes.Equal(info.XMP, testXMP) {
		t.Errorf("%s: orientation %d, Exif %d bytes, XMP %d bytes", what, info.Orientation, len(info.Exif), len(info.XMP))
	}
	return info
}

func TestJPEGMetadata(t *testing.T) {
	src := colourTestImage() // 96x64
	var b bytes.Buffer
	if err := jpeg.Encode(&b, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	file := ijpeg.InsertMetadata(b.Bytes(), nil, testExif, testXMP)

	// recompressed: orientation from Exif, metadata kept, file restorable
	data, err := ConvertToJXL(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, "recompressed", data, 6)
	if jpg, err := ReconstructJPEG(data); err != nil || !bytes.Equal(jpg, file) {
		t.Errorf("JPEG not restored (%v)", err)
	}
	img, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if r := img.Bounds(); r.Dx() != 64 || r.Dy() != 96 {
		t.Errorf("recompressed: decoded %v, want rotated 64x96", r)
	}

	// pixels: the same, and back to PNG and JPEG with oriented pixels
	data, err = ConvertToJXL(file, &EncodeOptions{JPEGPixels: true, Distance: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, "pixels", data, 6)
	var out bytes.Buffer
	if err := ConvertToPNG(&out, data, nil); err != nil {
		t.Fatal(err)
	}
	a := pngx.ReadAncillary(out.Bytes())
	if exif.Orientation(a.Exif) != 1 || len(a.Exif) != len(testExif) || !bytes.Equal(a.XMP, testXMP) {
		t.Errorf("PNG: Exif orientation %d, XMP %d bytes", exif.Orientation(a.Exif), len(a.XMP))
	}
	out.Reset()
	if err := ConvertToJPEG(&out, data, 90, nil); err != nil {
		t.Fatal(err)
	}
	_, ex, xmp := ijpeg.ReadMetadata(out.Bytes())
	if exif.Orientation(ex) != 1 || !bytes.Equal(xmp, testXMP) {
		t.Errorf("JPEG: Exif orientation %d, XMP %d bytes", exif.Orientation(ex), len(xmp))
	}
}

func TestPNGMetadata(t *testing.T) {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(testXMP)
	zw.Close()
	// compressed iTXt, language "en"
	itxt := append([]byte("XML:com.adobe.xmp\x00\x01\x00en\x00\x00"), z.Bytes()...)
	file := pngWith(t, colourTestImage(), chunk("eXIf", testExif), chunk("iTXt", itxt))
	for _, dist := range []float32{0, 1} {
		data, err := ConvertToJXL(file, &EncodeOptions{Distance: dist})
		if err != nil {
			t.Fatal(err)
		}
		checkMeta(t, "PNG", data, 6)
	}
	// options override the file's metadata
	data, err := EncodeBytes(colourTestImage(), &EncodeOptions{Exif: testExif, XMP: testXMP, Effort: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, "options", data, 6)
}
