package pngx

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
)

// Ancillary holds the colour and metadata chunks of a PNG file.
type Ancillary struct {
	ICC    []byte     // iCCP profile, decompressed
	SRGB   bool       // sRGB chunk
	Intent byte       // its rendering intent
	Gamma  uint32     // gAMA: gamma x 100000 (0: absent)
	CHRM   *[8]uint32 // cHRM: white, red, green, blue x, y (x 100000)
	CICP   *[4]byte   // cICP: primaries, transfer, matrix, full range
	Exif   []byte     // eXIf: Exif data in TIFF format
	XMP    []byte     // iTXt with the XMP keyword
}

// maxInflate bounds the size of an inflated iCCP profile or XMP packet.
const maxInflate = 1 << 24

func inflateChunk(z []byte) []byte {
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(zr, maxInflate+1))
	if err != nil || len(b) > maxInflate {
		return nil
	}
	return b
}

// ReadAncillary returns the colour chunks (before the image data) and the
// metadata chunks (anywhere) of a PNG or APNG file. Malformed chunks are
// ignored.
func ReadAncillary(data []byte) Ancillary {
	var ci Ancillary
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return ci
	}
	pixels := false // past the first image data
	for p := 8; p+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		if n < 0 || n > len(data)-p-12 {
			break
		}
		typ, body := string(data[p+4:p+8]), data[p+8:p+8+n]
		p += 12 + n
		switch typ {
		case "IDAT", "fdAT", "fcTL":
			pixels = true
			continue
		case "IEND":
			return ci
		case "eXIf":
			if ci.Exif == nil && n >= 8 {
				ci.Exif = body
			}
			continue
		case "iTXt":
			// keyword, NUL, compression flag, method, language, NUL,
			// translated keyword, NUL, text
			if ci.XMP == nil && len(body) > len(xmpKeyword)+3 && string(body[:len(xmpKeyword)+1]) == xmpKeyword+"\x00" {
				rest := body[len(xmpKeyword)+3:]
				i := bytes.IndexByte(rest, 0)
				if i < 0 {
					continue
				}
				j := bytes.IndexByte(rest[i+1:], 0)
				if j < 0 {
					continue
				}
				text := rest[i+1+j+1:]
				if body[len(xmpKeyword)+1] == 1 {
					text = inflateChunk(text)
				}
				ci.XMP = text
			}
			continue
		}
		if pixels {
			continue
		}
		switch typ {
		case "iCCP":
			// name, NUL, compression method 0, zlib stream
			if i := bytes.IndexByte(body, 0); i >= 1 && i+2 <= len(body) && body[i+1] == 0 {
				ci.ICC = inflateChunk(body[i+2:])
			}
		case "sRGB":
			if n == 1 {
				ci.SRGB, ci.Intent = true, body[0]
			}
		case "gAMA":
			if n == 4 {
				ci.Gamma = binary.BigEndian.Uint32(body)
			}
		case "cHRM":
			if n == 32 {
				var c [8]uint32
				for i := range c {
					c[i] = binary.BigEndian.Uint32(body[4*i:])
				}
				ci.CHRM = &c
			}
		case "cICP":
			if n == 4 {
				c := [4]byte(body)
				ci.CICP = &c
			}
		}
	}
	return ci
}
