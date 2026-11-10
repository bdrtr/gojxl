package jxl

import (
	"fmt"
	"os"
)

var debugOn = os.Getenv("JXLDEBUG") != ""

func dbg(format string, args ...any) {
	if debugOn {
		fmt.Fprintf(os.Stderr, "[jxl] "+format+"\n", args...)
	}
}
