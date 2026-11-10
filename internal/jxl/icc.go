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

var iccTagStrings = [17]string{"cprt", "wtpt", "bkpt", "rXYZ", "gXYZ", "bXYZ", "kXYZ", "rTRC", "gTRC", "bTRC", "kTRC", "chad", "desc", "chrm", "dmnd", "dmdd", "lumi"}
var iccTypeStrings = [8]string{"XYZ ", "desc", "text", "mluc", "para", "curv", "sf32", "gbd "}

var iccInitialHeader = [128]byte{
	0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0, 'm', 'n', 't', 'r',
	'R', 'G', 'B', ' ', 'X', 'Y', 'Z', ' ', 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 'a', 'c', 's', 'p', 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 246, 214, 0, 1, 0, 0, 0, 0, 211, 45,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
}

func iccVarint(b []byte, pos *int, end int) (uint64, error) {
	var r uint64
	for i := 0; i < 10; i++ {
		if *pos >= end {
			return 0, invalid("ICC varint")
		}
		c := b[*pos]
		*pos++
		r |= uint64(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return r, nil
		}
	}
	return 0, invalid("ICC varint")
}

func iccShuffle(data []byte, width int) {
	size := len(data)
	height := (size + width - 1) / width
	res := make([]byte, size)
	s, j := 0, 0
	for i := 0; i < size; i++ {
		res[i] = data[j]
		j += height
		if j >= size {
			s++
			j = s
		}
	}
	copy(data, res)
}

func be32(b []byte, p int) uint32 {
	return uint32(b[p])<<24 | uint32(b[p+1])<<16 | uint32(b[p+2])<<8 | uint32(b[p+3])
}

func iccPredict(data []byte, start, i, stride, width, order int) byte {
	pos := start + i
	pv := func(p1, p2, p3 uint32) uint32 {
		switch order {
		case 0:
			return p1
		case 1:
			return 2*p1 - p2
		case 2:
			return 3*p1 - 3*p2 + p3
		}
		return 0
	}
	switch width {
	case 1:
		return byte(pv(uint32(data[pos-stride]), uint32(data[pos-2*stride]), uint32(data[pos-3*stride])))
	case 2:
		p := start + (i &^ 1)
		g := func(q int) uint32 { return uint32(uint16(data[q])<<8 + uint16(data[q+1])) }
		pred := uint16(pv(g(p-stride), g(p-2*stride), g(p-3*stride)))
		if i&1 != 0 {
			return byte(pred)
		}
		return byte(pred >> 8)
	default:
		p := start + (i &^ 3)
		g := func(q int) uint32 {
			if q+4 > pos {
				return 0
			}
			return be32(data, q)
		}
		pred := pv(g(p-stride), g(p-2*stride), g(p-3*stride))
		sh := 3 - (i & 3)
		return byte(pred >> (8 * sh))
	}
}

// unpredictICC reconstructs the ICC profile from its encoded form.
func unpredictICC(enc []byte) ([]byte, error) {
	size := len(enc)
	pos := 0
	osize, err := iccVarint(enc, &pos, size)
	if err != nil {
		return nil, err
	}
	csize, err := iccVarint(enc, &pos, size)
	if err != nil {
		return nil, err
	}
	if osize > 1<<28 || csize > uint64(size-pos) {
		return nil, invalid("ICC sizes")
	}
	cpos := pos
	cend := cpos + int(csize)
	pos = cend
	out := make([]byte, 0, osize)
	header := iccInitialHeader
	header[0], header[1], header[2], header[3] = byte(osize>>24), byte(osize>>16), byte(osize>>8), byte(osize)
	for i := 0; i <= 128; i++ {
		if uint64(len(out)) == osize {
			if cpos != cend || pos != size {
				return nil, invalid("ICC trailing data")
			}
			return out, nil
		}
		if i == 128 {
			break
		}
		// header prediction tweaks
		if i == 8 && len(out) >= 8 {
			copy(header[80:84], out[4:8])
		}
		if i == 41 && len(out) >= 41 {
			if out[40] == 'A' {
				header[41], header[42], header[43] = 'P', 'P', 'L'
			}
			if out[40] == 'M' {
				header[41], header[42], header[43] = 'S', 'F', 'T'
			}
		}
		if i == 42 && len(out) >= 42 {
			if out[40] == 'S' && out[41] == 'G' {
				header[42], header[43] = 'I', ' '
			}
			if out[40] == 'S' && out[41] == 'U' {
				header[42], header[43] = 'N', 'W'
			}
		}
		if pos >= size {
			return nil, invalid("ICC header")
		}
		out = append(out, enc[pos]+header[i])
		pos++
	}
	app32 := func(v uint64) { out = append(out, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	appTag := func(t string) { out = append(out, t...) }
	if cpos >= cend {
		return nil, invalid("ICC commands")
	}
	numTags, err := iccVarint(enc, &cpos, cend)
	if err != nil {
		return nil, err
	}
	if numTags != 0 {
		numTags--
		app32(numTags)
		prevStart := uint64(128 + numTags*12)
		prevSize := uint64(0)
		for cpos < cend {
			if uint64(len(out)) > osize {
				return nil, invalid("ICC size")
			}
			cmd := enc[cpos]
			cpos++
			code := int(cmd & 63)
			var tag string
			switch {
			case code == 0:
				goto tagsDone
			case code == 1:
				if pos+4 > size {
					return nil, invalid("ICC tag")
				}
				tag = string(enc[pos : pos+4])
				pos += 4
			case code == 2:
				tag = "rTRC"
			case code == 3:
				tag = "rXYZ"
			default:
				if code-4 >= len(iccTagStrings) {
					return nil, invalid("ICC tag code")
				}
				tag = iccTagStrings[code-4]
			}
			appTag(tag)
			tagSize := prevSize
			switch tag {
			case "rXYZ", "gXYZ", "bXYZ", "kXYZ", "wtpt", "bkpt", "lumi":
				tagSize = 20
			}
			var tagStart uint64
			if cmd&64 != 0 {
				if tagStart, err = iccVarint(enc, &cpos, cend); err != nil {
					return nil, err
				}
			} else {
				tagStart = prevStart + prevSize
			}
			app32(tagStart)
			if cmd&128 != 0 {
				if tagSize, err = iccVarint(enc, &cpos, cend); err != nil {
					return nil, err
				}
			}
			app32(tagSize)
			prevStart, prevSize = tagStart, tagSize
			if code == 2 {
				appTag("gTRC")
				app32(tagStart)
				app32(tagSize)
				appTag("bTRC")
				app32(tagStart)
				app32(tagSize)
			}
			if code == 3 {
				appTag("gXYZ")
				app32(tagStart + tagSize)
				app32(tagSize)
				appTag("bXYZ")
				app32(tagStart + 2*tagSize)
				app32(tagSize)
			}
		}
	}
tagsDone:
	for cpos < cend {
		if uint64(len(out)) > osize {
			return nil, invalid("ICC size")
		}
		cmd := enc[cpos]
		cpos++
		switch {
		case cmd == 1: // insert
			n, err := iccVarint(enc, &cpos, cend)
			if err != nil {
				return nil, err
			}
			if uint64(pos)+n > uint64(size) {
				return nil, invalid("ICC insert")
			}
			out = append(out, enc[pos:pos+int(n)]...)
			pos += int(n)
		case cmd == 2 || cmd == 3: // shuffle
			n, err := iccVarint(enc, &cpos, cend)
			if err != nil {
				return nil, err
			}
			if uint64(pos)+n > uint64(size) {
				return nil, invalid("ICC shuffle")
			}
			buf := append([]byte(nil), enc[pos:pos+int(n)]...)
			w := 2
			if cmd == 3 {
				w = 4
			}
			iccShuffle(buf, w)
			out = append(out, buf...)
			pos += int(n)
		case cmd == 4: // predict
			if cpos+2 > cend {
				return nil, invalid("ICC predict")
			}
			flags := enc[cpos]
			cpos++
			width := int(flags&3) + 1
			if width == 3 {
				return nil, invalid("ICC predict width")
			}
			order := int(flags&12) >> 2
			if order == 3 {
				return nil, invalid("ICC predict order")
			}
			stride := uint64(width)
			if flags&16 != 0 {
				if stride, err = iccVarint(enc, &cpos, cend); err != nil {
					return nil, err
				}
				if stride < uint64(width) {
					return nil, invalid("ICC stride")
				}
			}
			if len(out) == 0 || uint64((len(out)-1)>>2) < stride {
				return nil, invalid("ICC stride")
			}
			n, err := iccVarint(enc, &cpos, cend)
			if err != nil {
				return nil, err
			}
			if uint64(pos)+n > uint64(size) {
				return nil, invalid("ICC predict data")
			}
			buf := append([]byte(nil), enc[pos:pos+int(n)]...)
			if width > 1 {
				iccShuffle(buf, width)
			}
			start := len(out)
			for i := 0; i < int(n); i++ {
				out = append(out, iccPredict(out, start, i, int(stride), width, order)+buf[i])
			}
			pos += int(n)
		case cmd == 10: // XYZ
			appTag("XYZ ")
			out = append(out, 0, 0, 0, 0)
			if pos+12 > size {
				return nil, invalid("ICC XYZ")
			}
			out = append(out, enc[pos:pos+12]...)
			pos += 12
		case cmd >= 16 && cmd < 16+8:
			appTag(iccTypeStrings[cmd-16])
			out = append(out, 0, 0, 0, 0)
		default:
			return nil, invalid("ICC command")
		}
	}
	if pos != size || uint64(len(out)) != osize {
		return nil, invalid("ICC size mismatch")
	}
	return out, nil
}
