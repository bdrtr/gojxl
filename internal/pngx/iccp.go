// Package pngx adds small extensions to PNG files produced by image/png.
package pngx

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
)

// InsertICC inserts an iCCP chunk with the given profile right after the
// IHDR chunk of an encoded PNG.
func InsertICC(png []byte, icc []byte) []byte {
	if len(icc) == 0 || len(png) < 33 {
		return png
	}
	var z bytes.Buffer
	w, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	w.Write(icc)
	w.Close()
	var data bytes.Buffer
	data.WriteString("ICC profile")
	data.WriteByte(0) // null separator
	data.WriteByte(0) // compression method
	data.Write(z.Bytes())
	chunk := makeChunk("iCCP", data.Bytes())
	// signature (8) + IHDR chunk (4+4+13+4 = 25)
	out := make([]byte, 0, len(png)+len(chunk))
	out = append(out, png[:33]...)
	out = append(out, chunk...)
	out = append(out, png[33:]...)
	return out
}

func makeChunk(typ string, data []byte) []byte {
	b := make([]byte, 8+len(data)+4)
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	copy(b[4:], typ)
	copy(b[8:], data)
	crc := crc32.NewIEEE()
	crc.Write(b[4 : 8+len(data)])
	binary.BigEndian.PutUint32(b[8+len(data):], crc.Sum32())
	return b
}

// InsertCICP inserts a cICP chunk (ITU-T H.273 code points, RGB, full
// range) right after IHDR. It must precede PLTE/IDAT, which holds here.
func InsertCICP(png []byte, primaries, transfer byte) []byte {
	if len(png) < 33 {
		return png
	}
	chunk := makeChunk("cICP", []byte{primaries, transfer, 0, 1})
	out := make([]byte, 0, len(png)+len(chunk))
	out = append(out, png[:33]...)
	out = append(out, chunk...)
	out = append(out, png[33:]...)
	return out
}
