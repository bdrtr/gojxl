package gojxl

import (
	"bytes"

	"github.com/bdrtr/gojxl/internal/brotli"
	"github.com/bdrtr/gojxl/internal/enc"
	"github.com/bdrtr/gojxl/internal/exif"
	"github.com/bdrtr/gojxl/internal/jpeg"
	"github.com/bdrtr/gojxl/internal/jxl"
	"github.com/bdrtr/gojxl/internal/pngx"
	"github.com/bdrtr/gojxl/internal/tiff"
	"github.com/bdrtr/gojxl/internal/webp"
)

// metadata is the Exif data (TIFF format) and XMP packet of an image.
type metadata struct{ exif, xmp []byte }

// readInput returns the colour description and metadata of an image file:
// PNG colour and metadata chunks, or JPEG ICC, Exif and XMP segments.
func readInput(data []byte) (colourSpec, metadata) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		a := pngx.ReadAncillary(data)
		return specFromPNG(a), metadata{a.Exif, a.XMP}
	case len(data) > 2 && data[0] == 0xff && data[1] == 0xd8:
		icc, ex, xmp := jpeg.ReadMetadata(data)
		s, _ := specFromICC(icc)
		return s, metadata{ex, xmp}
	case webp.Is(data):
		icc, ex, xmp := webp.Metadata(data)
		s, _ := specFromICC(icc)
		return s, metadata{ex, xmp}
	case tiff.Is(data):
		icc, xmp := tiff.Metadata(data)
		s, _ := specFromICC(icc)
		return s, metadata{nil, xmp}
	}
	return colourSpec{}, metadata{}
}

// optMetadata returns the metadata given in opt.
func optMetadata(opt *EncodeOptions) metadata {
	if opt == nil {
		return metadata{}
	}
	return metadata{opt.Exif, opt.XMP}
}

// apply sets the orientation of im from the Exif data.
func (m metadata) apply(im *enc.Image) {
	im.Orientation = exif.Orientation(m.exif)
}

// wrap puts a codestream into a container with Exif and XMP boxes when
// there is metadata.
func (m metadata) wrap(cs []byte) []byte {
	if m.exif == nil && m.xmp == nil {
		return cs
	}
	return enc.WrapContainer(cs, nil, m.exif, m.xmp)
}

// maxMetadata bounds decompressed Exif and XMP boxes.
const maxMetadata = 1 << 24

// boxMetadata returns the Exif (TIFF format) and XMP metadata of a
// container's boxes, and the jbrd box if wanted, decompressing brob boxes.
func boxMetadata(boxes []jxl.Box, wantJBRD bool) (jbrd []byte, m metadata, err error) {
	for _, b := range boxes {
		t, payload := b.Type, b.Data
		if t == "brob" && len(payload) >= 4 {
			t = string(payload[:4])
			limit := maxMetadata
			switch {
			case t == "jbrd" && wantJBRD:
				limit = 1 << 28
			case t == "Exif", t == "xml ":
			default:
				continue
			}
			if payload, err = brotli.Decode(payload[4:], limit); err != nil {
				return nil, metadata{}, err
			}
		}
		switch t {
		case "jbrd":
			jbrd = payload
		case "Exif":
			// after the offset field (0 in practice)
			if len(payload) >= 4 {
				m.exif = payload[4:]
			}
		case "xml ":
			m.xmp = payload
		}
	}
	return jbrd, m, nil
}
