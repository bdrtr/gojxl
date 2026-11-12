// Package exif reads and resets the orientation tag of Exif data in TIFF
// format (as in PNG eXIf chunks, JPEG APP1 segments after "Exif\0\0" and
// JPEG XL Exif boxes after their offset field).
package exif

import "encoding/binary"

// orientationValue returns the offset of the orientation tag's value in
// IFD0 and the byte order, or -1.
func orientationValue(t []byte) (int, binary.ByteOrder) {
	if len(t) < 8 {
		return -1, nil
	}
	var bo binary.ByteOrder
	switch string(t[:4]) {
	case "II*\x00":
		bo = binary.LittleEndian
	case "MM\x00*":
		bo = binary.BigEndian
	default:
		return -1, nil
	}
	off := uint64(bo.Uint32(t[4:]))
	if off < 8 || off+2 > uint64(len(t)) {
		return -1, nil
	}
	n := int(bo.Uint16(t[off:]))
	for i := 0; i < n; i++ {
		e := int(off) + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		// SHORT, count 1: the value is in the entry
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 && bo.Uint32(t[e+4:]) == 1 {
			return e + 8, bo
		}
	}
	return -1, nil
}

// Orientation returns the orientation tag (1-8), or 0 if there is none.
func Orientation(t []byte) int {
	p, bo := orientationValue(t)
	if p < 0 {
		return 0
	}
	if v := int(bo.Uint16(t[p:])); v >= 1 && v <= 8 {
		return v
	}
	return 0
}

// ResetOrientation returns the data with the orientation tag set to 1 (a
// copy if it changes), for images whose pixels are already oriented.
func ResetOrientation(t []byte) []byte {
	p, bo := orientationValue(t)
	if p < 0 || bo.Uint16(t[p:]) == 1 {
		return t
	}
	c := append([]byte(nil), t...)
	bo.PutUint16(c[p:], 1)
	return c
}
