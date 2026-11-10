package jxl

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
