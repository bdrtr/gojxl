package jxl

import (
	"bytes"
	"encoding/binary"
)

var containerSig = []byte{0, 0, 0, 0x0c, 'J', 'X', 'L', ' ', 0x0d, 0x0a, 0x87, 0x0a}

// Box is a raw ISOBMFF box from a JPEG XL container.
type Box struct {
	Type string
	Data []byte
}

// Container is the result of splitting a file into its codestream and
// metadata boxes.
type Container struct {
	Codestream []byte
	Boxes      []Box // non-codestream boxes (Exif, xml, jumb, jbrd, ...)
	Level      int
}

// IsJXL reports whether data starts with a JPEG XL signature.
func IsJXL(data []byte) bool {
	return (len(data) >= 2 && data[0] == 0xff && data[1] == 0x0a) || bytes.HasPrefix(data, containerSig)
}

// ParseContainer extracts the codestream from a bare codestream or an ISOBMFF
// container.
func ParseContainer(data []byte) (*Container, error) {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0x0a {
		return &Container{Codestream: data, Level: 5}, nil
	}
	if !bytes.HasPrefix(data, containerSig) {
		return nil, ErrNotJXL
	}
	c := &Container{Level: 5}
	pos := 0
	var parts [][]byte
	sawJxlc := false
	for pos < len(data) {
		if len(data)-pos < 8 {
			return nil, invalid("truncated box header")
		}
		size := uint64(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		hdr := 8
		if size == 1 {
			if len(data)-pos < 16 {
				return nil, invalid("truncated box header")
			}
			size = binary.BigEndian.Uint64(data[pos+8:])
			hdr = 16
		}
		var end int
		if size == 0 {
			end = len(data)
		} else {
			if size < uint64(hdr) || size > uint64(len(data)-pos) {
				// allow truncated last box (progressive / partial files)
				end = len(data)
			} else {
				end = pos + int(size)
			}
		}
		payload := data[pos+hdr : end]
		switch typ {
		case "JXL ", "ftyp":
		case "jxll":
			if len(payload) > 0 {
				c.Level = int(payload[0])
			}
		case "jxlc":
			sawJxlc = true
			parts = append(parts, payload)
		case "jxlp":
			if len(payload) < 4 {
				return nil, invalid("jxlp box too small")
			}
			parts = append(parts, payload[4:])
		default:
			c.Boxes = append(c.Boxes, Box{Type: typ, Data: payload})
		}
		pos = end
	}
	if len(parts) == 0 {
		return nil, invalid("no codestream box")
	}
	if sawJxlc && len(parts) > 1 {
		return nil, invalid("both jxlc and jxlp boxes")
	}
	if len(parts) == 1 {
		c.Codestream = parts[0]
	} else {
		c.Codestream = bytes.Join(parts, nil)
	}
	return c, nil
}
