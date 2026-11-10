package jxl

import "fmt"

func unsupported(what string) error { return fmt.Errorf("%w: %s", ErrUnsupported, what) }
