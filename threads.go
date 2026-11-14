package gojxl

import "github.com/bdrtr/gojxl/internal/par"

// SetMaxThreads caps the goroutines that decoding and encoding compute on
// at once, for the whole process (n <= 0: no cap beyond GOMAXPROCS, the
// default), and returns the previous cap. With 1, every call runs on its
// caller's goroutine: a server can keep a conversion to one core. The cap
// is read at each parallel step, so a change reaches calls in progress.
// Results do not depend on it.
func SetMaxThreads(n int) int { return par.SetLimit(n) }
