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
	"runtime"
	"sync"
)

// Encode writes img as PNG like image/png (opaque NRGBA images become RGB),
// but filters and compresses strips of rows in parallel: each strip is an
// independent run of deflate blocks ended by a sync flush, so the strips
// concatenate into one zlib stream (the Adler-32 sums are combined).
// Supported: *image.Gray, *image.Gray16, *image.NRGBA, *image.NRGBA64.
func Encode(w io.Writer, img image.Image, level int) error {
	b := img.Bounds()
	W, H := b.Dx(), b.Dy()
	if W <= 0 || H <= 0 {
		return errors.New("png: empty image")
	}
	var (
		ctype, depth, bpp int
		row               func(y int, dst []byte)
	)
	switch m := img.(type) {
	case *image.Gray:
		ctype, depth, bpp = 0, 8, 1
		row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
	case *image.Gray16:
		ctype, depth, bpp = 0, 16, 2
		row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
	case *image.NRGBA:
		depth = 8
		if opaque8(m) {
			ctype, bpp = 2, 3
			row = func(y int, dst []byte) {
				src := m.Pix[y*m.Stride : y*m.Stride+4*W]
				for x := 0; x < W; x++ {
					dst[3*x], dst[3*x+1], dst[3*x+2] = src[4*x], src[4*x+1], src[4*x+2]
				}
			}
		} else {
			ctype, bpp = 6, 4
			row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
		}
	case *image.NRGBA64:
		depth = 16
		if opaque16(m) {
			ctype, bpp = 2, 6
			row = func(y int, dst []byte) {
				src := m.Pix[y*m.Stride : y*m.Stride+8*W]
				for x := 0; x < W; x++ {
					copy(dst[6*x:6*x+6], src[8*x:8*x+6])
				}
			}
		} else {
			ctype, bpp = 6, 8
			row = func(y int, dst []byte) { copy(dst, m.Pix[y*m.Stride:]) }
		}
	default:
		return errors.New("pngx: unsupported image type")
	}
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
			var flt [5][]byte
			for i := range flt {
				flt[i] = make([]byte, rowLen+1)
			}
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
					raw = append(raw, filterRow(&flt, cur, prev, bpp, y == 0)...)
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
		return werr
	}
	out := &bytes.Buffer{}
	out.WriteString("\x89PNG\r\n\x1a\n")
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], uint32(W))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(H))
	ihdr[8], ihdr[9] = byte(depth), byte(ctype)
	writeChunk(out, "IHDR", ihdr[:])
	ad := uint32(1)
	for i, s := range strips {
		d := s.data
		if i == 0 {
			d = append([]byte{0x78, 0x01}, d...)
		}
		ad = adler32Combine(ad, s.adler, s.rawLen)
		if i == nstrips-1 {
			d = binary.BigEndian.AppendUint32(d, ad)
		}
		writeChunk(out, "IDAT", d)
	}
	writeChunk(out, "IEND", nil)
	_, err := w.Write(out.Bytes())
	return err
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

// filterRow picks the filter with the smallest sum of absolute values (as
// image/png and libpng do) and returns filter byte plus filtered row.
func filterRow(flt *[5][]byte, cur, prev []byte, bpp int, first bool) []byte {
	n := len(cur)
	best, bestSum := 0, 1<<62
	try := func(f int) {
		d := flt[f]
		d[0] = byte(f)
		r := d[1:]
		s := 0
		switch f {
		case 0:
			copy(r, cur)
		case 1:
			copy(r[:bpp], cur[:bpp])
			for i := bpp; i < n; i++ {
				r[i] = cur[i] - cur[i-bpp]
			}
		case 2:
			for i := 0; i < n; i++ {
				r[i] = cur[i] - prev[i]
			}
		case 3:
			for i := 0; i < bpp; i++ {
				r[i] = cur[i] - prev[i]>>1
			}
			for i := bpp; i < n; i++ {
				r[i] = cur[i] - byte((int(cur[i-bpp])+int(prev[i]))>>1)
			}
		case 4:
			for i := 0; i < bpp; i++ {
				r[i] = cur[i] - prev[i]
			}
			for i := bpp; i < n; i++ {
				a, b, c := int(cur[i-bpp]), int(prev[i]), int(prev[i-bpp])
				p := a + b - c
				pa, pb, pc := abs(p-a), abs(p-b), abs(p-c)
				pr := c
				if pa <= pb && pa <= pc {
					pr = a
				} else if pb <= pc {
					pr = b
				}
				r[i] = cur[i] - byte(pr)
			}
		}
		for _, v := range r {
			s += abs(int(int8(v)))
		}
		if s < bestSum {
			best, bestSum = f, s
		}
	}
	try(0)
	try(1)
	if !first {
		try(2)
		try(3)
		try(4)
	}
	return flt[best]
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
