package enc

// predictICC is a port of libjxl's PredictICC: the tag table is coded as
// commands relative to predicted offsets and sizes, and the tagged elements
// get type commands, XYZ commands and (shuffled) linear prediction for
// curves, lookup tables and UTF-16 text, which the entropy coder then
// compresses much better than raw bytes.
func predictICC(icc []byte) (enc []byte) {
	defer func() {
		if r := recover(); r != nil {
			enc = predictICCSimple(icc)
		}
	}()
	size := len(icc)
	res := iccVarint(nil, uint64(size))
	data := iccHeaderDiff(icc)
	if size <= 128 {
		res = iccVarint(res, 0)
		return append(res, data...)
	}
	u32 := func(p int) uint32 {
		if p < 0 || p+4 > size {
			return 0
		}
		return be32(icc, p)
	}
	kw := func(p int) string {
		if p < 0 || p+4 > size {
			return "\x00\x00\x00\x00"
		}
		return string(icc[p : p+4])
	}
	var cmds []byte
	var tagstarts, tagsizes []int
	tagmap := map[int]int{}
	pos := 128
	if pos+4 <= size {
		numtags := int(u32(pos))
		pos += 4
		cmds = iccVarint(cmds, uint64(numtags)+1)
		prevStart, prevSize := 128+numtags*12, 0
		for i := 0; i < numtags; i++ {
			if pos+12 > size {
				break
			}
			tag := kw(pos)
			tagstart, tagsize := int(u32(pos+4)), int(u32(pos+8))
			pos += 12
			tagstarts = append(tagstarts, tagstart)
			tagsizes = append(tagsizes, tagsize)
			tagmap[tagstart] = len(tagstarts) - 1
			code := byte(1)
			for j, t := range iccTagNames {
				if tag == t {
					code = byte(j + 4)
					break
				}
			}
			if tag == "rTRC" && pos+24 < size && kw(pos) == "gTRC" && kw(pos+12) == "bTRC" {
				ok := true
				for k := 0; k < 8; k++ {
					if icc[pos-8+k] != icc[pos+4+k] || icc[pos-8+k] != icc[pos+16+k] {
						ok = false
					}
				}
				if ok {
					code = 2
					pos += 24
					i += 2
				}
			}
			if tag == "rXYZ" && pos+24 < size && kw(pos) == "gXYZ" && kw(pos+12) == "bXYZ" &&
				tagsize == 20 && u32(pos+8) == 20 && u32(pos+20) == 20 &&
				int(u32(pos+4)) == tagstart+20 && int(u32(pos+16)) == tagstart+40 {
				code = 3
				pos += 24
				i += 2
			}
			cmd := code
			if prevStart+prevSize != tagstart {
				cmd |= 64
			}
			predSize := prevSize
			switch tag {
			case "rXYZ", "gXYZ", "bXYZ", "kXYZ", "wtpt", "bkpt", "lumi":
				predSize = 20
			}
			if predSize != tagsize {
				cmd |= 128
			}
			cmds = append(cmds, cmd)
			if code == 1 {
				data = append(data, tag...)
			}
			if cmd&64 != 0 {
				cmds = iccVarint(cmds, uint64(tagstart))
			}
			if cmd&128 != 0 {
				cmds = iccVarint(cmds, uint64(tagsize))
			}
			prevStart, prevSize = tagstart, tagsize
		}
	}
	cmds = append(cmds, 0)

	const sizeLimit = (1<<32 - 1) >> 2
	tag := "\x00\x00\x00\x00"
	tagstart, tagsize, clutstart := 0, 0, 0
	sane := func() bool { return tagsize > 8 && tagsize < sizeLimit }
	predict := func(stride, width, order, num int, out []byte) []byte {
		if pos == 0 || (pos-1)>>2 < stride || pos < stride*4 || pos+num > size {
			panic("icc predict")
		}
		st := len(out)
		for i := 0; i < num; i++ {
			out = append(out, icc[pos+i]-iccLinearPredict(icc, pos, i, stride, width, order))
		}
		pos += num
		if width > 1 {
			iccUnshuffle(out[st:], width)
		}
		return out
	}
	last0 := pos
	for pos <= size {
		last1 := pos
		var ca, da []byte
		if pos > tagstart+tagsize && tagsize < sizeLimit {
			tag = "\x00\x00\x00\x00"
		}
		if idx, ok := tagmap[pos]; ok && pos+4 <= size {
			tag = kw(pos)
			tagstart, tagsize = tagstarts[idx], tagsizes[idx]
			zero4 := pos+8 <= size && u32(pos+4) == 0
			if tag == "mluc" && sane() && pos+tagsize <= size && zero4 {
				num := tagsize - 8
				ca = append(ca, 16+3, 2)
				pos += 8
				ca = iccVarint(ca, uint64(num))
				st := len(da)
				da = append(da, icc[pos:pos+num]...)
				pos += num
				iccUnshuffle(da[st:], 2)
			}
			if tag == "curv" && sane() && pos+tagsize <= size && zero4 {
				num := tagsize - 8
				if num > 16 && num < 1<<28 && pos+num <= size && pos > 0 {
					ca = append(ca, 16+5)
					pos += 8
					ca = append(ca, 4, 1<<2|1)
					ca = iccVarint(ca, uint64(num))
					da = predict(2, 2, 1, num, da)
				}
			}
		}
		if tag == "mAB " || tag == "mBA " {
			sub := kw(pos)
			if pos+12 < size && (sub == "curv" || sub == "vcgt") && u32(pos+4) == 0 {
				num := int(u32(pos+8)) * 2
				if num > 16 && num < 1<<28 && pos+12+num <= size {
					pos += 12
					last1 = pos
					ca = append(ca, 4, 1<<2|1)
					ca = iccVarint(ca, uint64(num))
					da = predict(2, 2, 1, num, da)
				}
			}
			if pos == tagstart+24 && pos+4 < size {
				clutstart = tagstart + int(u32(pos))
			}
			if pos == clutstart && clutstart+16 < size {
				numi, numo := int(icc[tagstart+8]), int(icc[tagstart+9])
				width := int(icc[clutstart+16])
				stride := width * numo
				num := width * numo
				for i := 0; i < numi && clutstart+i < size; i++ {
					num *= int(icc[clutstart+i])
				}
				if (width == 1 || width == 2) && num > 64 && num < 1<<28 && pos+num <= size && pos > stride*4 {
					flags := byte(1<<2 | (width - 1))
					if stride != width {
						flags |= 16
					}
					ca = append(ca, 4, flags)
					if flags&16 != 0 {
						ca = iccVarint(ca, uint64(stride))
					}
					ca = iccVarint(ca, uint64(num))
					da = predict(stride, width, 1, num, da)
				}
			}
		}
		if len(ca) == 0 && len(da) == 0 && tag == "gbd " && sane() && pos == tagstart+8 && pos+tagsize-8 <= size && pos > 16 {
			ca = append(ca, 4, 3)
			ca = iccVarint(ca, uint64(tagsize-8))
			da = predict(4, 4, 0, tagsize-8, da)
		}
		if len(ca) == 0 && len(da) == 0 && pos+20 <= size && kw(pos) == "XYZ " && u32(pos+4) == 0 {
			ca = append(ca, 10)
			pos += 8
			da = append(da, icc[pos:pos+12]...)
			pos += 12
		}
		if len(ca) == 0 && len(da) == 0 && pos+8 <= size && u32(pos+4) == 0 {
			sub := kw(pos)
			for i, t := range iccTypeNames {
				if sub == t {
					ca = append(ca, byte(16+i))
					pos += 8
					break
				}
			}
		}
		if len(ca) != 0 || len(da) != 0 || pos == size {
			if last0 < last1 {
				cmds = append(cmds, 1)
				cmds = iccVarint(cmds, uint64(last1-last0))
				data = append(data, icc[last0:last1]...)
			}
			cmds = append(cmds, ca...)
			data = append(data, da...)
			last0 = pos
		}
		if len(ca) == 0 && len(da) == 0 {
			pos++
		}
	}
	res = iccVarint(res, uint64(len(cmds)))
	res = append(res, cmds...)
	return append(res, data...)
}

var iccTagNames = [17]string{"cprt", "wtpt", "bkpt", "rXYZ", "gXYZ", "bXYZ", "kXYZ", "rTRC", "gTRC", "bTRC", "kTRC", "chad", "desc", "chrm", "dmnd", "dmdd", "lumi"}
var iccTypeNames = [8]string{"XYZ ", "desc", "text", "mluc", "para", "curv", "sf32", "gbd "}

func be32(b []byte, p int) uint32 {
	return uint32(b[p])<<24 | uint32(b[p+1])<<16 | uint32(b[p+2])<<8 | uint32(b[p+3])
}

// iccUnshuffle transposes width-interleaved bytes (inverse of the
// decoder's shuffle).
func iccUnshuffle(data []byte, width int) {
	size := len(data)
	height := (size + width - 1) / width
	res := make([]byte, size)
	s, j := 0, 0
	for i := 0; i < size; i++ {
		res[j] = data[i]
		j += height
		if j >= size {
			s++
			j = s
		}
	}
	copy(data, res)
}

func iccLinearPredict(data []byte, start, i, stride, width, order int) byte {
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
		return byte(pred >> (8 * (3 - (i & 3))))
	}
}
