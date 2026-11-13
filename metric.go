package gojxl

import (
	"image"

	"github.com/bdrtr/gojxl/internal/metric"
)

// SSIMULACRA2 returns the SSIMULACRA 2 perceptual score of dist against
// orig: 100 = identical, about 90 = visually lossless, 70 = high quality,
// 50 = medium quality. Both images are taken as sRGB; alpha is blended onto
// a grey background.
func SSIMULACRA2(orig, dist image.Image) (float64, error) {
	return metric.SSIMULACRA2(orig, dist)
}
