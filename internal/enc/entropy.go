package enc

import (
	"math/bits"

	"gojxl/internal/bitio"
)

type uintCfg struct{ se, msb, lsb uint32 }

var defaultCfg = uintCfg{4, 2, 0}

// encode splits v into (token, extra bit count, extra bits).
func (c uintCfg) encode(v uint32) (tok uint32, nb uint, bv uint32) {
	if v < 1<<c.se {
		return v, 0, 0
	}
	n := uint32(bits.Len32(v) - 1)
	m := v - (1 << n)
	tok = (1 << c.se) + ((n - c.se) << (c.msb + c.lsb)) + ((m >> (n - c.msb)) << c.lsb) + (m & (1<<c.lsb - 1))
	nb = uint(n - c.msb - c.lsb)
	bv = (m >> c.lsb) & (1<<nb - 1)
	return
}

func addLog2Ceil(x uint32) uint {
	n := uint(0)
	for (uint64(1) << n) < uint64(x)+1 {
		n++
	}
	return n
}

func (c uintCfg) write(w *bitio.Writer, logAlpha uint32) {
	w.Write(addLog2Ceil(logAlpha), uint64(c.se))
	if c.se != logAlpha {
		w.Write(addLog2Ceil(c.se), uint64(c.msb))
		w.Write(addLog2Ceil(c.se-c.msb), uint64(c.lsb))
	}
}

// histograms accumulates token counts per context.
type histograms struct {
	cfg    uintCfg
	cfgs   []uintCfg // optional per-histogram configs (ANS only)
	counts [][]uint64
}

func newHistograms(n int, cfg uintCfg) *histograms {
	h := &histograms{cfg: cfg, counts: make([][]uint64, n)}
	for i := range h.counts {
		h.counts[i] = make([]uint64, 40)
	}
	return h
}

func (h *histograms) add(ctx int, v uint32) {
	tok, _, _ := h.cfg.encode(v)
	c := h.counts[ctx]
	if int(tok) >= len(c) {
		nc := make([]uint64, tok+16)
		copy(nc, c)
		c = nc
		h.counts[ctx] = c
	}
	c[tok]++
}

// mergeWorkerHists merges per-worker histograms into the first one
// present: a worker may have got no work at all (when the others took it
// all), worker 0 included. hs must not be all nil.
func mergeWorkerHists(hs []*histograms) *histograms {
	var hf *histograms
	for _, h := range hs {
		switch {
		case h == nil:
		case hf == nil:
			hf = h
		default:
			hf.merge(h)
		}
	}
	return hf
}

func (h *histograms) merge(o *histograms) {
	for i, c := range o.counts {
		d := h.counts[i]
		if len(c) > len(d) {
			nd := make([]uint64, len(c))
			copy(nd, d)
			d = nd
			h.counts[i] = d
		}
		for k, v := range c {
			d[k] += v
		}
	}
}

// code is a written entropy code ready to emit symbols.
type code struct {
	cfg   uintCfg
	codes []*prefixCode
}

func (c *code) write(w *bitio.Writer, ctx int, v uint32) {
	tok, nb, bv := c.cfg.encode(v)
	pc := c.codes[ctx]
	w.Write(uint(pc.lens[tok]), uint64(pc.codes[tok]))
	w.Write(nb, uint64(bv))
}

// writeCode writes histograms (prefix codes, no LZ77, identity clustering).
func writeCode(w *bitio.Writer, h *histograms) *code {
	n := len(h.counts)
	w.WriteBool(false) // lz77
	if n > 1 {
		writeIdentityContextMap(w, n)
	}
	w.WriteBool(true) // prefix codes
	for i := 0; i < n; i++ {
		h.cfg.write(w, 15)
	}
	alphas := make([]int, n)
	for i, c := range h.counts {
		a := 1
		for k := len(c) - 1; k >= 0; k-- {
			if c[k] > 0 {
				a = k + 1
				break
			}
		}
		alphas[i] = a
		if a == 1 {
			w.WriteBool(false)
		} else {
			m := a - 1
			nb := bits.Len(uint(m)) - 1
			w.WriteBool(true)
			w.Write(4, uint64(nb))
			w.Write(uint(nb), uint64(m-(1<<nb)))
		}
	}
	cd := &code{cfg: h.cfg, codes: make([]*prefixCode, n)}
	for i, c := range h.counts {
		a := alphas[i]
		if a == 1 {
			cd.codes[i] = &prefixCode{lens: make([]uint8, 1), codes: make([]uint16, 1)}
			continue
		}
		f := make([]uint64, a)
		copy(f, c)
		cd.codes[i] = writePrefixCode(w, f, a)
	}
	return cd
}

func writeIdentityContextMap(w *bitio.Writer, n int) {
	if n <= 8 {
		w.WriteBool(true) // simple
		nb := ceilLog2(n)
		w.Write(2, uint64(nb))
		for i := 0; i < n; i++ {
			w.Write(uint(nb), uint64(i))
		}
		return
	}
	w.WriteBool(false) // complex
	w.WriteBool(false) // no MTF
	h := newHistograms(1, uintCfg{8, 0, 0})
	for i := 0; i < n; i++ {
		h.add(0, uint32(i))
	}
	c := writeCode(w, h)
	for i := 0; i < n; i++ {
		c.write(w, 0, uint32(i))
	}
}
