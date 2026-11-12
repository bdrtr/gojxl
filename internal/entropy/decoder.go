// Package entropy implements the JPEG XL entropy decoder: ANS and prefix
// codes, hybrid integer coding, LZ77 and context clustering.
package entropy

import (
	"errors"

	"gojxl/internal/bitio"
)

var (
	errInvalidHisto  = errors.New("jxl: invalid entropy histogram")
	errInvalidConfig = errors.New("jxl: invalid hybrid uint config")
	errClusterHole   = errors.New("jxl: invalid context map")
	errLZ77          = errors.New("jxl: invalid lz77 stream")
	// ErrANSFinalState is returned when the ANS stream ends in a wrong state.
	ErrANSFinalState = errors.New("jxl: invalid ANS final state")
)

// UintConfig is a hybrid integer configuration.
type UintConfig struct {
	SplitExp uint32
	Split    uint32
	MsbInTok uint32
	LsbInTok uint32
}

func addLog2Ceil(x uint32) uint {
	// ceil(log2(x+1))
	n := uint(0)
	for (uint64(1) << n) < uint64(x)+1 {
		n++
	}
	return n
}

func readUintConfig(br *bitio.Reader, logAlpha uint32) (UintConfig, error) {
	se := br.Read(addLog2Ceil(logAlpha))
	var msb, lsb uint32
	if se != logAlpha {
		msb = br.Read(addLog2Ceil(se))
		if msb > se {
			return UintConfig{}, errInvalidConfig
		}
		lsb = br.Read(addLog2Ceil(se - msb))
	}
	if msb+lsb > se || se > 31 {
		return UintConfig{}, errInvalidConfig
	}
	return UintConfig{SplitExp: se, Split: 1 << se, MsbInTok: msb, LsbInTok: lsb}, nil
}

// readUint decodes the rest of a hybrid integer given its token.
func (c *UintConfig) readUint(br *bitio.Reader, token uint32) uint32 {
	if token < c.Split {
		return token
	}
	ml := c.MsbInTok + c.LsbInTok
	n := c.SplitExp - ml + ((token - c.Split) >> ml)
	n &= 31
	low := token & ((1 << c.LsbInTok) - 1)
	token >>= c.LsbInTok
	token &= (1 << c.MsbInTok) - 1
	token |= 1 << c.MsbInTok
	rest := br.Read(uint(n))
	return uint32((((uint64(token) << n) | uint64(rest)) << c.LsbInTok) | uint64(low))
}

// Code holds decoded histograms; it is immutable and can be shared by
// several Decoders (e.g. one per group decoded in parallel).
type Code struct {
	ContextMap []uint8
	configs    []UintConfig
	usePrefix  bool
	ans        []*ansHistogram
	prefix     []*prefixHistogram

	lz77        bool
	lzMinSym    uint32
	lzMinLen    uint32
	lzLenConf   UintConfig
	numClusters int
}

// Decoder is a stateful symbol reader over a Code.
type Decoder struct {
	*Code
	state   uint32
	started bool

	// lz77 state
	window     []uint32
	numToCopy  uint32
	copyPos    uint32
	numDecoded uint32
	distMult   uint32
}

// ReadCode reads histograms for numDist contexts.
func ReadCode(br *bitio.Reader, numDist int) (*Code, error) {
	return readCode(br, numDist, true)
}

func readCode(br *bitio.Reader, numDist int, allowLZ bool) (*Code, error) {
	c := &Code{}
	if br.Bool() {
		if !allowLZ {
			return nil, errLZ77
		}
		c.lz77 = true
		c.lzMinSym = br.U32(bitio.Val(224), bitio.Val(512), bitio.Val(4096), bitio.BitsOff(15, 8))
		c.lzMinLen = br.U32(bitio.Val(3), bitio.Val(4), bitio.BitsOff(2, 5), bitio.BitsOff(8, 9))
		var err error
		c.lzLenConf, err = readUintConfig(br, 8)
		if err != nil {
			return nil, err
		}
		numDist++
	}
	cm, nc, err := readContextMap(br, numDist)
	if err != nil {
		return nil, err
	}
	c.ContextMap = cm
	c.numClusters = nc
	c.usePrefix = br.Bool()
	logAlpha := uint32(15)
	if !c.usePrefix {
		logAlpha = 5 + br.Read(2)
	}
	c.configs = make([]UintConfig, nc)
	for i := range c.configs {
		if c.configs[i], err = readUintConfig(br, logAlpha); err != nil {
			return nil, err
		}
	}
	if c.usePrefix {
		counts := make([]uint32, nc)
		for i := range counts {
			counts[i] = 1
			if br.Bool() {
				n := br.Read(4)
				counts[i] = 1 + (1 << n) + br.Read(uint(n))
			}
			if counts[i] > 1<<15 {
				return nil, errInvalidHisto
			}
		}
		c.prefix = make([]*prefixHistogram, nc)
		for i := range c.prefix {
			if c.prefix[i], err = readPrefixHistogram(br, counts[i]); err != nil {
				return nil, err
			}
		}
	} else {
		c.ans = make([]*ansHistogram, nc)
		for i := range c.ans {
			if c.ans[i], err = readANSHistogram(br, uint(logAlpha)); err != nil {
				return nil, err
			}
		}
	}
	if br.Overflowed() {
		return nil, bitio.ErrEOF
	}
	return c, nil
}

func readContextMap(br *bitio.Reader, numDist int) ([]uint8, int, error) {
	cm := make([]uint8, numDist)
	if numDist == 1 {
		return cm, 1, nil
	}
	if br.Bool() { // simple
		nbits := uint(br.Read(2))
		for i := range cm {
			cm[i] = uint8(br.Read(nbits))
		}
	} else {
		useMTF := br.Bool()
		code, err := readCode(br, 1, numDist > 2)
		if err != nil {
			return nil, 0, err
		}
		d := code.NewDecoder(br)
		for i := range cm {
			v := d.ReadUint(br, 0)
			if v > 255 {
				return nil, 0, errClusterHole
			}
			cm[i] = uint8(v)
		}
		if err := d.Finish(); err != nil {
			return nil, 0, err
		}
		if useMTF {
			var mtf [256]uint8
			for i := range mtf {
				mtf[i] = uint8(i)
			}
			for i, v := range cm {
				val := mtf[v]
				cm[i] = val
				copy(mtf[1:int(v)+1], mtf[:v])
				mtf[0] = val
			}
		}
	}
	var seen [256]bool
	maxc := 0
	for _, v := range cm {
		seen[v] = true
		if int(v) > maxc {
			maxc = int(v)
		}
	}
	for i := 0; i <= maxc; i++ {
		if !seen[i] {
			return nil, 0, errClusterHole
		}
	}
	return cm, maxc + 1, nil
}

// NewDecoder creates a decoder reading from br. For ANS codes the initial
// 32-bit state is read immediately (as libjxl does).
func (c *Code) NewDecoder(br *bitio.Reader) *Decoder {
	d := &Decoder{Code: c}
	if !c.usePrefix {
		d.state = br.Read(32)
		d.started = true
	}
	return d
}

// SetDistMultiplier sets the LZ77 distance multiplier (image width for
// modular streams).
func (d *Decoder) SetDistMultiplier(m uint32) { d.distMult = m }

// Finish checks the final ANS state.
func (d *Decoder) Finish() error {
	if !d.usePrefix && d.state != ansFinalState {
		return ErrANSFinalState
	}
	return nil
}

// UsesLZ77 reports whether LZ77 is enabled.
func (c *Code) UsesLZ77() bool { return c.lz77 }

// SingleToken returns (token, true) if the cluster always decodes to the same
// value without reading any bits.
func (c *Code) SingleToken(cluster uint8) (uint32, bool) {
	if c.lz77 {
		return 0, false
	}
	var sym int
	if c.usePrefix {
		sym = c.prefix[cluster].single
	} else {
		sym = c.ans[cluster].singleSymbol
	}
	if sym < 0 || uint32(sym) >= c.configs[cluster].Split {
		return 0, false
	}
	return uint32(sym), true
}

func (d *Decoder) readSymbol(br *bitio.Reader, cluster uint8) uint32 {
	if d.usePrefix {
		return d.prefix[cluster].readSymbol(br)
	}
	return d.ans[cluster].readSymbol(br, &d.state)
}

var specialDistances = [120][2]int8{
	{0, 1}, {1, 0}, {1, 1}, {-1, 1}, {0, 2}, {2, 0}, {1, 2}, {-1, 2}, {2, 1}, {-2, 1},
	{2, 2}, {-2, 2}, {0, 3}, {3, 0}, {1, 3}, {-1, 3}, {3, 1}, {-3, 1}, {2, 3}, {-2, 3},
	{3, 2}, {-3, 2}, {0, 4}, {4, 0}, {1, 4}, {-1, 4}, {4, 1}, {-4, 1}, {3, 3}, {-3, 3},
	{2, 4}, {-2, 4}, {4, 2}, {-4, 2}, {0, 5}, {3, 4}, {-3, 4}, {4, 3}, {-4, 3}, {5, 0},
	{1, 5}, {-1, 5}, {5, 1}, {-5, 1}, {2, 5}, {-2, 5}, {5, 2}, {-5, 2}, {4, 4}, {-4, 4},
	{3, 5}, {-3, 5}, {5, 3}, {-5, 3}, {0, 6}, {6, 0}, {1, 6}, {-1, 6}, {6, 1}, {-6, 1},
	{2, 6}, {-2, 6}, {6, 2}, {-6, 2}, {4, 5}, {-4, 5}, {5, 4}, {-5, 4}, {3, 6}, {-3, 6},
	{6, 3}, {-6, 3}, {0, 7}, {7, 0}, {1, 7}, {-1, 7}, {5, 5}, {-5, 5}, {7, 1}, {-7, 1},
	{4, 6}, {-4, 6}, {6, 4}, {-6, 4}, {2, 7}, {-2, 7}, {7, 2}, {-7, 2}, {3, 7}, {-3, 7},
	{7, 3}, {-7, 3}, {5, 6}, {-5, 6}, {6, 5}, {-6, 5}, {8, 0}, {4, 7}, {-4, 7}, {7, 4},
	{-7, 4}, {8, 1}, {8, 2}, {6, 6}, {-6, 6}, {8, 3}, {5, 7}, {-5, 7}, {7, 5}, {-7, 5},
	{8, 4}, {6, 7}, {-6, 7}, {7, 6}, {-7, 6}, {8, 5}, {7, 7}, {-7, 7}, {8, 6}, {8, 7},
}

const lzWindow = 1 << 20

// ReadUint decodes a hybrid integer in context ctx.
func (d *Decoder) ReadUint(br *bitio.Reader, ctx int) uint32 {
	return d.ReadUintClustered(br, d.ContextMap[ctx])
}

// ReadUintClustered decodes a hybrid integer for an explicit cluster.
func (d *Decoder) ReadUintClustered(br *bitio.Reader, cluster uint8) uint32 {
	if d.lz77 {
		return d.readLZ77(br, cluster)
	}
	if d.usePrefix {
		tok := d.prefix[cluster].readSymbol(br)
		return d.configs[cluster].readUint(br, tok)
	}
	// ANS fast path, manually inlined.
	br.Refill()
	buf, nbits := br.State()
	h := d.ans[cluster]
	s := d.state
	idx := s & 0xfff
	b := &h.buckets[idx>>h.logBucket]
	pos := idx & h.bucketMask
	sym := idx >> h.logBucket
	freq := uint32(b.freq0)
	if pos >= uint32(b.cutoff) {
		sym = uint32(b.aliasSymbol)
		freq ^= uint32(b.freq1XorF0)
		pos += uint32(b.offset1)
	}
	s = (s>>12)*freq + pos
	if s < 1<<16 {
		s = s<<16 | uint32(buf&0xffff)
		buf >>= 16
		nbits -= 16
	}
	d.state = s
	c := &d.configs[cluster]
	if sym < c.Split {
		br.SetState(buf, nbits)
		return sym
	}
	ml := c.MsbInTok + c.LsbInTok
	n := (c.SplitExp - ml + ((sym - c.Split) >> ml)) & 31
	low := sym & (1<<c.LsbInTok - 1)
	tok := sym >> c.LsbInTok
	tok &= 1<<c.MsbInTok - 1
	tok |= 1 << c.MsbInTok
	if nbits < uint(n) {
		br.SetState(buf, nbits)
		rest := br.Read(uint(n))
		return uint32((((uint64(tok) << n) | uint64(rest)) << c.LsbInTok) | uint64(low))
	}
	rest := buf & (1<<(n&63) - 1)
	br.SetState(buf>>(n&63), nbits-uint(n))
	return uint32((((uint64(tok) << n) | rest) << c.LsbInTok) | uint64(low))
}

// ReadHFBlock decodes the coefficient tokens of one VarDCT block (the hot
// loop of HF decoding) without a call per token: for k = k0, ...,
// len(order)-1 while nz > 0, the cluster is clusters[nzc+freq[k]+prev],
// where nzc = 2*nzCtx[(nz-1)>>lg] and prev is 1 if the previous token was
// nonzero; a nonzero token t adds UnpackSigned(t) << shift to
// coef[order[k]]. It returns the nonzero count left. ok is false (and
// nothing is read) when the stream uses LZ77 or prefix codes.
func (d *Decoder) ReadHFBlock(br *bitio.Reader, clusters []uint8, nzCtx *[63]uint16, freq []uint8, order, coef []int32, k0 int, nz int32, prev int, lg, shift uint) (left int32, ok bool) {
	if d.lz77 || d.usePrefix {
		return nz, false
	}
	ans, cfgs := d.ans, d.configs
	s := d.state
	n := len(order)
	freq = freq[:n]
	nzc := int(nzCtx[(nz-1)>>lg]) * 2
	for k := k0; k < n; k++ {
		cluster := clusters[nzc+int(freq[k])+prev]
		// ANS symbol (as in ReadUintClustered)
		br.Refill()
		buf, nbits := br.State()
		h := ans[cluster]
		idx := s & 0xfff
		b := &h.buckets[idx>>h.logBucket]
		pos := idx & h.bucketMask
		sym := idx >> h.logBucket
		fq := uint32(b.freq0)
		if pos >= uint32(b.cutoff) {
			sym = uint32(b.aliasSymbol)
			fq ^= uint32(b.freq1XorF0)
			pos += uint32(b.offset1)
		}
		s = (s>>12)*fq + pos
		if s < 1<<16 {
			s = s<<16 | uint32(buf&0xffff)
			buf >>= 16
			nbits -= 16
		}
		var u uint32
		if c := &cfgs[cluster]; sym < c.Split {
			u = sym
			br.SetState(buf, nbits)
		} else {
			ml := c.MsbInTok + c.LsbInTok
			e := (c.SplitExp - ml + ((sym - c.Split) >> ml)) & 31
			low := sym & (1<<c.LsbInTok - 1)
			tok := sym>>c.LsbInTok&(1<<c.MsbInTok-1) | 1<<c.MsbInTok
			var rest uint64
			if nbits < uint(e) {
				br.SetState(buf, nbits)
				rest = uint64(br.Read(uint(e)))
			} else {
				rest = buf & (1<<(e&63) - 1)
				br.SetState(buf>>(e&63), nbits-uint(e))
			}
			u = uint32((((uint64(tok) << e) | rest) << c.LsbInTok) | uint64(low))
		}
		if u == 0 {
			prev = 0
			continue
		}
		coef[order[k]] += bitio.UnpackSigned(u) << shift
		prev = 1
		nz--
		if nz == 0 {
			break
		}
		nzc = int(nzCtx[(nz-1)>>lg]) * 2
	}
	d.state = s
	return nz, true
}

func (d *Decoder) readLZ77(br *bitio.Reader, cluster uint8) uint32 {
	if int(d.numDecoded) >= len(d.window) && len(d.window) < lzWindow {
		// grow the window lazily (it is a ring buffer once full size)
		n := max(1024, 2*len(d.window))
		n = min(n, lzWindow)
		w := make([]uint32, n)
		copy(w, d.window)
		d.window = w
	}
	var r uint32
	if d.numToCopy > 0 {
		r = d.window[d.copyPos&(lzWindow-1)&uint32(len(d.window)-1)]
		d.copyPos++
		d.numToCopy--
	} else {
		tok := d.readSymbol(br, cluster)
		if tok >= d.lzMinSym {
			if d.numDecoded == 0 {
				// invalid stream; treat as zero
				d.numToCopy = 0
				return 0
			}
			distCluster := d.ContextMap[len(d.ContextMap)-1]
			n := d.lzLenConf.readUint(br, tok-d.lzMinSym) + d.lzMinLen
			dtok := d.readSymbol(br, distCluster)
			dist := d.configs[distCluster].readUint(br, dtok)
			if d.distMult != 0 {
				if dist < 120 {
					sd := specialDistances[dist]
					v := int32(sd[0]) + int32(d.distMult)*int32(sd[1])
					v--
					if v < 0 {
						v = 0
					}
					dist = uint32(v)
				} else {
					dist -= 120
				}
			}
			if dist > lzWindow-1 {
				dist = lzWindow - 1
			}
			dist++
			if dist > d.numDecoded {
				dist = d.numDecoded
			}
			d.copyPos = d.numDecoded - dist
			d.numToCopy = n
			r = d.window[d.copyPos&(lzWindow-1)&uint32(len(d.window)-1)]
			d.copyPos++
			d.numToCopy--
		} else {
			r = d.configs[cluster].readUint(br, tok)
		}
	}
	d.window[d.numDecoded&(lzWindow-1)&uint32(len(d.window)-1)] = r
	d.numDecoded++
	return r
}

// ReadPermutation decodes a Lehmer-coded permutation of size elements,
// skipping the first skip entries (used by TOC and coefficient orders).
func ReadPermutation(br *bitio.Reader, d *Decoder, size, skip int) ([]int, error) {
	end := int(d.ReadUint(br, coeffOrderCtx(uint32(size))))
	if end > size-skip {
		return nil, errInvalidHisto
	}
	lehmer := make([]uint32, size)
	prev := uint32(0)
	for i := skip; i < skip+end; i++ {
		v := d.ReadUint(br, coeffOrderCtx(prev))
		if int(v) >= size-i {
			return nil, errInvalidHisto
		}
		lehmer[i] = v
		prev = v
	}
	return decodeLehmer(lehmer, skip+end), nil
}

// decodeLehmer turns a Lehmer code (entry i: index of the value among the
// unused ones, in increasing order) into the permutation. Only the first m
// entries may be nonzero: they are found with a Fenwick tree of unused
// counts (O(m log n); removing from a list costs O(n) each, quadratic for
// the 65536-entry orders of large transforms), the rest take the unused
// values in order.
func decodeLehmer(lehmer []uint32, m int) []int {
	n := len(lehmer)
	perm := make([]int, n)
	bit := make([]int32, n+1)
	for i := 1; i <= n; i++ {
		bit[i]++
		if j := i + i&-i; j <= n {
			bit[j] += bit[i]
		}
	}
	top := 1
	for top*2 <= n {
		top *= 2
	}
	used := make([]bool, n)
	for i := 0; i < m; i++ {
		k := int32(lehmer[i]) + 1 // k-th unused value
		pos := 0
		for step := top; step > 0; step >>= 1 {
			if p := pos + step; p <= n && bit[p] < k {
				pos = p
				k -= bit[p]
			}
		}
		perm[i] = pos
		used[pos] = true
		for j := pos + 1; j <= n; j += j & -j {
			bit[j]--
		}
	}
	v := 0
	for i := m; i < n; i++ {
		for used[v] {
			v++
		}
		perm[i] = v
		v++
	}
	return perm
}

func coeffOrderCtx(x uint32) int {
	// min(7, ceil(log2(x+1)))
	n := int(addLog2Ceil(x))
	if n > 7 {
		n = 7
	}
	return n
}

// ReadContextMap reads a clustering map for numDist contexts.
func ReadContextMap(br *bitio.Reader, numDist int) ([]uint8, int, error) {
	return readContextMap(br, numDist)
}
