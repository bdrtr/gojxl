package pngx

import (
	"bytes"
	"math/rand"
	"testing"
)

// filterRowRef (previous implementation) picks the filter with the smallest sum of absolute values (as
// image/png and libpng do) and returns filter byte plus filtered row.
func filterRowRef(flt *[5][]byte, cur, prev []byte, bpp int, first bool) []byte {
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

func TestFilterRowMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for it := 0; it < 3000; it++ {
		bpp := []int{1, 2, 3, 4, 6, 8}[rng.Intn(6)]
		n := bpp * (1 + rng.Intn(40))
		cur, prev := make([]byte, n), make([]byte, n)
		smooth := rng.Intn(2) == 0
		for i := range cur {
			if smooth && i >= bpp {
				cur[i] = cur[i-bpp] + byte(rng.Intn(5)-2)
				prev[i] = prev[i-bpp] + byte(rng.Intn(5)-2)
			} else {
				cur[i], prev[i] = byte(rng.Intn(256)), byte(rng.Intn(256))
			}
		}
		first := rng.Intn(8) == 0
		if first {
			clear(prev)
		}
		var flt [5][]byte
		for i := range flt {
			flt[i] = make([]byte, n+1)
		}
		want := filterRowRef(&flt, cur, prev, bpp, first)
		got := filterRow([]byte{0xAA}, cur, prev, bpp, first)
		if got[0] != 0xAA || !bytes.Equal(got[1:], want) {
			t.Fatalf("bpp %d n %d first %v: filter %d vs %d", bpp, n, first, got[1], want[0])
		}
		// the filtered row must unfilter back to cur
		r := append([]byte(nil), got[2:]...)
		if err := unfilterRow(got[1], r, prev, bpp); err != nil || !bytes.Equal(r, cur) {
			t.Fatalf("unfilter mismatch (filter %d)", got[1])
		}
	}
}

func benchRows() (rows [][]byte) {
	rng := rand.New(rand.NewSource(2))
	const w, bpp = 2048, 3
	prev := make([]byte, w*bpp)
	for y := 0; y < 64; y++ {
		r := make([]byte, w*bpp)
		for i := range r {
			if i >= bpp {
				r[i] = byte((int(r[i-bpp])+int(prev[i]))/2) + byte(rng.Intn(9)-4)
			} else {
				r[i] = byte(rng.Intn(256))
			}
		}
		rows = append(rows, r)
		prev = r
	}
	return rows
}

func BenchmarkFilterRowRef(b *testing.B) {
	rows := benchRows()
	var flt [5][]byte
	for i := range flt {
		flt[i] = make([]byte, len(rows[0])+1)
	}
	b.SetBytes(int64(len(rows[0]) * (len(rows) - 1)))
	for it := 0; it < b.N; it++ {
		for y := 1; y < len(rows); y++ {
			filterRowRef(&flt, rows[y], rows[y-1], 3, false)
		}
	}
}

func BenchmarkFilterRow(b *testing.B) {
	rows := benchRows()
	dst := make([]byte, 0, len(rows[0])+1)
	b.SetBytes(int64(len(rows[0]) * (len(rows) - 1)))
	for it := 0; it < b.N; it++ {
		for y := 1; y < len(rows); y++ {
			dst = filterRow(dst[:0], rows[y], rows[y-1], 3, false)
		}
	}
}
