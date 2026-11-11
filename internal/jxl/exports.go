package jxl

import "gojxl/internal/bitio"

// Exports for the encoder (same module).

// DCT8Multipliers returns the default DCT8 dequantisation multipliers
// (1/weight) per channel X, Y, B in coefficient storage layout.
func DCT8Multipliers() [3][]float32 {
	q := defaultQuantEncoding(0)
	t, _ := cachedQuantTable(&q, 0)
	return t
}

// NaturalOrderDCT8 returns the natural coefficient order for DCT8 blocks.
func NaturalOrderDCT8() []int32 { return naturalOrder(0) }

// CoeffFreqCtx and CoeffNumNonzeroCtx are the HF context tables.
func CoeffFreqCtx() []uint8        { return coeffFreqCtx[:] }
func CoeffNumNonzeroCtx() []uint16 { return coeffNumNonzeroCtx[:] }

// DefaultBlockCtxMap is the default block context map (15 clusters).
func DefaultBlockCtxMap() []uint8 { return defaultBlockCtxMap }

// OpsinInverseDefault returns the default XYB inverse matrix and bias.
func OpsinInverseDefault() OpsinInverse { return defaultOpsinInverse() }

// QuantMultipliers returns default dequantisation multipliers for a quant
// table index (0 = DCT8, 4 = DCT16, 5 = DCT32).
func QuantMultipliers(table int) [3][]float32 {
	q := defaultQuantEncoding(table)
	t, _ := cachedQuantTable(&q, table)
	return t
}

// NaturalOrder returns the natural coefficient order for an order id.
func NaturalOrder(id int) []int32 { return naturalOrder(id) }

// ResampleScale is the LLF<->DC scale for an N-point LF DCT.
func ResampleScale(n, k int) float32 { return resampleScale(n, k) }

// UnpredictICC decodes the predicted ICC form (for encoder tests).
func UnpredictICC(enc []byte) ([]byte, error) { return unpredictICC(enc) }

// ReadICC reads an entropy-coded ICC stream and unpredicts it.
func ReadICC(br *bitio.Reader) ([]byte, error) {
	enc, err := readEncodedICC(br)
	if err != nil {
		return nil, err
	}
	return unpredictICC(enc)
}
