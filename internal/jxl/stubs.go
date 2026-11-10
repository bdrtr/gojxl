package jxl

import "fmt"

func unsupported(what string) error { return fmt.Errorf("%w: %s", ErrUnsupported, what) }

func unpredictICC(enc []byte) ([]byte, error) { return nil, unsupported("ICC") }
