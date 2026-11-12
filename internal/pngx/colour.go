package pngx

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
)

// ColourInfo holds the colour chunks of a PNG file.
type ColourInfo struct {
	ICC    []byte     // iCCP profile, decompressed
	SRGB   bool       // sRGB chunk
	Intent byte       // its rendering intent
	Gamma  uint32     // gAMA: gamma x 100000 (0: absent)
	CHRM   *[8]uint32 // cHRM: white, red, green, blue x, y (x 100000)
	CICP   *[4]byte   // cICP: primaries, transfer, matrix, full range
}

// maxICC bounds the size of an inflated iCCP profile.
const maxICC = 1 << 24

// ReadColourInfo returns the colour chunks before the image data of a PNG
// (or APNG) file. Malformed chunks are ignored.
func ReadColourInfo(data []byte) ColourInfo {
	var ci ColourInfo
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		return ci
	}
	for p := 8; p+12 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		if n < 0 || n > len(data)-p-12 {
			break
		}
		typ, body := string(data[p+4:p+8]), data[p+8:p+8+n]
		switch typ {
		case "IDAT", "fdAT", "fcTL", "IEND":
			return ci
		case "iCCP":
			// name, NUL, compression method 0, zlib stream
			if i := bytes.IndexByte(body, 0); i >= 1 && i+2 <= len(body) && body[i+1] == 0 {
				if zr, err := zlib.NewReader(bytes.NewReader(body[i+2:])); err == nil {
					icc, err := io.ReadAll(io.LimitReader(zr, maxICC+1))
					if err == nil && len(icc) <= maxICC {
						ci.ICC = icc
					}
				}
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
		p += 12 + n
	}
	return ci
}
