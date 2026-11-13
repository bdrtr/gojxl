package jxl

import (
	"math"

	"github.com/bdrtr/gojxl/internal/bitio"
)

// Quant table indices.
const numQuantTables = 17

var qtRequiredX = [numQuantTables]int{1, 1, 1, 1, 2, 4, 1, 1, 2, 1, 1, 8, 4, 16, 8, 32, 16}
var qtRequiredY = [numQuantTables]int{1, 1, 1, 1, 2, 4, 2, 4, 4, 1, 1, 8, 8, 16, 16, 32, 32}

const (
	qmLibrary = 0
	qmID      = 1
	qmDCT2    = 2
	qmDCT4    = 3
	qmDCT4X8  = 4
	qmAFV     = 5
	qmDCT     = 6
	qmRAW     = 7
)

type quantEncoding struct {
	mode      int
	bands     [3][]float32 // dct params (distance bands)
	bands4x4  [3][]float32 // afv 4x4 params
	idw       [3][3]float32
	dct2w     [3][6]float32
	dct4mul   [3][2]float32
	dct4x8mul [3]float32
	afvw      [3][9]float32
	rawDen    float32
	raw       []int32
}

var seqA = []float32{-1.025, -0.78, -0.65012, -0.19041574, -0.20819396, -0.421064, -0.32733846}
var seqB = []float32{-0.30419582, -0.36330363, -0.3566038, -0.34430745, -0.33699593, -0.30180866, -0.27321684}
var seqC = []float32{-1.2, -1.2, -0.8, -0.7, -0.7, -0.4, -0.5}

var dct4x8Params = [3][]float32{
	{2198.0505, -0.96269625, -0.7619425, -0.65511405},
	{764.36554, -0.926302, -0.967523, -0.2784529},
	{527.10754, -1.4594386, -1.4500821, -1.5843723},
}
var dct4Params = [3][]float32{
	{2200.0, 0.0, 0.0, 0.0},
	{392.0, 0.0, 0.0, 0.0},
	{112.0, -0.25, -0.25, -0.5},
}

func seqParams(a, b, c float32) [3][]float32 {
	return [3][]float32{
		append([]float32{a}, seqA...),
		append([]float32{b}, seqB...),
		append([]float32{c}, seqC...),
	}
}

func defaultQuantEncoding(t int) quantEncoding {
	q := quantEncoding{mode: qmDCT}
	switch t {
	case 0:
		q.bands = [3][]float32{{3150.0, 0.0, -0.4, -0.4, -0.4, -2.0}, {560.0, 0.0, -0.3, -0.3, -0.3, -0.3}, {512.0, -2.0, -1.0, 0.0, -1.0, -2.0}}
	case 1:
		q.mode = qmID
		q.idw = [3][3]float32{{280.0, 3160.0, 3160.0}, {60.0, 864.0, 864.0}, {18.0, 200.0, 200.0}}
	case 2:
		q.mode = qmDCT2
		q.dct2w = [3][6]float32{{3840.0, 2560.0, 1280.0, 640.0, 480.0, 300.0}, {960.0, 640.0, 320.0, 180.0, 140.0, 120.0}, {640.0, 320.0, 128.0, 64.0, 32.0, 16.0}}
	case 3:
		q.mode = qmDCT4
		q.bands = dct4Params
		q.dct4mul = [3][2]float32{{1, 1}, {1, 1}, {1, 1}}
	case 4:
		q.bands = [3][]float32{
			{8996.873, -1.3000778, -0.4942453, -0.43909377, -0.6350102, -0.9017726, -1.6162099},
			{3191.4836, -0.67424583, -0.80745816, -0.4492584, -0.3586544, -0.3132239, -0.37615025},
			{1157.504, -2.0531423, -1.4, -0.5068713, -0.4270873, -1.4856834, -4.920914}}
	case 5:
		q.bands = [3][]float32{
			{15718.408, -1.025, -0.98, -0.9012, -0.4, -0.48819396, -0.421064, -0.27},
			{7305.7637, -0.8041958, -0.76330364, -0.5566038, -0.49785304, -0.43699592, -0.40180868, -0.27321684},
			{3803.5317, -3.0607336, -2.041327, -2.023565, -0.54953897, -0.4, -0.4, -0.3}}
	case 6:
		q.bands = [3][]float32{
			{7240.7734, -0.7, -0.7, -0.2, -0.2, -0.2, -0.5},
			{1448.1547, -0.5, -0.5, -0.5, -0.2, -0.2, -0.2},
			{506.85413, -1.4, -0.2, -0.5, -0.5, -1.5, -3.6}}
	case 7:
		q.bands = [3][]float32{
			{16283.249, -1.7812846, -1.6309059, -1.0382179, -0.85, -0.7, -0.9, -1.2360638},
			{5089.1577, -0.3200494, -0.3536285, -0.3034, -0.61, -0.5, -0.5, -0.6},
			{3397.7761, -0.32132736, -0.3450762, -0.7034, -0.9, -1.0, -1.0, -1.1754606}}
	case 8:
		q.bands = [3][]float32{
			{13844.971, -0.971138, -0.658, -0.42026, -0.22712, -0.2206, -0.226, -0.6},
			{4798.964, -0.6112531, -0.8377079, -0.7901486, -0.26927274, -0.38272768, -0.22924222, -0.20719099},
			{1807.2369, -1.2, -1.2, -0.7, -0.7, -0.7, -0.4, -0.5}}
	case 9:
		q.mode = qmDCT4X8
		q.bands = dct4x8Params
		q.dct4x8mul = [3]float32{1, 1, 1}
	case 10:
		q.mode = qmAFV
		q.bands = dct4x8Params
		q.bands4x4 = dct4Params
		q.afvw = [3][9]float32{
			{3072.0, 3072.0, 256.0, 256.0, 256.0, 414.0, 0.0, 0.0, 0.0},
			{1024.0, 1024.0, 50.0, 50.0, 50.0, 58.0, 0.0, 0.0, 0.0},
			{384.0, 384.0, 12.0, 12.0, 12.0, 22.0, -0.25, -0.25, -0.25}}
	case 11:
		q.bands = seqParams(23966.166, 8380.191, 4493.024)
	case 12:
		q.bands = seqParams(15358.898, 5597.3604, 2919.9617)
	case 13:
		q.bands = seqParams(47932.332, 16760.383, 8986.048)
	case 14:
		q.bands = seqParams(30717.797, 11194.721, 5839.9233)
	case 15:
		q.bands = seqParams(95864.664, 33520.766, 17972.096)
	case 16:
		q.bands = seqParams(61435.594, 24209.441, 12979.847)
	}
	return q
}

func readDctParams(br *bitio.Reader) ([3][]float32, error) {
	var out [3][]float32
	n := int(br.Read(4)) + 1
	for c := 0; c < 3; c++ {
		out[c] = make([]float32, n)
		for i := 0; i < n; i++ {
			v, err := br.F16()
			if err != nil {
				return out, err
			}
			out[c][i] = v
		}
		if out[c][0] < 1e-8 {
			return out, invalid("dct param band")
		}
		out[c][0] *= 64
	}
	return out, nil
}

func (fd *frameDecoder) readQuantEncoding(br *bitio.Reader, idx int) (quantEncoding, error) {
	var q quantEncoding
	rs := qtRequiredX[idx] * qtRequiredY[idx]
	q.mode = int(br.Read(3))
	var err error
	f16 := func() float32 {
		v, e := br.F16()
		if e != nil {
			err = e
		}
		return v
	}
	switch q.mode {
	case qmLibrary:
		return defaultQuantEncoding(idx), nil
	case qmID:
		if rs != 1 {
			return q, invalid("quant mode")
		}
		for c := 0; c < 3; c++ {
			for i := 0; i < 3; i++ {
				q.idw[c][i] = f16() * 64
			}
		}
	case qmDCT2:
		if rs != 1 {
			return q, invalid("quant mode")
		}
		for c := 0; c < 3; c++ {
			for i := 0; i < 6; i++ {
				q.dct2w[c][i] = f16() * 64
			}
		}
	case qmDCT4X8:
		if rs != 1 {
			return q, invalid("quant mode")
		}
		for c := 0; c < 3; c++ {
			q.dct4x8mul[c] = f16()
		}
		if err == nil {
			q.bands, err = readDctParams(br)
		}
	case qmDCT4:
		if rs != 1 {
			return q, invalid("quant mode")
		}
		for c := 0; c < 3; c++ {
			q.dct4mul[c][0] = f16()
			q.dct4mul[c][1] = f16()
		}
		if err == nil {
			q.bands, err = readDctParams(br)
		}
	case qmAFV:
		if rs != 1 {
			return q, invalid("quant mode")
		}
		for c := 0; c < 3; c++ {
			for i := 0; i < 9; i++ {
				q.afvw[c][i] = f16()
				if i < 6 {
					q.afvw[c][i] *= 64
				}
			}
		}
		if err == nil {
			q.bands, err = readDctParams(br)
		}
		if err == nil {
			q.bands4x4, err = readDctParams(br)
		}
	case qmDCT:
		q.bands, err = readDctParams(br)
	case qmRAW:
		q.rawDen = f16()
		if err != nil {
			return q, err
		}
		if q.rawDen < 1e-8 {
			return q, invalid("raw quant den")
		}
		w, h := qtRequiredX[idx]*8, qtRequiredY[idx]*8
		img := &ModularImage{BitDepth: 8}
		for c := 0; c < 3; c++ {
			img.Channels = append(img.Channels, &Channel{W: w, H: h})
		}
		stream := 1 + 3*fd.fh.NumLFGroups + idx
		if err := decodeModular(br, img, stream, &fd.global, 0, false); err != nil {
			return q, err
		}
		if err := img.undoTransforms(); err != nil {
			return q, err
		}
		q.raw = make([]int32, 0, 3*w*h)
		for c := 0; c < 3; c++ {
			for _, v := range img.Channels[c].Pix {
				if v <= 0 {
					return q, invalid("raw quant table")
				}
				q.raw = append(q.raw, v)
			}
		}
	}
	return q, err
}

func qmMult(v float32) float32 {
	if v > 0 {
		return 1 + v
	}
	return 1 / (1 - v)
}

func powf(a, b float32) float32 { return float32(math.Pow(float64(a), float64(b))) }

// getQuantWeights computes ROWS x COLS weights from distance bands.
func getQuantWeights(rows, cols int, params []float32) ([]float32, error) {
	nb := len(params)
	bands := make([]float32, nb)
	bands[0] = params[0]
	if bands[0] < 1e-8 {
		return nil, invalid("distance bands")
	}
	for i := 1; i < nb; i++ {
		bands[i] = bands[i-1] * qmMult(params[i])
		if bands[i] < 1e-8 {
			return nil, invalid("distance bands")
		}
	}
	out := make([]float32, rows*cols)
	scale := float32(nb-1) / (float32(math.Sqrt2) + 1e-6)
	rcpcol := scale / float32(cols-1)
	rcprow := scale / float32(rows-1)
	for y := 0; y < rows; y++ {
		dy := float32(y) * rcprow
		dy2 := dy * dy
		for x := 0; x < cols; x++ {
			dx := float32(x) * rcpcol
			d := float32(math.Sqrt(float64(dx*dx + dy2)))
			if nb == 1 {
				out[y*cols+x] = bands[0]
				continue
			}
			idx := int(d)
			if idx+1 >= nb {
				idx = nb - 2
			}
			frac := d - float32(idx)
			a, b := bands[idx], bands[idx+1]
			out[y*cols+x] = a * powf(b/a, frac)
		}
	}
	return out, nil
}

func interpolateBands(pos, maxv float32, bands []float32) float32 {
	sp := pos * float32(len(bands)-1) / maxv
	idx := int(sp)
	if idx+1 >= len(bands) {
		idx = len(bands) - 2
	}
	a, b := bands[idx], bands[idx+1]
	return a * powf(b/a, sp-float32(idx))
}

var afvFreqs = [16]float32{
	0, 0, 0.8517778890324296, 5.37778436506804, 0, 0, 4.734747904497923, 5.449245381693219,
	1.6598270267479331, 4, 7.275749096817861, 10.423227632456525, 2.662932286148962,
	7.630657783650829, 8.962388608184032, 12.97166202570235,
}

// computeQuantTable returns the dequantization multipliers (1/weight) for
// the 3 channels in the "wide" coefficient layout (rows=8*rx, cols=8*ry).
func computeQuantTable(q *quantEncoding, idx int) ([3][]float32, error) {
	var res [3][]float32
	wrows := 8 * qtRequiredX[idx]
	wcols := 8 * qtRequiredY[idx]
	num := wrows * wcols
	w := make([]float32, 3*num)
	switch q.mode {
	case qmID:
		for c := 0; c < 3; c++ {
			for i := 0; i < 64; i++ {
				w[64*c+i] = q.idw[c][0]
			}
			w[64*c+1] = q.idw[c][1]
			w[64*c+8] = q.idw[c][1]
			w[64*c+9] = q.idw[c][2]
		}
	case qmDCT2:
		for c := 0; c < 3; c++ {
			s := c * 64
			d := q.dct2w[c]
			w[s] = 1
			w[s+1], w[s+8] = d[0], d[0]
			w[s+9] = d[1]
			for y := 0; y < 2; y++ {
				for x := 0; x < 2; x++ {
					w[s+y*8+x+2] = d[2]
					w[s+(y+2)*8+x] = d[2]
					w[s+(y+2)*8+x+2] = d[3]
				}
			}
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					w[s+y*8+x+4] = d[4]
					w[s+(y+4)*8+x] = d[4]
					w[s+(y+4)*8+x+4] = d[5]
				}
			}
		}
	case qmDCT4:
		for c := 0; c < 3; c++ {
			w4, err := getQuantWeights(4, 4, q.bands[c])
			if err != nil {
				return res, err
			}
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					w[c*num+y*8+x] = w4[(y/2)*4+x/2]
				}
			}
			w[c*num+1] /= q.dct4mul[c][0]
			w[c*num+8] /= q.dct4mul[c][0]
			w[c*num+9] /= q.dct4mul[c][1]
		}
	case qmDCT4X8:
		for c := 0; c < 3; c++ {
			w48, err := getQuantWeights(4, 8, q.bands[c])
			if err != nil {
				return res, err
			}
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					w[c*num+y*8+x] = w48[(y/2)*8+x]
				}
			}
			w[c*num+8] /= q.dct4x8mul[c]
		}
	case qmDCT:
		for c := 0; c < 3; c++ {
			wc, err := getQuantWeights(wrows, wcols, q.bands[c])
			if err != nil {
				return res, err
			}
			copy(w[c*num:], wc)
		}
	case qmRAW:
		// libjxl: weights = 1/(den*q); the multiplier table is 1/weights.
		for i := range w {
			w[i] = q.rawDen * float32(q.raw[i])
		}
	case qmAFV:
		lo := float32(0.8517778890324296)
		hi := float32(12.97166202570235) - lo + 1e-6
		for c := 0; c < 3; c++ {
			w48, err := getQuantWeights(4, 8, q.bands[c])
			if err != nil {
				return res, err
			}
			w44, err := getQuantWeights(4, 4, q.bands4x4[c])
			if err != nil {
				return res, err
			}
			var bands [4]float32
			bands[0] = q.afvw[c][5]
			if bands[0] < 1e-8 {
				return res, invalid("afv bands")
			}
			for i := 1; i < 4; i++ {
				bands[i] = bands[i-1] * qmMult(q.afvw[c][i+5])
				if bands[i] < 1e-8 {
					return res, invalid("afv bands")
				}
			}
			s := c * 64
			set := func(x, y int, v float32) { w[s+y*8+x] = v }
			w[s] = 1
			set(0, 1, q.afvw[c][0])
			set(1, 0, q.afvw[c][1])
			set(0, 2, q.afvw[c][2])
			set(2, 0, q.afvw[c][3])
			set(2, 2, q.afvw[c][4])
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					if x < 2 && y < 2 {
						continue
					}
					set(2*x, 2*y, interpolateBands(afvFreqs[y*4+x]-lo, hi, bands[:]))
				}
			}
			for y := 0; y < 4; y++ {
				for x := 0; x < 8; x++ {
					if x == 0 && y == 0 {
						continue
					}
					w[s+(2*y+1)*8+x] = w48[y*8+x]
				}
			}
			for y := 0; y < 4; y++ {
				for x := 0; x < 4; x++ {
					if x == 0 && y == 0 {
						continue
					}
					w[s+(2*y)*8+2*x+1] = w44[y*4+x]
				}
			}
		}
	}
	for c := 0; c < 3; c++ {
		res[c] = make([]float32, num)
		for i := 0; i < num; i++ {
			v := w[c*num+i]
			if q.mode == qmRAW {
				res[c][i] = v
				continue
			}
			if v >= 1/1e-8 || v < 1e-8 {
				return res, invalid("quant table")
			}
			res[c][i] = 1 / v
		}
	}
	return res, nil
}
