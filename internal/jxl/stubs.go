package jxl

import (
	"fmt"

	"gojxl/internal/bitio"
)

func unsupported(what string) error { return fmt.Errorf("%w: %s", ErrUnsupported, what) }

type vardctState struct{}

func newVarDCTState(fd *frameDecoder) *vardctState                      { return &vardctState{} }
func (v *vardctState) readGlobal(br *bitio.Reader) error                { return unsupported("VarDCT") }
func (v *vardctState) decodeLfGroup(br *bitio.Reader, g int) error      { return unsupported("VarDCT") }
func (v *vardctState) decodeHFMeta(br *bitio.Reader, g int) error       { return unsupported("VarDCT") }
func (v *vardctState) readHfGlobal(br *bitio.Reader) error              { return unsupported("VarDCT") }
func (v *vardctState) allocHF(np int)                                   {}
func (v *vardctState) decodePassGroup(br *bitio.Reader, p, g int) error { return unsupported("VarDCT") }
func (v *vardctState) finish() error                                    { return unsupported("VarDCT") }

func (fd *frameDecoder) readPatches(br *bitio.Reader) error { return unsupported("patches") }
func (fd *frameDecoder) readSplines(br *bitio.Reader) error { return unsupported("splines") }
func (fd *frameDecoder) readNoise(br *bitio.Reader) error   { return unsupported("noise") }
func (fd *frameDecoder) applyFeatures() error               { return nil }
func (fd *frameDecoder) applyGaborish()                     {}
func (fd *frameDecoder) applyEPF()                          {}

func unpredictICC(enc []byte) ([]byte, error) { return nil, unsupported("ICC") }
