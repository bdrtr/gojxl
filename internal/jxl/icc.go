package jxl

import (
	"gojxl/internal/bitio"
	"gojxl/internal/entropy"
)

func iccByteKind1(b byte) int {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return 0
	case '0' <= b && b <= '9', b == '.', b == ',':
		return 1
	case b == 0:
		return 2
	case b == 1:
		return 3
	case b < 16:
		return 4
	case b == 255:
		return 6
	case b > 240:
		return 5
	}
	return 7
}

func iccByteKind2(b byte) int {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z':
		return 0
	case '0' <= b && b <= '9', b == '.', b == ',':
		return 1
	case b < 16:
		return 2
	case b > 240:
		return 3
	}
	return 4
}

// readEncodedICC reads the entropy coded (but still "predicted") ICC stream.
func readEncodedICC(br *bitio.Reader) ([]byte, error) {
	size := br.U64()
	if size > 1<<28 {
		return nil, invalid("ICC too large")
	}
	code, err := entropy.ReadCode(br, 41)
	if err != nil {
		return nil, err
	}
	d := code.NewDecoder(br)
	out := make([]byte, size)
	for i := range out {
		ctx := 0
		if i > 128 {
			var b1, b2 byte
			if i > 0 {
				b1 = out[i-1]
			}
			if i > 1 {
				b2 = out[i-2]
			}
			ctx = 1 + iccByteKind1(b1) + iccByteKind2(b2)*8
		}
		v := d.ReadUint(br, ctx)
		if v > 255 {
			return nil, invalid("ICC byte")
		}
		out[i] = byte(v)
		if i&4095 == 0 && br.Overflowed() {
			return nil, bitio.ErrEOF
		}
	}
	if err := d.Finish(); err != nil {
		return nil, err
	}
	return out, nil
}
