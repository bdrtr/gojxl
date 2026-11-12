package pngx

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"hash/crc32"
	"image"
	"io"
	"math"
	"runtime"
	"sync"
)

// Encode writes img as PNG like image/png (opaque NRGBA images become RGB),
// but filters and compresses strips of rows in parallel: each strip is an
// independent run of deflate blocks ended by a sync flush, so the strips
// concatenate into one zlib stream (the Adler-32 sums are combined).
// Supported: *image.Gray, *image.Gray16, *image.NRGBA, *image.NRGBA64.
func Encode(w io.Writer, img image.Image, level int) error {
	l, err := pixelLayout(img, !isOpaque(img))
	if err != nil {
		return err
	}
	z, err := compress(l, level)
	if err != nil {
		return err
	}
	out := &bytes.Buffer{}
	writeHeader(out, l)
	writeData(out, "IDAT", -1, z)
	writeChunk(out, "IEND", nil)
	_, err = w.Write(out.Bytes())
	return err
}

// EncodeRows writes a width x height PNG whose rows row fills in PNG
// layout (ctype 0 grey, 2 RGB or 6 RGBA; depth 8 or 16 bits per sample,
// big endian), so images need not exist in memory as a whole. row is
// called concurrently for different rows.
//
// The extra chunks follow IHDR. The file goes to w chunk by chunk, the
// compressed strips without being joined first.
func EncodeRows(w io.Writer, width, height, ctype, depth int, row func(y int, dst []byte), level int, extra ...Chunk) error {
	if width <= 0 || height <= 0 {
		return errors.New("png: empty image")
	}
	ch := map[int]int{0: 1, 2: 3, 6: 4}[ctype]
	if ch == 0 || (depth != 8 && depth != 16) {
		return errors.New("png: unsupported row format")
	}
	l := layout{w: width, h: height, ctype: ctype, depth: depth, bpp: ch * depth / 8, row: row}
	zp, err := compressParts(l, level)
	if err != nil {
		return err
	}
	head := &bytes.Buffer{}
	writeHeader(head, l)
	for _, c := range extra {
		writeChunk(head, c.Type, c.Data)
	}
	if _, err := w.Write(head.Bytes()); err != nil {
		return err
	}
	if err := writeDataTo(w, "IDAT", zp.size(), zp.pieces()); err != nil {
		return err
	}
	return writeChunkTo(w, "IEND")
}

// Chunk is an extra PNG chunk.
type Chunk struct {
	Type string
	Data []byte
}

// writeDataTo writes n bytes of zlib data, given as consecutive pieces,
// as typ chunks of at most 1 MiB (as writeData does).
func writeDataTo(w io.Writer, typ string, n int, pieces [][]byte) error {
	const maxChunk = 1 << 20
	for n > 0 {
		cn := min(n, maxChunk)
		var hdr [8]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(cn))
		copy(hdr[4:], typ)
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		crc := crc32.Update(0, crc32.IEEETable, hdr[4:])
		for left := cn; left > 0; {
			p := pieces[0]
			k := min(left, len(p))
			if _, err := w.Write(p[:k]); err != nil {
				return err
			}
			crc = crc32.Update(crc, crc32.IEEETable, p[:k])
			left -= k
			if pieces[0] = p[k:]; len(pieces[0]) == 0 {
				pieces = pieces[1:]
			}
		}
		var tail [4]byte
		binary.BigEndian.PutUint32(tail[:], crc)
		if _, err := w.Write(tail[:]); err != nil {
			return err
		}
		n -= cn
	}
	return nil
}

func writeChunkTo(w io.Writer, typ string) error {
	b := &bytes.Buffer{}
	writeChunk(b, typ, nil)
	_, err := w.Write(b.Bytes())
	return err
}

// EncodeAPNG writes an animated PNG: frames (all of the same size and
// type) shown for delays[i] seconds each, looping loops times (0 =
// forever). The first frame is also the default image.
func EncodeAPNG(w io.Writer, frames []image.Image, delays []float64, loops int, level int) error {
	if len(frames) == 0 || len(delays) != len(frames) {
		return errors.New("pngx: no frames")
	}
	alpha := false
	for _, f := range frames {
		if f.Bounds() != frames[0].Bounds() {
			return errors.New("pngx: frames differ in size")
		}
		alpha = alpha || !isOpaque(f)
	}
	ls := make([]layout, len(frames))
	for i, f := range frames {
		l, err := pixelLayout(f, alpha)
		if err != nil {
			return err
		}
		if l.ctype != ls[0].ctype && i > 0 || i > 0 && l.depth != ls[0].depth {
			return errors.New("pngx: frames differ in type")
		}
		ls[i] = l
	}
	out := &bytes.Buffer{}
	writeHeader(out, ls[0])
	var actl [8]byte
	binary.BigEndian.PutUint32(actl[0:], uint32(len(frames)))
	binary.BigEndian.PutUint32(actl[4:], uint32(loops))
	writeChunk(out, "acTL", actl[:])
	seq := uint32(0)
	for i, l := range ls {
		num, den := apngDelay(delays[i])
		var fc [26]byte
		binary.BigEndian.PutUint32(fc[0:], seq)
		binary.BigEndian.PutUint32(fc[4:], uint32(l.w))
		binary.BigEndian.PutUint32(fc[8:], uint32(l.h))
		binary.BigEndian.PutUint16(fc[20:], num)
		binary.BigEndian.PutUint16(fc[22:], den)
		// dispose_op NONE, blend_op SOURCE (frames are full canvases)
		writeChunk(out, "fcTL", fc[:])
		seq++
		z, err := compress(l, level)
		if err != nil {
			return err
		}
		if i == 0 {
			writeData(out, "IDAT", -1, z)
		} else {
			seq = writeData(out, "fdAT", int64(seq), z)
		}
	}
	writeChunk(out, "IEND", nil)
	_, err := w.Write(out.Bytes())
	return err
}

// apngDelay converts seconds to an APNG delay fraction.
func apngDelay(sec float64) (uint16, uint16) {
	for _, den := range []float64{1000, 100, 10, 1} {
		if n := math.Round(sec * den); n <= 65535 {
			return uint16(max(n, 0)), uint16(den)
		}
	}
	return 65535, 1
}

type layout struct {
	w, h, ctype, depth, bpp int
	row                     func(y int, dst []byte)
}

func isOpaque(img image.Image) bool {
	switch m := img.(type) {
	case *image.NRGBA:
		return opaque8(m)
	case *image.NRGBA64:
		return opaque16(m)
	}
	return true
}

// pixelLayout returns the PNG pixel format of img and a row converter;
// alpha selects RGBA output for NRGBA images.
func pixelLayout(img image.Image, alpha bool) (layout, error) {
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	l := layout{w: W, h: H}
	if W <= 0 || H <= 0 {
		return l, errors.New("png: empty image")
	}
	switch m := img.(type) {
	case *image.Gray:
		l.ctype, l.depth, l.bpp = 0, 8, 1
		l.row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
	case *image.Gray16:
		l.ctype, l.depth, l.bpp = 0, 16, 2
		l.row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
	case *image.NRGBA:
		l.depth = 8
		if !alpha {
			l.ctype, l.bpp = 2, 3
			l.row = func(y int, dst []byte) {
				src := m.Pix[y*m.Stride : y*m.Stride+4*W]
				for x := 0; x < W; x++ {
					dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x], src[4*x+1], src[4*x+2]
				}
			}
		} else {
			l.ctype, l.bpp = 6, 4
			l.row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
		}
	case *image.NRGBA64:
		l.depth = 16
		if !alpha {
			l.ctype, l.bpp = 2, 6
			l.row = func(y int, dst []byte) {
				src := m.Pix[y*m.Stride : y*m.Stride+8*W]
				for x := 0; x < W; x++ {
					copy(dst[6*x:6*x+6], src[8*x:8*x+6])
				}
			}
		} else {
			l.ctype, l.bpp = 6, 8
			l.row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
		}
	default:
		return l, errors.New("pngx: unsupported image type")
	}
	return l, nil
}

func writeHeader(out *bytes.Buffer, l layout) {
	out.WriteString("\x89PNG\r\n\x1a\n")
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(l.w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(l.h))
	ihdr[8], ihdr[9] = byte(l.depth), byte(l.ctype)
	writeChunk(out, "IHDR", ihdr[:])
}

// writeData writes zlib data as chunks of at most 1 MiB; for fdAT
// (seq >= 0) each chunk starts with a sequence number. It returns the next
// sequence number.
func writeData(out *bytes.Buffer, typ string, seq int64, z []byte) uint32 {
	const maxChunk = 1 << 20
	for len(z) > 0 {
		n := min(len(z), maxChunk)
		if seq >= 0 {
			d := binary.BigEndian.AppendUint32(make([]byte, 0, n+4), uint32(seq))
			writeChunk(out, typ, append(d, z[:n]...))
			seq++
		} else {
			writeChunk(out, typ, z[:n])
		}
		z = z[n:]
	}
	return uint32(seq)
}

// zparts is a zlib stream in pieces: the header, the deflate data of each
// strip, and the Adler-32 trailer.
type zparts struct {
	strips [][]byte
	adler  uint32
}

func (z *zparts) size() int {
	n := 2 + 4
	for _, s := range z.strips {
		n += len(s)
	}
	return n
}

// pieces returns the stream as consecutive byte slices.
func (z *zparts) pieces() [][]byte {
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], z.adler)
	ps := append([][]byte{{0x78, 0x01}}, z.strips...)
	return append(ps, tail[:])
}

// compress filters and deflates the image into a zlib stream, strips of
// rows in parallel.
func compress(l layout, level int) ([]byte, error) {
	zp, err := compressParts(l, level)
	if err != nil {
		return nil, err
	}
	z := make([]byte, 0, zp.size())
	for _, p := range zp.pieces() {
		z = append(z, p...)
	}
	return z, nil
}

// compressParts is compress without joining the strips.
func compressParts(l layout, level int) (*zparts, error) {
	W, H, bpp, row := l.w, l.h, l.bpp, l.row
	rowLen := W * bpp
	stripRows := max(16, (1<<18)/(rowLen+1))
	nstrips := (H + stripRows - 1) / stripRows
	type strip struct {
		data   []byte
		adler  uint32
		rawLen int
	}
	strips := make([]strip, nstrips)
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	var werr error
	for k := 0; k < min(runtime.GOMAXPROCS(0), nstrips); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := make([]byte, rowLen)
			cur := make([]byte, rowLen)
			raw := make([]byte, 0, stripRows*(rowLen+1))
			var buf bytes.Buffer
			fw, _ := flate.NewWriter(&buf, level)
			for {
				mu.Lock()
				s := next
				next++
				mu.Unlock()
				if s >= nstrips {
					return
				}
				y0, y1 := s*stripRows, min(H, (s+1)*stripRows)
				if y0 > 0 {
					row(y0-1, prev)
				} else {
					clear(prev)
				}
				raw = raw[:0]
				for y := y0; y < y1; y++ {
					row(y, cur)
					raw = filterRow(raw, cur, prev, bpp, y == 0)
					prev, cur = cur, prev
				}
				buf.Reset()
				fw.Reset(&buf)
				fw.Write(raw)
				var err error
				if s == nstrips-1 {
					err = fw.Close()
				} else {
					err = fw.Flush()
				}
				if err != nil {
					mu.Lock()
					werr = err
					mu.Unlock()
				}
				strips[s] = strip{append([]byte(nil), buf.Bytes()...), adler32.Checksum(raw), len(raw)}
			}
		}()
	}
	wg.Wait()
	if werr != nil {
		return nil, werr
	}
	zp := &zparts{adler: 1}
	for _, s := range strips {
		zp.strips = append(zp.strips, s.data)
		zp.adler = adler32Combine(zp.adler, s.adler, s.rawLen)
	}
	return zp, nil
}

func writeChunk(out *bytes.Buffer, typ string, data []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	copy(hdr[4:], typ)
	out.Write(hdr[:])
	out.Write(data)
	c := crc32.NewIEEE()
	c.Write(hdr[4:])
	c.Write(data)
	binary.BigEndian.PutUint32(hdr[:4], c.Sum32())
	out.Write(hdr[:4])
}

// adler32Combine returns the Adler-32 of A||B from those of A and B.
func adler32Combine(a1, a2 uint32, len2 int) uint32 {
	const base = 65521
	rem := uint32(len2 % base)
	sum1 := a1 & 0xffff
	sum2 := rem * sum1 % base
	sum1 += (a2 & 0xffff) + base - 1
	sum2 += (a1 >> 16) + (a2 >> 16) + base - rem
	if sum1 >= base {
		sum1 -= base
	}
	if sum1 >= base {
		sum1 -= base
	}
	if sum2 >= 2*base {
		sum2 -= 2 * base
	}
	if sum2 >= base {
		sum2 -= base
	}
	return sum2<<16 | sum1
}

// filterRow appends the filter byte and the filtered row to dst, picking
// the filter with the smallest sum of absolute values (as image/png and
// libpng do; ties go to the lower filter type). The first row only tries
// None and Sub. The sums for all filters are computed in one pass.
func filterRow(dst, cur, prev []byte, bpp int, first bool) []byte {
	n := len(cur)
	prev = prev[:n]
	k := min(bpp, n)
	var s0, s1, s2, s3, s4 int
	for i := 0; i < k; i++ {
		x, b := cur[i], prev[i]
		s0 += absB(x)
		s2 += absB(x - b)
		s3 += absB(x - b>>1)
	}
	s1, s4 = s0, s2
	x, left := cur[k:], cur[:n-k]
	up, ul := prev[k:], prev[:n-k]
	left, up, ul = left[:len(x)], up[:len(x)], ul[:len(x)]
	if first {
		for j, v := range x {
			s0 += absB(v)
			s1 += absB(v - left[j])
		}
	} else {
		// two loops: one with all five sums spills registers
		for j, v := range x {
			s0 += absB(v)
			s1 += absB(v - left[j])
			s2 += absB(v - up[j])
		}
		for j, v := range x {
			a, b := left[j], up[j]
			s3 += absB(v - byte((uint(a)+uint(b))>>1))
			s4 += absB(v - paethPred(a, b, ul[j]))
		}
	}
	best, bs := 0, s0
	if s1 < bs {
		best, bs = 1, s1
	}
	if !first {
		if s2 < bs {
			best, bs = 2, s2
		}
		if s3 < bs {
			best, bs = 3, s3
		}
		if s4 < bs {
			best = 4
		}
	}
	o := len(dst) + 1
	dst = append(dst, byte(best))
	dst = append(dst, cur...)
	r := dst[o : o+n]
	rx := r[k:]
	rx = rx[:len(x)]
	switch best {
	case 1:
		for j, v := range x {
			rx[j] = v - left[j]
		}
	case 2:
		for i := range r {
			r[i] -= prev[i]
		}
	case 3:
		for i := 0; i < k; i++ {
			r[i] -= prev[i] >> 1
		}
		for j, v := range x {
			rx[j] = v - byte((uint(left[j])+uint(up[j]))>>1)
		}
	case 4:
		for i := 0; i < k; i++ {
			r[i] -= prev[i]
		}
		for j, v := range x {
			rx[j] = v - paethPred(left[j], up[j], ul[j])
		}
	}
	return dst
}

// absB is |int8(v)|.
func absB(v byte) int {
	x := int(int8(v))
	m := x >> 63
	return (x ^ m) - m
}

// paethPred is the Paeth predictor, branch-free (selection by masks: the
// compiler leaves branches in the comparison form, and they mispredict on
// noisy rows).
func paethPred(a, b, c byte) byte {
	ia, ib, ic := int32(a), int32(b), int32(c)
	pa, pb := ib-ic, ia-ic
	pc := pa + pb
	pa, pb, pc = max(pa, -pa), max(pb, -pb), max(pc, -pc)
	bc := ib ^ ((ib ^ ic) & ((pc - pb) >> 31))                 // c if pc < pb
	return byte(ia ^ ((ia ^ bc) & ((min(pb, pc) - pa) >> 31))) // bc if pa > min
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func opaque8(m *image.NRGBA) bool {
	b := m.Bounds()
	for y := 0; y < b.Dy(); y++ {
		r := m.Pix[y*m.Stride : y*m.Stride+4*b.Dx()]
		for i := 3; i < len(r); i += 4 {
			if r[i] != 0xff {
				return false
			}
		}
	}
	return true
}

func opaque16(m *image.NRGBA64) bool {
	b := m.Bounds()
	for y := 0; y < b.Dy(); y++ {
		r := m.Pix[y*m.Stride : y*m.Stride+8*b.Dx()]
		for i := 6; i < len(r); i += 8 {
			if r[i] != 0xff || r[i+1] != 0xff {
				return false
			}
		}
	}
	return true
}
