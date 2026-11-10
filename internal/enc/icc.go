package enc

import "gojxl/internal/bitio"

// ICC profile encoding. The profile is stored in the "predicted" form the
// decoder expects; this encoder uses the simplest valid layout: the 128
// byte header as differences to the predicted header, the rest as a single
// literal insert. The context-modelled entropy coder then compresses it.

func iccVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

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

// predictICC returns the encoded (predicted) form of an ICC profile.
func predictICC(icc []byte) []byte {
	n := len(icc)
	var cmds []byte
	if n > 128 {
		cmds = append(cmds, 0)                // no tag table commands
		cmds = append(cmds, 1)                // insert
		cmds = iccVarint(cmds, uint64(n-128)) // the rest of the profile
	}
	enc := iccVarint(nil, uint64(n))
	enc = iccVarint(enc, uint64(len(cmds)))
	enc = append(enc, cmds...)
	header := iccInitialHeader
	header[0], header[1], header[2], header[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	for i := 0; i < min(n, 128); i++ {
		// the decoder adapts its prediction to bytes already seen
		if i == 8 {
			copy(header[80:84], icc[4:8])
		}
		if i == 41 {
			if icc[40] == 'A' {
				header[41], header[42], header[43] = 'P', 'P', 'L'
			}
			if icc[40] == 'M' {
				header[41], header[42], header[43] = 'S', 'F', 'T'
			}
		}
		if i == 42 {
			if icc[40] == 'S' && icc[41] == 'G' {
				header[42], header[43] = 'I', ' '
			}
			if icc[40] == 'S' && icc[41] == 'U' {
				header[42], header[43] = 'N', 'W'
			}
		}
		enc = append(enc, icc[i]-header[i])
	}
	if n > 128 {
		enc = append(enc, icc[128:]...)
	}
	return enc
}

func iccKind1(b byte) int {
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

func iccKind2(b byte) int {
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

// writeICC writes the ICC stream that follows the image header when
// want_icc is set.
func writeICC(w *bitio.Writer, icc []byte) {
	enc := predictICC(icc)
	w.WriteU64(uint64(len(enc)))
	ctxOf := func(i int) int {
		if i <= 128 {
			return 0
		}
		var b1, b2 byte
		b1 = enc[i-1]
		if i > 1 {
			b2 = enc[i-2]
		}
		return 1 + iccKind1(b1) + iccKind2(b2)*8
	}
	h := newHistograms(41, defaultCfg)
	for i, b := range enc {
		h.add(ctxOf(i), uint32(b))
	}
	c := writeANSCodeClustered(w, defaultCfg, h.counts, 41)
	st := &ansStream{c: c}
	for i, b := range enc {
		st.add(ctxOf(i), uint32(b))
	}
	st.flush(w)
}
