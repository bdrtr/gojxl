package jxl

import "errors"

// JPEGCoeffs holds the quantised DCT data of a VarDCT frame, as needed to
// reconstruct a JPEG file (see Decoder.CaptureJPEG).
type JPEGCoeffs struct {
	BW, BH int        // frame size in (luma) blocks
	HS, VS [3]int     // per channel subsampling shifts
	DC     [3][]int32 // per channel block grid (BW>>HS, BH>>VS)
	// AC per channel: 64 coefficients per block of the channel grid, in
	// JPEG natural order (CfL not yet undone).
	AC     [3][]int32
	YtoX   []int8
	YtoB   []int8
	TW     int // CfL tiles per row
	RawQ   []int32
	RawDen float32
	YCbCr  bool
	UpCode [3]uint32 // jpeg_upsampling code per channel
	OK     bool      // frame is JPEG compatible (DCT8 only, RAW table)
	// CoeffsOnly stops decoding once the coefficients are captured (no
	// pixels); NextFrame then returns ErrCoeffsCaptured.
	CoeffsOnly bool
}

// ErrCoeffsCaptured is returned by NextFrame after a CoeffsOnly capture.
var ErrCoeffsCaptured = errors.New("jxl: JPEG coefficients captured")

func (v *vardctState) captureDC(img *ModularImage, x0, y0 int) {
	cp := v.fd.dec.capture
	if cp == nil {
		return
	}
	chans := [3]*Channel{img.Channels[1], img.Channels[0], img.Channels[2]}
	for c := 0; c < 3; c++ {
		ch := chans[c]
		gw := v.bw >> v.hs[c]
		cx0, cy0 := x0>>v.hs[c], y0>>v.vs[c]
		for y := 0; y < ch.H; y++ {
			copy(cp.DC[c][(cy0+y)*gw+cx0:], ch.Pix[y*ch.W:(y+1)*ch.W])
		}
	}
}

func (v *vardctState) initCapture() {
	cp := v.fd.dec.capture
	if cp == nil {
		return
	}
	*cp = JPEGCoeffs{BW: v.bw, BH: v.bh, HS: v.hs, VS: v.vs, YCbCr: v.fh.DoYCbCr, UpCode: v.fh.JPEGUpsampling, OK: true, CoeffsOnly: cp.CoeffsOnly}
	for c := 0; c < 3; c++ {
		n := (v.bw >> v.hs[c]) * (v.bh >> v.vs[c])
		cp.DC[c] = make([]int32, n)
		cp.AC[c] = make([]int32, 64*n)
	}
}

// captureAC copies the integer coefficients before dequantisation.
func (v *vardctState) captureAC() {
	cp := v.fd.dec.capture
	if cp == nil {
		return
	}
	q := v.qenc[0]
	if q.mode != qmRAW || len(q.raw) != 3*64 {
		cp.OK = false
		return
	}
	cp.RawQ, cp.RawDen = q.raw, q.rawDen
	cp.YtoX, cp.YtoB, cp.TW = v.ytox, v.ytob, v.tw
	for _, gc := range v.groups {
		for vi := range gc.vbs {
			vb := &gc.vbs[vi]
			if vb.typ != 0 { // DCT8 only
				cp.OK = false
				return
			}
			for c := 0; c < 3; c++ {
				if vb.off[c] < 0 {
					continue
				}
				sbx, sby := int(vb.bx)>>v.hs[c], int(vb.by)>>v.vs[c]
				gw := v.bw >> v.hs[c]
				dst := cp.AC[c][(sby*gw+sbx)*64:]
				src := gc.coef[c][vb.off[c] : vb.off[c]+64]
				for k := 0; k < 64; k++ {
					// decoder layout is transposed
					dst[(k%8)*8+k/8] = src[k]
				}
			}
		}
	}
}

// CaptureJPEG makes the decoder record the quantised coefficients of the
// next VarDCT frame it decodes into cp.
func (d *Decoder) CaptureJPEG(cp *JPEGCoeffs) { d.capture = cp }
