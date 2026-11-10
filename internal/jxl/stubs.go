package jxl

import (
	"fmt"

	"gojxl/internal/bitio"
)

func unsupported(what string) error { return fmt.Errorf("%w: %s", ErrUnsupported, what) }

func (fd *frameDecoder) readPatches(br *bitio.Reader) error { return unsupported("patches") }
func (fd *frameDecoder) readSplines(br *bitio.Reader) error { return unsupported("splines") }
func (fd *frameDecoder) readNoise(br *bitio.Reader) error   { return unsupported("noise") }
func (fd *frameDecoder) applyFeatures() error               { return nil }

func unpredictICC(enc []byte) ([]byte, error) { return nil, unsupported("ICC") }
